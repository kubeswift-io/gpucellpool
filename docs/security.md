# Security

## The launcher pod is a node-level trust boundary

Every KubeSwift launcher pod runs `privileged: true` **by design** — it is
documented KubeSwift behaviour, not an oversight, and not something this
project works around (KubeSwift tried a capability-scoped launcher and reverted
it; see KubeSwift's own security-audit doc). A cell's launcher pod is no
exception.

The consequence for this operator: **the right to create a `GPUCellPool` in a
namespace is node-root-equivalent authority in the infrastructure cluster.**
Anything a `GPUCellPool` spec can make a cell's launcher pod mount, request, or
run is effectively node-level access on whichever KubeSwift node schedules it.
This is why `spec.cell.guestTemplate` is not a free-form passthrough — see the
webhook, below — and why standard Kubernetes namespace isolation is **not**
sufficient to isolate `GPUCellPool` creators from each other or from the rest
of the infrastructure cluster. Restrict who may create `GPUCellPool` objects
the same way you would restrict who may create a privileged Pod directly (an
admission policy, a separate namespace with tighter RBAC, or simply: only
infrastructure administrators).

## Layered isolation, not tenant isolation

State the isolation claim precisely (see `docs/concepts.md`): a cell gives a
group of workloads a VM boundary around a physical GPU, and HAMi gives sharing
efficiency inside that boundary. That is a real, useful property — a stronger
boundary between groups of workloads than GPU-sharing alone provides — but it
is not absolute tenant isolation, and it does not change who is trusted to
*create* cells in the first place. Do not describe GPUCellPool as providing
multi-tenant security; describe it as providing layered isolation between
workload groups that already trust the same infrastructure operators.

## The validating webhook is a security control

The webhook (`internal/webhook/v1alpha1`, on by default,
`failurePolicy: fail`) enforces the `guestTemplate` field contract (see
`docs/api-reference.md`): a set of fields a user is not permitted to set
because the operator owns them, and a set that is denied outright because it
is incompatible with a GPU cell. This denylist is not a convenience check — it
is what stops a `GPUCellPool` spec from asking the launcher pod for something
that should be refused. **That is why the webhook fails closed
(`failurePolicy: fail`)**: if the webhook cannot be reached, admission is
rejected rather than silently allowed through.

Running with the webhook disabled (`webhook.enabled: false` in the Helm
values) removes this control. The chart's `NOTES.txt` warns about this
explicitly on install. Do not disable it in a cluster where `GPUCellPool`
creation is not already restricted to fully-trusted operators.

The same rules are enforced a second time at render time
(`internal/provisioner.ValidateTemplate`/`ValidateGPU`), so a spec that slipped
past a disabled or unreachable webhook — an object created before the webhook
was installed, for instance — still cannot misconfigure a cell when the
controller renders it.

## Two RBAC scopes

GPUCellPool touches two clusters with two different credentials, and neither
needs broad access:

**Outer (infrastructure) cluster** — the operator's own ServiceAccount
(`config/rbac/role.yaml`). Notably **absent**: `pods`, `nodes`, and any write
verb on `resourceclaims`/`swiftgpuprofiles`. The operator never allocates a
GPU itself — KubeSwift and the Kubernetes scheduler do that. It creates and
watches `SwiftGuest`/`SwiftSeedProfile` (and, when `provisioner: ClusterAPI`,
`Machine`/`KubeSwiftMachine`), and reads GPU inventory (`resourceslices`,
`resourceclaims`, `resourceclaimtemplates`, `deviceclasses`) read-only.

**Inner (workload) cluster** — the credential in
`spec.workloadCluster.kubeconfigSecretRef`, scoped by
`config/rbac/workload-cluster-observer.yaml`. `cluster-admin` is never
required or expected; hand over the `ServiceAccount` token this manifest
creates, not an admin kubeconfig. The grants:

| Resource | Verbs | Consumer |
|---|---|---|
| `nodes` | `get, list, watch, patch, update, delete` | correlation, readiness, applying identity labels/taints, cordoning, and — `delete` is **not optional and not a drain-only feature** — reaping a stale Node left by a replaced cell, and removing a cell's Node when the cell itself is deleted. Both are always-on paths; without `delete`, every teardown fails `Forbidden` and stale Nodes accumulate |
| `pods` | `get, list, watch` | HAMi `DevicePlugin`-mode accounting (allocations live on Pod annotations) |
| `resourceslices`, `resourceclaims` | `get, list, watch` | HAMi `DRA`-mode accounting |

Nothing else is granted. In particular the operator does **not** hold
`pods/eviction`: it never evicts a workload to force a cell empty — it waits
for the capacity provider to report the cell idle, and if you need a cell
emptied faster, drain it yourself with `kubectl drain`.

If a pool reports `Forbidden` on the `WorkloadClusterReachable` condition, the
credential Secret is missing one of the grants above — see the runbook entry
in `docs/runbook.md`.
