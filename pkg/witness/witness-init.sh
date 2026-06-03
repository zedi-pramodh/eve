#!/bin/sh
#
# Copyright (c) 2026 Zededa, Inc.
# SPDX-License-Identifier: Apache-2.0
#
# pkg/witness entrypoint.
#
# Phase 1 ONLY — this script proves the package builds and bootstraps:
#   - cgroup / module / vault prereqs
#   - dummy interface eve-witness0 with the reserved 10.244.244.244 IP
#   - /persist/vault/witness bind-mounted onto /var/lib (persistent state)
#   - k3s downloaded and unpacked under /var/lib/k3s/bin
#   - witness-private containerd at /run/witness/containerd/containerd.sock
#   - k3s server in dedicated-etcd-node mode:
#       * apiserver / controller-manager / scheduler DISABLED (config.yaml)
#       * kubelet ENABLED — embedded etcd needs a Node object for cluster
#         membership tracking, so --disable-agent is NOT used
#       * kube-proxy + flannel disabled (config.yaml); workloads kept off
#         by NoSchedule + NoExecute taints
#   - Witness's k3s binds to 10.244.244.244 (node-ip), so kubelet :10250,
#     etcd :2379/:2380 all live on the dummy interface — no collision with
#     pkg/kube's k3s on the seed's real interface.
#
# Phase 2 (not in this script): read EdgeNodeClusterStatus, render
# 01-clusterconfig.yaml with server/token, and join the seed's cluster as
# the third etcd member.

# shellcheck source=pkg/witness/witness-utils.sh
. /usr/bin/witness-utils.sh
# shellcheck source=pkg/witness/lib/config.sh
. /usr/bin/witness-config.sh

INITIAL_WAIT_TIME=5
MAX_WAIT_TIME=$((10 * 60))
current_wait_time=$INITIAL_WAIT_TIME
RESTART_COUNT=0

setup_prereqs() {
    mkdir -p "$K3S_LOG_DIR"
    mkdir -p /run/witness /run/witness/containerd
    rm -rf /var/log
    ln -s "$K3S_LOG_DIR" /var/log

    # Modules kubelet + containerd + flannel substrate need available.
    # We don't run flannel, but br_netfilter / overlay are still expected
    # by kubelet's sandbox setup.
    modprobe dummy        2>/dev/null || true
    modprobe br_netfilter 2>/dev/null || true
    modprobe overlay      2>/dev/null || true

    mkdir -p /run/lock
    chmod o+rw /dev/null
    mount --make-rshared / 2>/dev/null || true
    setup_cgroup

    wait_for_default_route

    # Create the dummy interface BEFORE k3s starts — node-ip requires the
    # IP to already exist on a local interface.
    while ! setup_witness_interface; do
        logmsg "Retrying setup_witness_interface in 5s"
        sleep 5
    done

    # Vault must be unsealed before we bind-mount /var/lib — the source
    # path /persist/vault/witness lives inside the encrypted vault.
    wait_for_vault
    while ! mount_witness_root; do
        logmsg "Retrying mount_witness_root in 5s"
        sleep 5
    done
}

start_k3s_once() {
    # Check via PID file, NOT pgrep. k3s strips its argv shortly after
    # startup so a cmdline-based pgrep can't reliably distinguish our k3s
    # from pkg/kube's in the shared host PID namespace.
    if is_witness_k3s_running; then
        current_wait_time=$INITIAL_WAIT_TIME
        return 0
    fi

    RESTART_COUNT=$((RESTART_COUNT + 1))
    logmsg "k3s not running, attempt $RESTART_COUNT, backoff ${current_wait_time}s"
    sleep "$current_wait_time"
    current_wait_time=$((current_wait_time * 2))
    [ "$current_wait_time" -gt "$MAX_WAIT_TIME" ] && current_wait_time=$MAX_WAIT_TIME

    # Phase 1: --cluster-init brings up the etcd cluster (this one member,
    # the witness). config.yaml disables apiserver/controller-manager/
    # scheduler/kube-proxy/flannel and applies the NoSchedule taint.
    # --node-name eve-witness is also in 00-nodename.yaml as the source
    # of truth; passing it on the CLI is harmless (k3s accepts both) but
    # is NOT load-bearing for process identification — see PID file dance.
    nohup /usr/bin/k3s server \
        --node-name eve-witness \
        --cluster-init \
        >> "${K3S_LOG_DIR}/${WITNESS_LOG_FILE}" 2>&1 &
    k3s_pid=$!
    # Write the PID file IMMEDIATELY — that's our only reliable handle on
    # this process once k3s strips its argv.
    mkdir -p "$(dirname "$WITNESS_K3S_PID_FILE")"
    echo "$k3s_pid" > "$WITNESS_K3S_PID_FILE"
    # etcd fsync latency is critical — give the witness's etcd parity with
    # pkg/kube's in IO scheduling.
    ionice -c2 -n0 -p "$k3s_pid" 2>/dev/null || true
    logmsg "Started k3s server (pid=$k3s_pid) as witness ${WITNESS_NODE_NAME} on ${WITNESS_NODE_IP}, pid file: $WITNESS_K3S_PID_FILE"
    return 0
}

DATESTR=$(date)
mkdir -p "$K3S_LOG_DIR"
echo "========================== $DATESTR ==========================" >> "$INSTALL_LOG"
logmsg "pkg/witness starting up (node=${WITNESS_NODE_NAME} ip=${WITNESS_NODE_IP})"

setup_prereqs

# Install k3s (with retry — first boot may not have DNS yet). This also
# triggers k3s's self-extraction of /var/lib/rancher/k3s/data/current/bin/
# which gives us the containerd + runc + shim binaries we need below.
while ! install_k3s; do
    logmsg "k3s install failed, retrying in 10s"
    sleep 10
done
logmsg "k3s ${K3S_VERSION} installed under /var/lib/k3s/bin"

# Render the dynamic node-ip overlay BEFORE the supervisor loop launches
# k3s. If /persist/witness-override.env set a new WITNESS_NODE_IP, that
# value is what lands in config.yaml.d/02-witness-network.yaml here, and
# what k3s binds to on its first start.
render_witness_network_config

# Cordon the witness Node as soon as it registers. Backgrounded so the
# supervisor loop below isn't blocked waiting for the apiserver/Node to
# come up. cordon_witness_node polls up to 5 minutes for the Node object
# and the local kubeconfig; once both are ready, it issues `kubectl cordon`
# and exits. The config.yaml taints already block scheduling — this is the
# operator-visible reinforcement (STATUS shows SchedulingDisabled).
( cordon_witness_node ) &

# Main supervision loop.
#
# Order matters: containerd must be up before k3s server, because kubelet
# (started by k3s) will try to dial WITNESS_CTRD_SOCK as soon as it boots.
# check_start_containerd is idempotent — it's a no-op when our containerd
# is already running.
while true; do
    if ! check_start_containerd; then
        logmsg "containerd not ready, will retry"
        sleep 5
        continue
    fi
    start_k3s_once
    check_log_file_size "$WITNESS_LOG_FILE"
    check_log_file_size "witness-install.log"
    check_log_file_size "witness-containerd.log"
    sleep 15
done
