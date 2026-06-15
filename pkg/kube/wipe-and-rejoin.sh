#!/bin/sh
#
# Copyright (c) 2026 Zededa, Inc.
# SPDX-License-Identifier: Apache-2.0
#
# wipe-and-rejoin.sh — recover a stale pre-reset etcd member into a
# post-cluster-reset cluster.
#
# WHEN TO RUN: after `cluster-reset.sh` has been run on the surviving
# node, on any OTHER node that was a member of the pre-reset cluster.
# Symptoms on such a node:
#   - /var/log/k3s.log spins with "dial tcp 127.0.0.1:2379: connection
#     refused" — its local etcd refuses to start because its on-disk
#     DB has the OLD cluster-ID.
#   - kubectl never becomes ready.
#
# WHY:
#   `cluster-reset` on the survivor creates a brand-new etcd cluster
#   (new cluster-ID, member list = just that one node). Other nodes'
#   local etcd DBs encode the OLD cluster-ID and can't reconcile. To
#   rejoin, they must wipe their etcd DB and add themselves as fresh
#   learners via the join URL. CA + token are preserved by
#   cluster-reset, so no need to wipe certs.
#
# WHERE TO RUN: inside the pkg/kube container on the stale node:
#     eve enter kube
#     /usr/bin/wipe-and-rejoin.sh https://<survivor-node-ip>:6443
#
# The argument is REQUIRED if this node was originally the cluster's
# bootstrap (had `cluster-init: true` in its k3s config). Without it,
# k3s would come back up trying to bootstrap a NEW single-node cluster
# instead of joining the survivor — splitting the system. The script
# rewrites the `cluster-init: true` line to `server: <URL>` before
# restarting k3s.
#
# If this node was already a non-bootstrap member (config has `server:`
# already pointing at the survivor), the argument is optional; the
# script will just wipe the DB and restart.
#
# WHAT IT DOES:
#   1. Sets the k3s-stop flag so the pkg/kube supervisor won't fight us.
#   2. Stops running k3s server processes (SIGTERM 60s, then SIGKILL).
#   3. (Optional) Rewrites `cluster-init: true` → `server: <URL>` in
#      /etc/rancher/k3s/config.yaml.d/01-clusterconfig.yaml.
#   4. Snapshots etcd db to db.backup-<ts> for rollback.
#   5. Wipes /persist/vault/kube/rancher/k3s/server/db (NOT tls or
#      token — CA and token are preserved across cluster-reset).
#   6. Re-enables supervisor; k3s restarts, joins the survivor's
#      cluster as a fresh learner.
#   7. Tails k3s.log until etcd is healthy locally (max 5min).
#
# PRESERVED:
#   - server token (CA-signed, still valid against new cluster)
#   - cluster CA (cluster-reset preserves it)
#   - kubeconfig, certs
#   - node-ip, hostname, all other k3s config except cluster-init/server
#
# ROLLBACK:
#   Snapshot at /persist/vault/kube/rancher/k3s/server/db.backup-<ts>.
#   To revert:
#     touch /run/kube/k3s-stop
#     pkill -KILL -f "k3s server"
#     rm -rf /persist/vault/kube/rancher/k3s/server/db
#     mv /persist/vault/kube/rancher/k3s/server/db.backup-* /persist/vault/kube/rancher/k3s/server/db
#     # And restore 01-clusterconfig.yaml from .pre-wipe-and-rejoin backup
#     rm /run/kube/k3s-stop

set -u

NEW_SERVER_URL="${1:-}"

K3S_STOP_FLAG=/run/kube/k3s-stop
K3S_START_FLAG=/run/kube/k3s-start
K3S_CONFIG_DIR=/etc/rancher/k3s/config.yaml.d
CLUSTER_CONFIG_FILE="${K3S_CONFIG_DIR}/01-clusterconfig.yaml"
K3S_DB_DIR=/persist/vault/kube/rancher/k3s/server/db

echo "=== K3s Wipe-and-Rejoin ==="
echo

# === STEP 0: detect bootstrap-mode and validate args ===
IS_BOOTSTRAP=no
if [ -f "$CLUSTER_CONFIG_FILE" ] && grep -q "^cluster-init: true" "$CLUSTER_CONFIG_FILE"; then
    IS_BOOTSTRAP=yes
    if [ -z "$NEW_SERVER_URL" ]; then
        echo "FATAL: this node has 'cluster-init: true' in $CLUSTER_CONFIG_FILE" >&2
        echo "       — it would re-bootstrap a NEW cluster instead of joining." >&2
        echo "       Re-run with the survivor's apiserver URL:" >&2
        echo "         /usr/bin/wipe-and-rejoin.sh https://10.244.240.3:6443" >&2
        exit 1
    fi
    echo "Node is bootstrap (cluster-init: true). Will switch to server: $NEW_SERVER_URL"
elif [ -f "$CLUSTER_CONFIG_FILE" ]; then
    current_server=$(grep "^server:" "$CLUSTER_CONFIG_FILE" | head -1 | sed 's/^server:[[:space:]]*//' | tr -d '"' | tr -d "'" | tr -d '[:space:]')
    if [ -n "$NEW_SERVER_URL" ] && [ "$current_server" != "$NEW_SERVER_URL" ]; then
        echo "Will update server URL: $current_server -> $NEW_SERVER_URL"
    else
        echo "Node is already non-bootstrap (server: $current_server). Just wiping db."
    fi
else
    echo "WARNING: $CLUSTER_CONFIG_FILE missing — proceeding with db wipe only." >&2
    echo "         If k3s fails to start, the cluster config wasn't initialized." >&2
fi
echo

# === STEP 1: stop the supervisor ===
echo "[1/6] Setting k3s-stop flag ..."
mkdir -p /run/kube
touch "$K3S_STOP_FLAG"
rm -f "$K3S_START_FLAG"
sleep 2

# === STEP 2: stop running k3s server processes ===
echo "[2/6] Stopping k3s server ..."
if [ -f /usr/bin/cluster-utils.sh ]; then
    # shellcheck source=/dev/null
    . /usr/bin/cluster-utils.sh
    get_pids() { kube_k3s_pids; }
else
    get_pids() { pgrep -f "k3s server" 2>/dev/null; }
fi

pids=$(get_pids)
if [ -n "$pids" ]; then
    echo "  pids: $pids — SIGTERM (60s grace)"
    for pid in $pids; do kill -TERM "$pid" 2>/dev/null || true; done
    waited=0
    while [ $waited -lt 60 ] && [ -n "$(get_pids)" ]; do
        sleep 1; waited=$((waited + 1))
    done
    stragglers=$(get_pids)
    if [ -n "$stragglers" ]; then
        echo "  SIGKILL stragglers: $stragglers"
        for pid in $stragglers; do kill -KILL "$pid" 2>/dev/null || true; done
        sleep 2
    fi
else
    echo "  No k3s server running."
fi

# === STEP 3: rewrite config if needed ===
if [ -n "$NEW_SERVER_URL" ] && [ -f "$CLUSTER_CONFIG_FILE" ]; then
    echo "[3/6] Rewriting $CLUSTER_CONFIG_FILE ..."
    cp -a "$CLUSTER_CONFIG_FILE" "${CLUSTER_CONFIG_FILE}.pre-wipe-and-rejoin"
    if [ "$IS_BOOTSTRAP" = "yes" ]; then
        # Replace 'cluster-init: true' with 'server: <URL>'
        sed -i "s|^cluster-init: true.*|server: \"${NEW_SERVER_URL}\"|" "$CLUSTER_CONFIG_FILE"
    else
        # Replace existing 'server:' line
        sed -i "s|^server:.*|server: \"${NEW_SERVER_URL}\"|" "$CLUSTER_CONFIG_FILE"
    fi
    echo "  Updated. Backup: ${CLUSTER_CONFIG_FILE}.pre-wipe-and-rejoin"
    echo "  --- new contents ---"
    sed 's/^/    /' "$CLUSTER_CONFIG_FILE"
    echo "  --------------------"
else
    echo "[3/6] No config changes needed."
fi

# === STEP 4: snapshot db ===
echo "[4/6] Snapshotting etcd db ..."
if [ -d "$K3S_DB_DIR" ]; then
    SNAPSHOT_DIR="${K3S_DB_DIR}.backup-$(date +%s)"
    cp -a "$K3S_DB_DIR" "$SNAPSHOT_DIR"
    echo "  Snapshot: $SNAPSHOT_DIR"
else
    echo "  No db to snapshot (already gone?)."
fi

# === STEP 5: wipe etcd db ===
echo "[5/6] Wiping $K3S_DB_DIR ..."
rm -rf "$K3S_DB_DIR"
echo "  Done."

# === STEP 6: restart and wait ===
echo "[6/6] Re-enabling supervisor and waiting for k3s ..."
rm -f "$K3S_STOP_FLAG"
touch "$K3S_START_FLAG"

waited=0
while [ $waited -lt 300 ]; do
    if [ -n "$(get_pids)" ] && \
       etcdctl --dial-timeout=2s --command-timeout=2s \
           --endpoints=https://127.0.0.1:2379 \
           --cacert=/var/lib/rancher/k3s/server/tls/etcd/server-ca.crt \
           --cert=/var/lib/rancher/k3s/server/tls/etcd/client.crt \
           --key=/var/lib/rancher/k3s/server/tls/etcd/client.key \
           endpoint health >/dev/null 2>&1; then
        break
    fi
    sleep 5; waited=$((waited + 5))
done

if [ $waited -ge 300 ]; then
    echo
    echo "WARNING: etcd not healthy after 5min." >&2
    echo "         Inspect /var/log/k3s.log on this node, and run on the survivor:" >&2
    echo "           etcdctl ... member list -w table" >&2
    echo "         The new member should appear (initially as a learner)." >&2
    exit 1
fi

echo "  etcd healthy locally after ${waited}s."
echo
echo "=== DONE ==="
echo
echo "Verify on the survivor node (where you ran cluster-reset.sh):"
echo "  etcdctl --endpoints=$NEW_SERVER_URL/... member list -w table"
echo "  kubectl get nodes -o wide"
echo
echo "This node should appear as a started (non-learner) member within ~60s."
