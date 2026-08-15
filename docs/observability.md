# Observability

The operator exports twelve `gpucell_*` series, a Grafana dashboard and a starter
alert pack. All of it is off by default: turning it on requires the Prometheus
Operator CRDs, and a default install should not depend on them.

```bash
helm upgrade --install gpucellpool oci://ghcr.io/kubeswift-io/charts/gpucellpool \
  --namespace gpucellpool-system --create-namespace \
  --set monitoring.enabled=true
```

That renders a `ServiceMonitor`, a `PrometheusRule` and the dashboard as a
sidecar-discovered `ConfigMap`. Each piece can be disabled on its own
(`monitoring.serviceMonitor.enabled`, `.prometheusRule.enabled`,
`.dashboards.enabled`).

Two settings usually need adjusting for a given cluster:

| Value | When |
|---|---|
| `monitoring.serviceMonitor.additionalLabels` | your Prometheus selects ServiceMonitors by label — kube-prometheus-stack wants `release: <its release>` |
| `monitoring.dashboards.namespace` | your Grafana sidecar only watches its own namespace, which is its default. Point this at the Grafana namespace |

## The metrics

| Metric | Type | Labels | What it is for |
|---|---|---|---|
| `gpucell_cells_desired` | gauge | `pool`, `namespace` | the count the pool is aiming for — `spec.replicas`, or `status.desiredReplicas` when autoscaling is on |
| `gpucell_cells` | gauge | + `phase` | cells in each phase |
| `gpucell_cell_startup_seconds` | histogram | `pool`, `namespace` | creation to *first* Ready; a later Ready after a regression is not a startup |
| `gpucell_cell_transitions_total` | counter | + `from`, `to` | churn — an oscillating cell looks healthy in any instantaneous phase |
| `gpucell_physical_gpus` | gauge | + `state` (`held`, `free`) | **outer layer**: whole PCI devices |
| `gpucell_capacity_gpu_devices` | gauge | `pool`, `namespace` | **inner layer**: devices the capacity provider advertises |
| `gpucell_capacity_gpu_memory_bytes` | gauge | + `state` (`total`, `allocated`, `available`) | inner layer: GPU memory |
| `gpucell_capacity_gpu_compute_percent` | gauge | + `state` | inner layer: compute, homogeneous pools only |
| `gpucell_workload_cluster_reachable` | gauge | `pool`, `namespace` | 1 when the last reconcile reached the workload cluster |
| `gpucell_capacity_scrape_errors_total` | counter | + `reason` | capacity figures are stale |
| `gpucell_scale_decisions_total` | counter | + `reason`, `scaled_up` | every scaling decision, **including refusals** |
| `gpucell_reconcile_errors_total` | counter | `pool`, `namespace` | the operator is erroring rather than reporting |

### Why the two capacity families are separate

This is the one thing to understand before writing your own queries.
`gpucell_physical_gpus` counts **whole PCI devices in the infrastructure
cluster**. `gpucell_capacity_gpu_*` counts **memory and compute fractions inside
the cells' own cluster**, as HAMi accounts for them. They are never merged,
because "no GPU left in the cluster" and "the shared GPU inside the cells is
full" are different problems with different remedies:

```promql
# the infrastructure cluster is out of devices, and this pool wants one
gpucell_physical_gpus{state="free"} == 0
  and on (pool, namespace) (gpucell_cells_desired - sum by (pool, namespace) (gpucell_cells{phase="Ready"})) > 0

# the cells are healthy and their GPU is fully handed out
gpucell_capacity_gpu_memory_bytes{state="available"} == 0
  and on (pool, namespace) gpucell_capacity_gpu_devices > 0
```

Two traps worth naming:

- **Device counts never come from `nvidia.com/gpu` allocatable.** HAMi inflates
  it by `deviceSplitCount` (default 10), so a one-GPU node advertises 10. Use
  `gpucell_capacity_gpu_devices`, which is parsed from HAMi's node registration.
- **Compute is a percentage *of a device*,** so the pool-wide aggregate is only
  published while the pool is homogeneous. 100 of a GTX 1080 and 100 of an H200
  are not 200 of anything.

### Stale, not zero

A failed read of the workload cluster **retains** the previous capacity values
rather than reporting zero — reporting "0 GPUs, plenty free" from a failed scrape
would be the worst possible lie. `gpucell_capacity_scrape_errors_total` is
therefore not a nice-to-have: a rising rate means every capacity number on the
dashboard is untrustworthy, and `GPUCellPoolCapacityStale` alerts on exactly
that.

## The alerts

Eight rules, all `warning`. Nothing here means "wake someone": several of these
conditions are states the operator reports deliberately rather than faults.

| Alert | Fires when | Layer |
|---|---|---|
| `GPUCellPoolMetricsUnscrapable` | the scrape has failed for 10m — **every other alert here is silent while it lasts**, which is why this one is written over `up` rather than `gpucell_*` | scrape |
| `GPUCellPoolWorkloadClusterUnreachable` | unreachable for 10m — every destructive path is frozen meanwhile | transport |
| `GPUCellPoolCellsNotReady` | short of Ready cells for 30m (a cell takes ~5 min, so this is stuck, not starting) | cells |
| `GPUCellPoolCellFailed` | a Failed cell persists for 15m — replacement is not fixing it, or the pool has stopped replacing (`Progressing=FaultNotInTheCell`) because the fault is pool-wide | cells |
| `GPUCellPoolCellFlapping` | sustained phase churn | cells |
| `GPUCellPoolNoFreePhysicalGPU` | the pool wants a cell **and** no device is free | outer |
| `GPUCellPoolSharedGPUExhausted` | healthy cells whose GPU memory is entirely allocated | inner |
| `GPUCellPoolCapacityStale` | capacity reads are failing | inner |
| `GPUCellPoolReconcileErrors` | the operator is erroring | operator |

`GPUCellPoolNoFreePhysicalGPU` is deliberately conjoined with "the pool actually
wants a cell". A saturated cluster whose pools are all satisfied is not a fault,
and alerting on it would train people to ignore the rule.

## The dashboard

`config/grafana/gpucellpool-overview.json` is the source of truth; the copy under
`charts/gpucellpool/dashboards/` is synced by `make manifests` (Helm can only
package files inside the chart). Drift between them fails `make verify`, so a
hand-edit to one is caught rather than shipping a dashboard nobody sees.

Four rows, in the order you would actually read them: pool health, the physical
layer, the shared-capacity layer, then decisions and reliability. It is templated
by namespace and pool, so one dashboard covers a fleet.

## The metrics endpoint authenticates

`metrics.secure: true` serves HTTPS with a self-signed certificate — which is why
the ServiceMonitor sets `insecureSkipVerify` — and requires a bearer token whose
owner is authorized to `get` the `/metrics` path. Measured behaviour:

| Request | Response |
|---|---|
| no token | `401` |
| unparseable or expired token | `401` |
| valid token, principal not authorized | `403` |
| valid token, authorized | `200` |

So a scraper needs **two** things, and they fail differently.

**A credential.** The chart's ServiceMonitor sends Prometheus's own projected
ServiceAccount token (`monitoring.serviceMonitor.bearerTokenFile`). Nothing to do
unless your Prometheus keeps its token somewhere else.

**Authorization for that credential.** Check rather than assume:

```bash
kubectl auth can-i get /metrics --as=system:serviceaccount:<ns>:<prometheus-sa>
```

kube-prometheus-stack answers `yes` — its Prometheus ClusterRole already grants
`nonResourceURLs: ["/metrics"]`. If yours answers `no`, either bind it to the
`metrics-reader` ClusterRole this chart ships, or let the chart do it:

```yaml
monitoring:
  serviceMonitor:
    prometheusServiceAccount:
      name: monitoring-kube-prometheus-prometheus
      namespace: monitoring
```

The role is shipped **unbound** because binding it names a ServiceAccount the
chart cannot know, and silently granting an unnamed principal read access would
reopen what the authentication closed.

### Why one alert is not written over `gpucell_*`

Every other rule in the pack is expressed over our own series, so a scrape that
fails takes the entire pack down with it — no series, no alerts, and a dashboard
of empty panels that looks exactly like a fleet with nothing wrong. That is not
hypothetical: it is what the pack did the first time the endpoint grew
authentication and the ServiceMonitor was left without a credential.

`GPUCellPoolMetricsUnscrapable` is written over Prometheus's own `up`, which
survives precisely that failure. If it fires, read the target's error in
Prometheus: `401` means no credential is reaching the endpoint, `403` means the
credential is fine and its owner is not authorized.

To serve no metrics at all, set `metrics.bindAddress: "0"`. `metrics.secure:
false` serves plain HTTP with **no** authentication — a bearer token over
cleartext would be worse than none — and is for local debugging only.
