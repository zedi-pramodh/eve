#!/bin/sh
#
# Copyright (c) 2026 Zededa, Inc.
# SPDX-License-Identifier: Apache-2.0
#
# wipe-and-rejoin.sh — make the witness rejoin a post-cluster-reset
# cluster fresh.
#
# WHEN TO RUN: after the operator has run pkg/kube's cluster-reset.sh
# on the surviving cluster node. The witness's local k3s/etcd state
# encodes the OLD cluster-ID and won't reconcile with the new cluster;
# symptoms in /persist/kubelog/witness.log:
#
#   "Failed to test etcd connection: ... authentication handshake
#    failed: context deadline exceeded"
#   "Failed to check local etcd status for learner management: ..."
#
# These indicate the witness's local etcd can't come up against the
# new cluster CA/cluster-ID even though WITNESS_JOIN_URL still points
# at a (now-reset) seed.
#
# WHY THE NORMAL TRANSITION LOGIC DOESN'T HANDLE THIS:
#   witness-init.sh's witness_check_mode_transition() detects join-URL
#   CHANGES (e.g. "joined:URL_A" -> "joined:URL_B") and wipes state
#   automatically. But cluster-reset preserves the URL — same seed IP,
#   same token, same CA — only the etcd cluster-ID changes. The
#   transition detector sees no change and keeps the old state, which
#   is now stale. This script is the manual override.
#
# WHERE TO RUN: inside the pkg/witness container on the witness host:
#     # the actual command depends on your eve / linuxkit setup; one of:
#     ctr -n services.linuxkit -a /run/containerd/containerd.sock task exec \
#         --exec-id wipe-and-rejoin --tty witness /usr/bin/wipe-and-rejoin.sh
#     # or, if eve has a `enter witness` helper:
#     eve enter witness  ;  /usr/bin/wipe-and-rejoin.sh
#
# It must run inside the witness container because the k3s state lives
# at /var/lib/rancher/k3s (bind-mounted from /persist/vault/witness)
# and the k3s PID file at /run/witness/k3s.pid is only meaningful in
# the witness container's PID namespace.
#
# WHAT IT DOES:
#   1. Kills the running k3s server (PID from /run/witness/k3s.pid).
#   2. Snapshots etcd db to db.backup-<ts> for rollback.
#   3. Wipes /var/lib/rancher/k3s/server/{db,tls,cred,manifests} and
#      /var/lib/rancher/k3s/agent. This forces a fresh join: k3s will
#      re-fetch the cluster CA and bootstrap certs from the seed via
#      the join URL.
#   4. LEAVES /var/lib/witness/.cluster-mode INTACT. The mode is still
#      "joined:<URL>" — we want the supervisor to rejoin the same
#      cluster, not flip to standalone. (Flipping to standalone would
#      bootstrap a brand-new witness-only single-member cluster, which
#      would then be unable to rejoin.)
#   5. Leaves /etc/rancher/k3s/config.yaml.d/01-clusterconfig.yaml
#      intact — that's where WITNESS_JOIN_URL was rendered into a
#      `server:` line, which is correct for the next start.
#   6. Returns. The witness-init.sh supervisor loop (15s cadence)
#      will notice k3s is dead and restart it with the wiped state,
#      doing a fresh learner add against the seed.
#
# PRESERVED:
#   - WITNESS_MODE_FILE (.cluster-mode = "joined:<URL>")
#   - k3s config (01-clusterconfig.yaml with server: + token:)
#   - 02-witness-network.yaml (node-ip)
#   - /var/lib/witness/* (other witness state)
#
# ROLLBACK:
#   Snapshot at /var/lib/rancher/k3s/server/db.backup-<ts>.
#   To revert:
#     pkill -KILL -f "k3s server"
#     rm -rf /var/lib/rancher/k3s/server/db
#     mv /var/lib/rancher/k3s/server/db.backup-* /var/lib/rancher/k3s/server/db
#   (tls/cred/manifests are NOT backed up — if you need them back, the
#   only recourse is wiping fully and re-joining anyway.)

set -u

WITNESS_K3S_PID_FILE=/run/witness/k3s.pid
K3S_SERVER_DIR=/var/lib/rancher/k3s/server
WITNESS_MODE_FILE=/var/lib/witness/.cluster-mode

echo "=== Witness Wipe-and-Rejoin ==="
echo

# Sanity: are we actually inside the witness container?
if [ ! -f /usr/bin/witness-init.sh ]; then
    echo "ERROR: /usr/bin/witness-init.sh not found." >&2
    echo "       This script must run INSIDE the pkg/witness container." >&2
    exit 1
fi

# Diagnostic: show current mode marker.
if [ -r "$WITNESS_MODE_FILE" ]; then
    echo "Current cluster mode: $(cat "$WITNESS_MODE_FILE")"
else
    echo "Current cluster mode: (none — marker missing)"
fi
echo

# === STEP 1: stop k3s ===
echo "[1/4] Stopping k3s server ..."
if [ -r "$WITNESS_K3S_PID_FILE" ]; then
    pid=$(cat "$WITNESS_K3S_PID_FILE" 2>/dev/null || echo "")
    if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
        echo "  k3s pid=$pid — SIGTERM (30s grace)"
        kill -TERM "$pid" 2>/dev/null || true
        i=0
        while [ $i -lt 30 ] && kill -0 "$pid" 2>/dev/null; do
            sleep 1; i=$((i + 1))
        done
        if kill -0 "$pid" 2>/dev/null; then
            echo "  SIGKILL (didn't exit in 30s)"
            kill -KILL "$pid" 2>/dev/null || true
            sleep 2
        fi
    else
        echo "  PID file present but process not running."
    fi
    rm -f "$WITNESS_K3S_PID_FILE"
else
    echo "  No PID file — falling back to pgrep."
    stray=$(pgrep -f "k3s server" 2>/dev/null || true)
    if [ -n "$stray" ]; then
        echo "  Found k3s pids: $stray — SIGTERM then SIGKILL"
        for p in $stray; do kill -TERM "$p" 2>/dev/null || true; done
        sleep 5
        for p in $stray; do kill -KILL "$p" 2>/dev/null || true; done
    else
        echo "  No k3s server processes running."
    fi
fi

# === STEP 2: snapshot etcd db ===
echo "[2/4] Snapshotting etcd db ..."
if [ -d "$K3S_SERVER_DIR/db" ]; then
    SNAPSHOT_DIR="$K3S_SERVER_DIR/db.backup-$(date +%s)"
    cp -a "$K3S_SERVER_DIR/db" "$SNAPSHOT_DIR"
    echo "  Snapshot: $SNAPSHOT_DIR"
else
    echo "  No db to snapshot."
fi

# === STEP 3: wipe state ===
# Same set of paths witness_leave_cluster() wipes, MINUS WITNESS_MODE_FILE
# (we want to stay in joined mode) and MINUS k3s.yaml (kubeconfig, harmless
# to leave; will be regenerated anyway on first successful join).
echo "[3/4] Wiping k3s state ..."
rm -rf "$K3S_SERVER_DIR/db"
rm -rf "$K3S_SERVER_DIR/tls"
rm -rf "$K3S_SERVER_DIR/cred"
rm -rf "$K3S_SERVER_DIR/manifests"
rm -rf /var/lib/rancher/k3s/agent
echo "  Wiped: db, tls, cred, manifests, agent"
echo "  Preserved: $WITNESS_MODE_FILE ($(cat "$WITNESS_MODE_FILE" 2>/dev/null || echo none))"
echo "  Preserved: /etc/rancher/k3s/config.yaml.d/*.yaml (join URL + node-ip)"

# === STEP 4: wait for supervisor restart ===
echo "[4/4] Waiting for witness-init.sh supervisor to restart k3s ..."
echo "  (supervisor cadence is 15s — first restart attempt within 20s.)"
echo
echo "Tail the witness log to watch the rejoin:"
echo "    tail -f /persist/kubelog/witness.log"
echo
echo "On the survivor (cluster-reset) node, verify the witness re-appears as a learner:"
echo "    etcdctl --endpoints=https://<survivor-ip>:2379 \\"
echo "        --cacert=/var/lib/rancher/k3s/server/tls/etcd/server-ca.crt \\"
echo "        --cert=/var/lib/rancher/k3s/server/tls/etcd/client.crt \\"
echo "        --key=/var/lib/rancher/k3s/server/tls/etcd/client.key \\"
echo "        member list -w table"
echo
echo "=== DONE ==="
