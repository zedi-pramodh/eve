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
# shellcheck disable=SC2034
WITNESS_NODE_IP="10.244.244.244"
# Dummy interface that carries the witness IP on the host. Both pkg/kube and
# pkg/witness share the host network namespace; this interface is what keeps
# every k3s listener (apiserver 6443, etcd 2379/2380) from colliding with
# pkg/kube's k3s on the real interface IP.
# shellcheck disable=SC2034
WITNESS_IFACE="eve-witness0"

# Base static k3s config lives in /etc/rancher/k3s/config.yaml, anything else
# drops into config.yaml.d for k3s to merge.
# shellcheck disable=SC2034
K3S_CONFIG_DIR="/etc/rancher/k3s/config.yaml.d"
# shellcheck disable=SC2034
K3S_NODENAME_CONFIG_FILE="${K3S_CONFIG_DIR}/00-nodename.yaml"
# shellcheck disable=SC2034
K3S_CLUSTER_CONFIG_FILE="${K3S_CONFIG_DIR}/01-clusterconfig.yaml"

# No kubeconfig path: the witness runs as a dedicated etcd node
# (disable-apiserver/controller-manager/scheduler in config.yaml). There
# is no apiserver to write a kubeconfig for, and no point exposing one.
# Liveness checks go through etcdctl against https://10.244.244.244:2379
# and "kubectl get nodes" on the seed (the witness appears there because
# kubelet runs). See design doc §7.

# Private containerd that kubelet talks to. pkg/kube uses
# /run/containerd-user/containerd.sock — we use a sibling path so the two
# don't fight in the shared host /run mount.
# shellcheck disable=SC2034
WITNESS_CTRD_SOCK="/run/witness/containerd/containerd.sock"
# shellcheck disable=SC2034
WITNESS_CTRD_CONFIG="/etc/witness/containerd-config.toml"
# shellcheck disable=SC2034
WITNESS_CTRD_LOG="/persist/kubelog/witness-containerd.log"
