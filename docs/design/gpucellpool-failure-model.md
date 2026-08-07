# GPUCellPool — failure model

> A two-layer system fails in two layers, and the dangerous failures are the
> asymmetric ones: outer success + inner failure (a cell that looks provisioned and
> is useless), and inner doubt + outer action (deleting infrastructure because a
> credential expired).
>
> Companions: `-reconciliation.md` (states, finalizers), `-capacity.md` §6.
> Date: 2026-07-30.

---

## 1. Five rules

1. **`SwiftGuest Running` never means `Ready`.** Readiness is the 3-way AND (D7).
2. **Inner doubt freezes outer action.** `WorkloadClusterReachable=False` suspends
   every destructive path — no deletes, no scale-down, no drain completion.
3. **Never zero a number you failed to read.** A failed scrape yields `Unknown` +
   retained last value + `lastObserved`, never `0`.
4. **Bounded churn.** Every automatic creation path has a concurrency cap and a
   per-index exponential backoff. A pool that cannot succeed must stall loudly, not
   spin.
5. **Explicit intent may force; inferred intent may not.** A user deleting a pool
   can force through a stuck drain. An autoscaler may not.

---

## 2. Scenario matrix

| # | Failure | Detection | Cell state | Pool signal | Action |
|---|---|---|---|---|---|
| 1 | **No free physical GPU** | pre-flight: outer `ResourceSlice`s minus allocated claims / `SwiftGPUNode` = 0 free | cell not created (or `Pending`) | `PhysicalGPUsAvailable=False` `InsufficientPhysicalGPU`; `Progressing=False` `Stalled` | stop creating; **do not** queue N unschedulable guests; requeue on inventory change |
| 2 | GPU exists but launcher pod unschedulable (CPU/mem, taints, `vfioReady=false`) | launcher pod `PodScheduled=False`, reason `Unschedulable` | `AllocatingGPU` → `Failed` after `bootstrap.readyTimeout` | `Progressing=False` `CellUnschedulable` + the scheduler message verbatim | backoff-replace once; then stall |
| 3 | Guest pinned to an unschedulable outer node (KubeSwift #444) | guest stalls with no pod / pod never binds; no KubeSwift condition to read | `AllocatingGPU` → `Failed` on timeout | reason `CellProvisionTimeout`, message names the outer node | the pool's timeout **is** the mitigation for a known upstream silent stall |
| 4 | `SwiftGuest phase=Failed` (image, disk, VFIO bind, gpu-init) | outer read | `Failed` immediately (no timeout wait) | `Progressing=False` + guest's own message | backoff-replace |
| 5 | **VM runs, node never joins** (bad/expired token, network, kubelet) | no inner `Node` named `<cell>` within `bootstrap.readyTimeout` | `Booting`/`Joining` → `Failed` `JoinTimeout` | if *every* new cell fails to join while existing cells stay healthy → also `BootstrapCredentialSuspect` | backoff-replace; console log pointer in the message |
| 6 | **Node joins, GPU absent** (driver, wrong passthrough, HAMi plugin, container runtime) | `Devices(node) < expected` past `capacity.readyTimeout` | `AwaitingGPUCapacity` → `Failed` `GPUNotAdvertised` | `Ready=False`; cell never counts as capacity | do **not** silently replace: this is usually an image/driver defect that replacement repeats. Replace once, then stall with the message |
| 7 | HAMi absent or unparseable | `Health()` (§`-capacity.md` 6) | cells stay `AwaitingGPUCapacity` | `CapacityProviderReady=False` | no destructive action; capacity `Unknown` |
| 8 | **Workload cluster unreachable** | inner API error | states frozen, not regressed | `WorkloadClusterReachable=False` `Unreachable` | **nothing destructive.** Existing cells keep running; capacity retained + `lastObserved` |
| 9 | Credential invalid / RBAC reduced | 401 vs 403 discrimination | frozen | `…Reachable=False` `CredentialInvalid` / `Forbidden` | distinct reasons — "your token expired" and "you removed my Node patch rights" are different tickets |
| 10 | Kubeconfig Secret rotated | Secret `resourceVersion` change | unaffected | — | rebuild client, drop old informers |
| 11 | **Stale inner Node** from a replaced cell (same name, old `instance`) | identity label mismatch | new cell held at `Joining` | event `StaleNodeReaped` | delete the stale Node before the new cell may join — otherwise a phantom Ready cell |
| 12 | Someone deletes the cell's `SwiftGuest` by hand | outer object gone, cell entry exists | `Failed`/recreate per backoff | event | the cell drain finalizer means a manual delete **blocks** until drained — a feature, and it must be documented so operators do not think it is a hang |
| 13 | Someone deletes the inner `Node` by hand | Node missing while guest Ready | `Ready` → `Joining` | `Ready=False` | wait for re-registration (kubelet re-registers); do not delete the VM |
| 14 | Outer node drained / cordoned under a cell | outer node unschedulable, or KubeSwift sets `kubeswift.io/drain-requested` | `Ready` + `CellDrainRequested` | condition + event | v1: surface only (cells are `migration.enabled: false`, so KubeSwift will not offline-migrate them behind the pool's back). Phase 4: inner-drain → delete → recreate elsewhere |
| 15 | Pool deleted while a drain is stuck | `drainTimeout` elapsed | `Draining` → `Deleting` | event `DrainTimedOut` | `policy: Drain` forces through **only** for explicit deletion (rule 5); `policy: Force` skips drain entirely |
| 16 | Scale-down requested with live allocations | `Allocations(node) > 0` | `Draining` (persists) | `Progressing` with reason `WaitingForAllocations` | never forced; visible indefinitely |
| 17 | Operator restart mid-create | orphan guest with pool label, missing status entry | re-derived | — | state comes from objects (`-reconciliation.md` §8) |
| 18 | Two pools, colliding cell names in one workload cluster | Node name collision at `Joining`; identity labels name another pool | `Joining` → `Failed` `NodeNameCollision` | actionable message | admission cannot see the other cluster; detect at runtime and refuse to adopt |
| 19 | `replicas` raised beyond free GPUs | scenario 1 per extra cell | extra cells not created | `PhysicalGPUsAvailable=False` | partial fulfilment is reported honestly (`readyCells < replicas`), not retried forever |
| 20 | Mixed GPU models in one pool | inner `type` strings differ | cells Ready | `CapacityAvailable` reason `Heterogeneous`; flat compute aggregate dropped | see `-capacity.md` §5 |
| 21 | Guest Ready, `status.gpu.devices` empty (backend regression) | outer read | held at `GuestProvisioning` | `Ready=False` | never build a cell whose device set is unknown |

---

## 3. Churn control

```
per-index backoff       30s → 1m → 2m → 5m → 15m → 30m (cap), reset on Ready
maxCreating             2 concurrent cells in flight (default, configurable)
replacement budget      a cell index that has failed 5 times stops being replaced;
                        Progressing=False, reason CellReplacementExhausted
pool-level guard        if 0 cells have ever reached Ready and ≥3 have failed,
                        stop creating entirely — this is a configuration defect,
                        not a transient (bad image, bad token, wrong claim template)
```

The pool-level guard is the difference between "a burst of failures" and "a pool
that will burn a GPU boot every 30 seconds forever". It is released by any spec
change (new generation) or by a manual annotation.

---

## 4. Representing partial failure

The pool is honest about being partially fulfilled and does not collapse to one
boolean:

- `status.readyCells` vs `spec.replicas` — the headline truth.
- `status.cells[]` — one row per cell with its own `phase`, `message`, and last
  transition. A user diagnoses "which cell, which layer, why" from `kubectl get
  cellpool -o yaml` alone.
- Conditions separate the layers deliberately: `Ready` (aggregate),
  `PhysicalGPUsAvailable` (outer), `WorkloadClusterReachable` (transport),
  `CapacityProviderReady` (inner provider), `CapacityAvailable` (inner capacity).
  A False on exactly one of them localises the fault.
- Events on every state transition and every reaping action, with the outer node,
  the inner node, and the device BDFs in the message.

---

## 5. What is allowed while degraded

| | inner unreachable | HAMi degraded | no free GPU | pool deleting |
|---|---|---|---|---|
| create cells | no (cannot verify or label) | yes (park in `AwaitingGPUCapacity`) | no | no |
| mark Ready | no | no | n/a | no |
| report capacity | last known + `lastObserved` | `Unknown` | n/a | last known |
| scale down | **no** | **no** | yes (down is always safe for GPUs) | n/a |
| drain | freeze | freeze completion | n/a | yes (bounded by `drainTimeout`) |
| delete cells | no | no | n/a | yes |

---

## 6. Ordering hazards (get these wrong and you lose workloads)

1. **Cordon before counting allocations.** Counting first races the scheduler
   placing one more Pod.
2. **Count in-flight allocations too** (`hami.io/vgpu-devices-to-allocate`) — the
   binding window is exactly the race.
3. **Reap the stale Node before the replacement cell joins**, never after.
4. **Delete the inner Node before the VM**, so kubelet cannot re-register a Node the
   pool has stopped tracking.
5. **The GPU comes back only when the guest is gone** — release is KubeSwift's
   finalizer/GC, so `physicalCapacity` must be re-read after deletion, not
   decremented optimistically.
6. **Never offline-migrate a cell.** Cells are pinned (`migration.enabled: false`);
   an offline migration is a VM restart under a live Kubernetes node.

---

## 7. Operator playbook (the doc a human reads at 02:00)

| Symptom | First look |
|---|---|
| `Ready=False`, cells `AwaitingGPUCapacity` | inner: `kubectl get node <cell> -o jsonpath='{.metadata.annotations.hami\.io/node-nvidia-register}'`; then in-guest `nvidia-smi -L` |
| cells `Failed JoinTimeout` | the join Secret's token validity; guest console via `swiftctl console <cell>` |
| `PhysicalGPUsAvailable=False` | outer: `kubectl get resourceslices` / `kubectl get swiftgpunodes` for free devices |
| `CapacityProviderReady=False DRAFeatureGateMissing` | inner cluster version + `DRAConsumableCapacity` gate |
| pool stuck `Terminating` | a cell in `Draining` with allocations; either drain the workloads or set `deletion.policy: Force` |
| capacity numbers stale | `status.workloadCapacity.lastObserved` + `WorkloadClusterReachable` |

---

## 8. The design brief's 16 questions, answered

| # | Question | Answer |
|---|---|---|
| 1 | `SwiftGuest` directly or a per-cell CRD? | Directly. No `GPUCell` kind (D1) — and CAPI would make `Machine` that object anyway |
| 2 | Minimal `GPUCellPoolSpec`? | `replicas` + `cell.{guestTemplate,gpu}` + `bootstrap.joinSecretRef` + `workloadCluster.kubeconfigSecretRef` (api §2) |
| 3 | `min/max` now? | No — `replicas` + scale subresource; `min/max` inside a later `spec.autoscaling` (D2) |
| 4 | Cell identity across clusters? | Name-derived (`<pool>-<index>` = guest = hostname = Node) + `pool`/`cell`/`instance` node labels, controller-patched if kubelet did not set them (recon §3) |
| 5 | Bootstrap? | Prebaked image + thin cloud-init enrollment; node IP derived in-guest; `SwiftSeedProfile` NoCloud (bootstrap §2–3) |
| 6 | Join credentials? | `Opaque` Secret → per-cell rendered Secret → `userDataFrom.secretKeyRef`; never in a CR. Optional `KubeadmToken` mints TTL'd tokens (bootstrap §4) |
| 7 | How is HAMi's GPU discovery confirmed? | `CapacityProvider.Devices(node) ≥ expectedDevicesPerCell`, from the register annotation (not `allocatable`) or the DRA slices (capacity §2–3) |
| 8 | Authoritative capacity objects? | DevicePlugin: Node annotation `hami.io/node-nvidia-register` + Pod `hami.io/vgpu-devices-*`. DRA: `ResourceSlice.capacity` + `ResourceClaim…consumedCapacity` |
| 9 | Normalization across models? | None. `byModel[]` always; flat memory always; flat compute only when homogeneous (capacity §5) |
| 10 | Condition model? | 6 conditions splitting the two layers + transport + provider (api §6) |
| 11 | Partial failure? | `readyCells` vs `replicas`, per-cell `status.cells[]` rows, layer-specific conditions, events (§4) |
| 12 | Orphan recovery? | Label-based re-discovery; owner-ref GC; stale-Node reaping by `instance` mismatch (recon §8) |
| 13 | Workload API unavailable? | Freeze all destructive paths, retain capacity, `WorkloadClusterReachable=False` (rule 2) |
| 14 | Operations during degraded HAMi? | Create/read/report yes; Ready/count/scale-down/drain-completion no (§5) |
| 15 | How does CAPI change things? | Adds `cell.provisioner: ClusterAPI`; needs GPU fields in `capi-kubeswift`; only possible for CAPI-managed workload clusters — hence a second provisioner, never a replacement (D8, bootstrap §5) |
| 16 | What is postponed? | Automatic scale-down, demand-driven scale-up, MIG, multi-GPU cells, overcommit, multi-cluster pools, HAMi install, workload integrations (overview §5) |

---

## 9. Assumptions and unresolved questions

**Assumptions** (each is a Phase-1/2 verification item):

- A1 A whole PCIe GPU passed through by KubeSwift is indistinguishable, to the
  in-guest NVIDIA driver and HAMi, from a bare-metal GPU.
- A2 HAMi's device plugin and HAMi-core operate unchanged inside a VM.
- A3 One cell = one GPU = one Kubernetes node is an acceptable unit for the target
  workloads (inference sharing, not multi-GPU training).
- A4 The workload cluster's operators accept extra nodes appearing/disappearing
  under their scheduler.
- A5 Cells are cattle: no live migration, no in-place upgrade.

**Unresolved:**

- U1 **k0s token minting via API only** — if impossible, `Opaque` is the only k0s
  mode (bootstrap §7.1).
- U2 **HAMi annotation format stability** — currently an internal protocol. Should
  the provider pin a supported HAMi version range and refuse newer ones loudly?
  Leaning yes, with an override annotation.
- U3 **HAMi DRA maturity** — `v0.1.0`, k8s ≥ 1.34. `mode: DRA` should probably ship
  behind an explicit `experimental` acknowledgement until validated.
- U4 **Preflight reporting from inside the guest** (bootstrap §7.3).
- U5 **Cell replacement policy on GPU-absent failures** (scenario 6): replace-once
  is a guess; the real number comes from Phase 1/2 experience.
- U6 **Multi-tenancy of the pool object itself** — a namespaced pool grants
  node-root-equivalent authority in the *outer* cluster (privileged launcher pods).
  Whether pool creation should be restricted to infrastructure admins, or gated by
  a policy engine, is a deployment decision this design must not silently answer.
- U7 **Interaction with inner-cluster autoscalers** — a cluster autoscaler in the
  workload cluster may try to manage nodes it does not own. Taints and the
  `cells.kubeswift.io/*` labels are the intended fence; validate against at least
  one real autoscaler before Phase 3.
