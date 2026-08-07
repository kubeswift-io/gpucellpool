# GPUCellPool — proof of concept and test strategy

> Phase 1 is a **hardware proof done by hand**: physical GPU → KubeSwift VM →
> in-guest NVIDIA driver → HAMi → two fractionally-limited workloads on the same
> GPU. No controller code until that path is real.
>
> Lab: dev k0s cluster (frida CP, miles/boba workers), **one GTX 1080 on boba**,
> CH v53.0, KubeSwift v0.13.4. Point `KUBECONFIG` at the infrastructure cluster.
> Date: 2026-07-30.

---

## 0. Step 0 — the topology constraint everyone forgets

A Kubernetes node must be **dialable by its own apiserver** (logs, exec,
port-forward, metrics). KubeSwift's default nat primary gives a cell node-local
egress only (`192.168.99.x`, per node), so a cell on that alone joins and then
half-works: `kubectl logs` against the HAMi workloads — the exact evidence the PoC
needs — fails.

**Therefore a GPU cell requires a routable interface**, not just egress. Use the
shape KubeSwift already validated for multi-node workers (#419): nat `mgmt` primary
+ a `networkRef` secondary carrying the routable address, kubelet `--node-ip` on the
secondary. This is not a PoC detail — it is a product requirement, and the reason
`cell.nodeIPFrom` exists in the API.

Verify before anything else:

```bash
kubectl get pods -A | grep -i multus
kubectl get network-attachment-definitions -A
```

Then pick the inner cluster:

| Option | Inner control plane | Prereqs | Verdict |
|---|---|---|---|
| **A (recommended)** | a `SwiftGuest` k0s/kubeadm CP on dev (hand-rolled or via `capi-kubeswift`, which has validated node-join on dev) | a NAD both VMs share | proves the two-cluster mechanics *and* keeps HAMi's admission webhook off the primary lab cluster |
| B (fastest) | **dev itself** (workload cluster == infrastructure cluster, the degenerate case) | cell needs a LAN-routable NAD IP | fastest to the hardware answer; installs HAMi's scheduler + mutating webhook on the primary lab cluster — scope it or accept the risk |
| C | sov (Hetzner, public endpoint) | none for join | cell is behind dev's NAT → apiserver cannot reach the kubelet → **no logs/exec**; use only if A and B are blocked |

Start with A if the NAD exists, else B. Record which one, because every later
number depends on it.

---

## 1. Step 1 — one GPU, one VM

```bash
export KUBECONFIG=<infrastructure-cluster-kubeconfig>
kubectl get resourceslices                        # driver gpu.kubeswift.io, pool=boba, gpu-0000:01:00.0
kubectl get swiftgpunodes                         # free device, vfioReady
```

Create a `SwiftGuest` from the prebaked cell image with
`gpuResourceClaim.resourceClaimTemplateName: single-vfio-gpu` (`tier: pcie`) plus
the dual-NIC interface shape. Then, in the guest:

```
lspci -nn | grep -i nvidia          # 10de:1b80
nvidia-smi -L                       # GPU 0: NVIDIA GeForce GTX 1080 (UUID: GPU-…)
nvidia-smi                          # driver + memory reported
```

**PASS**: one 3D controller, `nvidia-smi` healthy, `SwiftGuest status.gpu.devices`
matches the BDF from the ResourceSlice.

## 2. Step 2 — the VM is a Kubernetes worker

Join it (thin cloud-init, per `-bootstrap.md` §3), then from the inner cluster:

```bash
kubectl get nodes -o wide           # cell node Ready, INTERNAL-IP == the routable address
kubectl get node <cell> -o yaml     # labels/taints as declared
kubectl debug node/<cell> -it --image=busybox   # apiserver→kubelet path works
```

**PASS**: `Ready`, node IP is the routable one, `kubectl logs`/`exec` work against a
plain Pod scheduled on it. If exec fails, go back to Step 0 — do not proceed.

## 3. Step 3 — HAMi sees the GPU

Install HAMi on the inner cluster (prerequisite, D6), label the node `gpu=on`, then:

```bash
kubectl get node <cell> -o jsonpath='{.metadata.annotations.hami\.io/node-nvidia-register}' | jq .
# [{"id":"GPU-…","count":10,"devmem":8192,"devcore":100,"type":"NVIDIA-GeForce-GTX-1080","numa":0,"health":true}]
kubectl get node <cell> -o jsonpath='{.status.allocatable}' | jq '."nvidia.com/gpu"'   # 10, NOT 1
```

**PASS**: the register annotation lists exactly one healthy device with the right
model and `devmem: 8192`. Note the `allocatable = 10` inflation in the record — it
is the reason `-capacity.md` §2 forbids using `allocatable` as a device count.

## 4. Step 4 — two workloads share the one passthrough GPU

```yaml
resources:
  limits:
    nvidia.com/gpu:    1
    nvidia.com/gpumem: 3000     # MiB
    nvidia.com/gpucores: 30     # percent
```

Two such Pods, different names, same node.

**PASS, all four:**
1. both Pods `Running` on the cell node;
2. each Pod's `nvidia-smi` reports **~3000 MiB total**, not 8192 (HAMi-core is
   interposing);
3. an allocation beyond the limit fails inside the container (OOM from HAMi, not
   from the driver);
4. `hami.io/vgpu-devices-allocated` on both Pods references the **same GPU UUID**.

Point 4 is the whole architecture in one field: two logical consumers, one physical
device, inside one VM boundary.

## 5. Step 5 — a second cell (hardware-gated)

The lab has one GPU, so a second *real* cell is impossible. Prove the parts that do
not need a second device:

- a second cell VM **without** a GPU joins and is correctly held at
  `AwaitingGPUCapacity` (proves the readiness gate is real, not decorative);
- `Devices()`/`Capacity()` aggregation over 2+ nodes via the fake provider (§7);
- physical-inventory pre-flight returns `free == 0` and the pool refuses to create a
  third cell (scenario 1 in `-failure-model.md`).

## 6. Step 6 — only now, the controller

Implement the static pool (Phase 2, MVP scope in `-overview.md` §5) against the
topology chosen in Step 0, with `replicas: 1` real + faked cells for the rest.
Operator-perspective walkthrough (the house rule between phases): create the pool,
watch `kubectl get cellpool -w`, kill a cell VM, delete a cell's inner Node, revoke
the workload kubeconfig, and confirm each produces the designed condition and no
destructive action.

---

## 7. Hardware-free test strategy

Mirrors how KubeSwift validates HGX without HGX (fake-HGX harness,
`make verify-qemu-topology`).

| Layer | Harness | Covers |
|---|---|---|
| unit | pure funcs: index/name allocation, template overlay + denylist, HAMi annotation parser (golden JSON incl. malformed/newer-shape), DRA capacity math, condition aggregation, backoff | most of the logic, no cluster |
| **fake capacity provider** | in-memory `CapacityProvider` returning scripted device counts/capacity/allocations, incl. flapping and unparseable | readiness gate, degradation rules, `-capacity.md` §6 matrix |
| envtest (outer) | real apiserver + `SwiftGuest`/`SwiftSeedProfile` CRDs installed from the KubeSwift chart, guest status **hand-stamped** | provisioner, FSM up to `Booting`, finalizers, orphan recovery |
| envtest (inner) | a **second** envtest apiserver as the workload cluster; kubeconfig written into a Secret in the first | cross-cluster client, Node correlation, stale-node reaping, labels/taints, drain gating |
| two-envtest e2e | both of the above, fake Nodes carrying real HAMi-shaped annotations | the full `Pending→Ready` path with zero hardware |
| chaos | inner apiserver stopped mid-reconcile; Secret rotated; Node deleted; guest deleted | rule 2 (freeze on doubt) and every "must not delete" assertion |
| real hardware | Steps 1–4, one cell | the only thing the harness cannot fake |

Assertions worth writing as explicit negative tests, because they encode the
dangerous failures:

```
no destructive call is made while WorkloadClusterReachable=False
no cell reaches Ready with Devices(node) < expected
a Draining cell with Allocations>0 is never deleted (unless pool deletion + Force)
a parse failure never yields capacity 0
maxCreating is never exceeded; the pool-level guard stops a hopeless pool
a stale Node (instance mismatch) is reaped before the replacement joins
```

---

## 8. Risks to retire in Phase 1

| # | Risk | Retire by |
|---|---|---|
| R1 | HAMi does not function on a VFIO-passthrough GPU inside a VM | Step 4 |
| R2 | GTX 1080 (Pascal, 8 GB) is unrepresentative — no MIG, small memory, older CUDA | Step 4 proves the *mechanism*; document that capacity numbers are not representative |
| R3 | **HAMi-core's glibc window (≥2.17, <2.30)** silently disables limiting in modern workload images | Step 4 point 2 — if a Noble-based CUDA image reports the full 8192 MiB, the limit is not applied. Test one modern and one older image and record which works |
| R4 | apiserver→kubelet reachability (Step 0) | Step 2 `kubectl exec` |
| R5 | NVIDIA driver ↔ CUDA ↔ HAMi version alignment | pin all three in the cell image; record the matrix |
| R6 | HAMi's mutating webhook interferes with the host lab cluster (topology B) | prefer topology A; or scope the webhook and verify KubeSwift's own GPU tests still pass |
| R7 | Cell startup time makes autoscaling pointless | measure `Pending→Ready` in Step 6; if > ~5 min, Phase 3 becomes pre-warming |
| R8 | `nvidia.com/gpu` count inflation misread as device count anywhere in the stack | Step 3 records `allocatable == 10`; the parser test asserts `Devices() == 1` |

---

## 9. Exit criteria for Phase 1

All four Step-4 PASS conditions met, on real hardware, with the evidence captured
(annotations, both `nvidia-smi` outputs, both Pod annotations, the guest's
`SwiftGuest status.gpu`). Plus a written answer to R3 and R7.

If Step 4 fails, the architecture is not invalidated — but the *product* is: without
in-VM fractional sharing there is no cell, only a GPU VM, which KubeSwift already
does on its own. That is why nothing else gets built first.
