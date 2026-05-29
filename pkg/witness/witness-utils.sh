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

    # Already running? The config path is a witness-unique substring of the
    # containerd cmdline, so pgrep won't match pkg/kube's containerd.
    if pgrep -f "$WITNESS_CTRD_CONFIG" > /dev/null 2>&1; then
        return 0
    fi

    if [ ! -x /var/lib/rancher/k3s/data/current/bin/containerd ]; then
        logmsg "containerd binary not yet extracted under /var/lib/rancher/k3s/data — waiting"
        return 1
    fi

    mkdir -p /run/witness/containerd /var/lib/witness-containerd
    logmsg "Starting witness-private containerd (socket=$WITNESS_CTRD_SOCK)"
    nohup /var/lib/rancher/k3s/data/current/bin/containerd \
          --config "$WITNESS_CTRD_CONFIG" \
          >> "$WITNESS_CTRD_LOG" 2>&1 &
    ctrd_pid=$!
    logmsg "Started witness containerd pid=$ctrd_pid"
    return 0
}

# Returns 0 if the witness's own k3s server is alive, 1 otherwise. Reads
# the PID we wrote at launch time and validates it's still a k3s process
# (defends against PID reuse).
is_witness_k3s_running() {
    [ -r "$WITNESS_K3S_PID_FILE" ] || return 1
    pid=$(cat "$WITNESS_K3S_PID_FILE" 2>/dev/null)
    [ -n "$pid" ] || return 1
    kill -0 "$pid" 2>/dev/null || return 1
    [ -r "/proc/$pid/comm" ] || return 1
    case "$(cat /proc/$pid/comm 2>/dev/null)" in
        k3s*) return 0 ;;
        *)    return 1 ;;
    esac
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
