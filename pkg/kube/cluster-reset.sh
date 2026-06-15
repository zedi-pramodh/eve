#!/bin/sh
#
# Copyright (c) 2026 Zededa, Inc.
# SPDX-License-Identifier: Apache-2.0
#
# cluster-reset.sh — recover an EVE-k cluster from etcd quorum loss.
#
# WHEN TO RUN: when too many cluster members died simultaneously to
# maintain quorum (e.g. 2 of 3 members lost at once). Etcd cannot accept
# new members without quorum, so the normal join path is blocked. This
# script uses k3s's `--cluster-reset` to forcibly reduce the cluster to
# a single-member configuration on the surviving node, preserving all
# data (etcd db, k8s objects, secrets, certs, tokens). After it
# completes, the cluster runs as 1 node, and the witness (or other
# nodes) can rejoin normally.
#
# WHERE TO RUN: inside the pkg/kube container on the SURVIVING node.
# Only run on ONE node — running on multiple nodes simultaneously would
# fork the cluster.
#
# USAGE:
#   eve enter kube
#   /usr/bin/cluster-reset.sh
#
#   # Or with an explicit node-ip override (rare — only if config doesn't
#   # have a node-ip line):
#   NODE_IP_OVERRIDE=10.244.240.3 /usr/bin/cluster-reset.sh
#
# WHAT IT DOES:
#   1. Stops the pkg/kube supervisor so it doesn't restart k3s mid-reset.
#   2. Stops running k3s server processes (SIGTERM, 60s grace, SIGKILL).
#   3. Comments out any `server:` lines in k3s config (--cluster-reset
#      refuses to run when a join URL is configured). Comments only the
#      line, not the file, so node-ip and other settings are preserved.
#   4. Snapshots the etcd db to db.backup-<timestamp> for rollback.
#   5. Runs `k3s server --cluster-reset --node-ip=<NODE_IP>`. The
#      explicit --node-ip avoids the edge case where k3s falls back to
#      the LAN IP when constructing the new single-member entry.
#   6. On SUCCESS: replaces the commented `server: <old-url>` lines with
#      `cluster-init: true`. After reset, THIS node owns the cluster, so
#      every subsequent restart must self-bootstrap — not try to join the
#      (now-dead) old seed. On FAILURE: restores the original `server:`
#      line so the operator can retry.
#   7. Re-enables the pkg/kube supervisor and waits up to 3 minutes for
#      etcd to come back healthy.
#   8. Verifies the local etcd member's peer URL matches NODE_IP; if not,
#      uses `etcdctl member update` to fix it.
#   9. Removes any unstarted/orphan member entries (typically leftover
#      learners from pre-reset failed witness joins).
#
# PRESERVED ACROSS RESET:
#   - server/agent tokens (/var/lib/rancher/k3s/server/token, agent-token)
#   - cluster CA + all certs (/var/lib/rancher/k3s/server/tls/)
#   - all kubernetes objects in etcd
#   - kubeconfig (/etc/rancher/k3s/k3s.yaml)
#   - node-ip and other config (we comment server: line only)
#
# CHANGES AFTER RESET:
#   - etcd cluster ID (new — old members can't rejoin with their existing
#     state; they must wipe and rejoin as fresh members)
#   - etcd member list (down to 1 member; old members removed)
#   - Raft term/index resets
#   - k3s config: if this node was a follower (had `server:` set), that
#     line is replaced with `cluster-init: true`. This node is now the
#     bootstrap of the new cluster, and must self-bootstrap on every
#     restart.
#
# ROLLBACK IF SOMETHING GOES WRONG:
#   The snapshot is at /var/lib/rancher/k3s/server/db.backup-<timestamp>.
#   To revert:
#     rm -rf /var/lib/rancher/k3s/server/db
#     mv /var/lib/rancher/k3s/server/db.backup-* /var/lib/rancher/k3s/server/db
#     rm -f /run/kube/k3s-stop

set -u

K3S_BIN=/var/lib/k3s/bin/k3s
K3S_STOP_FLAG=/run/kube/k3s-stop
K3S_MANUAL_START_FLAG=/run/kube/k3s-start
CONFIG_FILE=/etc/rancher/k3s/config.yaml
CONFIG_DIR=/etc/rancher/k3s/config.yaml.d
ETCD_CA=/var/lib/rancher/k3s/server/tls/etcd/server-ca.crt
ETCD_CERT=/var/lib/rancher/k3s/server/tls/etcd/client.crt
ETCD_KEY=/var/lib/rancher/k3s/server/tls/etcd/client.key

# Marker used to identify lines we commented out, so the restore step
# only touches OUR comments (not pre-existing comments by the operator).
SERVER_MARK="#__cluster_reset_disabled__#"

echo "=== K3s Cluster-Reset Recovery ==="
echo

# === STEP 0: discover node-ip from existing config (or env override) ===
NODE_IP=$(grep -h "^node-ip:" "$CONFIG_FILE" "$CONFIG_DIR"/*.yaml 2>/dev/null \
    | head -1 | sed 's/^node-ip:[[:space:]]*//' | tr -d '"' | tr -d "'" | tr -d '[:space:]')
[ -z "$NODE_IP" ] && NODE_IP="${NODE_IP_OVERRIDE:-}"

if [ -z "$NODE_IP" ]; then
    echo "FATAL: cannot auto-detect node-ip from config." >&2
    echo "       Re-run with NODE_IP_OVERRIDE=10.244.240.X /usr/bin/cluster-reset.sh" >&2
    exit 1
fi
echo "Detected node-ip: $NODE_IP"
echo

# === STEP 1: stop the pkg/kube supervisor ===
echo "[1/8] Setting k3s-stop flag so supervisor doesn't restart k3s mid-reset ..."
mkdir -p /run/kube
touch "$K3S_STOP_FLAG"
rm -f "$K3S_MANUAL_START_FLAG"
sleep 2

# === STEP 2: stop running k3s server processes ===
echo "[2/8] Stopping k3s server processes ..."
# Use kube_k3s_pids if available — filters out pkg/witness's k3s by
# cgroup, so we don't kill the witness here if it's also running on this
# device.
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
    echo "  k3s stopped."
else
    echo "  No k3s server processes running."
fi

# === STEP 3: comment out 'server:' lines ===
# k3s --cluster-reset refuses to run when a server: (join URL) is set in
# config. We must temporarily disable just that line — NOT remove the
# whole file (the file may also contain node-ip, token, etc.). sed
# prefixes the line with our marker; step 5b reverses this.
echo "[3/8] Commenting out 'server:' lines in config ..."
DISABLED_FILES=""
for f in "$CONFIG_FILE" "$CONFIG_DIR"/*.yaml; do
    [ -f "$f" ] || continue
    if grep -q "^server:" "$f" 2>/dev/null; then
        echo "  $f: commenting 'server:' line"
        sed -i "s|^server:|${SERVER_MARK}server:|" "$f"
        DISABLED_FILES="$DISABLED_FILES $f"
    fi
done
[ -z "$DISABLED_FILES" ] && echo "  (no server: lines found)"

# === STEP 4: snapshot etcd db ===
echo "[4/8] Snapshotting etcd db ..."
SNAPSHOT_DIR=/var/lib/rancher/k3s/server/db.backup-$(date +%s)
if [ -d /var/lib/rancher/k3s/server/db ]; then
    cp -a /var/lib/rancher/k3s/server/db "$SNAPSHOT_DIR"
    echo "  Snapshot: $SNAPSHOT_DIR"
    echo "  Rollback if needed:"
    echo "    rm -rf /var/lib/rancher/k3s/server/db"
    echo "    mv $SNAPSHOT_DIR /var/lib/rancher/k3s/server/db"
else
    echo "  No db to snapshot."
fi

# === STEP 5: run cluster-reset ===
# --node-ip is passed explicitly so k3s doesn't pick the LAN IP for the
# new single-member entry (observed bug in some edge cases where the
# config's node-ip wasn't honored during reset).
echo "[5/8] Running: k3s server --cluster-reset --node-ip=$NODE_IP"
echo "---"
"$K3S_BIN" server --cluster-reset --node-ip="$NODE_IP"
RESET_RC=$?
echo "---"

# === STEP 5b: handle 'server:' lines based on reset outcome ===
# Critical: after a SUCCESSFUL cluster-reset, this node is now the new
# bootstrap of a single-member cluster. If we restore the original
# `server: <old-url>` line, k3s on the next restart will try to JOIN
# the (now-dead) old seed instead of bootstrapping locally — etcd will
# never start, and the cluster is dead again. So on success we
# PERMANENTLY swap `server:` for `cluster-init: true`.
#
# On failure, we restore the original config (operator wanted to retry,
# not commit to a half-baked new state).
if [ $RESET_RC -ne 0 ]; then
    echo "[5b/8] Reset FAILED — restoring original 'server:' lines ..."
    for f in $DISABLED_FILES; do
        [ -f "$f" ] || continue
        sed -i "s|^${SERVER_MARK}server:|server:|" "$f"
        echo "  $f: restored"
    done
    echo "WARNING: cluster-reset exit code $RESET_RC. Inspect output above." >&2
    echo "Config restored to pre-reset state. Rollback snapshot: $SNAPSHOT_DIR" >&2
    exit $RESET_RC
fi

echo "[5b/8] Reset OK — promoting this node to bootstrap (cluster-init: true) ..."
for f in $DISABLED_FILES; do
    [ -f "$f" ] || continue
    # Replace the whole commented line with `cluster-init: true`. After
    # cluster-reset, this node owns the cluster — it must self-bootstrap
    # on every subsequent restart, not try to join a dead seed.
    sed -i "s|^${SERVER_MARK}server:.*|cluster-init: true|" "$f"
    echo "  $f: server: <url> -> cluster-init: true"
done
echo "  cluster-reset OK."

# === STEP 6: re-enable supervisor and wait for etcd to come back ===
echo "[6/8] Re-enabling supervisor and waiting for etcd (up to 3min) ..."
rm -f "$K3S_STOP_FLAG"
touch "$K3S_MANUAL_START_FLAG"

waited=0
while [ $waited -lt 180 ]; do
    if etcdctl --dial-timeout=2s --command-timeout=2s \
        --endpoints=https://127.0.0.1:2379 \
        --cacert="$ETCD_CA" --cert="$ETCD_CERT" --key="$ETCD_KEY" \
        endpoint health >/dev/null 2>&1; then
        break
    fi
    sleep 5; waited=$((waited + 5))
done

if [ $waited -ge 180 ]; then
    echo "WARNING: etcd not healthy after 3min." >&2
    echo "         Check /persist/kubelog/k3s.log for errors." >&2
    exit 1
fi
echo "  etcd healthy after ${waited}s."

# === STEP 7: verify + fix peer URL ===
# In some cluster-reset edge cases, k3s sets the new member entry's
# peer URL to the LAN IP instead of the cluster node-ip. Detect and
# correct that here. Idempotent: skips if already correct.
echo "[7/8] Verifying peer URL on local member ..."
members_json=$(etcdctl --endpoints=https://127.0.0.1:2379 \
    --cacert="$ETCD_CA" --cert="$ETCD_CERT" --key="$ETCD_KEY" \
    member list -w json 2>/dev/null)

expected_peer="https://${NODE_IP}:2380"
member_id_dec=$(echo "$members_json" | jq -r --arg ip "$NODE_IP" \
    '.members[] | select(.clientURLs[]? | contains($ip)) | .ID' | head -1)
current_peer=$(echo "$members_json" | jq -r --arg ip "$NODE_IP" \
    '.members[] | select(.clientURLs[]? | contains($ip)) | .peerURLs[0]' | head -1)

if [ -z "$member_id_dec" ]; then
    echo "  WARNING: couldn't identify local member entry. Current members:" >&2
    etcdctl --endpoints=https://127.0.0.1:2379 \
        --cacert="$ETCD_CA" --cert="$ETCD_CERT" --key="$ETCD_KEY" \
        member list -w table
elif [ "$current_peer" != "$expected_peer" ]; then
    # etcdctl 'member update' expects HEX, jq gives DECIMAL.
    member_id_hex=$(printf '%x' "$member_id_dec")
    echo "  Peer URL mismatch (got '$current_peer', want '$expected_peer'). Updating ..."
    etcdctl --endpoints=https://127.0.0.1:2379 \
        --cacert="$ETCD_CA" --cert="$ETCD_CERT" --key="$ETCD_KEY" \
        member update "$member_id_hex" --peer-urls="$expected_peer"
else
    echo "  Peer URL correct: $current_peer"
fi

# === STEP 8: clean up unstarted/orphan member entries ===
# Pre-reset failed joins (e.g. a witness that was added as a learner
# but never started) leave entries in etcd with name="". Remove them so
# the cluster's member list is clean.
echo "[8/8] Cleaning up unstarted/orphan member entries ..."
unstarted=$(echo "$members_json" | \
    jq -r '.members[] | select(.name == null or .name == "") | .ID' 2>/dev/null)
if [ -n "$unstarted" ]; then
    for id_dec in $unstarted; do
        id_hex=$(printf '%x' "$id_dec")
        echo "  Removing orphan id=$id_hex"
        etcdctl --endpoints=https://127.0.0.1:2379 \
            --cacert="$ETCD_CA" --cert="$ETCD_CERT" --key="$ETCD_KEY" \
            member remove "$id_hex" || echo "    (failed)" >&2
    done
else
    echo "  No orphans."
fi

echo
echo "=== DONE ==="
echo
echo "Final cluster state:"
etcdctl --endpoints=https://127.0.0.1:2379 \
    --cacert="$ETCD_CA" --cert="$ETCD_CERT" --key="$ETCD_KEY" \
    member list -w table

echo
echo "kubectl get nodes:"
kubectl get nodes -o wide 2>/dev/null || \
    echo "  (kubectl not ready yet — wait ~30s and try again)"

echo
echo "Next steps:"
echo "  1. Verify cluster functional: 'kubectl get pods -A'"
echo "  2. To re-add the witness: drop /persist/witness-override.env on"
echo "     the witness device with WITNESS_JOIN_URL pointing to this"
echo "     node's apiserver. Witness will rejoin within ~30s."
echo "  3. When the dead node(s) come back, wipe their k3s state"
echo "     (/persist/vault/kube/rancher) and they'll rejoin as fresh"
echo "     members."
echo
echo "Backup snapshot: $SNAPSHOT_DIR"
