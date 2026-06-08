#!/bin/sh
#
# Copyright (c) 2026 Zededa, Inc.
# SPDX-License-Identifier: Apache-2.0
#
# witness-cleanup.sh — remove stale `eve-witness-*` etcd members from the
# cluster.
#
# WHEN TO RUN: before promoting (or re-promoting) a witness onto a node.
# E.g. if the node previously hosting the witness died, its etcd member
# entry lingers in the cluster's member list. The new witness, when it
# joins via `k3s server --server=... --token=...`, calls etcd's MemberAdd
# — and that fails with "peer URL conflict" if the orphan's peer URL
# matches the new witness's peer URL (same WITNESS_NODE_IP).
#
# WHERE TO RUN: on the seed (or ANY healthy cluster member running
# pkg/kube). pkg/kube has the cluster's etcd TLS certs unconditionally
# (it IS a cluster member), so we can authenticate to the local etcd
# endpoint and issue member-remove RPCs.
#
# This is a manual operator step today; later we'll wire it into pillar's
# witness-promotion workflow so pillar invokes it automatically before
# setting WITNESS_JOIN_URL in /persist/witness-override.env on the
# target witness device.
#
# Usage (from the host shell, inside the kube container):
#   eve enter kube
#   /usr/bin/witness-cleanup.sh
#
# Exit code: 0 if cleanup succeeded (including no-op when there were no
# stale entries), non-zero if etcdctl couldn't reach the local etcd.

set -e

ETCD_CA=/var/lib/rancher/k3s/server/tls/etcd/server-ca.crt
ETCD_CERT=/var/lib/rancher/k3s/server/tls/etcd/client.crt
ETCD_KEY=/var/lib/rancher/k3s/server/tls/etcd/client.key
ETCD_ENDPOINT=https://127.0.0.1:2379

# Sanity — we should always have certs since pkg/kube is a cluster member.
for f in "$ETCD_CA" "$ETCD_CERT" "$ETCD_KEY"; do
    if [ ! -r "$f" ]; then
        echo "ERROR: $f not readable. Is this node a healthy cluster member?" >&2
        echo "       This script must run inside the pkg/kube container on a node where k3s server is up." >&2
        exit 1
    fi
done

if ! command -v etcdctl >/dev/null 2>&1; then
    echo "ERROR: etcdctl not found in PATH. This script is meant to run inside the pkg/kube container." >&2
    exit 1
fi

if ! command -v jq >/dev/null 2>&1; then
    echo "ERROR: jq not found in PATH." >&2
    exit 1
fi

# List members and identify eve-witness-* entries. K3s names witness
# members "eve-witness-<random8hex>" (witness-side WITNESS_NODE_NAME =
# "eve-witness", with k3s's random suffix appended at registration time).
members_json=$(etcdctl --dial-timeout=5s --command-timeout=5s \
    --endpoints="$ETCD_ENDPOINT" \
    --cacert="$ETCD_CA" --cert="$ETCD_CERT" --key="$ETCD_KEY" \
    member list -w json 2>/dev/null) || {
    echo "ERROR: failed to list etcd members. Local etcd may be down." >&2
    exit 1
}

# Show the current state first — useful for the operator.
echo "Current etcd members BEFORE cleanup:"
etcdctl --endpoints="$ETCD_ENDPOINT" \
    --cacert="$ETCD_CA" --cert="$ETCD_CERT" --key="$ETCD_KEY" \
    member list -w table 2>/dev/null

stale_ids=$(echo "$members_json" | \
    jq -r '.members[] | select(.name | startswith("eve-witness-")) | .ID' 2>/dev/null)

if [ -z "$stale_ids" ]; then
    echo
    echo "No eve-witness-* members in cluster; nothing to remove."
    exit 0
fi

echo
echo "Removing stale eve-witness-* members:"
removed=0
failed=0
for id_dec in $stale_ids; do
    # etcdctl's `member list -w json` returns IDs as DECIMAL integers,
    # but `member remove` expects HEX. Convert before invoking — without
    # this, etcdctl rejects the ID with an unhelpful error.
    id_hex=$(printf '%x' "$id_dec" 2>/dev/null) || {
        echo "    FAILED: could not convert id $id_dec to hex" >&2
        failed=$((failed + 1))
        continue
    }
    # Extract member name + peer URL for clearer logging.
    name=$(echo "$members_json" | \
        jq -r --arg id "$id_dec" '.members[] | select(.ID == ($id | tonumber)) | .name')
    peer=$(echo "$members_json" | \
        jq -r --arg id "$id_dec" '.members[] | select(.ID == ($id | tonumber)) | .peerURLs[0]')
    echo "  - id=$id_hex name=$name peer=$peer"
    # Don't swallow stderr — etcdctl's actual error helps debugging.
    if etcdctl --dial-timeout=5s --command-timeout=5s \
        --endpoints="$ETCD_ENDPOINT" \
        --cacert="$ETCD_CA" --cert="$ETCD_CERT" --key="$ETCD_KEY" \
        member remove "$id_hex"; then
        removed=$((removed + 1))
    else
        echo "    FAILED to remove $id_hex" >&2
        failed=$((failed + 1))
    fi
done

echo
echo "Removed $removed of $((removed + failed)) entries."
echo
echo "Current etcd members AFTER cleanup:"
etcdctl --endpoints="$ETCD_ENDPOINT" \
    --cacert="$ETCD_CA" --cert="$ETCD_CERT" --key="$ETCD_KEY" \
    member list -w table

if [ "$failed" -gt 0 ]; then
    echo
    echo "WARNING: $failed removal(s) failed. Investigate with 'etcdctl ... member list' above."
    exit 2
fi

exit 0
