# Autoscaling

`spec.autoscaling` lets unsatisfiable GPU demand in the *workload* cluster grow
a pool, and lets it shrink again once that demand is gone. Both directions are
shipped. This page is the operator-facing view; the mechanism is designed in
`docs/design/gpucellpool-capacity.md` §8 and implemented in
`internal/controller/scaling.go`.

```yaml
spec:
  autoscaling:
    enabled: true
    minReplicas: 0          # optional; defaults to spec.replicas when omitted
    maxReplicas: 4          # required whenever enabled: true
    stabilizationWindow: 10m            # default
    scaleDown: Auto                     # Manual (default) | Auto
    scaleDownStabilizationWindow: 30m   # default
```

## Why two filters gate every scale-up

A pod pending for a GPU is not, by itself, a reason to create a cell. Two gates
are the entire safety of the feature — get either wrong and the pool burns a
physical GPU and several minutes of boot for nothing:

1. **The demand must be GPU-capacity-constrained.** Only pods with
   `PodScheduled=False` and reason `Unschedulable` count. A pod pending on a
   missing ConfigMap or an unpullable image has already been *scheduled*
   (`PodScheduled=True`) — it can never look like GPU demand, because the
   discriminator excludes it by construction, not by a heuristic that might
   miss a case.
2. **A fresh cell of *this pool's* shape must actually satisfy the request.**
   A request for two devices, or for more memory or compute than one device
   has, is counted as pending but explicitly **not** satisfiable — adding a
   cell would change nothing. Only `status.demand.satisfiableByOneCell` drives
   scale-up decisions, never the raw pending count.

HAMi itself helps here: a request larger than a single device is refused at
**scheduling** time (`NodeUnfitPod`), so an over-large request reliably shows up
as an unschedulable pod rather than silently binding somewhere wrong — which is
exactly the signal the first gate needs.

## Scale-up

One cell at a time, gated in order:

| Gate | `ScalingActive` reason if it blocks |
|---|---|
| demand could not be read from the workload cluster | `Unknown` |
| the pool has never advertised a device (no shape to compare against) | `CellShapeUnknown` |
| something is pending, but none of it fits one cell of this pool's shape | `DemandUnsatisfiable` |
| already at `maxReplicas` | `AtMaxReplicas` |
| demand is satisfiable, but no free GPU remains in the infrastructure cluster | `InsufficientPhysicalGPU` |
| a cell was created inside the `stabilizationWindow` | `Stabilizing` |
| — none of the above — | scales up by exactly one cell, reason `ScaledUp` |

`stabilizationWindow` defaults to **10 minutes**. A cell measures ~4m45s to
Ready (see `docs/design/gpucellpool-bootstrap.md` §6), and demand does not clear
until the new cell is actually Ready — without this window, one burst of
pending pods would create a cell per reconcile.

## Scale-down

Only reached once demand is entirely satisfied (`satisfiableByOneCell == 0`).
Every gate here is a reason **not** to shrink, because the failure mode is
asymmetric: holding an idle GPU costs money, removing a wanted one costs a full
cell boot and the workload that was about to run on it.

| Gate | `ScalingActive` reason if it blocks |
|---|---|
| `scaleDown != Auto` | `Idle` (nothing to do — use `spec.replicas`/`minReplicas` to shrink manually) |
| already at `minReplicas` | `AtMinReplicas` |
| every Ready cell still holds workloads | `NoIdleCell` |
| demand has not been absent for the *whole* `scaleDownStabilizationWindow` yet, or a scale action happened inside it | `Stabilizing` |
| — none of the above — | removes exactly one **idle** cell, reason `ScaledDown` |

`scaleDownStabilizationWindow` defaults to **30 minutes** — deliberately longer
than the scale-up window. Removing a cell that is about to be wanted again costs
a full boot; a pool that shrinks between two demand bursts is worse than one
that waits. "Demand absent" is tracked from `status.demandFreeSince`: absence
*right now* is not enough, it must have held for the entire window.

Only cells the capacity provider reports as **idle** (zero HAMi allocations) are
candidates, and a cell whose allocations cannot be read is never treated as
idle — "empty" and "unknown" are different answers, and only the first may lead
to a deletion. The drain gate re-checks allocations again immediately before the
cell is actually removed, so a workload that lands on a cell between the
autoscaler's decision and the deletion is not destroyed.

`scaleDown: Auto` **requires `minReplicas` to be set explicitly** — without a
floor the pool could shrink to zero and every later request would pay a full
cell boot. This is enforced by the validating webhook.

## `minReplicas: 0` and the remembered cell shape

`minReplicas: 0` is allowed and safe to use. The mechanism that makes it
recoverable is `status.cellDeviceShape`: whenever a live cell advertises a
device, the pool remembers its model, `memoryMiB` and `corePercent`. That
memory **outlives the cells** — it is not cleared when the pool scales to zero.

Without it, an emptied pool would have nothing to judge a request against: with
no live cell to read a shape from, every request would compare against nothing,
read as "does not fit", and the pool would refuse to grow back — forever,
while reporting a misleading `DemandUnsatisfiable`. Resolution order is
live-then-remembered: live capacity is preferred because it is current: the
remembered shape is consulted only in exactly the scaled-to-zero case.

**Caveat, stated plainly: nothing invalidates the remembered shape.** If a pool
scales to zero and its `guestTemplate.imageRef` or `cell.gpu` claim template is
then repointed at a **different GPU model**, `status.cellDeviceShape` still
holds the old model's numbers. A fresh cell judged against the stale shape can
be created (or refused) based on a device the pool no longer actually brings.
There is no drift detection for this in v1alpha1 — if you change a scaled-to-
zero pool's GPU shape, scale it to at least 1 once so the shape re-learns,
rather than trusting the memory.

When a pool has *never* advertised a device (a brand-new pool with
`minReplicas: 0`), it reports `CellShapeUnknown` rather than
`DemandUnsatisfiable` — a distinct reason, because the fix differs: set
`replicas` or `minReplicas` to 1 once so the pool learns its shape, rather than
concluding demand does not fit.

## Watching a decision happen

```bash
kubectl get cellpool <name> -n <ns> -o jsonpath='{.status.conditions[?(@.type=="ScalingActive")]}'
kubectl get cellpool <name> -n <ns> -o jsonpath='{.status.demand}'
kubectl get cellpool <name> -n <ns> -o jsonpath='{.status.cellDeviceShape}'
```

`status.demand.pendingRequests` is the raw pending count;
`status.demand.satisfiableByOneCell` is what actually drives the decision — the
two can legitimately differ (e.g. two pods pending, one of them asking for more
memory than a device has).

## Known gap

`hami.mode: DRA` reads demand from unallocated `ResourceClaims` naming the
pool's DeviceClass, which is a cleaner signal than a pending Pod: a claim is
unambiguously GPU-capacity demand, where a Pod can be pending for a dozen
unrelated reasons. Both modes then apply the same second filter — a fresh cell
of this pool's shape must actually satisfy the request — so a claim asking for
more than one whole device never drives a scale-up.
