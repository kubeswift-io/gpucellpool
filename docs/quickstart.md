# Quickstart

The shortest path from nothing to two workloads sharing one GPU inside a cell.
Budget **~5 minutes** for the first cell to boot and join (measured: 4m45s
guest-created to Ready, most of it cloning the root disk — see
`docs/design/gpucellpool-bootstrap.md` §6) plus a couple of minutes to install
the chart and stage prerequisites.

Two clusters are involved throughout: the **infrastructure** cluster (runs
KubeSwift and this operator) and the **workload** cluster (runs HAMi and
receives cells as worker nodes). They can be the same cluster in a lab, but
keep the distinction straight — every command below says which one it targets.

## Prerequisites

You must supply all of these before applying a pool. None of them is created
by GPUCellPool.

**In the infrastructure cluster:**

| | |
|---|---|
| KubeSwift | `>= v0.13.4`, installed and healthy |
| a GPU node | labelled `kubeswift.io/gpu-node=true`, with a physical GPU KubeSwift can pass through |
| a `DeviceClass` + `ResourceClaimTemplate` for the VFIO GPU | do not re-derive these — apply KubeSwift's own samples at `config/samples/dra-gpu/` in the KubeSwift repo (`resourceclaimtemplate-single-gpu.yaml` creates the `single-vfio-gpu` template this quickstart references) |
| Multus + a `NetworkAttachmentDefinition` carrying a **routable** address | not optional — see `docs/networking.md`. A minimal sample is at `config/samples/network-attachment-definition.yaml` |
| a `SwiftGuestClass` | CPU/memory/disk shape for the cell VM |
| a cell `SwiftImage` | a disk image with the NVIDIA driver, containerd + CDI, and your distribution's node binaries baked in — see `docs/cell-image.md` to build one |

**In the workload cluster:**

| | |
|---|---|
| a Kubernetes cluster | any distribution (k0s, kubeadm, RKE2, k3s all work — `spec.bootstrap.provider: Opaque` makes no assumption) |
| HAMi | already installed, with its DaemonSets able to tolerate whatever taints you plan to put on cell nodes |
| network path | the workload apiserver must be able to reach a cell's kubelet — this is the same routable-NAD requirement above, from the other direction |

**Two Secrets**, staged before you apply the pool (both created below).

## 1. Install the chart

In the **infrastructure** cluster:

```bash
helm install gpucellpool oci://ghcr.io/kubeswift-io/charts/gpucellpool \
  --version 0.1.0 \
  --namespace gpucellpool-system --create-namespace
```

The validating webhook installs on by default — leave it on (see
`docs/security.md`).

## 2. Give the operator a credential for the workload cluster

Apply the RBAC bundle **in the workload cluster** — it creates a
`ServiceAccount` scoped to exactly what the operator needs, no
`cluster-admin`:

```bash
kubectl --context workload apply -f config/rbac/workload-cluster-observer.yaml
```

Build a kubeconfig from that ServiceAccount's token and store it as a Secret
**in the infrastructure cluster**, in the namespace where you will create the
pool:

```bash
TOKEN=$(kubectl --context workload -n kube-system create token gpu-cell-pool-observer --duration=8760h)
SERVER=$(kubectl --context workload config view --minify -o jsonpath='{.clusters[0].cluster.server}')
CA=$(kubectl --context workload config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')

cat > workload-kubeconfig.yaml <<EOF
apiVersion: v1
kind: Config
clusters:
- name: workload
  cluster: {server: ${SERVER}, certificate-authority-data: ${CA}}
contexts:
- name: workload
  context: {cluster: workload, user: gpu-cell-pool-observer}
current-context: workload
users:
- name: gpu-cell-pool-observer
  user: {token: ${TOKEN}}
EOF

kubectl -n gpu-cells create namespace gpu-cells --dry-run=client -o yaml | kubectl apply -f -
kubectl -n gpu-cells create secret generic inference-cluster-kubeconfig \
  --from-file=value=workload-kubeconfig.yaml
```

## 3. Create the join Secret

The cell's cloud-init, referenced by `spec.bootstrap.joinSecretRef`. Do not
write this by hand from scratch — start from
`config/samples/cell-join-secret.yaml`, which covers both k0s and kubeadm and
documents every trap (the closed substitution set, YAML-quoting the tokens,
deriving the node IP by subnet, the containerd v3 config schema). Copy the
`Secret` matching your distribution, fill in the `FILL IN` placeholders (an
SSH key and your cluster's join token/endpoint), rename it to
`inference-cluster-join` to match `bootstrap.joinSecretRef` above, and apply
it:

```bash
kubectl -n gpu-cells apply -f cell-join-secret-rendered.yaml
```

## 4. Apply the NAD and a one-cell pool

```bash
kubectl -n gpu-cells apply -f config/samples/network-attachment-definition.yaml
```

Start with `replicas: 1` — verify one cell end to end before scaling:

```yaml
apiVersion: cells.kubeswift.io/v1alpha1
kind: GPUCellPool
metadata:
  name: inference
  namespace: gpu-cells
spec:
  replicas: 1
  cell:
    guestTemplate:
      guestClassRef: {name: gpu-worker-32c-128g}
      imageRef: {name: gpu-worker-noble-570}
      interfaces:
        - {name: mgmt, primary: true}
        - {name: node, networkRef: {name: cell-net}}
    nodeIPFrom: node
    gpu:
      backend: DRA
      dra: {resourceClaimTemplateName: single-vfio-gpu}
  bootstrap:
    joinSecretRef: {name: inference-cluster-join}
  workloadCluster:
    kubeconfigSecretRef: {name: inference-cluster-kubeconfig}
    node:
      labels: {gpu: "on"}      # HAMi's own gate — see docs/networking.md, docs/concepts.md
```

`config/samples/cells_v1alpha1_gpucellpool.yaml` is the same shape, fully
commented, ready to copy.

## 5. Watch it come up

```bash
kubectl -n gpu-cells get cellpool inference -w
```

A cell walks through named phases, each waiting on something specific — see
`docs/api-reference.md` for the full state machine and `docs/runbook.md` for
what to check if one sticks:

| Phase | Waiting for |
|---|---|
| `AllocatingGPU` | the scheduler (DRA) or KubeSwift's controller (Native) to bind a physical GPU |
| `GuestProvisioning` | the launcher pod to start and the VM to come up |
| `Booting` | the VM's network address to be reported |
| `Joining` | a workload `Node` named `inference-0` to register `Ready` |
| `AwaitingGPUCapacity` | HAMi to advertise the expected GPU count on that Node |
| `Ready` | — all three layers agree: VM running, Node Ready, GPU advertised |

```bash
kubectl -n gpu-cells get cellpool inference \
  -o jsonpath='{range .status.cells[*]}{.name} {.phase} | {.message}{"\n"}{end}'
```

## 6. Prove the point: two workloads, one GPU, one cell

This assertion — two independent workloads sharing one physical GPU, isolated
inside one KubeSwift VM — is the entire product. In the **workload** cluster:

```yaml
apiVersion: v1
kind: Pod
metadata: {name: hami-a}
spec:
  nodeSelector: {kubernetes.io/hostname: inference-0}
  containers:
    - name: cuda
      image: nvidia/cuda:12.4.1-base-ubuntu22.04
      command: ["sleep", "3600"]
      resources:
        limits:
          nvidia.com/gpu: 1
          nvidia.com/gpumem: "3000"      # MiB
          nvidia.com/gpucores: "30"      # percent
---
apiVersion: v1
kind: Pod
metadata: {name: hami-b}
spec:
  nodeSelector: {kubernetes.io/hostname: inference-0}
  containers:
    - name: cuda
      image: nvidia/cuda:12.4.1-base-ubuntu22.04
      command: ["sleep", "3600"]
      resources:
        limits:
          nvidia.com/gpu: 1
          nvidia.com/gpumem: "3000"
          nvidia.com/gpucores: "30"
```

```bash
kubectl --context workload apply -f hami-a.yaml -f hami-b.yaml
kubectl --context workload wait --for=condition=Ready pod/hami-a pod/hami-b --timeout=120s

kubectl --context workload get pod hami-a -o jsonpath='{.metadata.annotations.hami\.io/vgpu-devices-allocated}'
kubectl --context workload get pod hami-b -o jsonpath='{.metadata.annotations.hami\.io/vgpu-devices-allocated}'
```

Both annotations name the **same GPU UUID** — two logical HAMi consumers, one
physical passthrough device, isolated inside one VM. Confirm from inside each
Pod that the limit is real, not advisory:

```bash
kubectl --context workload exec hami-a -- nvidia-smi --query-gpu=memory.total --format=csv
kubectl --context workload exec hami-b -- nvidia-smi --query-gpu=memory.total --format=csv
```

Each reports **~3000 MiB**, not the card's full memory — HAMi-core is
interposing inside the container.

```bash
kubectl -n gpu-cells get cellpool inference \
  -o jsonpath='physical={.status.physicalCapacity}{"\n"}workload={.status.workloadCapacity}{"\n"}'
```

`physicalCapacity.gpus` stays `1` throughout — the whole-device count never
changes. `workloadCapacity.gpuMemory.allocated` moves as the two Pods land and
as you delete them.

## Next

- `docs/concepts.md` — the two-layer model this all rests on
- `docs/autoscaling.md` — grow and shrink the pool on GPU demand
- `docs/runbook.md` — what to check when a phase sticks
- `docs/clusterapi-cells.md` — cells as Cluster API Machines, for a CAPI-managed
  workload cluster
