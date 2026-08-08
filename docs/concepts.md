# Concepts

## The invariant

```
OUTER   KubeSwift    physical GPU -> VM        whole device, VFIO
INNER   HAMi         VM's GPU -> workloads      memory + core fractions
BETWEEN GPUCellPool   lifecycle + composition    no resource translation
```

KubeSwift owns the physical-GPU-to-VM boundary. HAMi owns GPU-to-workload
allocation inside that VM. GPUCellPool owns the lifecycle between them and knows
about both — but a HAMi allocation is never handed to VFIO, and a VFIO device is
never handed to HAMi. There is no code path between the two DRA domains: they run
in different clusters, use different drivers, and are never joined.

Positioning, precisely: GPUCellPool *composes* KubeSwift VM isolation with HAMi
GPU sharing. It is not "HAMi integration for KubeSwift" — neither project is
modified, and neither learns the other exists.

## A cell

A **cell** is one KubeSwift VM holding one whole passthrough GPU, running as a
worker node in a separate *workload* Kubernetes cluster, where HAMi shares that
GPU between workloads. A `GPUCellPool` declares N cells; there is no `GPUCell`
CRD — a cell is an owned `SwiftGuest` (or, under `provisioner: ClusterAPI`, a
`Machine` + `KubeSwiftMachine`) named `<pool>-<index>`, with per-cell state
recorded in `status.cells[]`.

```
                       cell "inference-0"
┌─────────────────────────────────────────────────────────┐
│  SwiftGuest inference-0     (infrastructure cluster)      │
│  ┌───────────────────────────────────────────────────┐   │
│  │ VM: Linux + NVIDIA driver + containerd + kubelet    │   │
│  │   physical GPU ─VFIO──────────────┐                 │   │
│  └────────────────────────────────────┼─────────────────┘   │
└─────────────────────────────────────────┼─────────────────┘
                                          │ joins as
                                          ▼
                       Node inference-0   (workload cluster)
              ┌───────────────┼───────────────┐
           pod A            pod B           pod C
         4Gi / 30%        8Gi / 50%       2Gi / 20%      ← HAMi fractions
```

## Two identities, never conflated

| | outer (infrastructure cluster) | inner (workload cluster) |
|---|---|---|
| object | `SwiftGuest` (or `Machine`) + launcher Pod | `Node` |
| resource | 1 physical GPU (PCI BDF), CPU, RAM, disk | HAMi device + memory/core capacity |
| owned by | KubeSwift | HAMi |
| read by | the pool's outer client | the pool's workload-cluster client |

Correlation between the two is by **name**, not IP or any other derived value:
cell name = `<pool>-<index>` = guest hostname = workload Node name. The Node
additionally carries `cells.kubeswift.io/instance` (the outer guest's UID), so a
stale Node left behind by a *previous* incarnation of the same cell index is
detected and reaped before its replacement is allowed to join — otherwise the
pool would report a phantom Ready cell, or HAMi would advertise capacity for a
GPU that no longer exists.

## Two capacities, never merged

`status.physicalCapacity` (outer) and `status.workloadCapacity` (inner) are
computed by separate code paths and are never inferred from one another:

- **`physicalCapacity`** — whole devices. Read from KubeSwift's and the outer
  DRA driver's inventory. `gpus` is how many this pool's cells hold;
  `freeGPUsInCluster` is how many more the pool could still claim.
- **`workloadCapacity`** — HAMi's fractional accounting. `gpuMemory` and
  `gpuCompute` are total/allocated/available, in HAMi's units (bytes, percent of
  one device).

The physical GPU only returns to the outer pool when the cell's guest is
deleted — draining a cell does not free it, because KubeSwift's own GPU release
(a finalizer in native mode, claim/pod garbage collection in DRA mode) happens
at guest deletion, not at drain.

A third-party consequence worth knowing: HAMi inflates the `nvidia.com/gpu`
allocatable count by `deviceSplitCount` (default ×10) so the scheduler can place
multiple fractional requests against one physical device. `allocatable` is
**not** a device count — a cell node with one physical GPU reports
`nvidia.com/gpu: 10`. Any other consumer of that Node in the workload cluster
(a cluster autoscaler reading capacity, a quota object, a dashboard) sees the
same inflation. GPUCellPool reads the device count from HAMi's
`hami.io/node-nvidia-register` annotation, never from `allocatable`, for exactly
this reason.

## Cells are cattle

A cell is a VFIO passthrough guest. KubeSwift can only move a VFIO guest with an
**offline** migration — a VM restart — never a live one
(`HasVFIODevices` in KubeSwift's migration gate). Applied to a cell that is also
a Kubernetes worker with running HAMi workloads on it, an offline migration is a
silent node reboot under live work. GPUCellPool therefore pins every cell with
`migration.enabled: false` and treats a cell that needs to move as one to be
**replaced**: drained, deleted, and recreated (possibly on another host), never
migrated. If the outer node holding a cell is cordoned or drained, the pool
surfaces `CellDrainRequested` for a human to act on — it does not attempt an
automated sequence in v1alpha1 (see `docs/limitations.md`).

## Layered isolation

A cell gives each group of workloads a VM boundary around the physical GPU;
HAMi gives efficiency inside that boundary. What that combination is worth
depends on the GPU hardware's DMA/IOMMU isolation, firmware, driver, and your
threat model — and on the infrastructure cluster, where every KubeSwift
launcher pod is `privileged: true` by design. That makes **the right to create
a `GPUCellPool` node-root-equivalent authority in the infrastructure cluster**
(see `docs/security.md`).

Say *layered isolation*. Do not claim absolute tenant isolation — a cell is a
stronger boundary between workload groups than GPU-sharing alone provides, not
a hard security perimeter on its own.
