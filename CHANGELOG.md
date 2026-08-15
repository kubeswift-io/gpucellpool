# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- **A join-credential guard.** A pool that had worked and whose bootstrap
  credential then expired was invisible to both existing guards — `EverReady`
  defeats the never-worked check, and no Node registering defeats the capacity
  veto — so every index burned five rebuilds, each a GPU allocation, a 30 GiB
  clone, a boot and a full join timeout. Two consecutive cells failing with no Node
  ever registering now stalls with `Progressing=False/BootstrapCredentialSuspect`,
  naming `spec.bootstrap.joinSecretRef`, and clears on any successful join. (#17)
- **`status.cells[].reason`** — the machine-readable cause behind a cell's phase.
  The FSM already computed it and the reconciler was discarding it, leaving prose
  as the only record of *why* a cell failed. Preserved on a tombstone, because the
  row outlives the guest precisely to carry that memory forward. **This is a new
  CRD field: `helm upgrade` will not install it** (`docs/upgrading.md`).
- **The metrics endpoint authenticates and authorizes.** A bearer token is now
  required, and its owner must be allowed to `get /metrics`: a TokenReview followed
  by a SubjectAccessReview, done with `client-go` rather than controller-runtime's
  filter, which measured at +249 compiled packages (+30%). Allows are cached for
  two minutes keyed on token and path; denials are never cached. (#11)
- **`GPUCellPoolMetricsUnscrapable`**, the one alert not written over `gpucell_*`.

### Fixed

- **The observability pack went dark the moment the metrics endpoint grew
  authentication.** The ServiceMonitor sent no credential, so scrapes returned
  `401`, every `gpucell_*` series vanished, and — because all eight existing alerts
  are expressed over those series — *not one of them could fire*. An empty
  dashboard and total silence is indistinguishable from a healthy fleet. Measured
  against a live Prometheus: `health: down`, `401 Unauthorized`, eight alerts
  silent. The ServiceMonitor now sends Prometheus's own projected ServiceAccount
  token, the chart can bind that ServiceAccount to the `metrics-reader` ClusterRole
  (`monitoring.serviceMonitor.prometheusServiceAccount`), and the new
  `GPUCellPoolMetricsUnscrapable` alert is written over Prometheus's `up` so this
  class of failure can never again be silent.

### Upgrading

Two things need attention, neither automatic:

- **Apply the CRD.** `status.cells[].reason` is new, and Helm does not update
  `crds/`. The manager names the dropped field at startup if you skip it.
- **Check your scraper.** If you scrape the metrics endpoint, it now needs a token
  whose owner may `get /metrics`. kube-prometheus-stack's Prometheus already holds
  that right; verify with
  `kubectl auth can-i get /metrics --as=system:serviceaccount:<ns>:<sa>`.
  `docs/observability.md` covers the `401` vs `403` distinction.

## [v0.1.1] — 2026-08-09

### Added

- **Template drift is visible.** Every cell records the template it was created
  from, surfaced as `status.cells[].templateHash`, and a new `Updated` condition
  compares it with the pool's current template. The hash was being written to each
  cell and never read back, so a `spec.cell` change was invisible to the pool.
- **`spec.updatePolicy.type: RollingUpdate`** replaces stale cells, one at a time,
  through the same drain gate automatic scale-down uses: only cells the capacity
  provider reports idle, never during a resize, never while another cell is coming
  or going, never while the workload cluster is unreachable, lowest index first.
  Opt-in, because a template edit is not consent to destroy running work — and it
  stalls visibly (`UpdateBlocked`) rather than evicting anything. See
  `docs/updates.md`.

- **An observability pack**, off by default because it needs the Prometheus Operator
  CRDs: a `ServiceMonitor`, a Grafana dashboard delivered as a sidecar-discovered
  `ConfigMap`, and eight warning-biased alerts. The two capacity layers get their own
  alerts — `GPUCellPoolNoFreePhysicalGPU` (no whole device left in the infrastructure
  cluster, and the pool wants one) versus `GPUCellPoolSharedGPUExhausted` (healthy
  cells whose GPU is entirely handed out) — because they are different problems with
  different remedies. Enable with `--set monitoring.enabled=true`; see
  `docs/observability.md`.
- A `Service` in front of the metrics port, and a `metrics` container port. There was
  no way to scrape the endpoint at all before: nothing exposed it.

### Fixed

- **The chart claimed the metrics endpoint had authn/authz.** It does not — it is
  HTTPS with a self-signed certificate and no authorization. The claim is corrected
  and the real posture (and how to restrict it) documented; adding the filter pulls
  `k8s.io/apiserver` into a deliberately small dependency tree, so it is tracked
  separately rather than done in passing.
- **A replacement cell inherited its predecessor's teardown.** Cell names are
  reused (index 0 is always `<pool>-0`) and status rows are keyed by name, so a
  freshly created cell adopted the previous incarnation's `Draining` phase and was
  deleted on the pass that created it — create, destroy, create, destroy, with no
  timeout that could break the loop. Rows for a different guest UID no longer lend
  their phase; the failure counter still carries, because the replacement backoff
  is counted per index. This affected any replacement path, not just the new
  rolling update, and was only masked because nothing had previously refilled an
  index whose row still said `Draining`.
- Cell idleness was read from the capacity provider only when *automatic
  scale-down* was enabled, so a rolling update always saw zero idle cells and
  silently never acted.

Everything below was found by installing the **released v0.1.0 chart** on a cluster
and driving a real pool through it, rather than by reading manifests. None of it
could fail a test suite that runs with admin credentials against rendered objects.

- **A default chart install could not create a cell.** The operator renders a
  per-cell bootstrap Secret from the user's join template, and the RBAC marker
  granted `secrets: get;list;watch`. Every pool failed on its first cell with
  `secrets is forbidden`, visible only in the manager log.
- **Every event the operator emitted was rejected.** The recorder writes through
  `events.k8s.io`, which nothing granted, so `kubectl describe gpucellpool` showed
  no events at all.
- **A default install pulled an image tag that was never published.** `appVersion`
  carries no leading `v` while the release workflow pushes the git tag verbatim, so
  the chart's default resolved to `0.1.0` against a published `v0.1.0`.
- **Alerts would have named the operator's namespace, not the pool's.** Without
  `honorLabels`, Prometheus overwrote the metrics' own `namespace` label, landing it
  as `exported_namespace`.
- **A cell was reaped as stale while its own kubelet was using it.** A replacement's
  kubelet registers under the reused node name and adopts the existing Node object,
  keeping the identity label it finds there — so a live cell was indistinguishable
  from a leftover, and deleting it is unrecoverable: a kubelet whose Node is removed
  under it never re-registers, and the cell waits in `Joining` forever. The reap now
  requires that nothing is heartbeating for the Node (its kubelet Lease, falling back
  to the Ready condition's heartbeat, and finally declining to delete when neither
  can be read — an unknown must never authorise a delete). Orphan cleanup became an
  idempotent sweep keyed on the pool label, because the moment a retired cell's row
  is dropped its kubelet has only just died and the Node still looks live: measured,
  a one-shot attempt there reaped nothing and left the Node for the next cell at that
  index to adopt. Needs `coordination.k8s.io/leases: [get]` in the workload cluster;
  without it the check degrades rather than fails.
- **Cells were rebuilt against faults that were not in the cell.** A cell whose Node
  joined and never advertised a GPU was replaced up to five times per index — a GPU
  allocation, a root-disk clone, a boot and a join each time — even when *no* cell
  node anywhere advertised one, which means the fault is HAMi, its tolerations or the
  workload cluster's network and a fresh VM will fail identically. The pool now stalls
  with `Progressing=False/FaultNotInTheCell` and leaves the VM up to be inspected. A
  single failing cell among healthy ones is still replaced, and with no cells at all
  the provider is usable by definition, so a pool cannot wedge itself out of ever
  creating one.
- **An upgraded cluster would have run this release's new fields into the void.**
  `helm upgrade` never updates a chart's `crds/`, and the apiserver silently drops
  what the older schema lacks — so on an upgraded release `updatePolicy.type:
  RollingUpdate` would have been accepted and discarded, and template drift would have
  read as up-to-date forever. The manager now embeds the CRD it was built against,
  compares it with the served schema at startup, and names the exact fields being
  dropped plus the command to fix them. The `installCRDs` value is **removed**: it was
  referenced by nothing, so setting it `false` silently did nothing. See
  `docs/upgrading.md` — new, and worth reading before upgrading.

### Known gaps

- A pool that **was** working and whose join credential later expires still rebuilds
  each cell up to five times per index. The two guards this release ships do not
  reach it — the pool has been Ready, and the Node never registers — and unlike the
  capacity case there is no authoritative signal to gate on, so it is tracked
  (issue #17) rather than guessed at. `docs/limitations.md`.
- The metrics endpoint is HTTPS with a self-signed certificate and **no
  authorization**. Restrict it with a NetworkPolicy; adding the authn/authz filter
  pulls `k8s.io/apiserver` into a deliberately small dependency tree (issue #11).
- Everything listed under v0.1.0's known gaps still applies except the rolling
  update, which shipped here.

### Upgrading from v0.1.0

Apply the CRD after `helm upgrade`. This release adds `spec.updatePolicy` and
`status.cells[].templateHash`, and Helm will not update them for you:

```bash
kubectl apply -f https://raw.githubusercontent.com/kubeswift-io/gpucellpool/v0.1.1/config/crd/bases/cells.kubeswift.io_gpucellpools.yaml
```

Skip it and rolling update is accepted-and-ignored. The manager logs the missing
fields at startup, so `kubectl -n gpucellpool-system logs deploy/gpucellpool | grep 'CRD schema'`
tells you whether you needed it.

Two RBAC additions are picked up by the chart automatically, but a credential built
from an older `workload-cluster-observer.yaml` should be refreshed:
`coordination.k8s.io/leases: [get]` in the **workload** cluster, and
`apiextensions.k8s.io/customresourcedefinitions: [get]` in the infrastructure one.
Both degrade rather than break when absent.

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
