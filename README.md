# gpucellpool

A Kubernetes operator that turns physical GPUs into pools of **VM-isolated,
fractionally-shared GPU worker nodes**.

It composes two projects and modifies neither:

- **[KubeSwift](https://github.com/kubeswift-io/kubeswift)** runs the VM and passes a
  whole physical GPU into it (VFIO).
- **[HAMi](https://github.com/Project-HAMi/HAMi)** shares that GPU between workloads
  inside the VM's Kubernetes cluster.

```
physical GPU ──VFIO──► SwiftGuest VM ──joins──► workload cluster node
                                                     │
                                                   HAMi
                                          ┌──────────┼──────────┐
                                       pod A      pod B      pod C
                                       4Gi/30%   8Gi/50%    2Gi/20%
```

One `GPUCellPool` declares N such cells. KubeSwift owns the physical GPU → VM
boundary; HAMi owns GPU → workload allocation; this operator owns the lifecycle
between them. A HAMi fraction is never handed to VFIO — the two layers nest, they
do not translate.

## Status

**v0.1.0 — alpha.** Every capability has been run on real hardware: one physical GPU
passed into a VM, the VM joined as a worker node, HAMi shared that GPU between
workloads, and the pool scaled up and down on demand. Both cell provisioners work —
`SwiftGuest` directly, or a Cluster API `Machine` for a CAPI-managed workload cluster
(`docs/clusterapi-cells.md`).

Alpha because the API is `v1alpha1` and one GPU is one GPU: pools of two or more cells
are covered by a two-apiserver test harness, not by hardware. HAMi's DRA mode is not
implemented.

## Example

```yaml
apiVersion: cells.kubeswift.io/v1alpha1
kind: GPUCellPool
metadata:
  name: inference
spec:
  replicas: 2
  cell:
    guestTemplate:                        # a verbatim KubeSwift SwiftGuestSpec
      guestClassRef: {name: gpu-worker-32c-128g}
      imageRef:      {name: gpu-worker-noble-570}
    gpu:
      backend: DRA
      dra:
        resourceClaimTemplateName: single-vfio-gpu
  bootstrap:
    joinSecretRef: {name: inference-cluster-join}
  workloadCluster:
    kubeconfigSecretRef: {name: inference-cluster-kubeconfig}
    node:
      labels: {gpu: "on"}                 # HAMi's own gate, declared by you
```

## Install

```bash
helm install gpucellpool oci://ghcr.io/kubeswift-io/charts/gpucellpool \
  --namespace gpucellpool-system --create-namespace
```

`helm upgrade` never updates files in `crds/`, so after a chart upgrade that
changes the API:

```bash
kubectl apply -f charts/gpucellpool/crds/
```

The validating webhook is on by default and the chart issues its own certificate
(set `webhook.certManager.enabled=true` to use cert-manager instead). Leaving the
webhook off means a pool spec can ask for things that should be rejected — the
`guestTemplate` denylist is a security control, because a cell's launcher pod is
privileged in the infrastructure cluster.

## Requirements

| | |
|---|---|
| infrastructure cluster | KubeSwift ≥ v0.13.4, a GPU node (`kubeswift.io/gpu-node=true`), a `DeviceClass` for VFIO GPUs |
| workload cluster | HAMi installed, reachable from the operator, and reachable **both ways** for kubelet (cells need a routable interface, not just egress) |
| cell image | a `SwiftImage` with the NVIDIA driver, containerd + CDI, and your distribution's node binaries |

## Documentation

| Doc | Contents |
|---|---|
| [overview](docs/design/gpucellpool-overview.md) | architecture, decisions, scope, phases |
| [api](docs/design/gpucellpool-api.md) | the v1alpha1 CRD and its validation rules |
| [reconciliation](docs/design/gpucellpool-reconciliation.md) | cell state machine, identity, RBAC, deletion |
| [bootstrap](docs/design/gpucellpool-bootstrap.md) | cell image strategy, join credentials |
| [capacity](docs/design/gpucellpool-capacity.md) | how HAMi capacity is read |
| [failure-model](docs/design/gpucellpool-failure-model.md) | what breaks and what the operator does about it |
| [poc](docs/design/gpucellpool-poc.md) | hardware proof and test strategy |
| [runbook](docs/runbook.md) | what to check when a pool misbehaves |

## Licence

Apache-2.0. KubeSwift is AGPL-3.0 and is reached only through the Kubernetes API
with unstructured clients — no KubeSwift Go package is imported. See `NOTICE`.
