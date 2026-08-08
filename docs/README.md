# Documentation index

## Using gpucellpool

Start here if you are installing or operating a pool.

| Doc | Read it for |
|---|---|
| [quickstart](quickstart.md) | the shortest path from nothing to two workloads sharing one GPU in a cell |
| [concepts](concepts.md) | the two-layer model — cells, the two identities, the two capacities, layered isolation |
| [api-reference](api-reference.md) | the full `GPUCellPool` v1alpha1 spec/status, generated from the Go types and the validating webhook |
| [autoscaling](autoscaling.md) | `spec.autoscaling` — both directions, the safety gates, the remembered cell shape |
| [networking](networking.md) | the routable-interface requirement, `nodeIPFrom`, NADs, DNS, `port-forward` |
| [security](security.md) | why creating a pool is node-root-equivalent authority, the webhook, the two RBAC scopes |
| [cell-image](cell-image.md) | building and publishing a cell image with `hack/build-cell-image.sh` |
| [clusterapi-cells](clusterapi-cells.md) | `provisioner: ClusterAPI` — cells as Cluster API Machines |
| [limitations](limitations.md) | what is not implemented, not validated on hardware, or deliberately manual |
| [runbook](runbook.md) | what to check when a pool is not doing what you expect |

`config/samples/` has ready-to-apply manifests: a `SwiftGuest`-provisioned pool,
a `ClusterAPI`-provisioned pool, a join-Secret template (k0s + kubeadm), and a
NAD.

## Design and rationale (historical)

`docs/design/*.md` is the design record: why the API and controller look the
way they do. It predates and partially postdates implementation — each file
carries a banner saying so. Read it when you want the reasoning behind a
decision, not the current behaviour; for current behaviour, use the docs above.

| Doc | Contents |
|---|---|
| [overview](design/gpucellpool-overview.md) | the architectural invariant, decisions D1–D10, scope, phases |
| [api](design/gpucellpool-api.md) | the original API design (superseded by `api-reference.md` for current fields) |
| [reconciliation](design/gpucellpool-reconciliation.md) | the cell state machine, identity model, RBAC, deletion sequencing |
| [bootstrap](design/gpucellpool-bootstrap.md) | why prebaked-image + thin cloud-init, the measured startup budget |
| [capacity](design/gpucellpool-capacity.md) | how HAMi capacity is read, both accounting modes |
| [failure-model](design/gpucellpool-failure-model.md) | the failure scenario matrix and churn control |
| [validation-record](design/gpucellpool-validation-record.md) | the hardware proof: Phase 1–2 results, what was measured, what broke |
| [clusterapi-cells (validation)](design/clusterapi-cells-validation.md) | the CAPI provisioner post-mortem: two bugs only a live cluster found, the dev recipe |
