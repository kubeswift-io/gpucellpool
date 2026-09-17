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
| `Updated` | cells are running an older `spec.cell` than the pool declares — see below |
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

**Which of those two commands is authoritative depends on `cell.gpu.backend`.** The
two backends keep separate books for the same physical devices: a DRA allocation is
a `ResourceClaim`, a native one is `SwiftGPUNode.status.gpus[].allocated` /
`.allocatedTo`, and neither appears in the other. The pool counts free devices from
the ledger matching its own backend, so on a cluster with both installed, compare
against the right one:

```bash
# backend: DRA
kubectl get resourceslices; kubectl get resourceclaims -A
# backend: Native
kubectl get swiftgpunode <node> -o jsonpath='{.status.gpus[*].allocatedTo}'
```

A device held by a native allocation — another pool, or a sandbox — is invisible to
the DRA ledger and vice versa. If the condition and the ledger you are reading
disagree, check which backend the pool declares before suspecting the operator.

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

After `spec.capacity.readyTimeout` the cell goes `Failed`. What happens next depends
on whether the fault is the cell's:

- **One cell advertises nothing, others advertise fine** → the cell is replaced,
  with backoff, up to five attempts per index (`CellReplacementExhausted`).
- **No cell node advertises anything** → the pool **stops** and reports
  `Progressing=False/FaultNotInTheCell`. Causes 2–4 above are workload-cluster
  faults, so a rebuilt VM would fail identically — rebuilding would just cost a GPU
  allocation, a root-disk clone, a boot and a join per attempt, and destroy the
  evidence. The failed cell's VM is left up for you to inspect. Fix the provider
  (`CapacityProviderReady` names the cause) and the cell is replaced on the next
  pass.

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

### The pool creates Machines but no cells appear in status

Only with `provisioner: ClusterAPI`. Cells are discovered by listing **Machines**
labelled `cells.kubeswift.io/pool=<pool>`; check the label survived:

```bash
kubectl get machines -n <ns> -l cells.kubeswift.io/pool=<pool>
```

If Machines exist but stay `Pending`, Cluster API has not acted on them — look at
`spec.bootstrap` (a Machine with neither `configRef` nor `dataSecretName` stays
Pending by design) and at whether the named Cluster exists in the same namespace.

### A ClusterAPI Machine stays `Pending` and CAPI logs "already owned by another controller"

The pool must be a **co-owner** of its Machines, never the controller — Cluster API
needs that slot. If you see this, the operator predates the fix:

```bash
kubectl get machine <cell> -n <ns> -o jsonpath='{.metadata.ownerReferences}'
```

A healthy cell has two owner references: the GPUCellPool (no `controller: true`) and
the Cluster (with it).

### A ClusterAPI cell never leaves `AllocatingGPU`

The GPU is found by following the KubeSwiftMachine's `providerID` to the backing
SwiftGuest, so it only becomes visible once capi-kubeswift has provisioned the VM:

```bash
kubectl get kubeswiftmachine <cell> -n <ns> -o jsonpath='{.spec.providerID}'
kubectl get machine <cell> -n <ns> -o jsonpath='{.status.phase}'
```

An empty providerID with the Machine in `Provisioning` means the provider is still
working. A `Provisioned` Machine with no providerID is a capi-kubeswift problem, not
a pool problem.

### `Updated=False` — cells are running an older template

Expected after any `spec.cell` edit. The reason says what will happen next:

| Reason | Meaning |
|---|---|
| `TemplateChanged` | drift detected, and `updatePolicy.type` is `Manual` — nothing will be replaced. Replace cells yourself, or switch to `RollingUpdate`. See `docs/updates.md` |
| `RollingUpdate` | a cell is being replaced right now |
| `UpdateBlocked` | a rolling update is wanted but cannot proceed. The message names the cause: every stale cell still holds workloads, the pool is mid-resize, another cell is already being replaced, or the workload cluster is unreachable |

`status.cells[].templateHash` tells you which cells are stale. An empty hash
counts as current — it predates the field, and treating unknown as out-of-date
would replace a healthy pool.

A rolling update that is blocked on busy cells stays blocked indefinitely, by
design; it will not evict anything. `kubectl drain <cell>` in the workload
cluster releases the GPU and the rollout proceeds on its next pass.

### Capacity numbers look stale

They are, and deliberately: a failed read retains the previous values rather than
reporting zero. Check `status.workloadCapacity.lastObserved` and
`WorkloadClusterReachable`.

### `Forbidden` when a cell's Node should have been removed

`WorkloadClusterReachable=False Forbidden`, or a stale Node that never gets
reaped. The credential in `spec.workloadCluster.kubeconfigSecretRef` is
missing `delete` on `nodes` in the workload cluster. This is not a
drain-only right — reaping a stale Node left by a replaced cell, and removing
a cell's own Node when the cell is deleted, are both always-on paths
(`docs/security.md`). Reapply the current
`config/rbac/workload-cluster-observer.yaml` (it grants
`nodes: [get, list, watch, patch, update, delete]`) and mint a fresh token if
the credential Secret was built from an older ClusterRole.

### A cell sits in `Joining` forever after being replaced

`waiting for the workload Node to register`, indefinitely, while the VM is
healthy and reachable — and the workload cluster shows **no Node** for the cell
even though its CSRs were approved. The kubelet logs
`Error updating node status, will retry` and
`Failed to get node when trying to set owner ref to the node lease`.

Its Node object was deleted from under a running kubelet, which then does not
re-register. Recover by restarting the kubelet inside the cell:

```bash
# k0s
ssh <cell> sudo systemctl restart k0sworker
# kubeadm
ssh <cell> sudo systemctl restart kubelet
```

This was a bug (#14, fixed): a Node carrying a previous incarnation's identity
label was reaped as stale even when the replacement's own kubelet had already
adopted it. The reap now requires that nothing is heartbeating for the Node —
so grant `coordination.k8s.io/leases: [get]` in the workload cluster
(reapply `config/rbac/workload-cluster-observer.yaml`) or the operator has to
fall back to the Ready condition's heartbeat, which lags by up to 5 minutes.

### Replacing a cell after a cell-image change

Automatic rolling update exists — set `spec.updatePolicy.type: RollingUpdate`
(`docs/updates.md`). The default is `Manual`: changing `spec.cell.guestTemplate`
(a new `imageRef`, a driver bump) bumps the per-cell template-hash annotation,
reports `Updated=False/TemplateChanged` with the stale cells named, and touches
nothing. To roll a change out by hand, for each cell in turn:

```bash
# 1. In the WORKLOAD cluster: drain the cell's inner workloads yourself.
#    The pool will not do this for you — it only waits for allocations to
#    clear once you cordon/drain.
kubectl --context workload cordon <cell>
kubectl --context workload drain <cell> --ignore-daemonsets --delete-emptydir-data

# 2. Confirm the cell is actually idle before deleting it.
kubectl -n <ns> get cellpool <name> \
  -o jsonpath='{range .status.cells[*]}{.name} {.capacityDevices}{"\n"}{end}'

# 3. Delete the cell's outer object. The pool recreates it from the CURRENT
#    guestTemplate (SwiftGuest) or Machine (ClusterAPI) — pick the matching
#    command.
kubectl -n <ns> delete swiftguest <cell>          # provisioner: SwiftGuest
kubectl -n <ns> delete machine <cell>             # provisioner: ClusterAPI

# 4. Watch it come back on the new template.
kubectl -n <ns> get cellpool <name> -w
```

The cell finalizer (`cells.kubeswift.io/cell-drain`) blocks the delete from
actually completing until HAMi allocations clear — if step 1 was skipped, the
delete in step 3 hangs rather than destroying running work, which is
correct, but slower than doing the drain first.

---

## No `gpucell_*` metrics, and every alert quiet

Not a healthy fleet — a dead scrape. The metrics endpoint requires a bearer token
whose owner may `get /metrics`, so a scraper missing either gets nothing, and
every alert in the pack except `GPUCellPoolMetricsUnscrapable` is written over the
series that just disappeared.

Read the target's error in Prometheus (Status → Targets, or the API):

```bash
kubectl -n <prometheus-ns> port-forward svc/<prometheus> 9090:9090
curl -s localhost:9090/api/v1/targets | grep -A2 gpucellpool-.*-metrics
```

| Error | Cause | Fix |
|---|---|---|
| `401 Unauthorized` | the scrape carries no token, or an expired/unparseable one | `monitoring.serviceMonitor.bearerTokenFile` — the chart defaults it to Prometheus's own projected token |
| `403 Forbidden` | the token is valid and its owner is not authorized | `kubectl auth can-i get /metrics --as=system:serviceaccount:<ns>:<sa>`; bind it to the `metrics-reader` ClusterRole, or set `monitoring.serviceMonitor.prometheusServiceAccount` |
| `connection refused` | `metrics.bindAddress` is `"0"` | intentional; nothing is served |

Verify the endpoint itself from inside the cluster before blaming the scraper:

```bash
kubectl -n <ns> run probe --rm -it --restart=Never --image=curlimages/curl:8.11.1 -- \
  sh -c 'curl -sk -o /dev/null -w "%{http_code}\n" \
    -H "Authorization: Bearer $(cat /var/run/secrets/kubernetes.io/serviceaccount/token)" \
    https://<release>-metrics.<ns>.svc:8443/metrics'
```

`200` means the endpoint is fine and the problem is the scraper's credential.

---

## Metrics

Twelve `gpucell_*` Prometheus series, every one labelled `{pool, namespace}`
(`internal/metrics/metrics.go`) plus whatever the metric itself is about.
Both layers are kept deliberately apart, so an alert can distinguish "no GPU
left in the infrastructure cluster" from "the shared GPU is full":

| Metric | Type | Extra labels | What it tells you |
|---|---|---|---|
| `gpucell_cells_desired` | gauge | — | what the scaling policy asked for |
| `gpucell_cells` | gauge | `phase` | cells per phase — every phase is written every pass, so an emptied phase reads `0`, not stale |
| `gpucell_cell_startup_seconds` | histogram | — | creation → first Ready; the number that decides whether reactive autoscaling makes sense |
| `gpucell_cell_transitions_total` | counter | `from`, `to` | catches an oscillating cell (`Ready → AwaitingGPUCapacity → Ready`) even when its current phase looks healthy |
| `gpucell_physical_gpus` | gauge | `state` (`held`\|`free`) | outer, whole devices |
| `gpucell_capacity_gpu_devices` | gauge | — | inner, devices HAMi advertises across this pool's cells |
| `gpucell_capacity_gpu_memory_bytes` | gauge | `state` (`total`\|`allocated`\|`available`) | inner, always valid (bytes are commensurable across GPU models) |
| `gpucell_capacity_gpu_compute_percent` | gauge | `state` | inner, homogeneous pools only |
| `gpucell_workload_cluster_reachable` | gauge | — | `1`/`0` — while `0`, every destructive path is frozen; alert on this to explain why a pool stopped changing |
| `gpucell_capacity_scrape_errors_total` | counter | `reason` | a failed capacity read is reported `Unknown` and the last value retained — this counter is how you'd notice the data went stale, since the gauges alone will not tell you |
| `gpucell_scale_decisions_total` | counter | `reason`, `scaled_up` | every autoscaling decision, including refusals — "did not scale, and here is why" is the interesting case |
| `gpucell_reconcile_errors_total` | counter | — | reconciles that returned an error |

A pool deleted from the cluster stops reporting — the controller drops its
label series on teardown rather than leaving a torn-down pool's last state
looking current forever.

---

## Networking: the thing that bites

See `docs/networking.md` for the full treatment; this is the short version.

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
