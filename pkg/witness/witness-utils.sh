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
WITNESS_VETH_NETNS="wit-eth0"   # interface name INSIDE the netns (both modes)
WITNESS_VETH_HOST="wit-host"    # host-side veth peer name (standalone only)

# Set up the eve-witness network namespace and its sole interface
# (wit-eth0). Both Phase 1.5 and Phase 2 use a plain veth pair; the only
# difference is what wit-host (the host-side end) attaches to:
#
#   Phase 1.5 (WITNESS_JOIN_URL unset — standalone):
#     wit-host is in host netns, UP but with no IP and no bridge
#     attachment. Witness is unreachable from outside the host — fine
#     for a self-contained single-member etcd cluster.
#
#   Phase 2 (WITNESS_JOIN_URL set — join mode):
#     wit-host is attached as a port on WITNESS_IFACE (the EVE cluster
#     bridge — `eth0` by default). This puts wit-eth0 on the same L2
#     segment as the host's cluster IP, allowing the witness to reach
#     the seed (same-host) AND any other physical node on the cluster
#     L2 (cross-host). WITNESS_GATEWAY is optional — only needed if
#     the witness will talk to anything outside its immediate subnet.
#
# Why bridge-port and not macvlan:
#   - macvlan-of-eth0 (the bridge): macvlan children of a Linux bridge
#     cause traffic disruption. We tried and it took eth0 offline.
#   - macvlan-of-keth0 (the physical NIC): works for a witness on a
#     SEPARATE physical device, but fails on the same-host case where
#     witness and seed share a wire — kernel refuses to deliver frames
#     from a macvlan child to the parent NIC's host IP. The seed running
#     on the host stack of the same NIC would be unreachable from the
#     witness inside the netns.
#   - Bridge-port attachment: a Linux bridge naturally forwards frames
#     between all its ports including the bridge's own host-stack
#     interface (eth0 itself). Works for both same-host and cross-host.
#
# Pillar coordination: pillar manages eth0 the bridge in EVE. Adding
# wit-host as a foreign port is a standard kernel op; pillar may or
# may not detect+evict it. If pillar does evict the port post-boot,
# witness's connectivity to the cluster breaks until next container
# restart. We'd need a watchdog that re-adds the port — out of scope
# for the initial join implementation; document as a known risk.
#
# Tear down + recreate on every Stage-A run rather than checking for
# matching existing config. Costs a few ms of `ip link` churn but
# guarantees correctness when mode flips between boots.
setup_witness_netns() {
    local mode
    if [ -n "${WITNESS_JOIN_URL:-}" ]; then
        mode="join"
    else
        mode="standalone"
    fi
    logmsg "setup_witness_netns: mode=$mode ip=${WITNESS_NODE_IP}${WITNESS_NODE_PREFIX:-/32} bridge=${WITNESS_IFACE:-<unused>}"

    # 1. Create the netns if missing. The netns itself persists across
    #    container restarts (lives in /run/netns/ which is bind-mounted
    #    from host); we just need to ensure it exists.
    if ! ip netns list | awk '{print $1}' | grep -qx "$WITNESS_NETNS"; then
        if ! ip netns add "$WITNESS_NETNS"; then
            logmsg "ERROR: ip netns add $WITNESS_NETNS failed"
            return 1
        fi
        logmsg "Created netns $WITNESS_NETNS"
    fi

    # 2. Tear down any existing wit-eth0 inside the netns + any stale
    #    host-side wit-host. Idempotent (no-op if already absent).
    #    Deleting wit-eth0 inside the netns also cleans up the veth
    #    peer in host netns automatically.
    ip netns exec "$WITNESS_NETNS" ip link del "$WITNESS_VETH_NETNS" 2>/dev/null
    ip link del "$WITNESS_VETH_HOST" 2>/dev/null

    # 3. Create the veth pair (same in both modes).
    #
    # MAC pinning — derive a STABLE MAC from WITNESS_NODE_IP so the
    # interface keeps the same L2 identity across container restarts.
    # Default `ip link add type veth` generates a fresh random MAC on
    # every call, which:
    #   - invalidates ARP caches on every cluster peer at each restart
    #   - causes traffic from peers using the stale MAC to silently
    #     vanish until they re-ARP (asymmetric reachability for ~60-300s
    #     per cycle, depending on each peer's ARP cache aging)
    #
    # MAC format: 02:WI:TN:<3 IP octets in hex>. The 02 prefix has the
    # locally-administered bit set (no IEEE OUI conflict). "WI:TN" is
    # 0x57:0x49:0x54:0x4E hex marker ("WITN" in ASCII) for grep-ability.
    # We use only the last 3 octets of the IP so a /28 IP swap during
    # Phase 2 cloud rotation produces a deterministically different MAC.
    local ip_oct2 ip_oct3 ip_oct4 wit_mac
    ip_oct2=$(printf '%02x' "$(echo "$WITNESS_NODE_IP" | cut -d. -f2)")
    ip_oct3=$(printf '%02x' "$(echo "$WITNESS_NODE_IP" | cut -d. -f3)")
    ip_oct4=$(printf '%02x' "$(echo "$WITNESS_NODE_IP" | cut -d. -f4)")
    wit_mac="02:57:49:${ip_oct2}:${ip_oct3}:${ip_oct4}"
    # Host-side MAC offset by setting the unicast bit differently —
    # keep the same first 4 octets but flip a low bit so wit-host and
    # wit-eth0 have distinct (but related) MACs.
    local host_mac="06:57:49:${ip_oct2}:${ip_oct3}:${ip_oct4}"

    if ! ip link add "$WITNESS_VETH_HOST" address "$host_mac" \
                     type veth peer name "$WITNESS_VETH_NETNS" address "$wit_mac"; then
        logmsg "ERROR: ip link add veth pair failed"
        return 1
    fi
    ip link set "$WITNESS_VETH_NETNS" netns "$WITNESS_NETNS"
    logmsg "Created veth pair ($WITNESS_VETH_HOST [MAC $host_mac] <-> $WITNESS_VETH_NETNS [MAC $wit_mac])"

    # 4. Host-side attachment differs per mode.
    if [ "$mode" = "join" ]; then
        # Defensive: WITNESS_IFACE must be set AND must be a Linux
        # bridge. Validating these here gives a clear error message
        # instead of an opaque `ip link set master` failure later.
        if [ -z "${WITNESS_IFACE:-}" ]; then
            logmsg "ERROR: WITNESS_IFACE not set; required for join mode (must be the cluster bridge name, typically 'eth0')"
            return 1
        fi
        if ! ip link show "$WITNESS_IFACE" >/dev/null 2>&1; then
            logmsg "ERROR: WITNESS_IFACE=$WITNESS_IFACE does not exist on the host"
            return 1
        fi
        # Bridge detection via netlink (ip -d link), NOT via
        # /sys/class/net/<iface>/bridge directory test. The /sys path
        # only works from the host's mount namespace; inside the
        # witness container, /sys is mounted independently and the
        # bridge subdirectory is not always exposed even when the
        # interface is in fact a Linux bridge in the kernel. Netlink
        # queries always reflect the kernel's view of the interface
        # type, regardless of sysfs presentation.
        if ! ip -d link show "$WITNESS_IFACE" 2>/dev/null | grep -q "bridge_id"; then
            logmsg "ERROR: WITNESS_IFACE=$WITNESS_IFACE is not a Linux bridge. Phase 2 requires a bridge (typically EVE's eth0) so wit-host can be attached as a port. Available bridges on this host:"
            for iface in $(ip -o link show 2>/dev/null | awk -F': ' '{print $2}' | awk '{print $1}'); do
                if ip -d link show "$iface" 2>/dev/null | grep -q "bridge_id"; then
                    logmsg "  - $iface"
                fi
            done
            logmsg "Set WITNESS_IFACE in /persist/witness-override.env to one of the above."
            return 1
        fi
        # Attach wit-host as a port of the cluster bridge, and set the
        # brport flooding flags. Uses ensure_wit_host_attached so the
        # exact same code path is reused by the supervisor's periodic
        # guard (see below).
        if ! ensure_wit_host_attached; then
            logmsg "ERROR: failed to attach $WITNESS_VETH_HOST to bridge $WITNESS_IFACE"
            return 1
        fi
        ip link set "$WITNESS_VETH_HOST" up

        # Disable bridge-netfilter so frames forwarded between bridge
        # ports (wit-host <-> keth0) are NOT processed by iptables.
        # See git log for the original problem statement (pillar's
        # FORWARD chain doesn't know about wit-host as a legitimate
        # bridge port, silently drops frames).
        #
        # IMPORTANT: this MUST be done per-bridge (sysfs attribute on
        # /sys/class/net/<br>/bridge/nf_call_iptables), NOT via the
        # global /proc/sys/net/bridge/bridge-nf-call-iptables sysctl.
        #
        # Earlier versions of this code disabled the GLOBAL sysctls.
        # That broke the entire cluster: those sysctls are host-wide,
        # so disabling them stops the kernel from invoking iptables on
        # ANY bridged frames — including pod traffic crossing cni0/
        # flannel. kube-proxy's DNAT rules for Service ClusterIPs live
        # in iptables, so with the globals off, Service-IP-targeted
        # traffic (e.g. cdi-uploadproxy dialing a per-PVC upload
        # service IP) is never DNAT'd to a pod IP — packets exit with
        # the unroutable Service CIDR destination, time out. Symptom:
        # CDI uploads hang at 0 bytes, PVCs created but never populated.
        #
        # The kernel supports per-bridge override of nf_call_iptables
        # via sysfs (per-port-group attribute set on the bridge
        # netlink object). Setting it to 0 on the witness bridge ONLY
        # skips iptables for frames bridged through that specific
        # bridge, while cni0/flannel's nf_call_iptables stays at the
        # global default (1) and kube-proxy DNAT keeps working.
        #
        # Restore globals to 1 first (in case a previous boot of this
        # code stomped them to 0), then set per-bridge to 0 on
        # $WITNESS_IFACE.
        for sysctl in bridge-nf-call-iptables bridge-nf-call-ip6tables bridge-nf-call-arptables; do
            if [ -w "/proc/sys/net/bridge/$sysctl" ]; then
                echo 1 > "/proc/sys/net/bridge/$sysctl" || true
            fi
        done
        logmsg "Restored global net.bridge.bridge-nf-call-* sysctls to 1 (kube-proxy DNAT depends on this)"

        # Now disable bridge-nf-call ONLY on the witness bridge. sysfs
        # path is /sys/class/net/<br>/bridge/nf_call_{iptables,ip6tables,arptables}
        # — present on any Linux bridge device.
        for attr in nf_call_iptables nf_call_ip6tables nf_call_arptables; do
            path="/sys/class/net/${WITNESS_IFACE}/bridge/$attr"
            if [ -w "$path" ]; then
                echo 0 > "$path" || true
            else
                logmsg "WARN: $path not writable — per-bridge nf_call override may not apply"
            fi
        done
        logmsg "Per-bridge nf_call_* disabled on bridge $WITNESS_IFACE only (witness etcd traffic bypasses iptables; cni0/flannel unaffected)"
    else
        # Standalone mode: wit-host UP but unattached (no cluster reach).
        ip link set "$WITNESS_VETH_HOST" up
        logmsg "wit-host UP, not attached to any bridge (standalone)"
    fi

    # 5. Assign IP and bring interfaces up inside the netns.
    ip netns exec "$WITNESS_NETNS" ip addr add "${WITNESS_NODE_IP}${WITNESS_NODE_PREFIX:-/32}" dev "$WITNESS_VETH_NETNS"
    ip netns exec "$WITNESS_NETNS" ip link set "$WITNESS_VETH_NETNS" up
    ip netns exec "$WITNESS_NETNS" ip link set lo up
    logmsg "Assigned ${WITNESS_NODE_IP}${WITNESS_NODE_PREFIX:-/32} to $WITNESS_VETH_NETNS"

    # 5b. Gratuitous ARP — proactively announce witness's (stable) MAC
    # to all cluster peers. Without this, peers that had us cached
    # under a previous MAC (from an earlier boot/version that didn't
    # pin MACs) would keep using the stale entry for up to ~5 minutes
    # — which is the asymmetric-reachability bug Phase 2 tripped on.
    # `arping -U` sends unsolicited announcements; if arping isn't
    # available, the fallback ping-broadcast achieves the same effect.
    # Failure is non-fatal: with a stable MAC, ARP entries will
    # converge through normal ARP within minutes regardless.
    if [ "$mode" = "join" ]; then
        ip netns exec "$WITNESS_NETNS" arping -U -c 3 -I "$WITNESS_VETH_NETNS" \
            "$WITNESS_NODE_IP" >/dev/null 2>&1 || \
            ip netns exec "$WITNESS_NETNS" ping -b -c 1 -W 1 \
                "${WITNESS_NODE_IP%.*}.255" >/dev/null 2>&1 || true
        logmsg "Sent gratuitous ARP announcing $WITNESS_NODE_IP on $WITNESS_VETH_NETNS"
    fi

    # 6. Default route.
    if [ "$mode" = "join" ] && [ -n "${WITNESS_GATEWAY:-}" ]; then
        # Real gateway for join mode if provided. Useful if cluster
        # traffic crosses subnets or if other services (DNS, etc.)
        # outside the immediate /28 need to be reached. Not strictly
        # required if all etcd peers are on-link in the same subnet.
        if ! ip netns exec "$WITNESS_NETNS" ip route replace default via "$WITNESS_GATEWAY"; then
            logmsg "ERROR: failed to set default route via $WITNESS_GATEWAY"
            return 1
        fi
        logmsg "Default route via $WITNESS_GATEWAY"
    else
        # Stub default route (standalone, or join without explicit
        # gateway). Harmless under --disable-agent.
        ip netns exec "$WITNESS_NETNS" ip route replace default dev "$WITNESS_VETH_NETNS"
    fi

    logmsg "setup_witness_netns: done (mode=$mode)"
    return 0
}

# Detect and handle Phase 1.5 ↔ Phase 2 mode transitions. Compares the
# current desired mode (computed from env vars) against the last-known
# mode recorded in WITNESS_MODE_FILE. On mismatch, wipes the persistent
# k3s server state so the next k3s start does a fresh cluster-init (in
# standalone) or fresh join (in Phase 2). On match, no-op.
#
# Why this is necessary:
#   - The witness's etcd db at /var/lib/rancher/k3s/server/db encodes
#     its cluster bootstrap identity (cluster ID, peer URLs, certs).
#     k3s cannot smoothly transition between standalone (its own
#     cluster) and joining a different cluster — it'll fail to start
#     with bootstrap conflicts or critical-config errors.
#   - Same applies to a join URL change: if WITNESS_JOIN_URL flips to a
#     different seed (e.g. after a cluster-reset on the seed side), the
#     witness's existing CA / certs are no longer valid for the new
#     cluster.
#
# The marker file content is `standalone` for Phase 1.5 standalone or
# `joined:<URL>` for Phase 2 join — different URLs produce different
# markers, so reseeding the cluster triggers a wipe automatically.
#
# Must be called from Stage A AFTER mount_witness_root (the marker lives
# under /var/lib which is bind-mounted from /persist/vault/witness) and
# BEFORE install_k3s (so install_k3s sees a clean state if we wiped).
witness_check_mode_transition() {
    local desired current
    if [ -n "${WITNESS_JOIN_URL:-}" ]; then
        desired="joined:${WITNESS_JOIN_URL}"
    else
        desired="standalone"
    fi
    current=$(cat "$WITNESS_MODE_FILE" 2>/dev/null || echo "")

    if [ "$current" = "$desired" ]; then
        logmsg "witness_check_mode_transition: mode unchanged ($desired); keeping existing cluster state"
        return 0
    fi

    logmsg "witness_check_mode_transition: mode transition '${current:-<none>}' -> '$desired'; wiping k3s cluster state"

    # Wipe the k3s server-side persistent state. /var/lib here is
    # /persist/vault/witness, so this clears the etcd db, cluster CA,
    # bootstrap certs, and rendered manifests permanently.
    rm -rf /var/lib/rancher/k3s/server/db
    rm -rf /var/lib/rancher/k3s/server/tls
    rm -rf /var/lib/rancher/k3s/server/cred
    rm -rf /var/lib/rancher/k3s/server/manifests
    rm -rf /var/lib/rancher/k3s/agent
    rm -f  /etc/rancher/k3s/k3s.yaml

    # Update the marker. Write happens BEFORE k3s starts — meaning if
    # k3s then fails to come up in the new mode, the next boot still
    # sees "current mode = desired mode" and won't re-wipe. That's
    # fine: the wipe is one-shot, and the supervisor's retry loop will
    # eventually make k3s succeed (or operator sees the error in logs).
    mkdir -p "$(dirname "$WITNESS_MODE_FILE")"
    echo "$desired" > "$WITNESS_MODE_FILE"
    logmsg "witness_check_mode_transition: wipe complete, marker updated to '$desired'"
    return 0
}

# Query the witness's local etcd to find OUR own member ID in the
# cluster (the witness's etcd member, identified by name prefix
# "eve-witness-*" — k3s appends a random suffix). Returns the ID as a
# hex string on stdout, empty on failure.
#
# Used by witness_leave_cluster to know which member to remove. Must be
# called while local k3s + etcd are still running (otherwise the local
# endpoint won't answer).
witness_get_local_member_id() {
    local etcd_ca=/var/lib/rancher/k3s/server/tls/etcd/server-ca.crt
    local etcd_cert=/var/lib/rancher/k3s/server/tls/etcd/client.crt
    local etcd_key=/var/lib/rancher/k3s/server/tls/etcd/client.key

    [ -r "$etcd_ca" ] && [ -r "$etcd_cert" ] && [ -r "$etcd_key" ] || return 1

    etcdctl --dial-timeout=5s \
        --endpoints="https://${WITNESS_NODE_IP}:2379" \
        --cacert="$etcd_ca" --cert="$etcd_cert" --key="$etcd_key" \
        member list -w json 2>/dev/null | \
        jq -r '.members[] | select(.name | startswith("eve-witness")) | .ID' 2>/dev/null | \
        head -1
}

# Gracefully leave a joined cluster:
#   1. Remove our etcd member entry from the cluster (best-effort, two
#      attempts: original join URL, then any non-self peer).
#   2. Stop local k3s (SIGTERM then SIGKILL if needed).
#   3. Wipe local cluster state so next k3s start does fresh
#      --cluster-init (single-node standalone, Phase 1.5 mode).
#   4. Update WITNESS_MODE_FILE marker to "standalone".
#
# After this returns, the supervisor loop in witness-init.sh will see
# k3s not running, render_witness_cluster_config will clear the stale
# join overlay (because WITNESS_JOIN_URL was unset), and start_k3s_once
# will launch k3s in --cluster-init mode.
#
# Designed to be called from the Stage B supervisor loop when the
# override file's WITNESS_JOIN_URL transitions from set → unset (pillar
# signals "you're no longer needed for quorum"). Container is NOT
# restarted — EVE's linuxkit does not auto-respawn dead service
# containers, so all transitions must happen in place.
#
# Best-effort semantics: if the etcd MemberRemove fails (cluster
# unreachable, all peer endpoints stale), we log a warning and proceed
# with the local stop + wipe anyway. The cluster will then have an
# orphan member entry that an operator can clean up manually.
witness_leave_cluster() {
    logmsg "witness_leave_cluster: initiating graceful leave"

    # 1. Find our member ID while etcd is still alive locally.
    local our_id
    our_id=$(witness_get_local_member_id)
    if [ -z "$our_id" ]; then
        logmsg "witness_leave_cluster: WARN — could not determine local etcd member ID"
    else
        logmsg "witness_leave_cluster: our etcd member ID is $our_id"
    fi

    # 2. Read previous cluster URL from the mode marker.
    local cluster_url="" marker_content
    marker_content=$(cat "$WITNESS_MODE_FILE" 2>/dev/null || echo "")
    case "$marker_content" in
        joined:*) cluster_url="${marker_content#joined:}" ;;
    esac

    # 3. Attempt MemberRemove. Two attempts max, 5s timeout each.
    local removed=0
    if [ -n "$our_id" ]; then
        local etcd_ca=/var/lib/rancher/k3s/server/tls/etcd/server-ca.crt
        local etcd_cert=/var/lib/rancher/k3s/server/tls/etcd/client.crt
        local etcd_key=/var/lib/rancher/k3s/server/tls/etcd/client.key

        # Attempt 1: the URL we joined to (translate 6443 → 2379).
        if [ -n "$cluster_url" ]; then
            local endpoint
            endpoint=$(echo "$cluster_url" | sed 's|:6443.*|:2379|')
            logmsg "witness_leave_cluster: attempting MemberRemove via $endpoint"
            if etcdctl --dial-timeout=5s --command-timeout=5s \
                --endpoints="$endpoint" \
                --cacert="$etcd_ca" --cert="$etcd_cert" --key="$etcd_key" \
                member remove "$our_id" >> "$INSTALL_LOG" 2>&1; then
                removed=1
                logmsg "witness_leave_cluster: MemberRemove succeeded via $endpoint"
            else
                logmsg "witness_leave_cluster: MemberRemove via $endpoint failed; trying fallback peer"
            fi
        fi

        # Attempt 2: pick a non-self peer from local etcd's view.
        if [ "$removed" = "0" ]; then
            local peer_endpoint
            peer_endpoint=$(etcdctl --dial-timeout=5s \
                --endpoints="https://${WITNESS_NODE_IP}:2379" \
                --cacert="$etcd_ca" --cert="$etcd_cert" --key="$etcd_key" \
                member list -w json 2>/dev/null | \
                jq -r --arg us "$our_id" \
                    '.members[] | select(.ID != ($us | tonumber)) | .clientURLs[0]' 2>/dev/null | \
                head -1)
            if [ -n "$peer_endpoint" ]; then
                logmsg "witness_leave_cluster: fallback — attempting MemberRemove via $peer_endpoint"
                if etcdctl --dial-timeout=5s --command-timeout=5s \
                    --endpoints="$peer_endpoint" \
                    --cacert="$etcd_ca" --cert="$etcd_cert" --key="$etcd_key" \
                    member remove "$our_id" >> "$INSTALL_LOG" 2>&1; then
                    removed=1
                    logmsg "witness_leave_cluster: MemberRemove succeeded via $peer_endpoint"
                fi
            fi
        fi

        if [ "$removed" = "0" ]; then
            logmsg "witness_leave_cluster: WARN — MemberRemove failed on all endpoints; cluster will have orphan member $our_id (operator should run 'etcdctl member remove $our_id' on a healthy cluster node)"
        fi
    fi

    # 4. Stop local k3s gracefully (SIGTERM → wait 30s → SIGKILL).
    if [ -r "$WITNESS_K3S_PID_FILE" ]; then
        local pid
        pid=$(cat "$WITNESS_K3S_PID_FILE")
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            logmsg "witness_leave_cluster: stopping local k3s (pid=$pid)"
            kill -TERM "$pid" 2>/dev/null
            local i=0
            while [ $i -lt 30 ] && kill -0 "$pid" 2>/dev/null; do
                sleep 1
                i=$((i + 1))
            done
            if kill -0 "$pid" 2>/dev/null; then
                logmsg "witness_leave_cluster: k3s didn't exit on SIGTERM after 30s, SIGKILL"
                kill -KILL "$pid" 2>/dev/null
                sleep 2
            fi
        fi
        rm -f "$WITNESS_K3S_PID_FILE"
    fi

    # 5. Wipe local cluster state so the next k3s start --cluster-init's
    #    cleanly without bootstrap conflicts from the prior cluster.
    rm -rf /var/lib/rancher/k3s/server/db
    rm -rf /var/lib/rancher/k3s/server/tls
    rm -rf /var/lib/rancher/k3s/server/cred
    rm -rf /var/lib/rancher/k3s/server/manifests
    rm -rf /var/lib/rancher/k3s/agent
    rm -f  /etc/rancher/k3s/k3s.yaml

    # 6. Update marker.
    mkdir -p "$(dirname "$WITNESS_MODE_FILE")"
    echo "standalone" > "$WITNESS_MODE_FILE"

    logmsg "witness_leave_cluster: complete; next k3s start will be standalone --cluster-init"
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

# Ensure $WITNESS_VETH_HOST is attached to bridge $WITNESS_IFACE and has
# broadcast/multicast/unicast flooding enabled. Idempotent:
#   - If already attached AND flood flags are set: returns 0 silently.
#   - If detached OR flooding is off: attaches, sets flags, logs LOUDLY.
#   - On unrecoverable error (bridge missing, ip link fails): returns 1.
#
# WHY THIS EXISTS:
#   wit-host is attached to the bridge in setup_witness_netns at boot.
#   We observed empirically that something on the host (suspected:
#   pillar/zedrouter's network reconciliation removing interfaces it
#   doesn't recognize) DETACHES wit-host mid-run. The first symptom is
#   the bridge's FDB aging out the witness MAC (default ~5 min on EVE),
#   after which unicast replies to the witness no longer reach wit-host,
#   broadcasts (ARP) don't reach it, and the witness goes deaf.
#
#   This is a defensive guard, not a real fix. The proper fix is pillar
#   coordination (see task #50: make zedrouter respect wit-host). Until
#   that lands, the witness supervisor calls this on every iteration to
#   self-heal within ~15s of any detach.
#
# WHY flood flags matter:
#   bcast_flood and mcast_flood on a bridge port control whether the
#   bridge floods broadcast/multicast frames OUT to that port. Default
#   is 1 on kernel-default-attached ports, but explicitly setting them
#   here is defense-in-depth — if pillar re-attaches the port with
#   flags off, the bridge still won't deliver ARP requests to the
#   witness. We must override.
ensure_wit_host_attached() {
    # Phase 1.5 standalone: no bridge attachment expected. No-op.
    [ -n "${WITNESS_JOIN_URL:-}" ] || return 0

    # If the port already has master == $WITNESS_IFACE AND brport dir
    # exists AND all three flood flags are 1, we're done.
    local current_master
    current_master=$(ip -o link show "$WITNESS_VETH_HOST" 2>/dev/null | \
        grep -o 'master [^ ]*' | awk '{print $2}')
    if [ "$current_master" = "$WITNESS_IFACE" ] && \
       [ -d "/sys/class/net/$WITNESS_VETH_HOST/brport" ] && \
       [ "$(cat /sys/class/net/$WITNESS_VETH_HOST/brport/broadcast_flood 2>/dev/null)" = "1" ] && \
       [ "$(cat /sys/class/net/$WITNESS_VETH_HOST/brport/multicast_flood 2>/dev/null)" = "1" ] && \
       [ "$(cat /sys/class/net/$WITNESS_VETH_HOST/brport/unicast_flood 2>/dev/null)" = "1" ]; then
        return 0
    fi

    # Something's off. Loud log so we can correlate with pillar logs.
    logmsg "ensure_wit_host_attached: state drift detected (current_master='${current_master:-<none>}', wanted='$WITNESS_IFACE'); reattaching"

    if ! ip link set "$WITNESS_VETH_HOST" master "$WITNESS_IFACE" 2>/dev/null; then
        logmsg "ensure_wit_host_attached: ERROR — 'ip link set $WITNESS_VETH_HOST master $WITNESS_IFACE' failed"
        return 1
    fi

    # Force flood flags on. Kernel default is 1 but pillar may have
    # cleared them; either way we want them on.
    for attr in broadcast_flood multicast_flood unicast_flood; do
        local path="/sys/class/net/$WITNESS_VETH_HOST/brport/$attr"
        if [ -w "$path" ]; then
            echo 1 > "$path" 2>/dev/null || \
                logmsg "ensure_wit_host_attached: WARN — couldn't set $attr=1"
        fi
    done

    logmsg "ensure_wit_host_attached: attached $WITNESS_VETH_HOST -> bridge $WITNESS_IFACE; flood flags forced on"
    return 0
}

# Render the witness's kube-component disables ONLY in Phase 2 (joined).
#
# Why this is conditional on join mode, not always-on:
#
# In Phase 1.5 (standalone), the witness IS the only k3s server in its
# own single-node cluster. Disabling apiserver leaves no apiserver
# anywhere, and k3s's internal "runtime core" (which wraps the
# apiserver clientset and drives the etcd-node-list reconciler) can
# never become ready. Symptom: an endless stream of
#   "Failed to list nodes with etcd role: runtime core not ready"
# in witness.log every 15s. Etcd itself runs fine, but the warning is
# spammy and indicates a partially-initialized control plane.
#
# In Phase 2 (joined), the OTHER cluster servers (pkg/kube on the
# physical nodes) provide apiserver/scheduler/controller-manager. The
# witness becomes a pure etcd vote, and disabling its own
# apiserver/scheduler/controller-manager is REQUIRED for correctness:
# otherwise the witness's apiserver gets registered as a member of
# the `kubernetes` Service endpoints, kube-proxy round-robins API
# traffic across all three apiservers, and the ~1/3 of requests that
# land on the witness fail when the apiserver tries to dial an
# admission-webhook pod IP — the witness netns has no CNI / no route
# to the cluster pod CIDR. Field symptom: random PVC creates time out
# with "no route to host" on longhorn-admission-webhook.
#
# So: in join mode write the disables; in standalone remove them. Must
# be called from the same places as render_witness_cluster_config —
# Stage A boot, and every supervisor-loop mode transition.
render_witness_disables_config() {
    mkdir -p "$K3S_CONFIG_DIR"
    if [ -z "${WITNESS_JOIN_URL:-}" ]; then
        if [ -f "$K3S_DISABLES_CONFIG_FILE" ]; then
            rm -f "$K3S_DISABLES_CONFIG_FILE"
            logmsg "Removed $K3S_DISABLES_CONFIG_FILE (standalone — witness needs its own apiserver)"
        fi
        return 0
    fi
    cat > "$K3S_DISABLES_CONFIG_FILE" <<EOF
---
# AUTO-GENERATED by witness-utils.sh:render_witness_disables_config.
# Written only when WITNESS_JOIN_URL is set (Phase 2 join mode). In
# standalone the witness needs its own apiserver and this file is
# removed. Do NOT edit by hand.
disable-apiserver: true
disable-scheduler: true
disable-controller-manager: true
EOF
    logmsg "Rendered $K3S_DISABLES_CONFIG_FILE (Phase 2 — witness is etcd-only)"
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
