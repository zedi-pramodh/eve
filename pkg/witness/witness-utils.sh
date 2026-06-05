#!/bin/sh
#
# Copyright (c) 2026 Zededa, Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Trimmed-down sibling of pkg/kube/cluster-utils.sh — just the helpers the
# witness actually needs for Phase 1 (logging, virtual interface, vault
# mount, k3s install + lifecycle).

# shellcheck source=pkg/witness/lib/config.sh
. /usr/bin/witness-config.sh

K3S_VERSION=v1.34.2+k3s1
# IMPORTANT: pkg/witness and pkg/kube both run with pid: host AND share the
# host network namespace, so a plain pgrep -f "k3s server" inside either
# container matches the OTHER container's k3s. We CANNOT disambiguate by
# cmdline string: k3s strips its own argv shortly after startup, so any
# flag we pass (e.g. --node-name) disappears from /proc/<pid>/cmdline.
# Both sides therefore identify their own k3s via:
#   - witness: PID file owned by us (/run/witness/k3s.pid)
#   - pkg/kube: cgroup membership filter (excludes /eve/services/witness)
# See is_witness_k3s_running() below.
K3S_SERVER_CMD="k3s server"
WITNESS_K3S_PID_FILE="/run/witness/k3s.pid"
K3S_LOG_DIR="/persist/kubelog"
INSTALL_LOG="${K3S_LOG_DIR}/witness-install.log"
WITNESS_LOG_FILE="witness.log"
LOG_SIZE=$((5 * 1024 * 1024))

# Persistent backing store for the witness. Sibling of /persist/vault/kube so
# pkg/kube and pkg/witness keep completely independent k3s state on the same
# physical seed node. Bind-mounted onto /var/lib once the vault is unsealed.
WITNESS_VAULT_ROOT="/persist/vault/witness"
WITNESS_ROOT_MOUNTPOINT="/var/lib"

logmsg() {
    local MSG TIME
    MSG="$*"
    TIME=$(date +"%F %T")
    echo "$TIME : $MSG" >> "$INSTALL_LOG"
}

setup_cgroup() {
    echo "cgroup /sys/fs/cgroup cgroup defaults 0 0" >> /etc/fstab
}

wait_for_default_route() {
    while read -r iface dest gw flags refcnt use metric mask mtu window irtt; do
        if [ "$dest" = "00000000" ] && [ "$mask" = "00000000" ]; then
            logmsg "Default route found"
            return 0
        fi
        logmsg "waiting for default route $iface $dest $gw $flags $refcnt $use $metric $mask $mtu $window $irtt"
        sleep 1
    done < /proc/net/route
    return 1
}

# Create the eve-witness network namespace and a macvlan child interface
# of the cluster interface. Idempotent across container restarts/reboots
# (the netns survives on host /run/netns/ as long as something keeps the
# bind-mount alive).
#
# Inputs (from lib/config.sh + /persist/witness-override.env):
#   WITNESS_NODE_IP, WITNESS_NODE_PREFIX, WITNESS_IFACE, WITNESS_GATEWAY
#
# Notes:
# - pkg/witness service has `rootfsPropagation: shared` + `/run:/run` bind,
#   which makes the bind-mount that `ip netns add` creates at /run/netns/X
#   propagate to host. (Onboot containers don't honour these — that's why
#   the earlier onboot-based attempt failed.)
# - We deliberately do NOT switch the witness process into this netns
#   here. Phase 1.5 step 1 just creates the netns; step 2 moves k3s into
#   it via build.yml `net: eve-witness`.
WITNESS_NETNS="eve-witness"
WITNESS_VETH_NETNS="wit-eth0"   # peer name INSIDE the netns
WITNESS_VETH_HOST="wit-host"    # peer name in host netns (no IP, no bridge)

# Phase 1.5 networking choice — plain veth pair, NOT attached to any
# EVE-managed bridge (eth0, cni0, bn1, etc.).
#
# History / why we don't macvlan eth0:
# We initially tried `ip link add wit-eth0 link eth0 type macvlan mode
# bridge` to give the witness a real L2 endpoint on the cluster network.
# That broke the device — `eth0` in EVE is actually a Linux bridge owned
# by pillar (with the physical NIC keth0 as its only port), and macvlan
# children of a bridge cause traffic disruption that took eth0 down.
#
# Plain veth-pair with no bridge attachment:
#   - Touches zero pillar-managed interfaces. Safe.
#   - Witness inside the netns has WITNESS_NODE_IP on wit-eth0, can reach
#     ITSELF and apiserver/etcd bound to that IP from inside the netns.
#   - No external connectivity — but Phase 1.5 standalone doesn't need it
#     (cordoned + tainted node never pulls images, no DNS lookups).
#   - For Phase 2 join later, we'll add a separate connectivity step
#     (probably bridge the host end into a pillar-coordinated subnet or
#     have pillar set up the attachment per ENC).
setup_witness_netns() {
    logmsg "setup_witness_netns: ip=$WITNESS_NODE_IP (Phase 1.5 standalone via plain veth, NO bridge attachment)"

    # 1. Create the netns if it doesn't exist.
    if ! ip netns list | awk '{print $1}' | grep -qx "$WITNESS_NETNS"; then
        if ! ip netns add "$WITNESS_NETNS"; then
            logmsg "ERROR: ip netns add $WITNESS_NETNS failed"
            return 1
        fi
        logmsg "Created netns $WITNESS_NETNS"
    fi

    # 2. Create the veth pair (host side + netns side) if not present, and
    #    move the netns side into the netns.
    if ! ip netns exec "$WITNESS_NETNS" ip link show "$WITNESS_VETH_NETNS" >/dev/null 2>&1; then
        # Clean up any stale host-side veth from a prior attempt.
        ip link show "$WITNESS_VETH_HOST"  >/dev/null 2>&1 && ip link del "$WITNESS_VETH_HOST"  2>/dev/null
        ip link show "$WITNESS_VETH_NETNS" >/dev/null 2>&1 && ip link del "$WITNESS_VETH_NETNS" 2>/dev/null
        if ! ip link add "$WITNESS_VETH_HOST" type veth peer name "$WITNESS_VETH_NETNS"; then
            logmsg "ERROR: ip link add veth pair failed"
            return 1
        fi
        ip link set "$WITNESS_VETH_NETNS" netns "$WITNESS_NETNS"
        logmsg "Created veth pair ($WITNESS_VETH_HOST <-> $WITNESS_VETH_NETNS) and moved netns side into $WITNESS_NETNS"
    fi

    # 3. Host-side end stays in host netns, brought UP but UNATTACHED to
    #    any bridge (intentional — keeps us off pillar's eth0/bn1/cni0).
    ip link set "$WITNESS_VETH_HOST" up 2>/dev/null

    # 4. Inside the netns: assign IP, bring iface + loopback up.
    if ! ip netns exec "$WITNESS_NETNS" ip addr show dev "$WITNESS_VETH_NETNS" | grep -qw "$WITNESS_NODE_IP"; then
        ip netns exec "$WITNESS_NETNS" ip addr add "${WITNESS_NODE_IP}${WITNESS_NODE_PREFIX:-/32}" dev "$WITNESS_VETH_NETNS"
        logmsg "Assigned ${WITNESS_NODE_IP}${WITNESS_NODE_PREFIX:-/32} to $WITNESS_VETH_NETNS"
    fi
    ip netns exec "$WITNESS_NETNS" ip link set "$WITNESS_VETH_NETNS" up
    ip netns exec "$WITNESS_NETNS" ip link set lo up

    # Stub default route. flannel's GetDefaultGatewayInterface() walks
    # /proc/net/route looking for a 0.0.0.0/0 entry to pick its public
    # interface; without one it exits with "Unable to find default route"
    # and k3s shuts down. The witness is single-node + cordoned + tainted,
    # so vxlan endpoints get created but no traffic ever flows over them —
    # the route just needs to *exist*, not actually reach anywhere. `ip
    # route replace` is idempotent across container restarts.
    #
    # Phase 2 will REPLACE this stub with a real route to the seed when
    # the witness joins an existing cluster (via pillar-coordinated
    # bridge attachment or similar). Until then, this is purely to
    # appease flannel's startup check.
    ip netns exec "$WITNESS_NETNS" ip route replace default dev "$WITNESS_VETH_NETNS"

    logmsg "setup_witness_netns: done"
    return 0
}

# Create a dummy interface that carries the reserved witness IP
# (10.244.244.244). This is what lets the witness's k3s bind apiserver/etcd
# on the same default ports as pkg/kube's k3s — they're distinguished by IP,
# not port. Idempotent: re-running on container restart is a no-op once the
# interface already exists with the right address.
setup_witness_interface() {
    if ! ip link show "$WITNESS_IFACE" > /dev/null 2>&1; then
        logmsg "Creating dummy interface $WITNESS_IFACE"
        if ! ip link add dev "$WITNESS_IFACE" type dummy; then
            logmsg "ERROR: failed to create dummy interface $WITNESS_IFACE"
            return 1
        fi
    fi
    if ! ip addr show dev "$WITNESS_IFACE" | grep -q "$WITNESS_NODE_IP"; then
        logmsg "Assigning $WITNESS_NODE_IP/32 to $WITNESS_IFACE"
        if ! ip addr add "$WITNESS_NODE_IP/32" dev "$WITNESS_IFACE"; then
            logmsg "ERROR: failed to assign $WITNESS_NODE_IP to $WITNESS_IFACE"
            return 1
        fi
    fi
    ip link set "$WITNESS_IFACE" up
    logmsg "Witness interface $WITNESS_IFACE ready with $WITNESS_NODE_IP"
    return 0
}

# Render the k3s "node network" overlay config from the current shell
# variables (WITNESS_NODE_IP). Called from witness-init.sh after config.sh
# has been sourced (with any /persist/witness-override.env values applied)
# and BEFORE the supervisor loop launches k3s. Idempotent — rewriting the
# same content on a no-op restart is safe; k3s only reads it on its own
# start.
#
# Why this is generated, not pinned: /etc/rancher/k3s/config.yaml is baked
# into the image (overlay-only, see design doc §8a "config.yaml.d is in
# the container overlay") so the only way to change node-ip without a
# rebuild is to keep node-ip out of the main config and render it here
# from a variable that the override file can flip.
render_witness_network_config() {
    mkdir -p "$K3S_CONFIG_DIR"
    cat > "$K3S_NETWORK_CONFIG_FILE" <<EOF
---
# AUTO-GENERATED by witness-utils.sh:render_witness_network_config.
# Source of truth: \$WITNESS_NODE_IP in /usr/bin/witness-config.sh,
# overridable via /persist/witness-override.env.
# Edits here are lost on the next container start. Do NOT edit by hand.
node-ip: "$WITNESS_NODE_IP"
EOF
    logmsg "Rendered $K3S_NETWORK_CONFIG_FILE (node-ip=$WITNESS_NODE_IP)"
}

# Render config.yaml.d/01-clusterconfig.yaml when WITNESS_JOIN_URL is set
# in the override file. Presence of this file (with a "server:" line)
# tells k3s "join the cluster at this URL" instead of "init a new cluster".
# start_k3s_once also drops --cluster-init from its CLI when this file
# exists.
#
# When join inputs are NOT set, this function removes any stale
# 01-clusterconfig.yaml left from a prior boot so the witness reverts to
# standalone (Phase 1) cleanly.
render_witness_cluster_config() {
    mkdir -p "$K3S_CONFIG_DIR"
    if [ -z "${WITNESS_JOIN_URL:-}" ]; then
        if [ -f "$K3S_CLUSTER_CONFIG_FILE" ]; then
            rm -f "$K3S_CLUSTER_CONFIG_FILE"
            logmsg "Removed stale $K3S_CLUSTER_CONFIG_FILE (no WITNESS_JOIN_URL set — Phase 1 standalone)"
        fi
        return 0
    fi
    if [ -z "${WITNESS_JOIN_TOKEN:-}" ]; then
        logmsg "ERROR: WITNESS_JOIN_URL set but WITNESS_JOIN_TOKEN is empty — refusing to render join config"
        return 1
    fi
    cat > "$K3S_CLUSTER_CONFIG_FILE" <<EOF
---
# AUTO-GENERATED by witness-utils.sh:render_witness_cluster_config.
# Source of truth: WITNESS_JOIN_URL / WITNESS_JOIN_TOKEN in
# /persist/witness-override.env. Edits here are lost on the next container
# start. Do NOT edit by hand.
server: "${WITNESS_JOIN_URL}"
token: "${WITNESS_JOIN_TOKEN}"
EOF
    logmsg "Rendered $K3S_CLUSTER_CONFIG_FILE (join server=${WITNESS_JOIN_URL})"
}

# True when we have join inputs and an 01-clusterconfig.yaml was written.
# Used by start_k3s_once to decide whether to pass --cluster-init.
is_witness_joining() {
    [ -n "${WITNESS_JOIN_URL:-}" ] && [ -f "$K3S_CLUSTER_CONFIG_FILE" ]
}

wait_for_vault() {
    logmsg "Waiting for /persist/vault to be readable"
    while [ ! -d /persist/vault ] || ! ls /persist/vault > /dev/null 2>&1; do
        sleep 1
    done
    logmsg "Vault ready"
}

# Bind-mount the witness vault directory onto /var/lib so k3s state
# (/var/lib/rancher/k3s/...) survives reboots. Must be called *after*
# wait_for_vault. Idempotent: safe to call on container restart.
mount_witness_root() {
    if mountpoint -q "$WITNESS_ROOT_MOUNTPOINT"; then
        logmsg "$WITNESS_ROOT_MOUNTPOINT already a mountpoint, skipping bind"
        return 0
    fi
    mkdir -p "$WITNESS_VAULT_ROOT"
    if ! mount --bind "$WITNESS_VAULT_ROOT" "$WITNESS_ROOT_MOUNTPOINT"; then
        logmsg "ERROR: failed to bind $WITNESS_VAULT_ROOT -> $WITNESS_ROOT_MOUNTPOINT"
        return 1
    fi
    logmsg "Bound $WITNESS_VAULT_ROOT onto $WITNESS_ROOT_MOUNTPOINT"
    return 0
}

check_log_file_size() {
    [ -f "$K3S_LOG_DIR/$1" ] || return 0
    currentSize=$(wc -c < "$K3S_LOG_DIR/$1")
    if [ "$currentSize" -gt "$LOG_SIZE" ]; then
        [ -f "$K3S_LOG_DIR/$1.2" ] && cp -p "$K3S_LOG_DIR/$1.2" "$K3S_LOG_DIR/$1.3"
        [ -f "$K3S_LOG_DIR/$1.1" ] && cp -p "$K3S_LOG_DIR/$1.1" "$K3S_LOG_DIR/$1.2"
        cp -p "$K3S_LOG_DIR/$1" "$K3S_LOG_DIR/$1.1"
        truncate -s 0 "$K3S_LOG_DIR/$1"
        logmsg "witness logfile $1, size $currentSize rotated"
    fi
}

# Pull and unpack the pinned k3s release. /var/lib is bind-mounted to
# WITNESS_VAULT_ROOT (/persist/vault/witness), so everything we write here —
# the binary under /var/lib/k3s/bin, the install marker, and later the
# runtime data under /var/lib/rancher/k3s — survives reboots. The witness
# binary path mirrors pkg/kube (/var/lib/k3s/bin/k3s), but because each
# container has its own /var/lib mount namespace there's no collision with
# pkg/kube's copy.
install_k3s() {
    if [ -f /var/lib/k3s_installed_unpacked ]; then
        [ -x /var/lib/k3s/bin/k3s ] && ln -sf /var/lib/k3s/bin/k3s /usr/bin/k3s
        return 0
    fi
    logmsg "Installing k3s $K3S_VERSION for witness"
    mkdir -p /var/lib/k3s/bin
    k3s_installer=/tmp/k3s-install.sh
    if ! curl -sfL https://get.k3s.io -o "$k3s_installer"; then
        logmsg "k3s installer download failed"
        return 1
    fi
    chmod +x "$k3s_installer"
    if ! INSTALL_K3S_VERSION=${K3S_VERSION} \
         INSTALL_K3S_SKIP_ENABLE=true \
         INSTALL_K3S_SKIP_START=true \
         INSTALL_K3S_BIN_DIR=/var/lib/k3s/bin \
         "$k3s_installer"; then
        logmsg "k3s installer failed"
        return 1
    fi
    ln -sf /var/lib/k3s/bin/k3s /usr/bin/k3s
    /usr/bin/k3s check-config >> "$INSTALL_LOG" 2>&1 || true
    touch /var/lib/k3s_installed_unpacked
    return 0
}

# Spawn the witness-private containerd that kubelet will talk to. Mirrors
# pkg/kube's check_start_containerd but uses witness-specific paths so the
# two packages don't share state or socket. Idempotent: if a containerd is
# already running with our config path in its cmdline, return without
# launching a new one.
check_start_containerd() {
    # k3s ships runc and the shim under /var/lib/rancher/k3s/data/current/bin
    # (populated by install_k3s after `k3s check-config`). Symlink them into
    # /usr/bin so containerd can exec them without a custom PATH.
    if [ ! -L /usr/bin/runc ] && [ -x /var/lib/rancher/k3s/data/current/bin/runc ]; then
        ln -sf /var/lib/rancher/k3s/data/current/bin/runc /usr/bin/runc
    fi
    if [ ! -L /usr/bin/containerd-shim-runc-v2 ] && \
       [ -x /var/lib/rancher/k3s/data/current/bin/containerd-shim-runc-v2 ]; then
        ln -sf /var/lib/rancher/k3s/data/current/bin/containerd-shim-runc-v2 \
               /usr/bin/containerd-shim-runc-v2
    fi

    # Exec containerd from a witness-private path so /proc/<pid>/cmdline
    # is *visibly* the witness's, not pkg/kube's. Both packages would
    # otherwise share the path string "/var/lib/rancher/k3s/data/current/bin
    # /containerd" (each has its own /var/lib bind, but the in-container
    # path is the same), and any pgrep on that string in pkg/kube would
    # falsely match the witness's containerd — silently suppressing
    # pkg/kube's "containerd died, restart it" supervisor. We park a
    # symlink under /var/lib/witness/bin/ (persistent: /var/lib is bound
    # to /persist/vault/witness) and exec from there.
    mkdir -p "$WITNESS_CTRD_BIN_DIR"
    if [ ! -L "$WITNESS_CTRD_BIN" ] && \
       [ -x /var/lib/rancher/k3s/data/current/bin/containerd ]; then
        ln -sf /var/lib/rancher/k3s/data/current/bin/containerd "$WITNESS_CTRD_BIN"
    fi

    # Already running? The binary path itself is now witness-unique, so a
    # straight pgrep on $WITNESS_CTRD_BIN cannot match pkg/kube's containerd.
    if pgrep -f "$WITNESS_CTRD_BIN" > /dev/null 2>&1; then
        return 0
    fi

    if [ ! -x "$WITNESS_CTRD_BIN" ]; then
        logmsg "witness containerd binary not yet linked at $WITNESS_CTRD_BIN — waiting"
        return 1
    fi

    mkdir -p /run/witness/containerd /var/lib/witness-containerd
    logmsg "Starting witness-private containerd (socket=$WITNESS_CTRD_SOCK)"
    nohup "$WITNESS_CTRD_BIN" \
          --config "$WITNESS_CTRD_CONFIG" \
          >> "$WITNESS_CTRD_LOG" 2>&1 &
    ctrd_pid=$!
    logmsg "Started witness containerd pid=$ctrd_pid"
    return 0
}

# Drop a stub CNI conflist for kubelet to find. Without ANY CNI config in
# /etc/cni/net.d, kubelet reports NetworkPluginNotReady and the node
# stays NotReady — even though the witness has no pod-networking needs
# (cordoned + tainted, no workloads will ever land here). The stub uses
# only the loopback CNI plugin (ships with k3s) so it satisfies kubelet's
# "is there a CNI" check without actually programming any pod networking.
# Without this, we'd need real flannel — which collides with pkg/kube's
# flannel.1 device when co-located.
render_witness_cni_stub() {
    # Witness's containerd-config.toml points CNI conf_dir at
    # /var/lib/rancher/k3s/agent/etc/cni/net.d (and bin_dir at
    # /var/lib/rancher/k3s/data/current/bin), NOT the standard
    # /etc/cni/net.d. Match containerd's expectation.
    cni_conf_dir=/var/lib/rancher/k3s/agent/etc/cni/net.d
    mkdir -p "$cni_conf_dir"
    cat > "$cni_conf_dir/00-witness-stub.conflist" <<'EOF'
{
  "cniVersion": "0.4.0",
  "name": "witness-stub",
  "plugins": [
    { "type": "loopback" }
  ]
}
EOF
    logmsg "Rendered CNI stub at $cni_conf_dir/00-witness-stub.conflist"
}

# Mark the witness Node as Unschedulable once k3s has registered it. The
# config.yaml taints (NoSchedule + NoExecute on
# node-role.kubernetes.io/witness=true and CriticalAddonsOnly=true) already
# block scheduling, but cordoning is the explicit, operator-visible signal
# in `kubectl get nodes` (STATUS=Ready,SchedulingDisabled). Idempotent —
# `kubectl cordon` on an already-cordoned node is a no-op.
#
# Called once after the first successful k3s start (see witness-init.sh).
# Re-cordon on every boot is intentional: if an operator accidentally
# uncordons the witness, the next restart restores the safety property.
cordon_witness_node() {
    # Retry forever — k3s on the witness can take a long time to come up on
    # busy hosts, and a 5-minute timeout that gives up was leaving the
    # witness uncordoned across long crash-loop windows. The cost of
    # polling is one kubectl-get every 10 seconds; well worth it.
    while true; do
        # Need both the kubeconfig and the Node object before kubectl works.
        if [ -r /etc/rancher/k3s/k3s.yaml ] && \
           /usr/bin/k3s kubectl get node "$WITNESS_NODE_NAME" \
               --kubeconfig=/etc/rancher/k3s/k3s.yaml >/dev/null 2>&1; then
            if /usr/bin/k3s kubectl cordon "$WITNESS_NODE_NAME" \
                   --kubeconfig=/etc/rancher/k3s/k3s.yaml >> "$INSTALL_LOG" 2>&1; then
                logmsg "Cordoned ${WITNESS_NODE_NAME} (SchedulingDisabled)"
                return 0
            fi
        fi
        sleep 10
    done
}

# Returns 0 if the witness's own k3s server is alive, 1 otherwise. Reads
# the PID we wrote at launch time and validates it's still a k3s process
# (defends against PID reuse).
#
# Zombie handling: witness-init.sh launches k3s with `nohup ... &` and never
# calls `wait`, so when k3s exits the child sits as a zombie in our process
# table. For a zombie, `kill -0` still succeeds and `/proc/$pid/comm` still
# reads "k3s" — so without the explicit state check below we'd report the
# dead k3s as running and never restart it. Field 3 of /proc/$pid/stat is
# the process state; "Z" means zombie.
is_witness_k3s_running() {
    [ -r "$WITNESS_K3S_PID_FILE" ] || return 1
    pid=$(cat "$WITNESS_K3S_PID_FILE" 2>/dev/null)
    [ -n "$pid" ] || return 1
    kill -0 "$pid" 2>/dev/null || return 1
    [ -r "/proc/$pid/comm" ] || return 1
    case "$(cat /proc/$pid/comm 2>/dev/null)" in
        k3s*) ;;
        *) return 1 ;;
    esac
    # Reject zombies — read state from /proc/$pid/stat (field after the
    # parenthesised comm). Reap the zombie while we're here so it doesn't
    # accumulate across restart loops.
    # /proc/$pid/stat is "pid (comm) state ...". comm can contain spaces and
    # parens, so we scan backwards for the last field that ENDS in ")" — the
    # state is the field immediately after it.
    state=$(awk '{ for (i=NF;i>=1;i--) if ($i ~ /\)$/) { print $(i+1); exit } }' \
            "/proc/$pid/stat" 2>/dev/null)
    if [ "$state" = "Z" ]; then
        wait "$pid" 2>/dev/null || true
        return 1
    fi
    return 0
}

# Terminate ONLY our k3s — read the PID file, signal that PID. Never use
# pgrep here: it would also match pkg/kube's k3s in the shared host PID
# namespace.
terminate_k3s() {
    is_witness_k3s_running || return 0
    pid=$(cat "$WITNESS_K3S_PID_FILE")
    max_attempts=4
    attempt=0
    while [ $attempt -lt $max_attempts ]; do
        kill -0 "$pid" 2>/dev/null || return 0
        if [ $attempt -lt 3 ]; then
            kill "$pid" 2>/dev/null
        else
            kill -9 "$pid" 2>/dev/null
        fi
        sleep 1
        attempt=$((attempt + 1))
    done
    kill -0 "$pid" 2>/dev/null && return 1
    return 0
}
