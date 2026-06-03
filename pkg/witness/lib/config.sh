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

# Phase 1 defaults. The IP rides a dummy interface (WITNESS_IFACE) created
# by setup_witness_interface; both pkg/kube and pkg/witness share the host
# network namespace, so binding the witness's k3s to a different IP is what
# keeps the listeners (apiserver 6443, etcd 2379/2380) from colliding with
# pkg/kube's k3s on the real interface.
# shellcheck disable=SC2034
WITNESS_NODE_IP="10.244.244.244"
# shellcheck disable=SC2034
WITNESS_IFACE="eve-witness0"

# Manual override for Phase 2 dry-runs (cluster IP + real interface,
# without rebuilding the image). Drop a shell-syntax key=value file at
# /persist/witness-override.env and the values below override the Phase 1
# defaults above:
#
#   # /persist/witness-override.env
#   WITNESS_NODE_IP=192.168.1.55
#   WITNESS_IFACE=eth0
#
# /persist is unsealed before the witness starts and is bind-mounted into
# the container, so the file is readable by the time this script sources.
# setup_witness_interface already no-ops `ip link add` when WITNESS_IFACE
# is an existing real interface and only adds WITNESS_NODE_IP as a
# secondary address — same shape Phase 2 will need.
#
# Phase 2 will REMOVE this file mechanism and instead read the values
# from /run/zedkube/EdgeNodeClusterStatus/global.json (see design doc
# §6.1). Keep the override file's variable names matching the canonical
# names here so the swap is mechanical.
WITNESS_OVERRIDE_FILE="/persist/witness-override.env"
if [ -r "$WITNESS_OVERRIDE_FILE" ]; then
    # shellcheck source=/dev/null
    . "$WITNESS_OVERRIDE_FILE"
fi

# Base static k3s config lives in /etc/rancher/k3s/config.yaml, anything else
# drops into config.yaml.d for k3s to merge.
# shellcheck disable=SC2034
K3S_CONFIG_DIR="/etc/rancher/k3s/config.yaml.d"
# shellcheck disable=SC2034
K3S_NODENAME_CONFIG_FILE="${K3S_CONFIG_DIR}/00-nodename.yaml"
# Phase 2 (cluster join) writes server/token/bind-address here.
# shellcheck disable=SC2034
K3S_CLUSTER_CONFIG_FILE="${K3S_CONFIG_DIR}/01-clusterconfig.yaml"
# Auto-generated every boot from $WITNESS_NODE_IP — see render_witness_network_config
# in witness-utils.sh. Lexically AFTER 00/01, so any later cluster-join
# settings keep the right precedence.
# shellcheck disable=SC2034
K3S_NETWORK_CONFIG_FILE="${K3S_CONFIG_DIR}/02-witness-network.yaml"

# The witness runs as a full k3s server (apiserver + controller-manager +
# scheduler + etcd + kubelet + flannel — same shape as pkg/kube), but is
# automatically cordoned at startup and tainted NoSchedule/NoExecute so it
# never hosts workloads. Local kubeconfig lands at the default k3s path:
#   /etc/rancher/k3s/k3s.yaml
# cordon_witness_node in witness-utils.sh uses that to mark the Node
# SchedulingDisabled once it registers.

# Private containerd that kubelet talks to. pkg/kube uses
# /run/containerd-user/containerd.sock — we use a sibling path so the two
# don't fight in the shared host /run mount.
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
