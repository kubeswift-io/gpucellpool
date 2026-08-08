# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [v0.1.0] — 2026-08-08

First release. Every capability below has been run on real hardware (one
physical GPU, one KubeSwift VM, two independent workloads sharing it through
HAMi) except where noted in **Known gaps**. See `docs/quickstart.md` to try it
and `docs/design/gpucellpool-validation-record.md` /
`docs/design/clusterapi-cells-validation.md` for the full hardware record,
including the bugs a live cluster found that the test harness could not.

### Added — core API and controller

- `GPUCellPool` v1alpha1 API (`cells.kubeswift.io`): one CRD, namespaced, with a
  scale subresource on `spec.replicas`. A cell is an owned `SwiftGuest` named
  `<pool>-<index>`, not a second kind. `spec.cell.guestTemplate` is an opaque
  `SwiftGuestSpec` passthrough so this API never mirrors KubeSwift's — see
  `docs/api-reference.md`.
- Cell state machine (`Pending` → `AllocatingGPU` → `GuestProvisioning` →
  `Booting` → `Joining` → `AwaitingGPUCapacity` → `Ready`), membership planning
  with churn control, physical-inventory pre-flight, per-cell bootstrap
  rendering, drain-gated teardown, and status that separates the two layers
  (`physicalCapacity` = whole devices, `workloadCapacity` = HAMi fractions).
- Validating webhook enforcing the API's rules, fail-closed — see
  `docs/security.md`.

### Added — GPU allocation

- Two backends behind `spec.cell.gpu.backend`: `DRA`
  (`gpuResourceClaim`/`ResourceClaimTemplate`, scheduler-time) and `Native`
  (`gpuProfileRef`, controller-time). One whole `pcie`-tier GPU per cell.

### Added — bootstrap

- `spec.bootstrap.provider: Opaque` — user-supplied cloud-init with a closed
  substitution set (`{{ cellName }}`, `{{ poolName }}`, `{{ nodeLabels }}`,
  `{{ nodeIPInterface }}`, `{{ expectedGPUs }}`), rendered per cell into an
  operator-owned Secret. The join credential is only ever referenced, never
  written into a CR.
- `hack/build-cell-image.sh`: a reference cell image build (Ubuntu Noble +
  NVIDIA driver + `nvidia-container-toolkit` + k0s worker), documented in
  `docs/cell-image.md`.

### Added — capacity and autoscaling

- HAMi capacity provider (`DevicePlugin` mode), written against annotations
  captured from real hardware rather than upstream documentation — see
  `docs/design/gpucellpool-capacity.md`.
- `spec.autoscaling`, both directions — see `docs/autoscaling.md`:
  - **Scale-up**: `enabled`, `minReplicas`, `maxReplicas` (required when
    enabled), `stabilizationWindow` (default 10m). Demand is read from pods
    the scheduler could not place (`PodScheduled=False`/`Unschedulable`) and
    filtered against whether a fresh cell of this pool's shape would actually
    satisfy it — the only two gates that make scale-up safe.
  - **Scale-down**: `scaleDown: Auto` (requires `minReplicas`), removing only
    cells the capacity provider reports **idle**, after demand has been
    absent for the whole `scaleDownStabilizationWindow` (default 30m, longer
    than scale-up on purpose).
  - `status.cellDeviceShape`: the remembered GPU shape, kept after the last
    cell is removed, so `minReplicas: 0` is recoverable instead of a one-way
    door.

### Added — Cluster API cells

- `spec.cell.provisioner: ClusterAPI` + `spec.cell.clusterAPI`
  (`clusterName`, `version`, `bootstrapConfigTemplateRef`): each cell becomes
  a `Machine` + `KubeSwiftMachine`, one per cell (never a `MachineDeployment`
  — see `docs/clusterapi-cells.md` for why that would have broken cell
  identity). When `bootstrapConfigTemplateRef` is set, the workload cluster's
  own bootstrap provider supplies join data instead of `spec.bootstrap`.
- `KubeSwiftMachine` can express only `imageRef`, `guestClassRef`,
  `interfaces` — any other `guestTemplate` field is rejected at admission
  under this provisioner rather than silently dropped.

### Added — observability

- `gpucell_*` Prometheus metrics (twelve series, `{pool, namespace}` labels
  throughout): cell counts by phase, `cell_startup_seconds`,
  `cell_transitions_total` (catches an oscillating cell even when its current
  phase looks healthy), physical vs. workload capacity kept as separate
  metric families, `capacity_scrape_errors_total` (a failed read retains the
  previous value rather than reporting zero — the errors counter is how you'd
  know), `scale_decisions_total` (including refusals), `reconcile_errors_total`.

### Added — packaging and security

- Helm chart, distroless/non-root/read-only-rootfs container image, CI.
- `config/rbac/workload-cluster-observer.yaml`: the minimal
  `ServiceAccount`+`ClusterRole` a workload-cluster credential needs — no
  `cluster-admin`, no `pods/eviction`, `nodes: delete` granted as an
  always-on right (stale-Node reaping and cell teardown both need it
  unconditionally, not only under automatic scale-down).

### Fixed (found during hardware validation)

- `cell.nodeIPFrom` is an observation field, not a readiness gate. KubeSwift
  v0.13.4 reports a secondary NAD interface's MAC but not its IP, and the
  operator does not control what the kubelet registers anyway (cloud-init
  derives the node IP in-guest), so gating readiness on it parked every
  bridge-NAD cell in `Booting` forever.
- `networkRef` takes `{name, namespace}` only; an earlier sample and design
  doc showed a `kind` field that does not exist and that KubeSwift's strict
  decoding rejects — the sample as first written could not have been applied.
- Scale-to-zero was a one-way door: with the satisfiability reference device
  coming from live capacity only, an emptied pool had nothing to judge new
  demand against, counted nothing satisfiable, and never grew back — while
  blaming the request. Fixed by `status.cellDeviceShape` (above).
- `Ready` was measured against `spec.replicas`, which stops being the target
  once autoscaling is on: a healthy pool holding two autoscaled cells reported
  "2 of 1 cells are Ready" and `False`. A pool deliberately holding zero cells
  also reported itself broken. Both now report `ScaledToZero` correctly
  against `status.desiredReplicas`.
- The pool claimed the **controller** owner reference on ClusterAPI Machines.
  Kubernetes allows one per object and Cluster API needs it, so every
  reconcile failed with "already owned by another GPUCellPool controller" —
  no bootstrap data, no VM, ever. Cells are co-owned now.
- ClusterAPI pool teardown listed SwiftGuests, so it saw no cells, drained
  nothing, and dropped its own finalizer — orphaning each Machine with the
  drain finalizer still on it, GPU claim leaked. Teardown goes through the
  provisioner's own lister now.

### Known gaps

- `hami.mode: DRA` reports `ErrUnsupported`; only `DevicePlugin` mode is
  implemented.
- Pools of two or more cells are covered by the two-apiserver test harness,
  not by hardware — the reference lab has one GPU. So are
  `deletion.policy: Force` and cell replacement backoff.
- Cell startup measures 4m45s to Ready, about three minutes of which is
  cloning a 30 GiB root disk. A smaller disk or a copy-on-write clone
  strategy is where the next minute would come from.
- A join template must derive the cell's routable address by subnet:
  `nodeIPFrom` names a KubeSwift interface, and cloud-init cannot map that to
  a guest device.
- No rolling update on `guestTemplate` change, no automated outer-drain
  sequencing, no bootstrap token minting. See `docs/limitations.md` for the
  full list and the operational workarounds.
