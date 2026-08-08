# Cells as Cluster API Machines

`spec.cell.provisioner: ClusterAPI` makes each cell a Cluster API `Machine` +
`KubeSwiftMachine`, instead of a bare `SwiftGuest`. Use it when the workload
cluster your cells join is **already Cluster-API-managed** and you want the
GPU cells to be first-class members of it — visible as Machines, carrying a
`providerID`, participating in the cluster's own node lifecycle — rather than
nodes that joined out of band.

It is an **additional** provisioner, never a replacement for the default
(`SwiftGuest`): Cluster API can only add Machines to a cluster it already
manages, and the common case for this operator is bring-your-own workload
cluster, which is not CAPI-managed at all. If your workload cluster is not
CAPI-managed, use the default `SwiftGuest` provisioner instead — see
`docs/quickstart.md`.

Validated end to end on hardware (dev/boba, GTX 1080): pool created to cell
`Ready` in 6m29s, two workloads sharing the cell's GPU through HAMi, clean
teardown. The post-mortem — two bugs a live cluster found that the test
harness could not, plus a step-by-step recipe for standing up a CAPI-managed
workload cluster on a lab with no cross-node L2 — is in
`docs/design/clusterapi-cells-validation.md`; this page is the how-to.

## Prerequisites

In addition to everything in `docs/quickstart.md`'s prerequisite table:

| | |
|---|---|
| Cluster API | `>= v1.13.4` — the validated floor. (An earlier draft of this project's sample referenced `>= v1.11`; `v1.13.4` is the version this was actually proven against and is the one to target.) |
| `cluster-api-provider-kubeswift` | `>= v0.2.0` — the release that ships `KubeSwiftMachine.spec.backend.swiftGuest.gpu` and `nodeName` placement. Earlier versions have no GPU surface at all |
| a CAPI `Cluster` | already provisioned, in the pool's own namespace (a pool never crosses namespaces) |
| a bootstrap config template (optional) | e.g. a `KubeadmConfigTemplate`, if you want the workload cluster's own bootstrap provider to mint join credentials per cell instead of maintaining `spec.bootstrap` yourself |

## Why one Machine per cell, not a MachineDeployment

A cell's identity model (`docs/concepts.md`) depends on the cell name, the
guest hostname, and the workload Node name being the *same string*. A
`MachineDeployment` generates its own Machine names, and `capi-kubeswift`
derives the guest hostname — and therefore the Node name — from the Machine
name. Random names would break that identity and leave no way to drain or
replace one specific cell. So GPUCellPool creates exactly one `Machine` +
`KubeSwiftMachine` per cell, named `<pool>-<index>` like every other cell
object, and reconciles them the way it reconciles `SwiftGuest`s under the
default provisioner.

## What a `KubeSwiftMachine` can express

A `KubeSwiftMachine` exposes a curated subset of `SwiftGuestSpec`: image,
guest class, up to two network interfaces, and GPU. Under
`provisioner: ClusterAPI`, `guestTemplate` is therefore restricted to
`imageRef`, `guestClassRef`, `interfaces` — anything else (data disks,
storage class, topology spread constraints) is **rejected at admission**
rather than silently dropped, because a cell that boots with less than you
asked for is worse than a pool that refuses to be created. See
`docs/api-reference.md` for the exact validation rule.

## Apply

```yaml
apiVersion: cells.kubeswift.io/v1alpha1
kind: GPUCellPool
metadata:
  name: inference
  namespace: gpu-cells
spec:
  replicas: 2
  cell:
    provisioner: ClusterAPI
    clusterAPI:
      clusterName: inference        # the CAPI Cluster in this namespace
      version: v1.33.3
      bootstrapConfigTemplateRef:    # optional — omit to use spec.bootstrap instead
        apiGroup: bootstrap.cluster.x-k8s.io
        kind: KubeadmConfigTemplate
        name: gpu-workers
    guestTemplate:
      guestClassRef: {name: gpu-worker-32c-128g}
      imageRef: {name: gpu-worker-noble-580}
      interfaces:
        - {name: mgmt, primary: true}
        - {name: node, networkRef: {name: cell-udn}}
    nodeIPFrom: node
    gpu:
      backend: DRA
      dra: {resourceClaimTemplateName: single-vfio-gpu}
  workloadCluster:
    kubeconfigSecretRef: {name: inference-kubeconfig}
    node:
      labels: {gpu: "on"}
```

The full, heavily-commented sample is at
`config/samples/cells_v1alpha1_gpucellpool_clusterapi.yaml`.

When `bootstrapConfigTemplateRef` is set, do **not** also set
`spec.bootstrap.joinSecretRef` — the workload cluster's own bootstrap provider
supplies the join data, instantiated once per cell the way a `MachineSet`
does, and the webhook rejects the redundant Secret reference rather than
silently ignoring it. Omit `bootstrapConfigTemplateRef` and the pool's own
rendered Secret is handed to the Machine as `bootstrap.dataSecretName` instead
— that works, but the join data (token validity, CA rotation) is then yours
to keep working, same as under the `SwiftGuest` provisioner.

## Watching and diagnosing

The FSM and `status.cells[]` behave identically to the `SwiftGuest`
provisioner — see `docs/api-reference.md` and `docs/runbook.md`. Two
CAPI-specific entries worth knowing:

- Cells are discovered by listing **Machines** labelled
  `cells.kubeswift.io/pool=<pool>`, not SwiftGuests. If the pool reports no
  cells despite Machines existing, the label did not survive — see the
  runbook.
- A cell only leaves `AllocatingGPU` once `capi-kubeswift` has provisioned the
  backing `SwiftGuest` and stamped a `providerID` on the `KubeSwiftMachine` —
  the GPU is found by following that reference. An empty `providerID` with the
  `Machine` still `Provisioning` is normal; a `Provisioned` `Machine` with no
  `providerID` is a `capi-kubeswift` problem, not a pool problem.
