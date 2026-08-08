# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [v0.1.0] — 2026-08-08

### Added

- `GPUCellPool` v1alpha1 API (`cells.kubeswift.io`): one CRD, namespaced, with a
  scale subresource on `spec.replicas`. A cell is an owned `SwiftGuest` named
  `<pool>-<index>`, not a second kind. `spec.cell.guestTemplate` is an opaque
  `SwiftGuestSpec` passthrough so this API never mirrors KubeSwift's.
- Controller: cell state machine, membership planning with churn control,
  physical-inventory pre-flight, per-cell bootstrap rendering, drain-gated
  teardown, and status that separates the two layers (`physicalCapacity` =
  whole devices, `workloadCapacity` = HAMi fractions).
- HAMi capacity provider (DevicePlugin mode), written against annotations
  captured from real hardware rather than upstream documentation.
- Validating webhook enforcing the API's rules, fail-closed.
- Helm chart, container image (distroless, non-root, read-only rootfs), CI.
- Design set under `docs/design/`, including the Phase-1 hardware proof: one
  physical GPU, one KubeSwift VM, two workloads sharing it through HAMi, with
  neither upstream project modified.

### Added — demand-driven scale-up (Phase 3)

- `spec.autoscaling`: `enabled`, `minReplicas`, `maxReplicas`, `stabilizationWindow`,
  `scaleDown` (`Manual` only; `Auto` is rejected). `maxReplicas` is required when
  enabled, because an unbounded pool that misreads demand can consume every GPU in
  the cluster.
- `PendingDemand` for HAMi DevicePlugin mode. Two filters are the whole safety of
  the feature: only pods the scheduler could not place count (`PodScheduled=False`
  / `Unschedulable`, which naturally excludes a pod stuck on a missing ConfigMap —
  that pod was scheduled), and only requests one fresh cell could actually satisfy
  count toward a decision.
- Scale-up is one cell at a time, behind a stabilization window, and never on
  unread demand, an unknown cell shape, a saturated cluster, or a ceiling already
  reached. `status.demand`, `status.desiredReplicas`, `status.lastScaleUpTime` and
  the `ScalingActive` condition report every decision and its reason.

### Added — automatic scale-down (Phase 4)

- `spec.autoscaling.scaleDown: Auto` now works, gated on `minReplicas` being set:
  without a floor the pool could shrink to zero and every later request would pay a
  full cell boot.
- Only cells the capacity provider reports as **idle** are removable, and a cell
  whose allocations cannot be read is never treated as idle — "empty" and "unknown"
  are different answers, and only the first may lead to a deletion.
- Demand must have been absent for the whole `scaleDownStabilizationWindow`
  (default 30m, deliberately longer than scale-up), tracked by
  `status.demandFreeSince`. Absent *right now* is not the same thing: a pool that
  shrinks between two bursts is worse than one that waits.
- Membership removes the autoscaler's named idle cells rather than the highest
  index; highest-index-first remains the rule for an operator-driven shrink, where
  the intent is "make it smaller" rather than "remove that one".

### Added — metrics

- `gpucell_*` Prometheus metrics, with the two layers deliberately kept apart:
  `physical_gpus{held,free}` counts whole devices from the infrastructure cluster,
  `capacity_gpu_*` reports fractional capacity from the workload cluster. An alert
  can therefore tell "no GPU left in the cluster" from "the shared GPU is full".
- `cell_startup_seconds` measures creation to first Ready — the number that decides
  whether autoscaling can be reactive at all.
- `cell_transitions_total` makes an oscillating cell visible even though its
  instantaneous phase looks healthy; `capacity_scrape_errors_total` says the gauges
  went stale, since a failed read retains the previous values rather than zeroing;
  `scale_decisions_total` records refusals as well as scale-ups.

### Fixed (from the first live run)

- `cell.nodeIPFrom` is an observation field, not a readiness gate. KubeSwift
  v0.13.4 reports a secondary NAD interface's MAC but not its IP, and the operator
  does not control what the kubelet registers anyway (cloud-init derives the node
  IP in-guest), so gating on it parked every bridge-NAD cell in `Booting`.
- The sample and design doc showed a `networkRef.kind` field that does not exist;
  KubeSwift's strict decoding rejects it, so the sample could not have been applied.

### Added — Cluster API cells (Phase 5)

- `cell.provisioner: ClusterAPI` plus `cell.clusterAPI` (`clusterName`, `version`,
  `bootstrapConfigTemplateRef`). Each cell becomes a `Machine` and a
  `KubeSwiftMachine`, so a GPU cell added to a CAPI-managed cluster is a member of
  it — with a `providerID`, visible to the cluster's own controllers — rather than a
  node attached out of band.
- One Machine per cell, named after the cell, not a MachineDeployment sized to the
  replica count. A MachineDeployment generates Machine names, and capi-kubeswift
  derives the guest hostname (and so the Node name) from the Machine name, which
  would break the cell-name==Node-name identity and leave no way to drain one cell.
- Bootstrap comes from the workload cluster's own provider when
  `bootstrapConfigTemplateRef` is set: the operator instantiates the template once
  per cell, as a MachineSet does, so tokens and CA hashes are the cluster's rather
  than a secret somebody maintains. `spec.bootstrap.joinSecretRef` is then rejected
  instead of ignored. Without a template ref, the pool's rendered Secret is handed
  over as `dataSecretName`.
- A `KubeSwiftMachine` can express only image, class, two networks and GPU, so any
  other `guestTemplate` field is rejected at admission rather than silently dropped.
- Validated end to end on hardware: pool to Ready in 6m29s, two workloads sharing the
  cell's GTX 1080, teardown returning the GPU claim in under a minute.
  See `docs/clusterapi-cells.md`, which also records what the workload cluster needs.

### Fixed (from the Cluster API validation)

- The pool claimed the **controller** owner reference on its Machines. Kubernetes
  allows one per object and Cluster API needs it, so CAPI failed every reconcile with
  "already owned by another GPUCellPool controller" — no bootstrap data, no VM, ever.
  Cells are co-owned now, which is all garbage collection requires.
- Pool teardown listed SwiftGuests, so a ClusterAPI pool saw no cells, drained
  nothing and dropped its own finalizer — orphaning each Machine with the drain
  finalizer still on it, unclearable, GPU claim leaked. Teardown goes through the
  provisioner now.
- Scale-to-zero was a one-way door. The satisfiability reference device came from
  live capacity only, so an emptied pool had nothing to judge a request against,
  counted nothing satisfiable, and never grew back — while blaming the request.
  `status.cellDeviceShape` outlives the cells; a pool that never advertised a device
  reports `CellShapeUnknown` and says what to do about it.
- `Ready` was measured against `spec.replicas`, which stops being the target once
  autoscaling is on: a healthy pool holding two autoscaled cells reported "2 of 1
  cells are Ready" and False. A pool that deliberately holds none also reported
  itself broken; both now say `ScaledToZero`.

### Known gaps

- `hami.mode: DRA` reports `ErrUnsupported`; only DevicePlugin mode is implemented.
- Pools of two or more cells are covered by the two-apiserver harness, not by
  hardware — the lab has one GPU. So are `deletion.policy: Force` and cell
  replacement backoff.
- Cell startup measures 4m45s, about three minutes of which is cloning a 30 GiB root
  disk. A smaller disk or a copy-on-write clone is where the next minute is.
- A join template must derive the cell's routable address by subnet: `nodeIPFrom`
  names a KubeSwift interface, and cloud-init cannot map that to a guest device.
