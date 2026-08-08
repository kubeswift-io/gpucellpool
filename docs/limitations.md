# Limitations

Everything here is a real, current gap — not a hedge. Where there is a
practical workaround, it is stated; where the answer is "a human has to act",
that is stated too.

## HAMi DRA mode is unimplemented

`spec.capacity.hami.mode: DRA` is accepted by the API but
`internal/capacity`'s DRA path returns `ErrUnsupported` — capacity reads,
demand signal (`docs/autoscaling.md`), and readiness all fail loudly rather
than silently reading zero. **`DevicePlugin` mode is the only implementation.**
Use it (the default) even if your HAMi install also has DRA mode available.

## No rolling update on `guestTemplate` change

Changing `spec.cell.guestTemplate` (a new `imageRef`, a driver bump, a
different `guestClassRef`) bumps the per-cell template-hash annotation but
**does not roll existing cells**. `status.conditions` will not tell you a
template drifted either — there is no `Updated` condition in v1alpha1.

This is the most likely real operational task you will hit: **updating the
NVIDIA driver (or anything else) in the cell image means manually recreating
every cell**, one at a time, and doing the *inner* drain yourself first —
cordon and drain the cell's workload Node before deleting the cell's
`SwiftGuest`/`Machine`, or you destroy running HAMi workloads. See
`docs/runbook.md` for the sequence.

## No automated outer-drain sequencing

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

## Pools of two or more cells: harness-only

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

- Scale-up and scale-down are both implemented (`docs/autoscaling.md`) — the
  earlier design draft that called scale-down "postponed" is stale; ignore
  any doc under `docs/design/` that still says so (they carry a banner).
- `provisioner: ClusterAPI` is shipped and hardware-validated, not "later" —
  see `docs/clusterapi-cells.md`.
