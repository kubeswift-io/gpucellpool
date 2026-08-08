# ClusterAPI cells — validated recipe and what it cost

> `provisioner: ClusterAPI` was validated end to end on real hardware on
> **2026-08-08** (dev/boba, GTX 1080, CAPI v1.13.4, capi-kubeswift @ main,
> Kubernetes v1.33.3, HAMi 2.9.0). Everything below is measured, including the
> mistakes.

## What was proven

A `GPUCellPool` added the only worker of a CAPI-managed workload cluster:

| | |
|---|---|
| pool created → cell `Ready` | **6 min 29 s** |
| Cluster API's view | `Machine cells-0` `Running`, `providerID kubeswift://capicell/cells-0`, `nodeRef cells-0` |
| workload cluster's view | Node `cells-0` `Ready`, same providerID |
| HAMi's view | `GPU-e71afe85…` GTX 1080, `devmem 8192`, `devcore 100`, healthy |
| the pool | `physicalCapacity{gpus 1}` + `workloadCapacity{1 device, 8Gi}`, reported separately |
| two workloads at 3000 MiB / 30 % | both `Running` on `cells-0`, **same GPU UUID**, pool at `6000Mi allocated / 2192Mi available`, compute `60/40` |
| teardown | pool deleted → Machine and KubeSwiftMachine gone in ~16 s, pool finalizer released at 47 s, GPU claim returned, Node reaped |

The FSM walked `AllocatingGPU → Joining → AwaitingGPUCapacity → Ready`, holding in
`AwaitingGPUCapacity` exactly while HAMi's device plugin started. The three-way Ready
gate behaves the same through Cluster API as it does without it.

## Two bugs only a live cluster could find

Both were invisible to the envtest harness, for the same reason: **a CRD stub accepts
anything, because the thing that objects is the controller that is not running there.**

1. **The pool claimed the controller owner reference.** Kubernetes allows one per
   object and Cluster API needs it (the Cluster on the Machine, the Machine on the
   infrastructure object). CAPI failed every reconcile with *"Object cells-0 is already
   owned by another GPUCellPool controller"* — a hard stop: no bootstrap data, no VM,
   ever. Cells are now **co-owned** (plain owner reference), which is all that garbage
   collection needs.

2. **Pool teardown listed SwiftGuests.** A ClusterAPI pool owns Machines, so deletion
   saw no cells, drained nothing, and dropped the pool finalizer — orphaning each cell
   Machine with `cells.kubeswift.io/cell-drain` still on it. Nothing was left to clear
   it, so the Machine could never be deleted and its GPU claim never came back.
   Observed exactly that, and cleared it only by patching the finalizer out by hand.
   Deletion now goes through `prov.List`, and the dead lister is gone.

## What the workload cluster needs (dev recipe)

The pool is the easy half. Getting a CAPI-managed cluster onto KubeSwift on a lab with
no cross-node L2 took four things worth writing down.

**A way to place the control plane.** The CP VM and the GPU cell must share the
node-local bridge NAD, which means sharing a host — and the cell's host is fixed by
where its GPU is. Cluster API cannot place an individual Machine, so
`KubeSwiftMachine.spec.backend.swiftGuest.nodeName` was added
(capi-kubeswift [#20](https://github.com/kubeswift-io/cluster-api-provider-kubeswift/pull/20)).
It rejects being combined with `gpu`: pinning bypasses the scheduler, and a DRA claim
is allocated *by* the scheduler, so the pair yields a VM with an unallocated claim and
no device.

**The control-plane endpoint hairpins back into the control plane VM.** With
`endpoint.mode: Service` the endpoint is a ClusterIP in the *management* cluster whose
backend is the CP's own launcher pod. `kubeadm init`'s `wait-control-plane` phase health
checks through it, so the request goes VM → in-pod MASQUERADE → node → Service → the
same pod → back to the VM, and conntrack cannot match the reply:

```
error execution phase wait-control-plane: kube-apiserver check failed at
https://192.168.99.10:6443/livez: Get "https://10.96.212.189:6443/livez?timeout=10s":
context deadline exceeded
```

The failure is quiet in the worst way: every control-plane static pod is `Running`, so
the cluster looks alive, while `kubeadm-config`, `kubelet-config`, `cluster-info`, the
bootstrap tokens, the control-plane role label, kube-proxy and CoreDNS are all absent.
Fix: alias the endpoint address on `lo` in the guest, so in-guest clients reach the
apiserver locally. **Control plane only** — a worker must reach the real Service.

**That alias must come after the node-IP derivation.** It is a scope-global address, so
deriving "the global IPv4 that is not on the default route" afterwards picks up the
alias and the node registers with the endpoint ClusterIP as its `InternalIP`. Measured,
and it silently breaks apiserver→kubelet. Exclude `lo` as well.

**Then the documented single-CP hairpin still applies** *inside* the workload cluster:
`masqueradeAll: true` in the kube-proxy ConfigMap (capi-kubeswift
`docs/operations/single-control-plane-hairpin.md`, fix 3), or the CNI on the lone
control-plane node never starts. And the CNI must be pinned to the datapath interface —
flannel picks the default-route interface, which here is KubeSwift's node-local nat
primary, the wrong side (`--iface-regex=10\.79\.0\.\d+`).

Two smaller ones: a CP-only cluster needs its control-plane taint removed or nothing
schedules, and `SwiftImage.spec.format` is the **input** format — declaring `raw` for
the Ubuntu qcow2 cloud image skips conversion and hands Cloud Hypervisor a qcow2 it
reads as raw (`Failed to get refcount`, in under a minute of "importing").

## Capacity arithmetic, since it bites

boba has 8 cores. A 4-vCPU control plane plus a 2-vCPU cell does not fit alongside the
node's existing load, and with `nodeName` the kubelet says so immediately —
`OutOfcpu: requested 4000, used 7790, capacity 8000` — rather than leaving the pod
Pending. The validated shape is a **2-vCPU** control plane (`capicell-cp`) and a 2-vCPU
cell.

Do not delete Machines mid-rollout to force a change: KubeadmControlPlane holds a
pre-terminate hook on the last control plane and will not release it until a
replacement joins, so you deadlock (`stage: WaitingForPreTerminateHook`). Delete the
`Cluster` and rebuild instead.
