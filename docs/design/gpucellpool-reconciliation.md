# GPUCellPool — reconciliation, identity, RBAC, deletion

> Design record, written before implementation (2026-07-30) and partially
> updated. It documents rationale, not current behaviour — see `docs/` for
> that, in particular `docs/api-reference.md` and `docs/autoscaling.md`.
> Several "v1"/"later phase" markers below are now stale — scale-down and
> demand-driven scale-up are both shipped; see the inline corrections.

> One controller, two clusters, one cached client per pool. Every state is derived
> from cluster objects, never from controller memory — the reconciler must survive
> a restart with no bookkeeping.
>
> Companions: `gpucellpool-api.md`, `-capacity.md`, `-failure-model.md`.
> Date: 2026-07-30.

---

## 1. Components

| Component | Owns | Never does |
|---|---|---|
| `GPUCellPoolReconciler` | the loop, status aggregation, conditions | talk to HAMi or KubeSwift types directly |
| `CellManager` | index/name allocation, per-cell FSM, replacement backoff | scaling decisions |
| `CellProvisioner` (iface) | outer objects for one cell: create / observe / delete | anything inner-cluster |
| `PhysicalInventory` | free/held GPUs from outer `ResourceSlice` + `ResourceClaim` | allocate anything |
| `WorkloadClusterClient` | one cached, scoped client + informers per kubeconfig; Node get/label/cordon/drain | interpret GPU capacity |
| `CapacityProvider` (iface) | HAMi device/memory/core capacity from inner objects | write to the inner cluster |
| `ScalingPolicy` | `desiredCells` — **shipped as `DecideScale`**, static (`= spec.replicas`) or demand-driven (`spec.autoscaling`, both directions); see `docs/autoscaling.md` | delete cells directly |

`CellProvisioner` mirrors KubeSwift's own `gpualloc.Backend` seam — two phases,
one struct as the contract:

```go
type CellProvisioner interface {
	Name() string // "SwiftGuest" | "ClusterAPI"

	// List names the cells this provisioner owns, read from live objects. It is
	// part of the interface because the object that REPRESENTS a cell differs per
	// mode — a SwiftGuest in one, a Cluster API Machine in the other. (Shipped as
	// a hard-coded SwiftGuest list first; the ClusterAPI harness found that
	// immediately, as a pool that created Machines and then discovered no cells.)
	List(ctx context.Context, namespace, pool string) ([]string, error)

	// Ensure is idempotent: create the outer objects for this cell if absent,
	// then report what is observable. It never blocks.
	Ensure(ctx context.Context, c CellRequest) (OuterState, error)

	// Delete removes the outer objects; done=false means "still terminating".
	Delete(ctx context.Context, c CellRequest) (done bool, err error)
}

type OuterState struct {
	Exists      bool
	Phase       string   // provisioner-native phase, for messages only
	Provisioned bool     // VM running with an IP
	GPUDevices  []string // PCI BDFs actually passed through
	HostNode    string   // outer node holding the device
	Address     string   // the address the kubelet registers as node IP
	UID         string   // identity "instance" anchor (D3)
	Failed      bool
	Reason, Message string
}
```

`SwiftGuestProvisioner` implements it over an **unstructured** client (D9), with
these GVKs and read paths — all verified against v0.13.4:

```
swift.kubeswift.io/v1alpha1 SwiftGuest        status.phase
seed.kubeswift.io/v1alpha1  SwiftSeedProfile  status.network.primaryIP
                                              status.network.interfaces[name=<nodeIPFrom>].ip
                                              status.gpu.devices[], status.gpu.nodeName
```

---

## 2. Cell state machine

```
                 ┌──────────────── Failed ◄──── (timeout | terminal outer failure)
                 │                   ▲
 Pending ─► AllocatingGPU ─► GuestProvisioning ─► Booting ─► Joining ─► AwaitingGPUCapacity ─► Ready
                 │                                                                              │
                 └──────────────────────────────────────────────────── Draining ◄────────────────┘
                                                                          │
                                                                       Deleting ─► (gone)
```

Every transition is driven by an observable, never by elapsed time alone:

| From → To | Observable |
|---|---|
| `Pending` → `AllocatingGPU` | cell guest + seed created (`Ensure` returned `Exists`) |
| `AllocatingGPU` → `GuestProvisioning` | **DRA**: launcher pod scheduled and `status.gpu.devices` non-empty. **Native**: `status.gpu.nodeName` + devices set |
| `GuestProvisioning` → `Booting` | `SwiftGuest status.phase == Running` |
| `Booting` → `Joining` | the node IP is known: `status.network.interfaces[nodeIPFrom].ip` (or `primaryIP`) |
| `Joining` → `AwaitingGPUCapacity` | inner `Node` named `<cell>` exists **and** `Ready=True` **and** its identity labels match this cell instance (§3) |
| `AwaitingGPUCapacity` → `Ready` | `CapacityProvider.DeviceCount(node) >= expectedDevicesPerCell` and the provider is healthy |
| any → `Failed` | terminal outer failure (`SwiftGuest phase=Failed`), or the state's timeout elapsed (`bootstrap.readyTimeout` for Booting/Joining, `capacity.readyTimeout` for AwaitingGPUCapacity) |
| `Ready` → `AwaitingGPUCapacity` | capacity/Node regressed — **regression is not `Failed`**; a NotReady node or a restarted HAMi plugin is recoverable |
| `Ready`/any → `Draining` | scale-down or pool deletion selected this cell |
| `Draining` → `Deleting` | inner Node cordoned, no HAMi allocations remain (or `drainTimeout` on pool deletion with `policy: Force`) |

`Ready` is the 3-way AND from D7. A guest that runs, joins, and never surfaces a
GPU sits in `AwaitingGPUCapacity` until the timeout and then reports `Failed` with
the real reason — it never counts as capacity. That is the whole point of the pool.

---

## 3. Cell identity (D3)

Deterministic, name-derived, verifiable, and recoverable after operator restart:

```
index          0..N-1, lowest free index wins; highest-index-first scale-down
cell name      <pool>-<index>                     e.g. inference-l40s-0
SwiftGuest     <cell name>          (labels cells.kubeswift.io/{pool,cell-index})
seed profile   <cell name>-seed
guest hostname <cell name>          (SwiftSeedProfile metaData: local-hostname)
inner Node     <cell name>          (kubelet registers with the hostname)
```

Correlation is `Node.name == cell.name`, **verified** by two labels the bootstrap
asks the kubelet to self-apply (`--node-labels`), with a controller-side fallback:

```
cells.kubeswift.io/pool      = <pool>
cells.kubeswift.io/cell      = <cell name>
cells.kubeswift.io/instance  = <outer guest UID>
```

- Custom-domain node labels are permitted for kubelet self-labelling (the
  `NodeRestriction` admission plugin only constrains `kubernetes.io`/`k8s.io`
  prefixes) — but the controller does **not** depend on that: if a label is
  missing, it patches the Node itself. `capi-kubeswift` already proves that
  patch-the-Node path is the reliable one (it abandoned the cloud-init drop-in for
  `providerID` for exactly this reason).
- `instance` is the discriminator that IP- or name-only correlation lacks: a Node
  named `inference-l40s-0` carrying a *previous* guest's UID is a **stale** Node
  from a replaced cell. The controller reaps it (delete Node) before letting the
  new cell reach `Joining` — otherwise the pool would report a phantom Ready cell,
  or worse, HAMi would advertise capacity for a GPU that no longer exists.
- Never correlate by IP: DHCP/UDN addresses are reassigned, and one of KubeSwift's
  own hard-won lessons is that address-derived identity breaks under
  restore/replacement.

---

## 4. Reconcile loop

```
1  fetch pool; if deleting → §7
2  ensure finalizer cells.kubeswift.io/pool
3  resolve workload client (kubeconfig Secret → cached client)
        unreachable → set WorkloadClusterReachable=False,
                      SUSPEND all destructive decisions, requeue, return
4  list owned cells (outer objects by label) → reconstruct status.cells[]
5  desired = ScalingPolicy.Desired(pool)          // DecideScale: spec.replicas,
                                                   // or the autoscaler's decision
6  reconcile membership
     create   while len(cells) < desired  and  inFlight < maxCreating(=2)
                                          and  PhysicalGPUsAvailable
     replace  Failed cells, per-index exponential backoff (30s→30m, capped)
     drain    while len(cells) > desired: pick highest index (operator-driven
              shrink) OR the autoscaler's named idle cells (scaleDown: Auto)
              → Draining
7  advance each cell's FSM (§2) using outer + inner reads
8  reap stale inner Nodes (identity mismatch, or cell gone) → §3
9  ensure inner Node labels/annotations/taints match spec.workloadCluster.node
10 read capacity (CapacityProvider) + physical inventory → status
11 aggregate conditions; requeue: 30s if Progressing, else 2m (plus watch-driven)
```

Invariants:

- **Idempotent** — every `create` is a get-then-create on a deterministic name.
- **At most `maxCreating` cells in flight** (default 2). Cell creation costs a
  physical GPU and a VM boot; a thundering herd on a saturated cluster produces N
  Pending launcher pods and no diagnosis.
- **Pre-flight before creating**: `PhysicalInventory` computes free devices from
  the outer cluster's `ResourceSlice`s (driver `gpu.kubeswift.io`, pool = node
  name, attribute `vfioReady`) minus allocated `ResourceClaim`s. If zero, the pool
  does not create a cell that would sit unschedulable forever; it sets
  `PhysicalGPUsAvailable=False`/`InsufficientPhysicalGPU` and stops. This is the
  designed answer to the brief's first failure scenario, and it also gives
  `status.physicalCapacity.freeGPUsInCluster` for free.
- **Never delete on inner-cluster doubt** (step 3), see `-failure-model.md` §3.

### Watches

| Source | Why |
|---|---|
| `GPUCellPool` | primary |
| outer `SwiftGuest` (unstructured, label-filtered) | cell phase/IP/GPU changes |
| outer `ResourceSlice`, `ResourceClaim` | physical inventory changes |
| Secret (kubeconfig, join) | credential rotation |
| inner `Node` (per-pool informer, label-filtered) | join / Ready / regression |
| inner `Pod` (DevicePlugin mode) or `ResourceClaim` (DRA mode) | allocation accounting |
| periodic resync 2m | catches anything a watch missed; bounded cost |

---

## 5. Remote-cluster access model

- **One client per kubeconfig Secret**, keyed by `namespace/name` + `resourceVersion`,
  held in a shared cache with an informer factory and refcount. N pools against one
  workload cluster share one connection set. Never a controller-runtime manager per
  cell, never a goroutine per cell (Risk 4 in the brief).
- Rotation: a change to the Secret's `resourceVersion` invalidates the entry and
  rebuilds the client on the next reconcile; the old informers stop when the
  refcount drops.
- Failure is *first-class state*, not an error return: connect/authz failures set
  `WorkloadClusterReachable=False` with a distinguishing reason
  (`Unreachable` / `CredentialInvalid` / `Forbidden`) and freeze destructive paths.
- No impersonation in v1alpha1 (unlike the gateway, this controller acts as itself).

---

## 6. RBAC

The two role manifests below are the design draft. **The shipped roles are
`config/rbac/role.yaml`** (outer) **and `config/rbac/workload-cluster-observer.yaml`**
(inner) — read those for ground truth; this section is kept for the reasoning.
Notable drift from the draft: the shipped outer role additionally grants
`cluster.x-k8s.io/clusters`, `cluster.x-k8s.io/machines` and
`infrastructure.cluster.x-k8s.io/kubeswiftmachines` (all
create/delete/get/list/patch/update/watch except `clusters`, which is read-only)
plus `bootstrap.cluster.x-k8s.io/*` — needed once `provisioner: ClusterAPI`
shipped (§1, `docs/clusterapi-cells.md`) and absent from the draft below because
it predates that provisioner. The shipped inner role grants `nodes: delete` as
an **always-on** right, not a Phase-4/drain-only one (see the correction after
the inner block) and has no `KubeadmToken`-only block, because that bootstrap
provider was never implemented (`docs/limitations.md`).

### Outer (management) cluster — the operator's own ServiceAccount

```yaml
rules:
  - apiGroups: [cells.kubeswift.io]
    resources: [gpucellpools, gpucellpools/status, gpucellpools/finalizers, gpucellpools/scale]
    verbs: [get, list, watch, update, patch]
  - apiGroups: [swift.kubeswift.io]
    resources: [swiftguests]
    verbs: [get, list, watch, create, update, patch, delete]
  - apiGroups: [seed.kubeswift.io]
    resources: [swiftseedprofiles]
    verbs: [get, list, watch, create, update, patch, delete]
  - apiGroups: [resource.k8s.io]
    resources: [resourceslices, resourceclaims, resourceclaimtemplates, deviceclasses]
    verbs: [get, list, watch]                    # READ ONLY — never allocates
  - apiGroups: [""]
    resources: [secrets]
    verbs: [get, list, watch]                    # kubeconfig + join secrets
  - apiGroups: [""]
    resources: [events]
    verbs: [create, patch]
  - apiGroups: [coordination.k8s.io]
    resources: [leases]
    verbs: [get, create, update]                 # leader election
  # ClusterAPI provisioner only (shipped after this draft was written — see
  # config/rbac/role.yaml for the exact, generated rules):
  - apiGroups: [cluster.x-k8s.io]
    resources: [clusters]
    verbs: [get, list, watch]
  - apiGroups: [cluster.x-k8s.io]
    resources: [machines]
    verbs: [get, list, watch, create, update, patch, delete]
  - apiGroups: [infrastructure.cluster.x-k8s.io]
    resources: [kubeswiftmachines]
    verbs: [get, list, watch, create, update, patch, delete]
  - apiGroups: [bootstrap.cluster.x-k8s.io]
    resources: ["*"]
    verbs: [get, list, watch, create, update, patch, delete]
```

Note what is still **absent**: no `pods`, no `nodes`, no `swiftgpuprofiles`
write, no `resourceclaims` write. The operator never allocates a GPU —
KubeSwift and the scheduler do.

### Inner (workload) cluster — the credential in `kubeconfigSecretRef`

Every verb has a named consumer:

```yaml
# ClusterRole gpu-cell-pool-observer
- apiGroups: [""]
  resources: [nodes]
  verbs: [get, list, watch, patch, update, delete]
  # DELETE IS ALWAYS-ON, NOT A DRAIN/PHASE-4 FEATURE — the draft below marked
  # it Phase-4-only; that was wrong even before scale-down shipped. Two paths
  # unconditionally need it: reaping a stale Node left by a replaced cell
  # (§3, §8), and removing a cell's Node when the cell itself is deleted.
  # Without it every teardown fails Forbidden and stale Nodes accumulate.
- apiGroups: [""]
  resources: [pods]
  verbs: [get, list, watch]                      # HAMi DevicePlugin accounting
- apiGroups: [resource.k8s.io]
  resources: [resourceslices, resourceclaims]
  verbs: [get, list, watch]                      # HAMi DRA accounting
```

Deliberately absent, both permanently: `pods/eviction` (the operator waits for
a cell's GPU to be released rather than evicting workloads to force it empty —
drain with `kubectl` if you need a cell emptied faster) and any `secrets`
write in the workload cluster (the `KubeadmToken` bootstrap provider that
would have needed it was never implemented — see `docs/limitations.md`).

`cluster-admin` is never required. `nodes/patch`+`delete` (can cordon/label/
remove any node in the workload cluster) is the one privilege escalation to be
conscious of, since it is the broadest grant here — it is why the shipped
`config/rbac/workload-cluster-observer.yaml` is a separate, minimal manifest
rather than something operators are expected to hand-write, and why the
default `Opaque` bootstrap provider (`-bootstrap.md` §5) needs no
bootstrap-token-minting rights at all (`pods/eviction` and bootstrap-token
creation, both once planned as later-phase grants here, were never needed:
the former because the operator never evicts, the latter because
`KubeadmToken` bootstrap was never implemented — `docs/limitations.md`). Ship
a ready-made `ClusterRole`+`ServiceAccount`+token manifest so operators do not
hand over an admin kubeconfig out of convenience — `config/rbac/
workload-cluster-observer.yaml` is exactly that.

---

## 7. Finalizers and deletion

Two finalizers, two different jobs:

| Finalizer | On | Guarantees |
|---|---|---|
| `cells.kubeswift.io/pool` | the `GPUCellPool` | cells are torn down (and inner Nodes reaped) before the pool object disappears |
| `cells.kubeswift.io/cell-drain` | each cell `SwiftGuest` | a cell cannot be deleted — by us, by `kubectl delete swiftguest`, or by GC — while HAMi workloads still hold its GPU |

Pool deletion:

```
1 mark pool Terminating; stop creating
2 all cells → Draining          (highest index first, all at once)
3 per cell: cordon inner Node → wait for zero HAMi allocations
                              → (Phase 4: evict remaining pods)
4 delete inner Node object
5 remove cell finalizer, delete SwiftGuest (+ seed profile after it is gone)
6 physical GPU returns to the outer pool when the guest is gone (release is
  KubeSwift's job: the native backend's kubeswift.io/gpu-allocation finalizer,
  or claim/pod GC in DRA mode)
7 remove pool finalizer
```

The asymmetry that matters:

- **Explicit pool deletion** with `policy: Drain` waits `drainTimeout`, then
  proceeds anyway with an Event and a `DrainTimedOut` message. Leaving VMs and GPUs
  pinned forever after a user asked for deletion is worse than a rough teardown —
  and the user's intent is unambiguous.
- **Implicit scale-down** never force-proceeds. If the inner cluster is unreachable
  or allocations persist, the cell stays `Draining` and the pool reports it. An
  automatic action must not destroy running workloads.

`policy: Force` skips step 3 entirely and is documented as workload-destroying.

---

## 8. Restart and orphan recovery

Nothing lives in memory. On startup, for each pool:

- cells are re-discovered by label (`cells.kubeswift.io/pool`), and their index by
  `cells.kubeswift.io/cell-index`; `status.cells[]` is rebuilt from live reads.
- an outer `SwiftGuest` carrying our pool label with **no matching pool** (pool
  deleted while the operator was down) is adopted for deletion by owner-reference
  GC; the operator additionally reaps its inner Node.
- an inner `Node` carrying `cells.kubeswift.io/pool=<pool>` with no matching cell,
  or with a mismatched `instance`, is a stale node → reaped (§3).
- a cell whose guest exists but whose `status.cells[]` entry was never written is
  simply re-derived — the guest is the source of truth, the status is a projection.

---

## 9. Metrics

Prefix `gpucell_`, one namespace for both levels. **Every metric actually
shipped carries `{pool, namespace}`, not just `{pool}`** as drafted below — a
pool name is only unique within a namespace, and the draft's label sets were
written before that was caught. See `internal/metrics/metrics.go` and
`docs/runbook.md` for the ground truth (twelve metrics, one more than drafted
here: `scale_decisions_total` was added with autoscaling):

```
gpucell_cells_desired{pool,namespace}                       gauge
gpucell_cells{pool,namespace,phase}                         gauge   # Ready|Booting|Joining|Awaiting…|Draining|Failed
gpucell_cell_startup_seconds{pool,namespace}                histogram  # creation→first Ready
gpucell_cell_transitions_total{pool,namespace,from,to}      counter
gpucell_physical_gpus{pool,namespace,state}                 gauge   # held|free-in-cluster
gpucell_capacity_gpu_devices{pool,namespace}                gauge
gpucell_capacity_gpu_memory_bytes{pool,namespace,state}     gauge   # total|allocated|available
gpucell_capacity_gpu_compute_percent{pool,namespace,state}  gauge
gpucell_capacity_scrape_errors_total{pool,namespace,reason} counter
gpucell_workload_cluster_reachable{pool,namespace}          gauge   # 1|0
gpucell_scale_decisions_total{pool,namespace,reason,scaled_up} counter
gpucell_reconcile_errors_total{pool,namespace}              counter
```

`gpucell_cell_startup_seconds` is the number that decides whether
demand-driven autoscaling is worth using reactively: at the measured ~4m45s
(`-bootstrap.md` §6) it is workable with the default 10-minute stabilization
window; a cell in the range this draft worried about (12+ minutes) would make
scale-up a capacity planner rather than an autoscaler.
