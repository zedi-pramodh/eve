#!/bin/sh
#
# Copyright (c) 2026 Zededa, Inc.
# SPDX-License-Identifier: Apache-2.0
#
# pkg/witness entrypoint.
#
# Phase 1.5 layout — TWO STAGES separated by a netns re-exec:
#
#   Stage A (host netns, this container's default):
#     - cgroup / module / route / vault prereqs (setup_prereqs)
#     - bind-mount /persist/vault/witness onto /var/lib (persistent state)
#     - k3s downloaded + unpacked under /var/lib/k3s/bin (install_k3s)
#         ^ NEEDS host networking to curl https://get.k3s.io
#     - config.yaml.d/02-witness-network.yaml (node-ip) rendered
#     - config.yaml.d/01-clusterconfig.yaml (join server+token) rendered
#       when WITNESS_JOIN_URL is set in /persist/witness-override.env
#     - eve-witness netns + veth pair (wit-host <-> wit-eth0) created
#
#   re-exec via `nsenter --net=/var/run/netns/eve-witness -- "$0" "$@"`
#
#   Stage B (eve-witness netns):
#     - witness-private containerd at /run/witness/containerd/containerd.sock
#     - k3s server (standalone Phase 1.5 or joining a cluster)
#     - cordon_witness_node (backgrounded) — marks the Node
#       Ready,SchedulingDisabled once it registers
#
# Why two stages?
#
# The eve-witness netns has NO default route (Phase 1.5 chose plain veth
# with no bridge attachment — see witness-utils.sh:setup_witness_netns).
# So anything that needs external connectivity (the k3s installer's curl,
# DNS, etc.) MUST run before we re-exec into the netns. Persistent state
# (mounts, /var/lib bind, cgroup setup) also lives in the mount namespace,
# which nsenter --net=... does NOT switch — so all of those operations
# stay visible inside the netns exactly as Stage A set them up.
#
# Why nsenter --net=<path> and not `ip netns exec`?
#
# `ip netns exec` internally does an extra `unshare(CLONE_NEWNS)` and
# remounts /sys, which hides the /sys/fs/cgroup bind-mount we inherited
# from the witness linuxkit service. k3s then dies with "cgroups: cgroup
# mountpoint does not exist". nsenter --net preserves the mount namespace
# entirely.

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

    # --cluster-init is mutually exclusive with joining an existing cluster.
    # is_witness_joining returns true when WITNESS_JOIN_URL is set in the
    # override file AND render_witness_cluster_config has written the
    # 01-clusterconfig.yaml (server: + token:). In that case k3s reads the
    # join inputs from the config file and we omit --cluster-init from the
    # CLI so the witness joins instead of forms a new cluster.
    #
    # --node-name eve-witness is also in 00-nodename.yaml as the source of
    # truth; passing it on the CLI is harmless (k3s accepts both) but is
    # NOT load-bearing for process identification — see PID file dance.
    if is_witness_joining; then
        logmsg "Joining cluster at ${WITNESS_JOIN_URL} (no --cluster-init)"
        nohup /usr/bin/k3s server \
            --node-name eve-witness \
            >> "${K3S_LOG_DIR}/${WITNESS_LOG_FILE}" 2>&1 &
    else
        logmsg "Phase 1.5 standalone — using --cluster-init"
        nohup /usr/bin/k3s server \
            --node-name eve-witness \
            --cluster-init \
            >> "${K3S_LOG_DIR}/${WITNESS_LOG_FILE}" 2>&1 &
    fi
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

# =====================================================================
# Stage A — host netns. Runs once per container start. Ends with a
# re-exec into the eve-witness netns; nothing below the `exec nsenter`
# at the bottom of this block runs in Stage A.
# =====================================================================
if [ "${WITNESS_IN_NETNS:-no}" != "yes" ]; then
    DATESTR=$(date)
    mkdir -p "$K3S_LOG_DIR" /run/witness
    echo "========================== $DATESTR ==========================" >> "$INSTALL_LOG"
    logmsg "pkg/witness Stage A (host netns) starting (node=${WITNESS_NODE_NAME} ip=${WITNESS_NODE_IP})"

    setup_prereqs

    # Detect Phase 1.5 ↔ Phase 2 mode transitions and wipe stale cluster
    # state when the override file flips between standalone and a
    # specific join URL (or between two different join URLs). MUST run
    # after setup_prereqs (which mounts /var/lib from the vault — that's
    # where both the mode marker and the k3s state live) and BEFORE
    # install_k3s (no point re-extracting binaries onto a doomed db).
    # No-op when the mode matches the marker.
    witness_check_mode_transition

    # Install k3s (with retry — first boot may not have DNS yet). This also
    # triggers k3s's self-extraction of /var/lib/rancher/k3s/data/current/bin/
    # which gives us the containerd + runc + shim binaries we need below.
    # MUST run in host netns: curl needs the host default route.
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

    # If WITNESS_JOIN_URL / WITNESS_JOIN_TOKEN are set in the override file,
    # write the join overlay (server + token) to config.yaml.d/01-clusterconfig.yaml.
    # When unset, this also clears any stale join overlay from a prior boot so
    # the witness reverts to Phase 1.5 standalone cleanly.
    render_witness_cluster_config

    # Create the netns + veth pair. MUST run in host netns: `ip netns add`
    # binds /proc/<pid>/ns/net into /var/run/netns/ which itself requires
    # being in the host netns. setup_witness_netns is idempotent across
    # container restarts (netns survives in host /run/netns/ until reboot).
    if ! setup_witness_netns; then
        logmsg "FATAL: setup_witness_netns failed"
        echo "FATAL: setup_witness_netns failed; cannot start witness in isolated netns" >&2
        exit 1
    fi

    export WITNESS_IN_NETNS=yes
    logmsg "Stage A complete; re-execing into eve-witness netns for Stage B"
    exec nsenter --net=/var/run/netns/eve-witness -- "$0" "$@"
fi

# =====================================================================
# Stage B — inside the eve-witness netns. Only the supervised services
# (containerd + k3s) live here.
# =====================================================================
logmsg "pkg/witness Stage B (eve-witness netns) starting"

# Note: there is no cordon_witness_node call here in --disable-agent
# mode. The witness has no Node object to cordon (and never will —
# kubelet is disabled). See config.yaml's long comment block for the
# architectural rationale. The cordon_witness_node function is kept
# in witness-utils.sh for now as it does no harm dead-code-wise, but
# nothing calls it.

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
