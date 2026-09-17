# Limitations

Everything here is a real, current gap — not a hedge. Where there is a
practical workaround, it is stated; where the answer is "a human has to act",
that is stated too. Each gap that is tracked links its issue, so you can see
whether it is being worked on rather than guessing.

## HAMi DRA mode is validated on one GPU only

`spec.capacity.hami.mode: DRA` is implemented: capacity, readiness, the drain
gate and the demand signal all read `resource.k8s.io` instead of HAMi's node
annotations. It needs, in the **workload** cluster:

- Kubernetes >= 1.34 with `DynamicResourceAllocation`, and the
  `DRAConsumableCapacity` gate — beta and on by default from 1.36, explicit on
  1.34 and 1.35;
- Project-HAMi's DRA driver, and `capacity.hami.deviceClassName` naming the
  DeviceClass it ships (the webhook requires the field in DRA mode);
- CDI in the cell's container runtime, which the reference cell image bakes.

Each of those failures is reported separately rather than as "no capacity",
because they imitate each other: no `resource.k8s.io` looks like no slices, a
missing DeviceClass looks like nothing can claim, and a device published with no
`capacity` — what a missing `DRAConsumableCapacity` gate produces, since the
apiserver drops gated fields rather than rejecting them — looks exactly like a
GPU with nothing left on it.

What is thin is the evidence, not the code: it is exercised against the shapes
Project-HAMi's driver publishes, and hardware-validated on a single-GPU node.
Multi-GPU and multi-model DRA pools are untested (see
[#5](https://github.com/kubeswift-io/gpucellpool/issues/5) and
[#49](https://github.com/kubeswift-io/gpucellpool/issues/49)). `DevicePlugin`
remains the default.

## No automated outer-drain sequencing

Tracked as [#3](https://github.com/kubeswift-io/gpucellpool/issues/3).

A cell is a VFIO guest, so KubeSwift can only move it with an *offline*
migration — a VM restart (`docs/concepts.md` — "Cells are cattle"). Every
cell is therefore pinned with `migration.enabled: false`. If the
infrastructure node holding a cell is cordoned or drained, the pool surfaces
`CellDrainRequested` and **does nothing further** — it does not cordon the
inner Node, drain inner workloads, delete the cell, or recreate it elsewhere.
**A human has to act**: drain the cell's workload Node, delete the cell, and
let the pool recreate it (on a different infrastructure node, if the original
one stays cordoned).

## Bootstrap token minting is not implemented

`spec.bootstrap.provider` accepts only `Opaque` — user-supplied cloud-init with
a closed substitution set. A `KubeadmToken` provider that would mint TTL'd join
tokens per cell (removing the token-expiry footgun for kubeadm-style clusters)
was designed but never implemented, and the value was **removed from the API
enum** rather than left accepted-and-ignored — an earlier iteration did exactly
that, and it failed silently: the object passed admission, then the controller
errored about a provider the user never actually selected. If your bootstrap
token expires, new cells fail at `Joining` with `JoinTimeout`
(`docs/runbook.md` has the diagnostic).

An expired credential is also the one futile-rebuild case still unguarded. A pool
that has **never** worked stops creating after three failures, and a cell that
joins and never advertises a GPU is not rebuilt when nothing advertises one
pool-wide (`FaultNotInTheCell`). Neither covers a pool that *was* working and
whose token then expires: the Node never registers, so each new cell is rebuilt up
to five times per index before the index gives up. Unlike the capacity case there
is no authoritative signal — the operator cannot validate a distribution-specific
token without trying it — so the fix is a heuristic on consecutive join timeouts
and is tracked in issue #17 rather than guessed at.

## Pools of two or more cells: harness-only

Tracked as [#5](https://github.com/kubeswift-io/gpucellpool/issues/5).

The reference lab has exactly one GPU. Everything about a *single* cell —
provisioning, boot, join, HAMi accounting, autoscale-to-one, teardown — is
hardware-validated. Behaviour specific to **multiple concurrent cells** —
membership planning with more than one cell in flight, per-index replacement
backoff at scale, cross-cell scheduling spread under real contention — is
covered by the two-envtest test harness (a real apiserver as the outer
cluster, a second as the workload cluster) but has not been run against real
hardware with two or more physical GPUs.

The same caveat applies to `spec.deletion.policy: Force` (skips drain
entirely — validated in the harness, not against a hardware cell with live
HAMi workloads on it) and to cell replacement backoff under repeated real
failures.

## One GPU per cell

`spec.cell.gpu.count` accepts only `1`. Multi-GPU cells (NVLink-connected
GPUs behind one VM) need PCIe/NUMA topology work this project has not done;
there is no roadmap item committed for it.

## `resourceClaimName` is single-cell-only

`spec.cell.gpu.dra.resourceClaimName` references one pre-created, shared
`ResourceClaim`. A VFIO device backs exactly one running VM, so pointing more
than one cell at the same claim double-books the device. This is only ever
correct for a pool whose `replicas` (and `autoscaling.maxReplicas`, if set)
is 1 — use `resourceClaimTemplateName` for anything larger.

## What is *not* a limitation, stated for clarity

Replacing cells after a `spec.cell` change is **implemented**: the `Updated`
condition reports drift, and `updatePolicy.type: RollingUpdate` replaces stale
cells one at a time behind the drain gate. See `docs/updates.md`. It is opt-in
because a template edit is not consent to destroy running work.

- Scale-up and scale-down are both implemented (`docs/autoscaling.md`) — the
  earlier design draft that called scale-down "postponed" is stale; ignore
  any doc under `docs/design/` that still says so (they carry a banner).
- `provisioner: ClusterAPI` is shipped and hardware-validated, not "later" —
  see `docs/clusterapi-cells.md`.
