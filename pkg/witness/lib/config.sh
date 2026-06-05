#!/bin/sh
#
# Copyright (c) 2026 Zededa, Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Witness-side config library. Mirrors pkg/kube/lib/config.sh layout so the
# two packages stay in sync as config knobs evolve.

# Reserved witness identity — DO NOT change. pkg/kube discovers and reclaims
# the third etcd member using this exact name + IP pair.
# shellcheck disable=SC2034
WITNESS_NODE_NAME="eve-witness"

# Phase 1.5 standalone default. The witness runs inside its own
# `eve-witness` network namespace; this IP lives on wit-eth0 (veth peer)
# inside that netns and is not reachable from outside the host (Phase 1.5
# witness is a self-contained single-member etcd cluster — see design doc
# §12).
#
# In Phase 2 (cluster join), the override file replaces WITNESS_NODE_IP
# with a cluster-routable IP and wit-eth0 becomes a macvlan child of
# WITNESS_IFACE (the physical NIC) instead of a host-side veth peer —
# see witness-utils.sh:setup_witness_netns and design doc §13.
# shellcheck disable=SC2034
WITNESS_NODE_IP="10.244.244.244"
# Subnet prefix for WITNESS_NODE_IP. /32 is the Phase 1.5 default (single-
# host, no on-link gateway possible — fine for a standalone witness).
# For Phase 2, set to /24 (or whatever matches the cluster subnet) in the
# override file so the default route via WITNESS_GATEWAY is on-link.
# shellcheck disable=SC2034
WITNESS_NODE_PREFIX="/32"
# Cluster bridge to attach the witness veth's host end to, in Phase 2
# (join) mode. MUST be a Linux BRIDGE (typically EVE's `eth0`), NOT a
# physical NIC like `keth0`. The witness's wit-host becomes a port of
# this bridge, so wit-eth0 (inside the netns) participates in the same
# L2 segment as the host's cluster IP — letting the witness reach the
# seed on the same physical device (a constraint that pure macvlan
# can't satisfy due to the kernel's macvlan-child-to-parent-host
# loopback block).
#
# History:
#   - We initially tried macvlan-of-bridge (`link eth0`). Broke the
#     device — macvlan children of a Linux bridge cause traffic
#     disruption.
#   - We considered macvlan-of-physical-NIC (`link keth0`). Works for a
#     witness on a SEPARATE physical device, but fails for the
#     same-host case (kernel refuses to deliver frames from a macvlan
#     child to the parent NIC's host IP — both seed and witness on the
#     same wire can't talk).
#   - Bridge-port attachment of wit-host onto the eth0 bridge works
#     for both same-host AND cross-host topologies, because bridges
#     forward frames between all ports including their own host stack.
#
# In Phase 1.5 standalone mode, WITNESS_IFACE is UNUSED — the witness
# uses a plain veth pair with no host-side bridge attachment.
# shellcheck disable=SC2034
WITNESS_IFACE="eth0"
# Default gateway inside the netns, used in Phase 2 join mode. Should
# be the cluster network's gateway IP if there is one (typical
# datacenter / lab setup) — letting the witness reach things outside
# its immediate subnet. Empty = no default route is configured beyond
# the stub used in standalone mode; cluster-only operation works
# without a gateway as long as all cluster members share an L2.
# UNUSED in Phase 1.5 standalone mode.
# shellcheck disable=SC2034
WITNESS_GATEWAY=""

# Manual override file — drops into /persist/vault before the vault is
# unsealed, then read by lib/config.sh on every witness boot. In Phase 2
# (production) this file is populated by pillar/zedkube from
# EdgeNodeClusterStatus; today it's also the path for manual testing.
#
# Recognised keys (shell-syntax key=value, no quotes needed for simple
# values):
#
#   # /persist/witness-override.env
#   #
#   # Network identity (always — Phase 1.5 standalone or Phase 2 join):
#   WITNESS_NODE_IP=192.168.1.55           # IP the witness's k3s binds to
#   WITNESS_NODE_PREFIX=/24                # subnet mask, /32 by default
#   WITNESS_IFACE=keth0                    # macvlan parent (Phase 2 only)
#   WITNESS_GATEWAY=192.168.1.1            # default gateway (Phase 2 only)
#   #
#   # Cluster join (set ALL three to enable Phase 2 join mode; leave
#   # UNSET for Phase 1.5 standalone — witness forms its own cluster):
#   WITNESS_JOIN_URL=https://192.168.1.10:6443     # seed's apiserver
#   WITNESS_JOIN_TOKEN="K10abc...::server:..."     # from seed:/var/lib/rancher/k3s/server/token
#
# /persist is unsealed before the witness starts and is bind-mounted into
# the container, so the file is readable by the time this script sources.
#
# Phase 2 with the cloud will REPLACE this file mechanism with reads from
# /run/zedkube/EdgeNodeClusterStatus/global.json (see design doc §13.3).
# Keep the variable names matching the canonical names here so the swap
# is mechanical.
WITNESS_OVERRIDE_FILE="/persist/witness-override.env"
if [ -r "$WITNESS_OVERRIDE_FILE" ]; then
    # shellcheck source=/dev/null
    . "$WITNESS_OVERRIDE_FILE"
fi

# The presence of WITNESS_JOIN_URL is the canonical signal "we're in
# Phase 2 join mode". It drives several decisions:
#
#   - witness-utils.sh:setup_witness_netns creates wit-eth0 as a macvlan
#     child of WITNESS_IFACE instead of as a veth peer.
#   - witness-utils.sh:render_witness_cluster_config writes the join
#     overlay config.yaml.d/01-clusterconfig.yaml.
#   - witness-utils.sh:witness_check_mode_transition wipes the standalone
#     etcd state on the first boot in a new mode.
#   - witness-init.sh:start_k3s_once omits --cluster-init.
#
# Without WITNESS_JOIN_URL, all paths default to Phase 1.5 standalone:
# veth pair, no join overlay, no wipe, --cluster-init on first start.

# Marker file recording the last-known cluster mode (standalone vs the
# specific URL we joined). Used by witness_check_mode_transition to
# detect when to wipe /var/lib/rancher/k3s/server on mode change. Lives
# under /var/lib (= /persist/vault/witness bind mount) so it survives
# reboots, but is cleared by an explicit wipe.
# shellcheck disable=SC2034
WITNESS_MODE_FILE="/var/lib/witness/.cluster-mode"

# Base static k3s config lives in /etc/rancher/k3s/config.yaml, anything else
# drops into config.yaml.d for k3s to merge.
# shellcheck disable=SC2034
K3S_CONFIG_DIR="/etc/rancher/k3s/config.yaml.d"
# shellcheck disable=SC2034
K3S_NODENAME_CONFIG_FILE="${K3S_CONFIG_DIR}/00-nodename.yaml"
# Phase 2 (cluster join) writes server/token here.
# shellcheck disable=SC2034
K3S_CLUSTER_CONFIG_FILE="${K3S_CONFIG_DIR}/01-clusterconfig.yaml"
# Auto-generated every boot from $WITNESS_NODE_IP — see render_witness_network_config
# in witness-utils.sh. Lexically AFTER 00/01, so any later cluster-join
# settings keep the right precedence.
# shellcheck disable=SC2034
K3S_NETWORK_CONFIG_FILE="${K3S_CONFIG_DIR}/02-witness-network.yaml"

# Private containerd that the k3s server-side controllers may invoke for
# helm-chart deploy jobs. pkg/kube uses /run/containerd-user/containerd.sock
# — we use a sibling path so the two don't fight in the shared host /run.
# Under --disable-agent there's no kubelet talking to this socket, but
# k3s server still opens a CRI client; cheap to keep around.
# shellcheck disable=SC2034
WITNESS_CTRD_SOCK="/run/witness/containerd/containerd.sock"
# shellcheck disable=SC2034
WITNESS_CTRD_CONFIG="/etc/witness/containerd-config.toml"
# shellcheck disable=SC2034
WITNESS_CTRD_LOG="/persist/kubelog/witness-containerd.log"
# Witness-private containerd binary path (a symlink to the k3s-shipped
# /var/lib/rancher/k3s/data/current/bin/containerd, created at startup).
# Using a witness-unique path is what keeps pkg/kube's pgrep-on-binary-path
# from matching our containerd in the shared host PID namespace. /var/lib
# is bind-mounted to /persist/vault/witness so the symlink survives reboots.
# shellcheck disable=SC2034
WITNESS_CTRD_BIN_DIR="/var/lib/witness/bin"
# shellcheck disable=SC2034
WITNESS_CTRD_BIN="${WITNESS_CTRD_BIN_DIR}/containerd"
