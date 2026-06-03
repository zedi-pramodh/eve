# EVE-k Witness Node (pkg/witness)

**Status:** Phase 1 working as of 2026-06-03 — witness comes up as a full
k3s control-plane + etcd master node alongside pkg/kube on the same
physical host, with `Ready,SchedulingDisabled` status and zero workload
pods. Validated:

```
$ kubectl get nodes
NAME          STATUS                     ROLES                AGE   VERSION
eve-witness   Ready,SchedulingDisabled   control-plane,etcd   24m   v1.34.2+k3s1
```

The path to Phase 1 working was much longer than the original §5 plan
suggested. Roughly every k3s subsystem (apiserver, controller-manager,
scheduler, kubelet, kubelet-healthz, etcd loopback) had a port or device
collision with pkg/kube in the shared host network namespace. §8a now
documents every one — read it before adding any new collocated
component.

Phase 2 (cluster join) is **partly wired but not validated**. The code
path is in place (override file → `render_witness_cluster_config` →
drops `--cluster-init` via `is_witness_joining`) but the architectural
blockers documented in §8a "Co-location ceiling" haven't been resolved.

**Branch:** `eve-witness`
**Related external doc:** *EVE-k 2-Node HA Design — pkg/k3s Witness Architecture* (Google Docs, dated May 26 2026).

This is the in-tree companion to the high-level design doc. It records the
concrete decisions, the rationale that isn't obvious from reading code, and
the open questions remaining for Phase 2.

---

## 1. Problem

Standard Kubernetes requires an odd number of etcd members for quorum. A
2-physical-node EVE-k cluster cannot achieve HA from the etcd side because
two members can never reach majority on their own.

`pkg/witness` introduces a lightweight third k3s server that runs as a
container on the seed node, providing the third etcd vote without a third
physical machine. Quorum becomes 2-of-3 across two devices: either the seed
or the non-seed can fail and the cluster keeps writing.

## 2. Identity

| Property | Value | Notes |
|---|---|---|
| Node name | `eve-witness` | Reserved. Fixed across all installs. pkg/kube reclaims the third member by this name. |
| Phase 1 IP | `10.244.244.244` (dummy iface `eve-witness0`) | Bootstrap-only crutch. See §5. |
| Phase 2 IP | Provided by cloud via `EdgeNodeClusterStatus` | Real, cluster-routable. Pillar provisions it as a secondary address on the cluster interface. See §6. |

## 3. Package shape

```
pkg/witness/
├── build.yml             # linuxkit manifest. image: eve-witness. pid:host, host net.
├── Dockerfile            # eve-alpine base. k3s install at runtime, etcdctl baked in.
├── config.yaml           # /etc/rancher/k3s/config.yaml — workload-free, bind to witness IP.
├── 00-nodename.yaml      # /etc/rancher/k3s/config.yaml.d/ — node-name fragment.
├── lib/config.sh         # Shared shell constants (WITNESS_NODE_NAME, WITNESS_NODE_IP, etc.)
├── witness-utils.sh      # logmsg, setup_witness_interface, mount_witness_root, install_k3s, terminate_k3s.
└── witness-init.sh       # Entrypoint. Orchestrates prereqs, install, supervise.
```

Wiring into the rootfs image lives in **two** files outside pkg/witness:

- `images/modifiers/hv/k.yq` — appends a `witness` service entry alongside `kube`
  whenever `HV=k`. Regenerated into `images/out/rootfs-k-generic.yml.in` by
  every build (`RESCAN_DEPS=FORCE`).
- `tools/parse-pkgs.sh` — exports `WITNESS_TAG=$(linuxkit_tag pkg/witness)`
  for placeholder substitution in the generated rootfs yml.

## 4. Coexistence with pkg/kube on the same physical node

Both containers run with `pid: host` *and* share the host network namespace
(neither build.yml declares `net: host`, but linuxkit treats services as
host-net by default). That means **everything is potentially shared**:
process list, port space, interface list, sockets in `/run`. Three places
need explicit deconfliction.

### 4.1 Process identification

Naive `pgrep -f "k3s server"` in either container matches both k3s
processes. Resolution:

- **Witness side**: launch k3s as `k3s server --node-name eve-witness …`
  even though node-name is also in `00-nodename.yaml`. The CLI repeat is
  purely to give the witness's process a unique cmdline. `K3S_SERVER_CMD`
  in `witness-utils.sh` includes the full signature.
- **pkg/kube side**: `cluster-utils.sh` defines a helper
  `kube_k3s_pids()` that runs the broad pgrep, then drops any PID whose
  `/proc/<pid>/cmdline` contains `eve-witness`. Every existing
  `pgrep -f "$K3S_SERVER_CMD"` callsite in `cluster-init.sh` and
  `cluster-utils.sh` was migrated to this helper.

**Rule for future patches:** any new pgrep against the k3s process in
either package MUST go through the witness-unique pattern or the
`kube_k3s_pids` helper.

### 4.2 Network ports

Almost solved by **binding the witness to a different IP**, not by port-shifting:
both k3s servers use the same default ports for IP-bound listeners (6443
supervisor, 2379/2380 etcd, 10250 kubelet) because they listen on different
IPs — `10.244.244.244` for the witness, the real cluster interface for pkg/kube.

- Phase 1: dummy interface `eve-witness0` with `10.244.244.244/32`. Created
  by `setup_witness_interface` in `witness-utils.sh`. Only reachable from
  the seed itself.
- Phase 2: cloud-provided IP on the real cluster interface (see §6).

The exception is the **agent loadbalancer**, which always binds `127.0.0.1`
(not the node-ip) and whose default port is hardcoded to `6444` regardless
of `supervisor-port`. Two k3s servers in the same network namespace both want
`127.0.0.1:6444` and the second one loses. This bit us — see §8a "agent
loadbalancer collides on 127.0.0.1:6444". The fix is `lb-server-port: 6645`
in `config.yaml`; the related `supervisor-port: 6644` and
`https-listen-port: 6644` shifts are belt-and-braces.

### 4.3 Control plane components — dedicated etcd node mode

The witness runs as a k3s "dedicated etcd node" — the canonical k3s
pattern for a server that participates in etcd consensus but doesn't
host control-plane logic. From `config.yaml`:

```yaml
# Listen-port relocations. Most listeners bind to node-ip (10.244.244.244)
# so they don't actually collide with pkg/kube's defaults on a different IP,
# but the agent loadbalancer is loopback-bound (127.0.0.1) and its port is
# a SEPARATE hardcoded default — not derived from supervisor-port. See §8a.
https-listen-port: 6644
supervisor-port: 6644
lb-server-port: 6645

disable-apiserver: true
disable-controller-manager: true
disable-scheduler: true
disable-kube-proxy: true
flannel-backend: none
container-runtime-endpoint: "unix:///run/witness/containerd/containerd.sock"
node-taint:
  - "node-role.kubernetes.io/witness=true:NoSchedule"
  - "CriticalAddonsOnly=true:NoExecute"
node-label:
  - "node-role.kubernetes.io/witness=true"
```

What runs vs. what doesn't:

- **etcd member** — runs. The whole point of the witness.
- **kube-apiserver** — disabled. No `:6443`, no hardcoded
  `127.0.0.1:6444` loopback proxy.
- **kube-controller-manager** — disabled. No competition for the
  controller-manager lease.
- **kube-scheduler** — disabled. Same rationale.
- **kubelet** — **runs**. This is the key thing. Embedded etcd in k3s
  requires kubelet so a Node object exists to track etcd cluster
  membership — see §8a "etcd-only k3s + --disable-agent is unsupported".
  Kubelet binds to `node-ip` (`10.244.244.244:10250`), no collision with
  pkg/kube's kubelet on the seed's real interface.
- **kube-proxy** — disabled. Witness hosts no services and we don't want
  it programming host iptables (shared with pkg/kube's kube-proxy).
- **flannel** — disabled (`flannel-backend: none`). No `flannel.1` VXLAN
  device (which would collide with pkg/kube's, same global name).
- **containerd** — runs, but as our own private process at
  `/run/witness/containerd/containerd.sock` (see §4.5). k3s's embedded
  containerd at `/run/k3s/containerd/containerd.sock` is never started
  because `container-runtime-endpoint` points kubelet elsewhere.
- **Node object** — **registered** (named `eve-witness`). Visible in
  `kubectl get nodes`. Carries the `node-role.kubernetes.io/witness=true`
  label and the NoSchedule + NoExecute taints — nothing schedules on it.

The combination of "everything's running but nothing useful happens" is
intentional: the witness looks like a normal-but-tainted k3s node from
Kubernetes' perspective (which keeps k3s's internal invariants happy),
but no workload, service, or networking interaction actually occurs.
That eliminates the entire class of "k3s crashed because some internal
watcher couldn't reach component X" failures we hit in earlier iterations
(see §8a).

### 4.4 Kubeconfig

There is **no witness kubeconfig**. With `disable-apiserver: true` k3s
never starts an apiserver and never writes `/etc/rancher/k3s/k3s.yaml`.

Witness liveness is verified by `etcdctl member list` from inside the
witness container (§7.2) AND by `kubectl get nodes` on the seed (the
witness appears there because kubelet runs — §4.3). Workload-level
debugging happens through pkg/kube's kubeconfig on the seed or non-seed.

### 4.5 Private containerd

Kubelet on the witness MUST talk to a CRI, and `/run` is shared with
pkg/kube. We spawn a witness-private containerd at
`/run/witness/containerd/containerd.sock` (mirroring pkg/kube's
`/run/containerd-user/containerd.sock` pattern) and pass that path to
k3s via `container-runtime-endpoint`. k3s never starts its embedded
containerd — kubelet uses ours.

Containerd config lives at `/etc/witness/containerd-config.toml` (baked
into the image; see overlayfs note in §8a). Key witness-specific values:

| Field | Witness | pkg/kube |
|---|---|---|
| `state` | `/run/witness/containerd` | `/run/containerd-user` |
| `root` | `/var/lib/witness-containerd` | `/persist/vault/containerd` |
| `grpc.address` | `/run/witness/containerd/containerd.sock` | `/run/containerd-user/containerd.sock` |
| CRI `stream_server_port` | `10011` | `10010` |

The `check_start_containerd` helper in `witness-utils.sh` spawns and
supervises this containerd, idempotent on container restart.

## 5. Phase 1 — Standalone bootstrap (implemented)

Goal: prove the container builds and k3s comes up. Nothing talks to the
witness except itself.

Flow in `witness-init.sh`:

1. `setup_prereqs` — log dirs, modprobe `dummy` and `br_netfilter`,
   cgroup mount, wait for default route.
2. `setup_witness_interface` — creates `eve-witness0` dummy iface with
   `10.244.244.244/32`. Idempotent.
3. `wait_for_vault` — polls `/persist/vault` until readable. The witness
   doesn't have `/hostfs` bind-mounted, so we can't call `vaultmgr`
   directly; readability of `/persist/vault` is the proxy signal that
   vaultmgr has unsealed it.
4. `mount_witness_root` — bind-mounts `/persist/vault/witness` (sibling
   of `/persist/vault/kube`) onto `/var/lib`. Makes k3s data persistent
   across reboots while keeping witness state completely isolated from
   pkg/kube's state.
5. `install_k3s` — downloads pinned k3s (`v1.34.2+k3s1`, matched to
   pkg/kube), installs binary into `/var/lib/k3s/bin/k3s`, symlinks to
   `/usr/bin/k3s`. Skipped on subsequent boots (marker file
   `/var/lib/k3s_installed_unpacked` lives in the persistent mount).
6. Main loop:
   a. `check_start_containerd` spawns/supervises witness-private
      containerd at `/run/witness/containerd/containerd.sock` using
      `/etc/witness/containerd-config.toml`. Idempotent.
   b. `start_k3s_once` launches
      `k3s server --node-name eve-witness --cluster-init` with
      exponential backoff on crashes. Everything else
      (`disable-apiserver`, `disable-controller-manager`,
      `disable-scheduler`, `disable-kube-proxy`, `flannel-backend: none`,
      `container-runtime-endpoint`, taints, labels) lives in
      `config.yaml` so the CLI stays short and the unique pgrep
      signature stays predictable.

In Phase 1 the witness is a self-contained one-member etcd cluster.
Apiserver/controller-manager/scheduler are off; kubelet IS on and
registers the Node as `eve-witness` (visible in `kubectl get nodes` only
once Phase 2 has connected the witness to a cluster with an apiserver —
in standalone Phase 1 there's no apiserver, so kubelet logs warnings
about not being able to register and retries forever, which is fine).
etcd is reachable via `etcdctl --endpoints https://10.244.244.244:2379`.

## 6. Phase 2 — Joining the seed's cluster (not yet implemented)

The mechanic mirrors how a non-seed pkg/kube joins today.

### 6.1 Inputs

Read from `/run/zedkube/EdgeNodeClusterStatus/global.json` (zedkube pubsub
publication that pkg/kube already consumes):

- Join server URL (seed's apiserver, e.g. `https://192.168.1.10:6443`)
- Cluster token
- Cluster ID (for sanity-check, same as pkg/kube does)
- **Witness IP** — TBD where exactly this field lives. Open question.

### 6.2 Cloud-side contract

- The cloud allocates a second cluster-routable IP for the seed (the
  witness's IP). It's a real address on the cluster subnet, not a
  10.244.244.244 fiction.
- Pillar / zedkube — the same EVE component that already provisions the
  seed's primary cluster IP — adds the witness IP as a secondary address
  on the cluster interface before publishing the ENC.
- By the time pkg/witness reads the ENC, the IP is already on the wire
  and routable from the non-seed.

### 6.3 Witness-side flow

1. Detect ENC publication (file-watch on the global.json path).
2. Render `/etc/rancher/k3s/config.yaml.d/01-clusterconfig.yaml`:
   ```yaml
   server: "https://<JoinServerIP>:6443"
   token: "<cluster-token>"
   node-ip: "<witness-cloud-ip>"
   bind-address: "<witness-cloud-ip>"
   advertise-address: "<witness-cloud-ip>"
   tls-san:
     - "<witness-cloud-ip>"
   ```
3. Tear down Phase 1 standalone state:
   - `terminate_k3s` (witness-unique signature is safe)
   - Remove the dummy `eve-witness0` interface
   - Optionally wipe `/var/lib/rancher/k3s/server/db/etcd` to force fresh
     join (Phase 1 etcd has its own bootstrap cluster ID).
4. Restart k3s: drop `--cluster-init`, keep `--disable-agent`, keep
   `--node-name eve-witness`. Server-mode launch reads the config files
   we just rendered.
5. Witness joins as the third etcd member with the cloud-provided IP.

### 6.4 Open questions

- **ENC field shape.** Three options floated:
  - `EdgeNodeClusterStatus.WitnessIPPrefix` (top-level)
  - `EdgeNodeClusterStatus.Witness = { IPPrefix, ... }` (nested substruct
    leaving room for future witness-only settings)
  - Whatever the API surface ends up being

  Witness-side code should isolate the JSON-path read behind a single
  helper so the field can move without rippling.
- **Monitor for ENC arrival mid-flight.** If the witness booted into
  Phase 1 (standalone) and then the cloud config arrives, we need an
  analog of pkg/kube's `monitor_cluster_config_change`. Probably a
  background poll that triggers the transition.
- **Cluster-reset path on seed failure.** Design doc §6 describes the
  recovery flow when the seed dies — the non-seed runs `k3s server
  --cluster-reset` and a new seed device is provisioned. Witness side
  needs to handle being torn down on the dying seed and re-formed when
  the new seed boots. Hasn't been designed yet.

## 7. Operations & observability

The witness is a dedicated etcd-node — the agent (kubelet) IS running, so
a Node object IS registered (visible in `kubectl get nodes` in Phase 2),
but no control-plane component runs and the NoSchedule + NoExecute taints
keep workloads off. The witness is alive and voting iff it shows up in
**etcd membership**, not just in node lists — in Phase 1 standalone there
is no apiserver to back `kubectl` at all (see §7.1 / §7.5).

### 7.1 What `kubectl get nodes` shows

In Phase 2 (joined cluster):

```
NAME              STATUS   ROLES                       AGE   VERSION
seed-node-uuid    Ready    control-plane,etcd,master   1h    v1.34.2+k3s1
eve-witness       Ready    etcd,witness                1h    v1.34.2+k3s1
non-seed-uuid     Ready    control-plane,etcd,master   1h    v1.34.2+k3s1
```

The witness appears because its kubelet runs and registers the Node (this
is required by embedded etcd — see §8a). The `witness` role label and a
NoSchedule taint keep workloads off.

In Phase 1 standalone the witness has no apiserver locally and there's
no other cluster to register with, so `kubectl get nodes` against the
witness is not possible. Use `etcdctl member list` (§7.2) instead.

### 7.2 Canonical witness liveness check — etcd membership

The witness's identity in the cluster is its etcd membership. `etcdctl` is
baked into the witness Dockerfile precisely so this check is one command
away:

```sh
# From inside the witness container:
ETCDCTL_CACERT=/var/lib/rancher/k3s/server/tls/etcd/server-ca.crt \
ETCDCTL_CERT=/var/lib/rancher/k3s/server/tls/etcd/client.crt \
ETCDCTL_KEY=/var/lib/rancher/k3s/server/tls/etcd/client.key \
etcdctl --endpoints https://127.0.0.1:2379 member list

# Phase 2 — three rows expected:
# <id>, started, seed-node-uuid, https://<seed-ip>:2380,    https://<seed-ip>:2379
# <id>, started, eve-witness,    https://<witness-ip>:2380, https://<witness-ip>:2379
# <id>, started, non-seed-uuid,  https://<non-seed-ip>:2380, https://<non-seed-ip>:2379
```

If `eve-witness` is missing from this list, the third vote isn't being
counted and the cluster is operating without HA even if the other two
members are healthy.

`etcdctl endpoint health` and `etcdctl endpoint status` work the same way
and report per-member health + raft term + leader info.

### 7.3 Cluster-wide signals

The witness's visibility into Kubernetes is mixed because it runs
kubelet but no apiserver/controller-manager/scheduler:

- **`kubectl get nodes`** — witness appears (Phase 2). Role: `etcd,witness`.
- **`kubectl -n default get endpoints kubernetes`** — only pkg/kube
  apiservers appear. The witness has no apiserver to register here.
- **Controller-manager / scheduler leases** — only pkg/kube candidates
  compete; the witness never participates in leader election (those
  components don't run on it).
- **`kubectl get componentstatuses`** — irrelevant on the witness side
  (no apiserver to query).
- **Pod scheduling** — never. The NoSchedule + NoExecute taints prevent
  it, and even if a wildcard toleration existed, there's no kube-proxy
  and no CNI for the pod to actually run.

### 7.4 ~~Talking directly to the witness's apiserver~~ (removed)

There is no witness apiserver in dedicated-etcd-node mode. Skip directly
to §7.5.

### 7.5 The dumbest, most reliable check

When everything else is suspicious, tail the supervisor log:

```sh
tail -f /persist/kubelog/witness.log         # k3s server stdout/stderr
tail -f /persist/kubelog/witness-install.log # supervisor / install lifecycle
```

If `witness-install.log` is growing every 15 seconds and `witness.log`
isn't crash-looping (no repeated "Starting k3s …" lines), the witness is
running.

### 7.6 Quick mental model

| Question | Where to look |
|---|---|
| Is the witness's k3s process alive? | `pgrep -f "k3s server --node-name eve-witness"` on the seed |
| Is the witness's containerd alive? | `pgrep -f "/etc/witness/containerd-config.toml"` on the seed |
| Is the witness voting in etcd? | `etcdctl member list` — must contain `eve-witness` |
| Is the witness Node registered? | `kubectl get nodes` shows `eve-witness` with role `etcd,witness` (Phase 2 only) |
| Is the witness's apiserver healthy? | **N/A — there is no witness apiserver.** |
| Is the witness scheduling workloads? | **No, it never does.** NoSchedule + NoExecute taints. |
| Should anything ever run on the witness's kubelet? | **No.** Taints prevent it; even if they didn't, no CNI/kube-proxy. |

## 8. Persistence layout

```
/persist/vault/                  ← encrypted vault (managed by vaultmgr)
├── kube/                        ← pkg/kube's persistent state
│   └── rancher/k3s/…
└── witness/                     ← pkg/witness's persistent state (sibling)
    ├── k3s/bin/k3s              ← witness's k3s binary
    ├── k3s_installed_unpacked   ← install marker
    └── rancher/k3s/             ← witness's etcd, server certs, kubeconfig source
```

Inside the witness container, `/persist/vault/witness` is bind-mounted
onto `/var/lib`. So in-container paths like `/var/lib/rancher/k3s/server/db`
resolve to `/persist/vault/witness/rancher/k3s/server/db` on disk.

## 8a. Gotchas hit during bring-up

Things that broke during the first successful boot, with the fix recorded
so we don't re-learn them.

### Don't override etcd peer/client URLs via `etcd-arg`

**Symptom:** k3s starts, succeeds in launching its main etcd, then on a
subsequent restart fails during bootstrap reconciliation with:

```
configuring peer listeners","listen-peer-urls":["https://10.244.244.244:2380"]
creating peer listener failed",
"error":"cannot listen on TLS for 10.244.244.244:2380:
        KeyFile and CertFile are not presented"
Failed to reconcile with temporary etcd: …
```

**Cause:** k3s's `StartEmbeddedTemporary` (in `pkg/cluster/bootstrap.go`)
honours user-supplied `etcd-arg listen-peer-urls=…` but does **not** inject
the matching `--peer-cert-file` / `--peer-key-file`. The transient etcd
gets a TLS URL with no certs and dies. The persistent etcd path doesn't
hit this because it builds the full TLS config from k3s's own cert store.

**Fix:** drop the URL overrides. k3s derives all four etcd URLs
(`listen-client-urls`, `listen-peer-urls`, `advertise-client-urls`,
`initial-advertise-peer-urls`) from `node-ip`. Only override etcd flags
that don't touch cert plumbing (quota, compaction, snapshot count).

**Rule:** if you find yourself writing `etcd-arg listen-…=https://…`,
stop. Set `node-ip` and let k3s do the wiring.

**Recovery on a device that already booted into the broken state:** the
witness's etcd data is stale and must be wiped along with the config fix.
Editing in-place does not work — see next gotcha.

### `/etc/rancher/k3s/config.yaml` is in the container overlay, not persistent

**Symptom:** edit `/etc/rancher/k3s/config.yaml` inside the running witness
container, restart k3s — works once. Restart the container (or reboot the
device) — the fix is gone and the broken config is back.

**Cause:** the Dockerfile bakes the file in via
`COPY config.yaml /etc/rancher/k3s/config.yaml`. linuxkit gives each
container a fresh overlayfs on every start, so anything written under
`/etc` only lives until the next container restart. `/var/lib` is the
only writable path that survives, because it's bind-mounted to
`/persist/vault/witness`.

**Implication:** any config-shape change to the witness requires a
**rebuild + redeploy**. There is no in-container hotfix path.

```sh
cd ~/eve
make pkg/witness          # rebuild eve-witness container
make HV=k eve             # rebuild rootfs with the new image
# Re-flash / OTA the device, then wipe stale etcd state:
rm -rf /persist/vault/witness/rancher/k3s/server/db
# Restart the witness container or reboot.
```

### `--disable-agent` is not supported with embedded etcd

**Symptom:** with `--disable-agent` and embedded etcd enabled
(`--cluster-init`), k3s fatal-exits every ~15 minutes:

```
level=fatal msg="Tunnel watches failed to wait for apiserver ready:
  timed out waiting for the condition,
  failed to get apiserver /readyz status: apiserver disabled"
```

**Cause:** k3s maintainer brandond, on
[issue #7085](https://github.com/k3s-io/k3s/issues/7085):
> "disable-agent is experimental, and not supported with embedded etcd.
> Embedded etcd needs an agent so that a node object is created to track
> etcd cluster membership."

We were trying to combine both. Internal watchers expect a Node object
to exist; without the agent, no Node is registered; the watchers eventually
give up and exit.

**Fix:** don't use `--disable-agent`. Run kubelet (which registers the
Node) but disable workload-side components — kube-proxy and flannel — at
the `config.yaml` level, and keep workloads off via `node-taint`. The
docs-blessed pattern for dedicated etcd servers explicitly assumes the
agent is running. See §4.3 for the full set of flags.

**Rule:** when in doubt about combining k3s flags, look for a documented
node-role pattern at https://docs.k3s.io/installation/server-roles before
inventing one.

### Agent loadbalancer collides on 127.0.0.1:6444 — disable-apiserver is necessary but not sufficient

**Symptom (round 1, apiserver enabled):** with `bind-address: 10.244.244.244`
correctly relocating the apiserver from `0.0.0.0:6443` to `10.244.244.244:6443`,
k3s still fails:

```
external host was not specified, using 10.244.244.244
Error: failed to create listener: failed to listen on 127.0.0.1:6444:
       listen tcp 127.0.0.1:6444: bind: address already in use
```

**Cause:** k3s's apiserver runs an internal loopback proxy at
`127.0.0.1:6444` to bridge the kubelet to the apiserver. The port is
hardcoded to 127.0.0.1.

**Partial fix:** add `disable-apiserver: true` to `config.yaml`. This
removes the apiserver's binder on `127.0.0.1:6444`.

**Symptom (round 2, apiserver disabled, embedded etcd up):** even with
the apiserver gone — etcd visibly bootstrapping ("Managed etcd cluster
initializing") — the same error reappears:

```
Managed etcd cluster initializing
Error starting load balancer: listen tcp 127.0.0.1:6444: bind: address already in use
Error: listen tcp 127.0.0.1:6444: bind: address already in use
```

**Cause:** k3s has a **second** binder on `127.0.0.1:6444` — the agent's
client-side supervisor loadbalancer (k3s code: `pkg/agent/loadbalancer/`).
This LB exists to give the kubelet a stable local endpoint that proxies
to whichever supervisor is alive. It runs whenever the agent runs, and
the agent has to run because embedded etcd needs the kubelet to track
member-Node mappings (see the "etcd-only k3s + --disable-agent" gotcha
above). The LB's bind port is **a separate hardcoded default of 6444 —
NOT derived from `supervisor-port`**, even though intuition says it
should be. Reading the source is the only way to find this.

**Real fix:** relocate the LB explicitly via the agent flag `lb-server-port`
(which k3s server accepts because the server embeds the agent). Add to
`config.yaml`:

```yaml
lb-server-port: 6645
# Belt-and-braces — also shift the server-side listeners so the witness's
# whole port range is a single contiguous block (6644/6645), easier to
# audit. With disable-apiserver: true these don't actually collide today
# (apiserver isn't bound; supervisor binds on node-ip not 127.0.0.1),
# but pinning them off-default future-proofs against config-shape changes.
https-listen-port: 6644
supervisor-port: 6644
```

After this, `ss -ltn` on the seed shows:

```
10.244.244.244:6644  LISTEN   <-- witness supervisor
127.0.0.1:6645       LISTEN   <-- witness agent LB
<real-ip>:6443       LISTEN   <-- pkg/kube supervisor
127.0.0.1:6444       LISTEN   <-- pkg/kube agent LB
```

**Worse, before the real fix:** k3s shuts down its entire supervised
process tree when any component fails — including the etcd member that
was already running. You see "ETCD server is now running" briefly, then
"etcd server stopped" right after the LB failure. The supervisor in
`witness-init.sh` loops, exec's k3s again, etcd comes up briefly, LB
fails, etcd dies again — forever.

**Empirical record:** validated on k3s `v1.34.2+k3s1`, 2026-05-28. Removing
`lb-server-port` reintroduces the failure. `supervisor-port` alone does
NOT shift the LB port, despite what the k3s flag descriptions suggest.

**Rule:** if k3s has a flag to disable a component, prefer disabling it
over port-shifting around it — but when the component literally cannot
be disabled (agent LB, because the agent is required by embedded etcd),
shift the port via the specific agent flag (`lb-server-port`), not via
`supervisor-port` or `https-listen-port` (those control the *server*
listener, not the agent's LB).

### Witness containerd MUST exec from a witness-unique path

**Symptom:** pkg/kube's k3s server is up, `kubectl get nodes` works, but
the node goes NotReady. `kubectl describe node` shows kubelet stopped
posting status. `tail /persist/kubelog/k3s.log` (pkg/kube side) shows an
endless loop:

```
Waiting for CRI startup: rpc error: code = Unavailable …
dial unix /run/containerd-user/containerd.sock: connect: no such file or directory
```

The `/run/containerd-user/containerd.sock` doesn't exist — pkg/kube's
standalone containerd has died and has NOT been restarted by pkg/kube's
own supervisor (`check_start_containerd` in cluster-init.sh), which uses
`pgrep -f "/var/lib/rancher/k3s/data/current/bin/containerd"` as its
liveness test.

**Cause:** the witness's `check_start_containerd` was exec'ing containerd
from the *exact same path string* — `/var/lib/rancher/k3s/data/current/bin/containerd`.
Each container has its own `/var/lib` (witness's is bound to
`/persist/vault/witness`), so the binary on disk is different, but the
**path string in `/proc/<pid>/cmdline` is identical**. In the shared host
PID namespace pkg/kube's pgrep matches the witness's containerd as if it
were its own, concludes "containerd is fine", and never restarts the dead
pkg/kube containerd. k3s then sits forever on CRI dial.

**Fix:** exec the witness's containerd from a witness-unique path. We
symlink `/var/lib/witness/bin/containerd` → the k3s-shipped binary and
exec from the symlink (`WITNESS_CTRD_BIN` in `lib/config.sh`). The
`/proc/<pid>/cmdline` then visibly identifies the witness's containerd,
and pkg/kube's existing `pgrep -f "/var/lib/rancher/k3s/data/current/bin
/containerd"` no longer matches it.

**Rule (general):** any binary the witness exec's from a path shared with
pkg/kube (k3s itself, containerd, future helpers) needs a witness-unique
in-container path. k3s strips its argv so we use a PID file (§4.1);
containerd doesn't strip argv, so a witness-unique exec path is enough.
Do **not** rely on `--config` or other flags as the discriminator —
they're easier to overlook when adding new pgrep callsites in pkg/kube.

### Full collision matrix on a co-located host

The witness shares the host network namespace, PID namespace, `/run`,
and `/sys/fs/cgroup` with pkg/kube. Every k3s subsystem that hardcodes a
loopback bind or a global device name collides. The complete fix set
that gets Phase 1 working (witness `Ready,SchedulingDisabled` next to a
fully-functional pkg/kube) lives in `pkg/witness/config.yaml`:

| # | Collision | k3s default | Witness fix |
|---|---|---|---|
| 1 | Supervisor / apiserver | `0.0.0.0:6443` | `https-listen-port: 6644`, `supervisor-port: 6644` |
| 2 | Agent loadbalancer | `127.0.0.1:6444` (hardcoded, NOT supervisor-port-derived) | `lb-server-port: 6645` |
| 3 | Etcd client | `127.0.0.1:2379` (SO_REUSEPORT'd with pkg/kube → random misroute → TLS unknown-authority) | `kube-apiserver-arg: etcd-servers=https://${NODE_IP}:2379` (route the apiserver around the collision; trying to override `listen-client-urls` via `etcd-arg` triggers the §8a transient-etcd cert-missing crash) |
| 4 | Etcd peer | `127.0.0.1:2380` (same SO_REUSEPORT issue) | NO FIX in Phase 1. Affects Phase 2 join (raft replication ambiguity). See "Co-location ceiling" below. |
| 5 | Controller-manager secure-port | `127.0.0.1:10257` | `kube-controller-arg: secure-port=10357` |
| 6 | Scheduler secure-port | `127.0.0.1:10259` | `kube-scheduler-arg: secure-port=10359` |
| 7 | Kubelet API | `*:10250` (wildcard, blocks specific-IP bind on a second instance) | `kubelet-arg: port=10350` + matching `kube-apiserver-arg: kubelet-port=10350` so apiserver dials the new port |
| 8 | Kubelet healthz | `127.0.0.1:10248` | `kubelet-arg: healthz-port=10448` |
| 9 | Flannel `flannel.1` device | global VXLAN name | `flannel-backend: none` (no flannel → no `flannel.1`) + `disable-kube-proxy: true` (same iptables-collision logic) |
| 10 | Local-storage volume path | `/persist/vault/volumes` shared with pkg/kube | `default-local-storage-path: /persist/vault/witness-volumes` |
| 11 | k3s embedded containerd socket | `/run/k3s/containerd/containerd.sock` shared with pkg/kube | `container-runtime-endpoint: unix:///run/witness/containerd/containerd.sock` + witness-private containerd spawned by `check_start_containerd` (see "Witness containerd MUST exec from a witness-unique path" above) |
| 12 | k3s addon Pending pods (single-node + tainted = nothing schedules) | `coredns`, `local-storage`, `metrics-server`, `servicelb`, `traefik` all default-on | `disable: [coredns, local-storage, metrics-server, servicelb, traefik]` |
| 13 | k8s 1.34 NodeRestriction admission | `kubelet --node-labels=node-role.kubernetes.io/witness=true` is rejected | Removed `node-label`; only `node-taint` (NoSchedule + NoExecute) blocks scheduling. Apply role label via `kubectl` post-startup if needed. |
| 14 | kubelet wants CNI conf | `flannel-backend: none` means no CNI → kubelet stays NotReady (`cni plugin not initialized`) | `render_witness_cni_stub` writes a loopback-only conflist at `/var/lib/rancher/k3s/agent/etc/cni/net.d/00-witness-stub.conflist` — the path the witness's `containerd-config.toml` looks at (NOT the standard `/etc/cni/net.d/`) |

### Co-location ceiling: what Phase 1 leaves unsolved

After working through the matrix above, two architectural issues remain
that block Phase 2 cluster-join when the witness is co-located with a
seed:

1. **Etcd peer port `127.0.0.1:2380`**. The witness's etcd and pkg/kube's
   etcd both bind this via SO_REUSEPORT. `--etcd-arg listen-peer-urls=`
   would deconflict, but trips the bootstrap-transient-etcd cert-missing
   bug already documented at the top of §8a. There's no clean k3s flag
   to skip the loopback peer listener. For Phase 1 standalone (one
   member, no raft replication needed), this doesn't matter. For Phase
   2 join, raft messages between members are routed randomly between
   pkg/kube's and witness's etcd → cluster-info fetch fails or returns
   wrong-cluster certs.

2. **`flannel-backend` critical-config mismatch**. k3s's join handshake
   checks that `flannel-backend` is identical across all etcd-member
   servers. pkg/kube uses the default (vxlan); witness needs `none` to
   avoid the `flannel.1` collision in (9) above. Matching them by giving
   witness `vxlan` reintroduces the flannel device collision at runtime
   (`failed to add device flannel.1: file exists`). The user can sidestep
   this only if pkg/kube *also* uses `flannel-backend: none` (e.g. with
   Multus providing the primary CNI), in which case witness's `none`
   matches.

The genuine fix for both is **don't co-locate the witness with a cluster
member on the same physical host**. The original §1 use-case (2-node
clusters where there is no third host) is the design's actual target:
the witness lives next to the seed, but the seed is the *only* node on
that host. In that topology, the loopback collisions wouldn't matter
because there's no second k3s server on the same host to fight with.

For testing Phase 2 join in setups that *do* have co-located peers
(e.g. running on a 3-node cluster on a single dev box), the realistic
options are:

- Move the witness to a separate physical host (or VM).
- Move to k3d-style net-namespace isolation in `build.yml` (remove
  `pid: host`, drop `/run` + `/sys/fs/cgroup` shared binds, plumb veth
  pair). This is a sizable architectural change but is what k3d does
  successfully.
- Use external etcd (drop k3s-embedded etcd entirely, all cluster
  members talk to a separate etcd cluster via `--datastore-endpoint`).
  Listed as alternative (c) in §9.

### Cordon helper polls forever (not 5 minutes)

Earlier the `cordon_witness_node` helper in `witness-utils.sh` timed
out after 5 minutes if the Node didn't register by then. With long
crash-loops during initial bring-up (or when k3s is slow on a busy
host), the cordon was missing — the node came up `Ready` instead of
`Ready,SchedulingDisabled`. Helper is now an infinite-retry loop with
a 10-second poll interval. Cost is negligible (one kubectl-get every
10s); benefit is the witness is always cordoned regardless of how slow
the apiserver came up.

### State persistence map (which paths survive what)

| Path in container | Backed by | Survives container restart? | Survives device reboot? |
|---|---|---|---|
| `/etc/rancher/k3s/config.yaml` | container overlay (image baseline) | No | No |
| `/etc/rancher/k3s/config.yaml.d/*` | container overlay | No | No |
| `/var/lib/...` | `/persist/vault/witness` bind | Yes | Yes |
| `/persist/...` (direct) | host disk | Yes | Yes |
| `/run/witness/...` | host `/run` (tmpfs) | Yes within boot | No |

The Phase 2 mechanism for runtime-tunable config will need to put any
persistent override files under `/var/lib/...` (or under `/persist/...`
directly) and have `witness-init.sh` copy them into
`/etc/rancher/k3s/config.yaml.d/` at startup, the same way pkg/kube
handles `K3S_USER_OVERRIDE_CONFIG_SRC`.

## 9. Decisions in retrospect

| Decision | Why | Alternatives considered |
|---|---|---|
| Dedicated etcd-node mode: `disable-apiserver` + `disable-controller-manager` + `disable-scheduler` + `disable-kube-proxy` + `flannel-backend: none`, agent left ON, witness Node tainted NoSchedule + NoExecute | Witness needs only the etcd vote. Disabling apiserver/controller-manager/scheduler removes the lease churn and the apiserver's `127.0.0.1:6444` loopback proxy. Keeping the agent running is required by embedded etcd (it expects a Node object — see §8a). Disabling kube-proxy + flannel removes the host iptables and flannel.1 device collisions. The taint keeps workloads off. | (a) `--disable-agent` with embedded etcd — unsupported per k3s maintainers, restart-loops every 15 min. (b) Run full control plane on witness with port-shifting — apiserver's `127.0.0.1:6444` is hardcoded. (c) Replace k3s with plain etcd — clean architecturally but requires pkg/kube to consume external etcd via `--datastore-endpoint` and manual cert plumbing. |
| Relocate agent loadbalancer with `lb-server-port: 6645`; pin `supervisor-port` and `https-listen-port` to 6644 | The agent LB is the OTHER binder of `127.0.0.1:6444`, separate from the apiserver loopback proxy (§8a). Its port is hardcoded — NOT derived from `supervisor-port`. Disabling the apiserver doesn't help. The LB cannot be turned off without disabling the agent, which is itself unsupported. Pinning the server listeners alongside keeps all witness ports in a contiguous range. | (a) Disable the agent — see prior row, unsupported. (b) Don't relocate the LB and accept the failure — not viable; k3s's supervised tree tears down etcd when any component fails. (c) Move pkg/witness into its own net namespace via `net: <ns>` in build.yml — clean architecturally but trades port-juggling for veth/routing complexity and changes pkg/kube's reachability to the witness. |
| Private containerd at `/run/witness/containerd/containerd.sock` | Same pattern pkg/kube uses with `/run/containerd-user/`. k3s's embedded containerd path is shared with the host `/run` bind, so two k3s servers would collide. | Use k3s's embedded containerd and hope two instances peacefully coexist (they don't). |
| Dummy interface for Phase 1 IP | Self-contained bootstrap; no dependency on pillar/cloud before k3s can start. | Bind to `0.0.0.0` with port-shifted ports (works but creates a different shape than Phase 2, more code to throw away). |
| Cloud-provided IP for Phase 2 | Real, routable IP works with non-seed without NAT. Pillar already owns cluster IP provisioning. | NAT/DNAT on the seed forwarding `:12380` → `10.244.244.244:2380` (adds NAT bookkeeping and MASQUERADE complicates etcd's source-IP identity). |
| Sibling `/persist/vault/witness` | Clean separation from pkg/kube state; `Registration_Cleanup` on pkg/kube can't accidentally touch witness data. | Nested under `/persist/vault/kube/witness`. |
| `--node-name eve-witness` on CLI | Disambiguates from pkg/kube's k3s process in shared PID namespace. | PID file with cmdline verification (more code, same outcome). |

## 10. Files touched by Phase 1

New:

- `pkg/witness/build.yml` — linuxkit service (pid:host, /run+/persist binds, cgroupsPath=/eve/services/witness)
- `pkg/witness/Dockerfile` — eve-alpine base + etcdctl + scripts
- `pkg/witness/config.yaml` — the full collision-avoidance config (see §8a Full collision matrix)
- `pkg/witness/00-nodename.yaml` — static `node-name: eve-witness`
- `pkg/witness/containerd-config.toml` — witness-private containerd config (paths under `/run/witness/`, `/var/lib/witness-containerd`, CNI conf_dir at `/var/lib/rancher/k3s/agent/etc/cni/net.d/`)
- `pkg/witness/lib/config.sh` — runtime override (WITNESS_NODE_IP / WITNESS_IFACE / WITNESS_JOIN_URL / WITNESS_JOIN_TOKEN read from /persist/witness-override.env)
- `pkg/witness/witness-utils.sh` — render helpers (network, cluster-join, CNI stub), cordon (infinite retry), `is_witness_k3s_running` (PID file + zombie reject), `check_start_containerd` (witness-unique binary path)
- `pkg/witness/witness-init.sh` — boot flow + supervisor loop
- `docs/design/eve-witness.md` (this file)

Modified (pkg/kube and rootfs wiring):

- `images/modifiers/hv/k.yq` — append witness service entry with `cgroupsPath: /eve/services/witness`
- `tools/parse-pkgs.sh` — declare and export `WITNESS_TAG`
- `pkg/kube/cluster-utils.sh` — add `kube_k3s_pids()` helper (cgroup filter, not cmdline — k3s strips argv)
- `pkg/kube/cluster-init.sh` — migrate `pgrep` callsites to helper; `check_start_containerd` still pgrep-on-binary-path because witness uses `/var/lib/witness/bin/containerd` (see "Witness containerd MUST exec from a witness-unique path" in §8a)

## 11. Commit plan

Phase 1 is a single working unit — every fix in the §8a Full collision
matrix is inter-dependent (remove any one and k3s either won't come up
or won't go Ready). Squash the wip commits on `eve-witness` into a small
conventional series:

1. `feat(pkg/witness): scaffold witness package for 2-node EVE-k HA`
   build.yml, Dockerfile, 00-nodename.yaml, containerd-config.toml,
   lib/config.sh (with override-file mechanism), witness-init.sh,
   witness-utils.sh, initial config.yaml.
2. `feat(pkg/witness): include witness service in EVE-k rootfs`
   k.yq + parse-pkgs.sh — WITNESS_TAG, `cgroupsPath: /eve/services/witness`.
3. `fix(eve-k): disambiguate pkg/kube vs pkg/witness in shared host namespace`
   pkg/kube/cluster-utils.sh `kube_k3s_pids()` via cgroup filter; witness
   PID-file lifecycle (`is_witness_k3s_running` with zombie reject);
   witness-private containerd at `/var/lib/witness/bin/containerd`.
4. `fix(pkg/witness): port + device collision matrix for co-located k3s`
   The full §8a Full collision matrix in `config.yaml`: port shifts
   (6644/6645/10350/10357/10359/10448), `flannel-backend: none`,
   `disable-kube-proxy: true`, witness-specific
   `default-local-storage-path`, addon disables,
   `kube-apiserver-arg: etcd-servers=https://${IP}:2379`,
   `kube-apiserver-arg: kubelet-port=10350`.
5. `feat(pkg/witness): runtime overrides + cluster-join wiring`
   Phase 2 plumbing — `WITNESS_JOIN_URL`/`WITNESS_JOIN_TOKEN` in
   lib/config.sh, `render_witness_cluster_config`, `is_witness_joining`,
   `start_k3s_once` drops `--cluster-init` when joining.
6. `feat(pkg/witness): CNI stub + auto-cordon for Phase 1 Ready`
   `render_witness_cni_stub` writes the loopback-only conflist at the
   containerd-configured path; `cordon_witness_node` polls forever.
7. `docs(design): document Phase 1 collision matrix + co-location ceiling`
   this file — comprehensive §8a Full collision matrix and the
   unresolved etcd-peer-port + flannel-backend-mismatch blockers for
   Phase 2 join.
