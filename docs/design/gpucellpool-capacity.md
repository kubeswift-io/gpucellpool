# GPUCellPool — capacity discovery

> Two capacities, never merged: **physical** (outer, KubeSwift/DRA — whole devices)
> and **workload** (inner, HAMi — memory + core fractions). One internal
> `CapacityProvider` interface isolates HAMi so its churn cannot reach the CRD (D5).
>
> Companions: `gpucellpool-api.md` §6 (status), `-reconciliation.md` §4.
> Date: 2026-07-30.

---

## 1. The interface

```go
// CapacityProvider observes the INNER cluster. It never writes, never allocates,
// and never learns about KubeSwift. HAMi is one implementation; the NVIDIA DRA
// driver, MIG, or a time-slicing plugin could be others.
type CapacityProvider interface {
	Name() string // "HAMi"

	// Health reports whether the provider is present and parseable at all
	// (drives CapacityProviderReady).
	Health(ctx context.Context, nodes []string) (Health, error)

	// Devices reports how many GPU devices the provider sees on one node.
	// Drives the third leg of cell readiness (D7).
	Devices(ctx context.Context, node string) (int, error)

	// Capacity reports total/allocated per node, per device, per model.
	Capacity(ctx context.Context, nodes []string) (Capacity, error)

	// Allocations reports whether a node still has consumers holding capacity —
	// the scale-down / drain gate. Non-zero blocks deletion.
	Allocations(ctx context.Context, node string) (Allocations, error)

	// PendingDemand reports unsatisfiable GPU demand (Phase 3; returns
	// ErrUnsupported in the MVP).
	PendingDemand(ctx context.Context) (Demand, error)
}
```

Every method takes the node set from `status.cells[]`, so the provider only ever
looks at nodes this pool owns. A workload cluster with other GPU nodes (bare metal,
another pool) is unaffected and uncounted.

---

## 2. HAMi — DevicePlugin mode (default)

### Inventory (authoritative: the Node annotation)

```
Node <cell>
  labels:       gpu=on                      # HAMi's scheduling gate
  annotations:  hami.io/node-nvidia-register: |
    [{"id":"GPU-e71afe85-9309-864a-477e-91caa89f3932","count":10,"devmem":8192,
      "devcore":100,"type":"NVIDIA GeForce GTX 1080","mode":"hami-core",
      "health":true,"devicepairscore":{}}]
```

Per GPU: `id` (UUID), `count` (**inflated** logical slots — `deviceSplitCount`,
default 10), `devmem` (MiB), `devcore` (percent), `type` (model, **with spaces** —
do not assume a hyphenated form), `mode` (`hami-core`), `health`, and
`devicepairscore`. The above is a **verbatim capture from the Phase-1 cell**
(2026-08-07, HAMi 2.x, GTX 1080).

Two parser consequences, both learned from that capture rather than from the docs:
`numa` — which older documentation shows — was **absent**, and `mode` /
`devicepairscore` are present and undocumented. So the parser must tolerate missing
*and* unknown fields, and treat only `id`, `devmem`, `devcore` and `health` as
required. Anything stricter breaks on a HAMi upgrade.

Three parsing rules, all load-bearing:

1. **Device count comes from this annotation, not from `allocatable`.**
   `nvidia.com/gpu` allocatable is `devices × count` (30 on a 3-GPU node with the
   default split), so treating it as a device count would report a cell as holding
   10 GPUs. `Devices(node)` = number of entries with `health: true`.
2. **`devmem` is MiB, `devcore` is a percentage of one device.** Convert once, at
   the edge: memory → bytes in status; compute stays percent and is only summed
   within a homogeneous pool (§5).
3. **A parse failure is `Unknown`, never zero.** The annotation format is an
   internal HAMi protocol and can change. `Health` returns
   `RegistrationUnparseable`, `CapacityProviderReady=False`, and the pool refuses
   to mark cells Ready or to scale down. Reporting "0 GPUs, plenty free" from a
   failed parse would be the worst possible silent failure.

### Allocated (authoritative: Pod annotations, fallback: requests)

HAMi's scheduler↔device-plugin protocol lives on the **Pod**:

```
hami.io/vgpu-devices-allocated    GPU-<uuid>,NVIDIA,<memMiB>,<cores>:;   (one record per device)
hami.io/vgpu-devices-to-allocate  ";;" once binding completed; non-empty while in flight
hami.io/bind-time                 unix seconds, e.g. "1786087583" (timeout detection)
```

Measured on the Phase-1 cell: two pods, each limited to 3000 MiB / 30%, both carried
`vgpu-devices-allocated` naming the **same** UUID — which is the whole architecture in
one field, and the assertion the capacity tests should encode.

Accounting per cell node:

```
allocated = Σ over non-terminal Pods on the node:
              parse(hami.io/vgpu-devices-allocated)          # preferred
              ⊕ parse(hami.io/vgpu-devices-to-allocate)      # in-flight, counted
              ⊕ fallback: nvidia.com/gpumem / gpumem-percentage / gpucores requests
available = total − allocated
```

The fallback matters: the annotation is the truth of what HAMi *bound*, the
requests are the truth of what the user *asked*. When they disagree, report the
annotation and record `gpucell_capacity_scrape_errors_total{reason="annotation_request_mismatch"}`
— disagreement is a signal about HAMi's state, not a value to average.

Terminal pods (`Succeeded`/`Failed`) are excluded; in-flight
(`-to-allocate` non-empty, i.e. not `";;"`) is **included**, because a drain that
ignored in-flight allocations would race the scheduler.

Note that a request larger than the device is refused by HAMi's **scheduler**, not at
runtime: 9000 MiB against an 8192 MiB card left the pod Pending with
`NodeUnfitPod` (measured). So over-capacity demand shows up as unschedulable pods —
which is exactly the Phase-3 scale-up signal, and exactly why that signal must be
filtered against "would a fresh cell of this shape actually satisfy it" (§8).

---

## 3. HAMi — DRA mode

```yaml
# ResourceSlice published by the HAMi DRA driver (per node)
devices:
  - name: gpu-0
    allowMultipleAllocations: true
    capacity:
      cores:  {value: "100"}
      memory: {value: "8Gi"}
    requestPolicy: {...}
```

```yaml
# ResourceClaim consuming a share of it
status:
  allocation:
    devices:
      results:
        - driver: <hami driver>
          pool: <node>
          device: gpu-0
          shareID: "…"
          consumedCapacity: {cores: "30", memory: "4Gi"}
```

Then:

```
total     = Σ device.capacity      over slices in pool=<cell node>, deviceClass = spec.capacity.hami.deviceClassName
allocated = Σ result.consumedCapacity  over allocated ResourceClaims whose results point at those devices
available = total − allocated
devices   = count of devices in the node's pool
```

This is the cleaner model — capacity is a first-class API field rather than an
annotation protocol — and it is the reason `mode: DRA` is the strategic target.
It is also the newer one: `projecthami/k8s-dra-driver:v0.1.0`, requiring
**Kubernetes ≥ 1.34** with the `DRAConsumableCapacity` feature gate and CDI. The
provider therefore preflights those facts and reports
`CapacityProviderReady=False`/`DRAFeatureGateMissing` rather than silently reading
empty slices (a missing gate looks exactly like "no capacity").

Deliberately **not** consumed: `Project-HAMi/HAMi-DRA`'s conversion webhook
(`nvidia.com/gpu` → ResourceClaims, defaults `resourceName`/`resourceMem`/
`resourceCores`). It is a workload-side convenience; it changes nothing about how a
pool observes capacity, and depending on it would drag workload semantics into this
operator.

---

## 4. Physical capacity (outer) — separate code path, separate field

Never inferred from the inner side:

```
held  = Σ len(SwiftGuest.status.gpu.devices) over this pool's cells
free  = devices published in outer ResourceSlices (driver gpu.kubeswift.io,
        attribute vfioReady == true)
        − devices referenced by allocated outer ResourceClaims
        − devices marked allocated on SwiftGPUNode (native backend)
```

`free` is what makes `InsufficientPhysicalGPU` a *pre-flight* determination instead
of a scheduler timeout (`-reconciliation.md` §4). The device naming contract makes
this cheap and exact: the KubeSwift DRA driver publishes pool = node name and
device = `gpu-<bdf>`, so the allocation result alone identifies the device
(`cmd/kubeswift-dra-driver/driver.go`).

`status.physicalCapacity.model` is read from the outer `vendorDevice` attribute /
`SwiftGPUNode.status.gpuModel`, and cross-checked against the inner `type` string.
A mismatch means the pool is claiming a different GPU than HAMi is advertising —
an event, and a `ModelMismatch` reason on `CapacityAvailable`.

---

## 5. Normalization across GPU models

v1alpha1 **does not normalize**. Rules:

- A pool is homogeneous by construction: one `cell.gpu` spec, one image, one claim
  template. Homogeneity is verified, not assumed — from the inner `type` strings.
- `workloadCapacity.byModel[]` is always populated and is the only compute truth.
- The flat `gpuMemory` aggregate is always valid (bytes are commensurable).
- The flat `gpuCompute` aggregate is published **only** when
  `homogeneous == true`. Percentages of different devices are not addable: 100
  cores of a GTX 1080 and 100 cores of an H200 are not 200 of anything. When a pool
  drifts mixed-model (a CEL selector that matches two models, a partial image
  rollout), `homogeneous: false` is set, the flat compute aggregate is dropped, and
  `CapacityAvailable` carries reason `Heterogeneous`.
- No normalized "GPU units", no TFLOPS estimates, no model score table. If a future
  scheduler needs cross-model comparison, that belongs to the consumer of the
  status, not to this operator.

---

## 6. Readiness and degradation

| Provider state | `CapacityProviderReady` | Effect |
|---|---|---|
| annotation/slices present, parseable, device count ≥ expected on every ready cell | True | cells may reach `Ready`; capacity reported |
| present but a cell reports fewer devices than expected | True | that cell stays `AwaitingGPUCapacity` → `Failed` after `capacity.readyTimeout` |
| `gpu=on` missing / no annotation / no slices | False `HAMiNotDetected` | no cell reaches `Ready`; **scale-down blocked**; capacity `Unknown` |
| parse error | False `RegistrationUnparseable` | as above, plus scrape-error metric |
| k8s < 1.34 or gate off (DRA mode) | False `DRAFeatureGateMissing` | as above, with an actionable message |
| inner cluster unreachable | (untouched) | `WorkloadClusterReachable=False`; **last known capacity retained** and stamped `lastObserved`; nothing destructive runs |

Permitted while degraded: creating cells (they park in `AwaitingGPUCapacity` — a
correct, visible state), reading, reporting. Forbidden while degraded: marking cells
`Ready`, counting them as capacity, scale-down, and drain-completion decisions.

Stale-but-honest beats fresh-but-wrong: capacity is never zeroed because a scrape
failed; it is retained with `lastObserved` and a False condition.

---

## 7. Scrape cadence and cost

- Node/Pod/ResourceClaim data comes from **per-pool informers**, so capacity is
  computed from cache on every reconcile — no polling storm, no per-cell client.
- Recompute triggers: any watched inner object change, plus the 2-minute resync.
- Cost scales with (cells × pods-on-those-cells), which is bounded and small; the
  informer is label/field-filtered to the pool's nodes where the API allows it.
- `lastObserved` is written on every successful full computation and is the field
  an operator should look at first when the numbers look wrong.

---

## 8. Pending demand (Phase 3 groundwork, not implemented)

`PendingDemand` returns `ErrUnsupported` in the MVP. The design intent, recorded so
Phase 3 does not have to re-derive it:

- **DRA mode is the good signal**: a `ResourceClaim` in `WaitingForFirstConsumer`/
  unallocated state whose `deviceClassName` is the pool's HAMi class, with
  `capacity.requests` that **would fit** on a fresh cell but fits nowhere now. That
  is unambiguous GPU-capacity demand.
- **DevicePlugin mode is the weak signal**: pending Pods requesting
  `nvidia.com/gpu`+`gpumem` whose `PodScheduled=False` reason is
  `Unschedulable`/`FailedScheduling` mentioning the HAMi resources.
- Both must pass two filters before any cell is created:
  `demand is GPU-capacity-constrained` **AND** `a fresh cell of this pool's shape
  would satisfy it`. A Pod pending on a missing ConfigMap, a wrong nodeSelector, an
  impossible model, or a request larger than one cell's whole GPU must **never**
  create a cell. That check is the entire safety of Phase 3, which is why it is
  designed here and shipped later.
