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

**Measured 2026-08-07, and stronger than expected:** `br0` lives inside *each
launcher pod's* network namespace, so two guests on the **same node** both receive
`192.168.99.10` in separate namespaces and cannot reach each other at all. Co-locating
a control-plane VM and a cell VM on one host does **not** give them a shared L2.
Anything a cell must talk to beyond the pod network — an apiserver in another VM, a
kubelet dialled from one — requires a `networkRef` NAD. There is no same-node
shortcut.

Verify before anything else:

```bash
kubectl get pods -A | grep -i multus
kubectl get network-attachment-definitions -A
```

Then pick the inner cluster:

| Option | Inner control plane | Prereqs | Verdict |
|---|---|---|---|
| **0 (Phase 1 only)** | **the cell VM itself** — `k0s install controller --single`, GPU attached | none | proves every *hardware* risk (R1, R3, R5) with zero networking work; apiserver→kubelet is loopback. Does not exercise the join or the cross-cluster path — that is Phase 2's problem, not the gate |
| A (Phase 2) | a `SwiftGuest` k0s/kubeadm CP on dev (hand-rolled or via `capi-kubeswift`, which has validated node-join on dev) | **a NAD both VMs share — mandatory, see above** | proves the two-cluster mechanics *and* keeps HAMi's admission webhook off the primary lab cluster |
| B | **dev itself** (workload cluster == infrastructure cluster, the degenerate case) | cell needs an IP the dev apiserver can reach → still a NAD | installs HAMi's scheduler + mutating webhook on the primary lab cluster — scope it or accept the risk |
| C | sov (Hetzner, public endpoint) | none for join | cell is behind dev's NAT → apiserver cannot reach the kubelet → **no logs/exec**; use only if A and B are blocked |

Phase 1 takes option 0. Option A is the first thing Phase 2 needs, and it needs a NAD
built first (`bridge` + `host-local` IPAM is enough while every VM is on one host;
cross-node needs the #419 shape).

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
| R3 | **HAMi-core's glibc window (≥2.17, <2.30)** silently disables limiting in modern workload images | **RETIRED 2026-08-07.** Both `nvidia/cuda:12.4.1-base-ubuntu22.04` (glibc 2.35, outside the documented window) and `nvidia/cuda:11.8.0-base-ubuntu18.04` (glibc 2.27, inside it) were limited to 3000 MiB. The upstream prerequisite does not reflect current behaviour for memory limiting on this stack — but keep the test in the suite, because it is a silent failure if it ever regresses |
| R4 | apiserver→kubelet reachability (Step 0) | Step 2 `kubectl exec` |
| R5 | NVIDIA driver ↔ CUDA ↔ HAMi version alignment | pin all three in the cell image; record the matrix |
| R6 | HAMi's mutating webhook interferes with the host lab cluster (topology B) | prefer topology A; or scope the webhook and verify KubeSwift's own GPU tests still pass |
| R7 | Cell startup time makes autoscaling pointless | measure `Pending→Ready` in Step 6; if > ~5 min, Phase 3 becomes pre-warming |
| R8 | `nvidia.com/gpu` count inflation misread as device count anywhere in the stack | Step 3 records `allocatable == 10`; the parser test asserts `Devices() == 1` |

---

## 8a. Phase 1 results (2026-08-07, dev/boba, GTX 1080)

Topology: option 0 — the cell VM is itself the single-node inner cluster
(`k0s install controller --single`), GPU attached via DRA.

| Step | Result | Evidence |
|---|---|---|
| 1 GPU in the VM | **PASS** | `lspci`: `GP104 [GeForce GTX 1080] [10de:1b80]` at `00:05.0`; `nvidia-smi -L` → 1 GPU, UUID `GPU-e71afe85-…`; driver **580.173.02**, 8192 MiB. Allocated by the scheduler: DRA device `gpu-0000-01-00-0` on boba, `status.gpu.devices: [0000:01:00.0]`, hypervisor `cloud-hypervisor` |
| 2 VM is a Kubernetes node | **PASS** | `cellpoc-cell-0` Ready, `v1.36.3+k0s`, `containerd://2.3.3`; labels `gpu=on`, `cells.kubeswift.io/{pool,cell}` applied by kubelet `--labels` |
| 3 HAMi sees the GPU | **PASS** | `hami.io/node-nvidia-register` = `[{"id":"GPU-e71afe85-…","count":10,"devmem":8192,"devcore":100,"type":"NVIDIA GeForce GTX 1080","mode":"hami-core","health":true,"devicepairscore":{}}]`; `allocatable nvidia.com/gpu: "10"` — the ×10 inflation, measured |
| 4 two workloads share it | **PASS (all four conditions)** | both pods Running on the cell; each `nvidia-smi` reports **3000 MiB** total, not 8192, with `HAMI-core Msg(...)` interposition logs; a 9000 MiB request on the 8192 MiB card is **rejected at scheduling** (`NodeUnfitPod`); both pods' `hami.io/vgpu-devices-allocated` name the **same** UUID `GPU-e71afe85-9309-864a-477e-91caa89f3932` |

**Phase 1 is PASS. The architecture is proven on real hardware: one physical GPU,
one KubeSwift VM, two independently-limited workloads sharing it through HAMi, with
neither KubeSwift nor HAMi modified.**

Findings that change the design or the image recipe:

1. **`br0` is per launcher pod, not per node.** Two guests on one host both get
   `192.168.99.10` in separate netns and cannot reach each other (§0). Any cell that
   must talk to another VM needs a `networkRef` NAD — there is no same-node shortcut.
   Phase 1 sidesteps it by collapsing the inner cluster into the cell.
2. **The guest's DNS breaks when the inner CNI comes up.** `systemd-resolved`'s
   stub forwards to the pod-netns dnsmasq at `192.168.99.1`; once k0s + kube-router
   programmed the node, that path stopped answering while raw egress kept working
   (`curl https://1.1.1.1` → 301). Every image pull and `apt` then fails, at the
   worst possible moment. **The cell image must pin an upstream resolver**
   (`/etc/systemd/resolved.conf.d/`) rather than inherit the pod's. Added to the
   recipe.
3. **Pascal needs the proprietary driver** — the open kernel modules are Turing+, so
   `nvidia-driver-*-server` (not `-open`) is required for a GTX 1080.
4. **The driver metapackage tracks its branch, it does not pin.**
   `nvidia-driver-570-server` resolved to **580.173.02**. If the image is to be
   reproducible, pin the exact version; otherwise record what the bake produced (the
   bake writes `/etc/gpucell-image-manifest`).
5. **`get.k0s.sh` installs latest** — v1.36.3+k0s.0 here, i.e. Kubernetes 1.36. Good
   news for `hami.mode: DRA`, which needs ≥ 1.34; bad news for reproducibility, so
   the image should pin `K0S_VERSION`.
6. **`k0s reset` does not clean up a second installed unit.** Having both
   `k0sworker.service` and `k0scontroller.service` installed wedges `k0s reset`
   ("another k0s process is still running"). The cell's join path must be idempotent
   *and* mutually exclusive — one role per cell, checked before install.

### The baked image (built in parallel, local qemu+KVM)

`build-cell-image.sh` produced a 30 GiB raw disk, **5.5 GiB sparse**,
`BAKE_EXIT=0`, containing driver 580.173.02 (kernel 6.8.0-136-generic), the NVIDIA
container toolkit, `k0s v1.36.3+k0s.0`, the k0s containerd nvidia drop-in, the CDI
generate unit, and a reset cloud-init/machine-id/host-keys. Not yet published;
`swiftctl image publish <raw> --to ghcr.io/… --tag …` is the remaining step.

Notes for the next bake: the local build needs the **distro** QEMU
(`/usr/bin/qemu-system-x86_64`) — Kata's bundled build has no user-mode networking
compiled in, which is easy to miss when `/opt/kata/bin` is first in `PATH`.

## 8b. Phase 2 end-to-end (2026-08-07): the operator drives it

The operator itself, chart-installed on dev (`gpucellpool-system`, distroless
non-root, webhook serving its own cert, leader lease held), took a `GPUCellPool`
with `replicas: 1` from nothing to usable GPU capacity in a **separate** cluster.

Topology: NAD `cellpoc/cellpoc-net` (bridge + host-local `10.77.0.0/24`, node-local
on boba) shared by the inner k0s control-plane VM `innercp` (`10.77.0.10`, HAMi
pre-installed) and the cell. The operator pod joins the same NAD via
`podAnnotations: k8s.v1.cni.cncf.io/networks` plus a nodeSelector — which is how it
reaches the inner apiserver with **no TLS shortcut**. Note that
`kubectl port-forward` cannot reach a KubeSwift nat-exposed VM at all: it dials
localhost inside the pod netns, while the DNAT maps podIP→VM.

| Observed | Evidence |
|---|---|
| cells created | `SwiftGuest cells-0`, `SwiftSeedProfile cells-0-seed`, and the per-cell `Secret cells-0-bootstrap` — the user's template Secret untouched, still holding its 4 unrendered tokens |
| render is per cell | `hostname: cells-0`, `--node-labels=cells.kubeswift.io/cell=cells-0,…,gpu=on`, `of 1 GPU(s)` |
| FSM walked both layers | `AllocatingGPU → Booting → Joining → AwaitingGPUCapacity → Ready` |
| identity fallback works | the operator patched `cells.kubeswift.io/instance=60ec5bcb-…` (the guest UID) plus pool/cell/index onto the Node the kubelet had not labelled |
| Ready needed all three | the cell sat in `AwaitingGPUCapacity` with "workload Node is Ready but the capacity provider is not usable yet" for as long as HAMi's device plugin was still starting |
| both capacities, separately | `physicalCapacity {gpus: 1, freeGPUsInCluster: 0, model: NVIDIA GeForce GTX 1080}`; `workloadCapacity {devices: 1, memory 8Gi/0/8Gi, compute 100/0/100, homogeneous, provider HAMi, mode DevicePlugin}` |
| conditions | all six True (`Progressing=False Idle` in steady state) |
| capacity is real | two workloads at 3000MiB/30% each landed on the cell, each `nvidia-smi` reporting **3000 MiB**, both bound to the **same** GPU UUID, and the pool moved to `8Gi / 6000Mi allocated / 2192Mi available`, `compute 100/60/40` |
| and it returns | deleting them returned the pool to `0 allocated / 8Gi available / compute 0` |

### The baked image, booted under Cloud Hypervisor (same day)

The QEMU-baked image (`hack/build-cell-image.sh`, published as an OCI artifact and
imported via `SwiftImage.spec.source.oci`) **boots and runs correctly under Cloud
Hypervisor**, with thin enrollment: no apt, no driver build, no reboot. Evidence
from the cell: `cloud-hypervisor` in the console, both addresses up
(`10.77.0.16` on the NAD and a nat primary), sshd answering, `nvidia-smi` reporting
`1 of 1 GPU(s)` off the baked 580.173.02 driver, a CDI spec generated at first boot,
and the pool reaching `Ready` with HAMi advertising 8Gi.

So "build the image on the same VMM that runs it" was a theoretical concern. The
in-cluster CH rebake pipeline is **not needed**, which is why it was worth testing
the cheap hypothesis first rather than building the pipeline speculatively.

It cost two cell rebuilds to get there, for a reason worth remembering: the bake
script and the install-at-boot template had **drifted** on the containerd config
schema. The template used containerd 2.x's v3 form; the bake used the v1 CRI form,
which k0s ≥ 1.34 rejects at pre-flight, so the worker never started. The divergence
only surfaced when both were actually run — and the failure was silent from every
angle that looked healthy (VM up, sshd answering, addresses assigned, cloud-init
successful, join log ending `EXIT=0`, because the join *script* finished and it was
the *service* that refused to start). Only `journalctl -u k0sworker` inside the
guest showed it.

Timing, with the caveat that it is NOT a clean measurement — the drop-in was patched
by hand mid-boot: ~8.5 minutes pool-scale to Ready, of which ~4–5 minutes was the
root-disk clone (30 GiB image, 8.5 GiB of real data) and thin enrollment itself took
seconds. Against ~14 minutes for install-at-boot. A clean number needs a rebake with
the fixed script.

Two bugs the earlier run found are recorded in §8a-adjacent commits: `nodeIPFrom` was a
readiness gate when it should only be an observation (KubeSwift reports a
secondary NAD interface's MAC but not its IP), and `networkRef` has no `kind`
field, so the sample as shipped could not have been applied.

## 9. Exit criteria for Phase 1

All four Step-4 PASS conditions met, on real hardware, with the evidence captured
(annotations, both `nvidia-smi` outputs, both Pod annotations, the guest's
`SwiftGuest status.gpu`). Plus a written answer to R3 and R7.

If Step 4 fails, the architecture is not invalidated — but the *product* is: without
in-VM fractional sharing there is no cell, only a GPU VM, which KubeSwift already
does on its own. That is why nothing else gets built first.
