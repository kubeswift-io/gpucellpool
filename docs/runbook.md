# gpucellpool — operator runbook

> What to look at when a pool is not doing what you expect. Every entry here comes
> from something that actually happened on hardware, not from imagination.

---

## First, localise the layer

The conditions are split so that one `False` tells you where to look. Read them in
this order:

```bash
kubectl get cellpool <name> -n <ns> -o jsonpath='{range .status.conditions[*]}{.type}={.status} ({.reason}){"\n"}{end}'
```

| False condition | The fault is in |
|---|---|
| `WorkloadClusterReachable` | the credential or the network between the operator and the workload cluster — **nothing below it is trustworthy** |
| `CapacityProviderReady` | HAMi in the workload cluster |
| `PhysicalGPUsAvailable` | the infrastructure cluster's GPU inventory |
| `CapacityAvailable` | the workload cluster's GPU is full, or unreadable |
| `ScalingActive` | demand could not be read (only present when autoscaling is on) |
| `Ready` alone | individual cells — read `status.cells[]` |

Per-cell detail, which names the layer and the reason:

```bash
kubectl get cellpool <name> -n <ns> \
  -o jsonpath='{range .status.cells[*]}{.name} {.phase} | {.message}{"\n"}{end}'
```

---

## Symptoms

### A cell sits in `AllocatingGPU`

No GPU has been assigned yet. Check the infrastructure cluster:

```bash
kubectl get resourceslices          # driver gpu.kubeswift.io — is the device published?
kubectl get swiftgpunodes           # vfioReady, free devices
kubectl describe pod <cell> -n <ns> # launcher pod: PodScheduled reason
```

If `PhysicalGPUsAvailable=False (InsufficientPhysicalGPU)`, every device is taken —
the pool is not broken, the cluster is full. The pool deliberately does **not** queue
guests that could never be scheduled.

### A cell sits in `Booting`

The VM runs but has no address yet. If it persists, the usual cause is
`cell.nodeIPFrom` naming an interface the guest does not have:

```bash
kubectl get swiftguest <cell> -n <ns> -o jsonpath='{.status.network}'
```

Note that KubeSwift v0.13.4 reports a secondary NAD interface's **MAC but not its
IP**. That is expected and does not block the cell — the operator falls back to the
primary address for readiness, because the guest derives its own node IP.

### A cell sits in `Joining`

The VM is up; its Node has not registered Ready. In order:

```bash
swiftctl ssh <cell> -n <ns> -- 'cloud-init status; sudo journalctl -u gpu-cell-join --no-pager | tail'
swiftctl console <cell> -n <ns>          # if ssh is not up yet
```

Common causes, in the order they actually occur:

1. **The join token expired.** If *every* new cell fails to join while existing
   cells stay healthy, the pool says so: reason `BootstrapCredentialSuspect`.
2. **The guest lost DNS.** Measured: once the inner CNI programs the node,
   `systemd-resolved`'s stub stops resolving through the pod-netns dnsmasq while raw
   egress still works, so image pulls and `apt` fail. The cell image must pin an
   upstream resolver.
3. **No route to the apiserver.** A cell needs a **routable** interface, not just
   egress — see "Networking" below.

### A cell sits in `AwaitingGPUCapacity`

The Node is Ready but HAMi is not advertising the expected GPU. This is the state the
pool exists to make visible; it is usually one of four things:

```bash
# in the WORKLOAD cluster
kubectl get node <cell> -o jsonpath='{.metadata.annotations.hami\.io/node-nvidia-register}'
kubectl get node <cell> -o jsonpath='{.metadata.labels}' | tr ',' '\n' | grep gpu
kubectl -n kube-system get pods -o wide | grep hami
# in the GUEST
swiftctl ssh <cell> -n <ns> -- 'nvidia-smi -L; cat /run/gpu-cell-preflight'
```

1. HAMi's device plugin has not started yet — normal for a minute or two after join.
2. The node lacks HAMi's gate label (`gpu=on`). Declare it in
   `spec.workloadCluster.node.labels`; the operator applies labels it does not
   interpret.
3. HAMi's DaemonSets do not tolerate your pool's taints, so the plugin never lands.
4. The in-guest driver is missing or broken — `nvidia-smi -L` in the guest is the
   fastest discriminator.

Do **not** expect the pool to call the cell Ready meanwhile. A running VM whose GPU
never surfaced is exactly the failure this design refuses to paper over.

### `allocatable nvidia.com/gpu` says 10 and I have one GPU

That is HAMi's `deviceSplitCount` inflation, not a bug and not a device count. The
device count comes from the `hami.io/node-nvidia-register` annotation, which is what
the operator reads.

### The pool is stuck `Terminating`

A cell is `Draining` with workloads still holding its GPU. That is deliberate:

```bash
kubectl get cellpool <name> -n <ns> -o jsonpath='{range .status.cells[*]}{.name} {.phase} {.message}{"\n"}{end}'
```

Either drain those workloads, or set `deletion.policy: Force` — which **destroys
running work**, and is why it is not the default.

### The pool will not scale up

`ScalingActive`'s reason is the answer:

| Reason | Meaning |
|---|---|
| `DemandUnsatisfiable` | pods are pending, but none fits one cell of this pool's shape (two devices, or more memory/compute than one device has). Another cell would change nothing |
| `CellShapeUnknown` | the pool has never advertised a device, so it cannot tell whether a fresh cell would help. Set `replicas` or `minReplicas` to 1 once; the shape is then remembered in `status.cellDeviceShape` and survives a later scale to zero |
| `Stabilizing` | a cell was just created; demand does not clear until it is Ready |
| `AtMaxReplicas` | raise `maxReplicas` |
| `InsufficientPhysicalGPU` | no free device in the infrastructure cluster |
| `Unknown` | demand could not be read — the pool refuses to act on a guess |

A pod pending for a non-capacity reason (missing ConfigMap, unpullable image) will
never trigger a scale-up: it has already been scheduled, so it is not demand.

### The pool will not scale down

`ScalingActive`'s reason again:

| Reason | Meaning |
|---|---|
| `AtMinReplicas` | it is already at the floor. `minReplicas: 0` is allowed and safe to use — the pool remembers its cell shape, so it can grow back |
| `NoIdleCell` | demand is gone but every cell still holds workloads. Not a fault; check `status.workloadCapacity.gpuMemory.allocated` |
| `Stabilizing` | demand has not been absent for the whole window yet (default 30m, and `status.demandFreeSince` says since when), or a scale action happened inside it |
| `Unknown` | demand or allocations could not be read. A cell whose allocations cannot be read is never treated as idle — "empty" and "unknown" are different answers |

Scale-down never removes a cell that holds work: idleness comes from the capacity
provider, and the drain gate re-checks allocations again before the guest is deleted.
`scaleDown: Auto` requires `minReplicas` to be set explicitly.

### Capacity numbers look stale

They are, and deliberately: a failed read retains the previous values rather than
reporting zero. Check `status.workloadCapacity.lastObserved` and
`WorkloadClusterReachable`.

---

## Networking: the thing that bites

A Kubernetes node must be **dialable by its own apiserver** (logs, exec,
port-forward, metrics). Two measured consequences:

- KubeSwift's `br0` lives inside **each launcher pod's** network namespace. Two
  guests on the *same host* both get `192.168.99.10` in separate namespaces and
  cannot reach each other. There is no same-node shortcut.
- A cell therefore needs a `networkRef` NAD carrying a routable address, and
  `cell.nodeIPFrom` should name it.

Also: `kubectl port-forward` **cannot** reach a KubeSwift nat-exposed VM. It dials
localhost inside the pod netns, while the DNAT maps podIP→VM. To reach a VM-hosted
apiserver from outside, expose it as a Service, or put the client on the same NAD.

---

## Private registries

Two separate credentials, easy to conflate:

| What | Where |
|---|---|
| the operator image | `imagePullSecrets` in the chart (SA-level; note pull secrets are injected at pod **create**, so an existing pod must be replaced) |
| a cell's `SwiftImage` from an OCI artifact | `spec.source.oci.credentialsSecretRef` on the SwiftImage |

If an import fails with `401 unauthorized`, it is the second one. Recreate the
SwiftImage rather than deleting its import Job — deleting an in-flight Job leaves
the image stuck in `Importing` with nothing to recreate it.

---

## Useful one-liners

```bash
# Both capacities, side by side
kubectl get cellpool <name> -n <ns> \
  -o jsonpath='physical={.status.physicalCapacity}{"\n"}workload={.status.workloadCapacity}{"\n"}'

# Which host GPU does each cell hold?
kubectl get cellpool <name> -n <ns> \
  -o jsonpath='{range .status.cells[*]}{.name} {.hostNode} {.devices}{"\n"}{end}'

# The rendered cloud-init for one cell (contains a join credential — treat with care)
kubectl get secret <cell>-bootstrap -n <ns> -o jsonpath='{.data.user-data}' | base64 -d
```
