# GPUCellPool — overview and architecture

> A **composition operator**: KubeSwift provides VM-isolated, whole-GPU-passthrough
> Kubernetes workers; HAMi fractionally shares the GPU inside them. `GPUCellPool`
> owns the lifecycle between the two layers and knows about both — while neither
> KubeSwift nor HAMi is modified, and neither learns about the other.
>
> Status: **DESIGN (first pass)** — grounded in source inspection of
> `kubeswift-io/kubeswift` @ v0.13.4 and `capi-kubeswift`, plus HAMi upstream
> (`Project-HAMi/HAMi`, `Project-HAMi/k8s-dra-driver`, `Project-HAMi/HAMi-DRA`).
> No hardware proof yet — Phase 1 is the hardware gate.
> Companions: `gpucellpool-api.md`, `-reconciliation.md`, `-bootstrap.md`,
> `-capacity.md`, `-failure-model.md`, `-poc.md`. Date: 2026-07-30.

---

## 1. The architectural invariant

```
OUTER   KubeSwift   physical GPU -> VM            (whole device, VFIO)
INNER   HAMi        VM's GPU -> workloads         (memory + core fractions)
BETWEEN GPUCellPool lifecycle + composition       (no resource translation)
```

A HAMi allocation is logical capacity, not a PCI function. There is **no path**
from a HAMi `ResourceClaim` to a KubeSwift GPU allocation, and the design contains
no such translation. The two DRA domains are independent by construction: they
live in different clusters, use different drivers, and are never joined.

A **GPU Cell** is a KubeSwift-managed VM holding one whole passthrough GPU, running
as a worker node in a *workload* Kubernetes cluster, where HAMi advertises that GPU
as shareable capacity. Two identities, deliberately not conflated:

| | outer (infrastructure cluster) | inner (workload cluster) |
|---|---|---|
| object | `SwiftGuest` + launcher Pod | `Node` |
| resource | 1 physical GPU (BDF), CPU, RAM, disk | HAMi device + memory/core capacity |
| owner | KubeSwift | HAMi |
| observed by | `GPUCellPool` outer client | `GPUCellPool` workload client |

---

## 2. What the source trees actually provide (verified, not assumed)

### KubeSwift (outer) — everything the MVP needs already exists

| Capability | Evidence |
|---|---|
| Whole-GPU DRA passthrough into a VM | `SwiftGuestSpec.GPUResourceClaim{ResourceClaimName \| ResourceClaimTemplateName, RequestName="gpu", Tier="pcie", Hugepages}` — `api/swift/v1alpha1/swiftguest_types.go:795` |
| Two GPU backends behind one seam | `internal/gpualloc/backend.go` — `Backend{Prepare,Resolve}`; `native` (controller-time) and `dra` (scheduler-time). Both converge on `SwiftGuest.Status.GPU` |
| DRA driver + inventory | driver `gpu.kubeswift.io`; ResourceSlice pool = node name, device = `gpu-<bdf>`; attributes `pciAddress`, `vendorDevice`, `numaNode`, `iommuGroup`, `vfioReady` — `cmd/kubeswift-dra-driver/{driver.go:23,main.go:100}` |
| Sample DeviceClass / claim templates | `config/samples/dra-gpu/` (`kubeswift-vfio-gpu` DeviceClass, `single-vfio-gpu` template, CEL selectors on the attributes above) |
| GPU + multi-NIC (routable cross-node worker) | GPU rejects only `vhostUserDevices` and `filesystems` (`internal/webhook/swiftguest/validator.go:314,383,419`). `interfaces` + `networkRef` (Multus/NAD/UDN) compose with GPU — this is what makes a cell reachable as a *node* |
| Guest → Node facts we can observe | `status.phase`, `status.network.primaryIP`, `status.gpu.{devices,nodeName,hypervisor}` |
| Replica-pool prior art to copy | `SwiftGuestPool`: stable `<pool>-<index>` names, scale subresource on `spec.replicas`, counters-only status, `swift.kubeswift.io/pool[-index]` labels, template-hash annotation |
| Cloud-init from a Secret | `SwiftSeedProfile.spec.userDataFrom.secretKeyRef` — the join token never has to sit in plaintext in a CR |
| Node opt-in gate | `kubeswift.io/gpu-node=true` gates both `gpu-discovery` and the DRA-driver DaemonSet |

Constraints inherited, not negotiable:

- GPU is **disk boot only** — mutually exclusive with `kernelRef`,
  `cloneFromSnapshot`, `osType: windows`.
- `tier: pcie` → Cloud Hypervisor; `hgx-shared` → QEMU; **`hgx-full` is rejected at
  allocation** (`UnsupportedTier`). Cells are `pcie` in v1alpha1.
- A VFIO guest **cannot live-migrate** (`HasVFIODevices`) — only offline
  release-and-reallocate. Cells are therefore **replaceable, never migrated**
  (§6 below has the consequence the brief did not anticipate).
- Every launcher pod is `privileged: true` **by design** (documented, deliberate,
  reverted once). A cell's launcher pod is a **node-level trust boundary in the
  outer cluster** — so "VM isolation between tenants" is a statement about the
  *inner* cluster's workloads, not about who may create cells (§7).

### capi-kubeswift — prior art for the half of the problem that is not GPU

`cluster-api-provider-kubeswift` already turns a `SwiftGuest` into a joined
Kubernetes worker. Reusable findings:

- It talks to KubeSwift **only through an unstructured client** with hard-coded
  GVKs, explicitly so the Apache-2.0 repo never imports AGPL Go packages
  (`internal/backend/swiftguest.go:19`). → D9.
- Bootstrap = a `SwiftSeedProfile` (`datasource: NoCloud`) carrying the CAPI
  bootstrap cloud-init **verbatim**, plus `metaData: instance-id/local-hostname =
  machine name`.
- Node correlation is by **name**, and `providerID` is *patched directly on the
  workload Node* over the `<cluster>-kubeconfig` Secret — the cloud-init drop-in
  path was tried and abandoned as fragile (`internal/controller/workload_node.go`).
  → the same pattern is our identity fallback (D3).
- The validated multi-node worker shape is **dual-NIC**: nat `mgmt` primary +
  `networkRef` `node` interface carrying the routable IP, with kubelet
  `--node-ip` set to the secondary.
- **It has no GPU support at all** (`grep -i gpu api/ internal/ templates/` → 0
  hits). Any CAPI-based cell provisioner needs a GPU field added there first. → D8.

### HAMi (inner) — two accounting models, both readable

| | DevicePlugin mode (today's default) | DRA mode |
|---|---|---|
| gate | Node label `gpu=on` | DeviceClass + driver installed |
| inventory truth | Node annotation `hami.io/node-nvidia-register` — JSON array, per GPU: `id` (UUID), `count` (inflated logical slots, default 10), `devmem` (MiB), `devcore` (%), `type` (model), `numa`, `health` | `ResourceSlice` devices with `allowMultipleAllocations: true`, `capacity: {cores, memory}`, `requestPolicy` |
| request surface | `nvidia.com/gpu` (logical slots), `nvidia.com/gpumem` (MiB), `nvidia.com/gpumem-percentage`, `nvidia.com/gpucores` (%) | `claim.spec.devices.requests[].exactly.capacity.requests.{cores,memory}` |
| allocation truth | Pod annotations `hami.io/vgpu-devices-allocated` / `-to-allocate`, `hami.io/bind-time` | `ResourceClaim.status.allocation.devices[].{shareID,consumedCapacity}` |
| maturity | stable, widely deployed | `projecthami/k8s-dra-driver:v0.1.0`; needs **k8s ≥ 1.34** + `DRAConsumableCapacity` + CDI |

Extra HAMi facts that shape the PoC: HAMi-core needs NVIDIA driver ≥ 440 and —
notably — **glibc ≥ 2.17 and < 2.30 in the workload container image** for its
`LD_PRELOAD` interposition. That is a workload-image constraint, not a node
constraint, and it is a Phase-1 verification item (`-poc.md` R6).

---

## 3. Decisions

| # | Question | Decision | Why |
|---|---|---|---|
| D1 | per-Cell CRD or `SwiftGuest` directly? | **No `GPUCell` CRD.** One CRD (`GPUCellPool`); a cell *is* an owned `SwiftGuest` named `<pool>-<index>`, with per-cell state in `status.cells[]` and the drain finalizer on the guest | `SwiftGuestPool` proves counters-plus-owned-objects is enough; and if CAPI lands (D8) the *`Machine`* becomes the per-cell object — inventing `GPUCell` now guarantees a third, doomed object |
| D2 | `replicas` or `min/max` in v1alpha1? | **`spec.replicas: int32` + scale subresource.** `min/max` arrives later inside `spec.autoscaling` | HPA-ready day one (same seam as `SwiftGuestPool`/`SwiftSandboxPool`); a scaling block is additive, whereas demoting `replicas` later is not |
| D3 | cell identity across clusters | **Name-derived, not IP-derived**: cell name = `<pool>-<index>` = guest name = guest hostname = **Node name**; verified by node labels `gpu-cell.kubeswift.io/{pool,cell}` and an *instance* label carrying the guest UID | deterministic, reconstructible after operator restart, and detects a stale Node left by a previous incarnation of the same index |
| D4 | cell shape in the API | **Opaque `SwiftGuestSpec` passthrough** (`cell.guestTemplate`, preserve-unknown-fields) + first-class `cell.gpu`, with an operator-owned/denied field contract enforced by a webhook | never chases KubeSwift's API surface (mirrors `SwiftGuestPool.spec.template.spec`); GPU stays first-class because the operator must own the tier/backend semantics |
| D5 | HAMi coupling | **`CapacityProvider` interface, one implementation (`HAMi`), two modes** (`DevicePlugin`, `DRA`). No HAMi Go dependency; annotations/ResourceSlices are read as data | HAMi DRA is `v0.1.0` — isolate it so its churn cannot reach the CRD |
| D6 | HAMi installation | **Prerequisite.** The operator *verifies* HAMi, never installs it | keeps `GPUCellPool` out of the addon-manager business; Flux/Helm/CAPI ClusterResourceSet already own it |
| D7 | cell `Ready` | outer `SwiftGuest` Running **AND** inner Node Ready **AND** capacity provider reports the expected device count on that node | "no silent failures" — a booted VM whose GPU never surfaced is the exact failure this pool exists to catch |
| D8 | Cluster API | **A second provisioner, never a replacement — shipped.** `provisioner: ClusterAPI` creates one **Machine + KubeSwiftMachine per cell**, named after the cell, NOT a MachineDeployment sized to the replica count | CAPI can only add Machines to a **CAPI-managed** workload cluster; the target use case is *bring-your-own* cluster. The per-cell-Machine departure from the original sketch is forced: a MachineDeployment generates Machine names, capi-kubeswift derives the guest hostname (and so the Node name) from the Machine name, and random names would break the cell-name==Node-name identity and leave no way to drain one specific cell. Needed `KubeSwiftMachine…swiftGuest.gpu` in capi-kubeswift first (merged as its #19) |
| D9 | repo + license | **Separate repo, Apache-2.0, unstructured client to KubeSwift** (no AGPL import), like `capi-kubeswift` | it is a composition layer over two independent projects; the license boundary is the mechanical proof of that claim |
| D10 | scale-down | **Shipped, both triggers.** `replicas`/`minReplicas` down = drain-then-delete; `autoscaling.scaleDown: Auto` removes an **idle** cell after a long quiet window. `minReplicas: 0` is allowed, and recoverable because the cell shape is remembered (§8.1 of the capacity doc) | deleting a cell with live HAMi workloads on it is the one unrecoverable mistake in this architecture — so idleness is read from the provider, never inferred, and the drain gate re-checks before the guest goes |

---

## 4. Architecture

```
        OUTER / INFRASTRUCTURE CLUSTER (KubeSwift)
 ┌───────────────────────────────────────────────────────────────┐
 │ GPUCellPool controller                                        │
 │   ├── CellManager        index/name allocation, per-cell FSM   │
 │   ├── CellProvisioner    SwiftGuest (v1) | ClusterAPI (later)  │
 │   ├── PhysicalInventory  outer ResourceSlices/Claims → free GPUs│
 │   ├── WorkloadClient     one cached client per kubeconfig      │
 │   ├── CapacityProvider   HAMi{DevicePlugin|DRA} (inner reads)  │
 │   └── ScalingPolicy      static (v1) → demand-driven (Ph. 3)   │
 │            │ owns                                             │
 │            ▼                                                  │
 │   SwiftGuest <pool>-0 … <pool>-N   (+ SwiftSeedProfile each)   │
 │            │ KubeSwift: launcher pod, gpu-init, VFIO bind      │
 │            ▼                                                  │
 │   physical GPU on a kubeswift.io/gpu-node=true node            │
 └────────────┬──────────────────────────────────────────────────┘
              │ PCI passthrough (whole device)
              ▼
 ┌───────────────────────────────────────────────────────────────┐
 │ GPU Cell VM: Linux + NVIDIA driver + containerd + kubelet      │
 │  prebaked image; cloud-init only enrolls (token + node labels) │
 └────────────┬──────────────────────────────────────────────────┘
              │ joins (routable secondary NIC, kubelet --node-ip)
              ▼
        INNER / WORKLOAD CLUSTER (HAMi)
 ┌───────────────────────────────────────────────────────────────┐
 │ Node <pool>-i   label gpu=on   annotation node-nvidia-register │
 │ HAMi scheduler + device plugin (or HAMi DRA driver)            │
 │   workload A (4Gi/30%)  workload B (8Gi/60%)  …                │
 └───────────────────────────────────────────────────────────────┘
```

Two reads, two clusters, one controller. Everything the controller learns about
the inner side is read through **one scoped kubeconfig** and cached per pool
(`-reconciliation.md` §6), never one manager per cell.

---

## 5. Scope

**MVP (Phase 2).** Fixed `replicas`; `provisioner: SwiftGuest`; one whole `pcie`
GPU per cell via `gpuResourceClaim` (DRA) or `gpuProfileRef` (native); prebaked
image + thin cloud-init enrollment; HAMi pre-installed in the workload cluster;
readiness = the 3-way AND; aggregated status for both layers; drain-guarded
deletion; metrics.

**Postponed, with the extension point named:**

| Postponed | Extension point already in the design |
|---|---|
| ~~automatic scale-down~~ | **shipped**: `spec.autoscaling.scaleDown: Auto` over `deletion.policy` + the `Draining` cell state + finalizer. Only provider-reported **idle** cells are candidates; demand must have been absent for the whole (longer) window; membership removes the cells the autoscaler *named* rather than the highest index, or it would destroy work on cell 2 while cell 0 sat empty. `status.cellDeviceShape` keeps `minReplicas: 0` recoverable |
| ~~demand-driven scale-up~~ | **shipped**: `spec.autoscaling` + `DecideScale` + `PendingDemand`. Four gates: unread demand, unsatisfiable demand, the ceiling, a saturated cluster — plus a stabilization window, because a cell measured ~14 minutes to Ready and demand does not clear until it is |
| multi-GPU / NVLink cells | `cell.gpu.count` validated `== 1` in v1alpha1; the GPU intent already carries topology |
| MIG, overcommit policy | `CapacityProvider` mode enum; MIG is an inner-layer concern that changes only capacity parsing |
| multiple workload clusters per pool | `spec.workloadCluster` is a struct, not a list — one pool per cluster; a fleet view is a `Cluster`-CRD/gateway concern |
| HAMi install lifecycle | D6 — verification only, `CapacityProviderReady` condition |
| workload/model/KServe/Ray/Kueue/llm-d | out of scope permanently: cells are capacity, Pods remain Pods |
| ~~CAPI as the provisioner~~ | **shipped and HARDWARE-VALIDATED 2026-08-08**: `cell.provisioner: ClusterAPI` + `cell.clusterAPI`. Bootstrap comes from the cluster's own provider when `bootstrapConfigTemplateRef` is set (instantiated per cell, as a MachineSet does); otherwise the pool's rendered Secret is handed over as `dataSecretName`. A pool added the only worker of a real CAPI-managed cluster (Machine + providerID + Node + HAMi fractions) in 6m29s. Two bugs the harness could not see were found doing it — see `docs/clusterapi-cells.md` |
| non-KubeSwift outer provisioners | same enum — the `CellProvisioner` seam is deliberately VMM-agnostic |

**Non-goals (hard).** No HAMi API/scheduler/device-plugin logic, resource names, or
libraries inside KubeSwift. No KubeSwift awareness in HAMi. No HAMi fraction ever
handed to VFIO. `GPUCellPool` is not a workload API.

---

## 6. Two cross-layer couplings the brief did not name

**(a) Outer node drain must be sequenced through inner node drain.** A cell is a
VFIO guest, so KubeSwift's default `drainPolicy: Migrate` resolves to *offline*
migration — a VM restart. Applied to a cell, that silently reboots a Kubernetes
worker with running HAMi workloads on it. v1alpha1 therefore stamps
`migration.enabled: false` on every cell guest (pinned) and surfaces a
`CellDrainRequested` condition when the outer node is cordoned or KubeSwift sets
`kubeswift.io/drain-requested`. The operator (human) then scales/replaces. Phase 4
automates the correct order: cordon inner Node → drain inner workloads → delete
cell → recreate elsewhere. Never the reverse.

**(b) The GPU is released by deleting the cell, not by draining it.** Outer
capacity only returns to the pool when the `SwiftGuest` is gone (native backend:
the `kubeswift.io/gpu-allocation` finalizer releases; DRA: the claim/pod is
collected). So `status.physicalCapacity` must be derived from the **outer**
inventory, never inferred from inner state.

---

## 7. Isolation claim — how to state it

Say: *layered isolation.* A cell gives each group of workloads a VM boundary
around the physical GPU; HAMi gives efficiency inside that boundary. What that is
worth depends on GPU hardware, DMA/IOMMU isolation, firmware, driver, and the
threat model — and on the outer cluster, where launcher pods are privileged, so
**the right to create cells is node-root-equivalent authority**. Do not claim
absolute tenant isolation; do claim a stronger boundary between cells than
GPU-sharing alone provides.

---

## 8. Phases

| Phase | Content | Gate |
|---|---|---|
| 0 | this design set | — |
| 1 | hardware proof: GPU → cell VM → nvidia driver → HAMi → 2 fractional workloads, done by hand | **boba/GTX 1080; blocks everything** |
| 2 | static `GPUCellPool` (MVP, §5) | Phase 1 PASS |
| 3 | demand-driven scale-**up** (pending-pod signal, guarded) — **DONE** | Phase 2 stable |
| 4 | safe scale-**down** + automated outer-drain sequencing | Phase 3 stable |
| 5 | `provisioner: ClusterAPI` — the `capi-kubeswift` GPU field landed as PR #19; the provisioner itself is next | Phase 2; independent of 3/4 |

The lab has exactly **one** GPU (boba, GTX 1080). Phase 1 is fully doable;
`replicas ≥ 2` is hardware-gated and must be validated against a faked capacity
provider + faked inner Nodes (`-poc.md` §6), the same way KubeSwift validates HGX.

---

## 9. The central test

> Can a user declare a pool of GPU isolation cells, have KubeSwift provide the
> physical GPU-backed VMs, and have those VMs appear as HAMi-managed GPU worker
> capacity in another cluster, **without either project knowing about the other**?

Nothing in this design requires a change to KubeSwift or HAMi. The two changes it
*would* like are both optional and both outside those two projects: GPU fields on
`capi-kubeswift` (Phase 5 only), and — if HAMi ever wants it — nothing at all.
Unresolved questions are collected in `-failure-model.md` §9.
