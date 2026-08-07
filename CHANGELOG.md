# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

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

### Fixed (from the first live run)

- `cell.nodeIPFrom` is an observation field, not a readiness gate. KubeSwift
  v0.13.4 reports a secondary NAD interface's MAC but not its IP, and the operator
  does not control what the kubelet registers anyway (cloud-init derives the node
  IP in-guest), so gating on it parked every bridge-NAD cell in `Booting`.
- The sample and design doc showed a `networkRef.kind` field that does not exist;
  KubeSwift's strict decoding rejects it, so the sample could not have been applied.

### Known gaps

- `hami.mode: DRA` reports `ErrUnsupported`; only DevicePlugin mode is implemented.
- Autoscaling is not implemented: `spec.replicas` is fixed, and scale-down is
  operator-triggered.
- `cell.provisioner: ClusterAPI` is rejected; only the SwiftGuest provisioner exists.
- The webhook's certificate wiring has no envtest coverage (its validation logic does).
- A join template must derive the cell's routable address by subnet: `nodeIPFrom`
  names a KubeSwift interface, and cloud-init cannot map that to a guest device.
