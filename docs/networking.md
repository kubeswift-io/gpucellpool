# Networking

This is the single authority on cell networking. Read it before applying a
pool — this is where deployments get stuck most often, and every consequence
below was measured on real hardware, not inferred.

## A cell needs a routable interface, not just egress

A Kubernetes node must be **dialable by its own apiserver** — the apiserver
initiates connections to a node's kubelet for logs, `exec`, `port-forward`, and
metrics. Egress alone (a cell that can reach *out* to the workload cluster)
is not enough: the cell also needs an address the workload apiserver can reach
*in* on.

KubeSwift's default nat primary interface gives a cell node-local egress only
(`192.168.99.x`, private to that launcher pod's network namespace — see below).
A cell on that interface alone will join the workload cluster and then
half-work: it registers Ready, but `kubectl logs`/`exec` against pods on it
fail, and (with `capacity.hami.mode: DevicePlugin`) HAMi's own accounting reads
depend on the same path.

**Therefore every cell needs a second, routable interface**, and
`cell.nodeIPFrom` must name it:

```yaml
cell:
  guestTemplate:
    interfaces:
      - name: mgmt
        primary: true                # KubeSwift's node-local nat egress
      - name: node
        networkRef:
          name: cell-net              # a Multus NetworkAttachmentDefinition, routable
  nodeIPFrom: node                    # kubelet registers THIS interface's address
```

`networkRef` takes `{name, namespace}` only — KubeSwift's strict decoding
rejects a `kind` field on it (an earlier draft of this project's samples had
one; it does not exist).

## `br0` is per launcher pod, not per node — there is no same-node shortcut

Measured: KubeSwift's `br0` bridge lives inside **each launcher pod's own
network namespace**. Two guests scheduled on the *same physical host* both
receive `192.168.99.10` in separate namespaces and **cannot reach each other**
over that interface at all. Co-locating a workload-cluster control-plane VM and
a GPU cell on one host does not give them a shared L2 for free.

Anything a cell needs to talk to beyond its own pod's egress — an apiserver
running in another VM, a kubelet dialed by that apiserver — needs a
`networkRef` NAD. There is no shortcut for VMs that happen to land on the same
node.

## A minimal NAD

A node-local bridge NAD is enough while every VM involved is on one host (the
common single-GPU-node case); cross-node reachability needs KubeSwift's
secondary-NAD-with-routable-IP shape (see the KubeSwift networking docs) —
GPUCellPool does not add anything beyond what a cell's `networkRef` already
gives it.

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: cell-net
  namespace: gpu-cells
spec:
  config: |
    {
      "cniVersion": "0.4.0",
      "name": "cell-net",
      "type": "bridge",
      "bridge": "cellbr0",
      "isGateway": true,
      "ipam": {
        "type": "host-local",
        "subnet": "10.77.0.0/24",
        "rangeStart": "10.77.0.10",
        "rangeEnd": "10.77.0.250"
      }
    }
```

A ready-to-apply copy is at `config/samples/network-attachment-definition.yaml`.
Every VM that must reach, or be reached by, another VM on this network — the
workload cluster's control plane included, if it is also a KubeSwift guest —
needs an `interfaces[].networkRef` entry pointing at the same NAD.

## The workload apiserver must ADVERTISE the cell-network address

Making the control plane reachable is not the same as making it *advertise* a
reachable address. A workload cluster brought up on a KubeSwift guest with a nat
primary will, by default, advertise the address it detected on that primary
(`192.168.99.x`) — and that address is private to its own launcher pod's network
namespace (see above), so no cell can route to it.

The failure is deeply misleading, because the join itself works. The cell's
kubelet only needs *egress*, so it registers, goes `Ready`, carries all its
identity labels, and the pool moves to `AwaitingGPUCapacity`. What breaks is
everything the cluster hands its own components:

- `kube-proxy` is given the advertised address and logs `dial tcp
  192.168.99.20:6443: connect: no route to host`, so it never programs service
  rules;
- with no service rules, the cell's pods fall through to the default route, and
  traffic to the service ClusterIP for `kubernetes` leaves the VM entirely;
- the CNI DaemonSet cannot start, so nothing on the cell gets a pod IP;
- HAMi's device plugin discovers the GPU, fails to write its node annotation,
  and the cell fails at `capacity.readyTimeout` with `GPUNotAdvertised` — which
  points at the GPU, and the GPU is fine.

Set the advertised address explicitly, to the cell-network address, when you
build the workload cluster. On k0s that is `spec.api.address` (plus `sans`):

```yaml
spec:
  api:
    address: 10.77.0.28        # the control plane's address ON the cell NAD
    sans: [10.77.0.28]
```

The address is DHCP/IPAM-assigned after the guest exists, so a cloud-init that
bootstraps the control plane has to read it from the guest's own interfaces by
subnet first — the same wait-loop pattern the join template uses. With it set,
`k0s token create` also mints join tokens that already point at the right
endpoint, so nothing downstream needs rewriting.

Quick check, from the control-plane VM:

```bash
kubectl get endpoints kubernetes -o jsonpath='{.subsets[*].addresses[*].ip}'
kubectl -n kube-system get cm kube-proxy -o jsonpath='{.data.kubeconfig\.conf}' | grep server
```

Both must show the cell-network address, not a `192.168.99.x` one.

## The workload cluster's pod and service CIDRs must not overlap the infrastructure cluster's

A cell's pod traffic is bridged **across the infrastructure node** to reach the
rest of the workload cluster. If the workload cluster's pod CIDR overlaps the
infrastructure cluster's, that node believes those addresses are its own local
pods and never forwards the frames. Both clusters defaulting to `10.244.0.0/16`
is enough to trigger it — and k0s, kubeadm and Calico all default there.

Measured: an inner cluster on the default `10.244.0.0/16` handed its cell
`10.244.1.0/24`, which was byte-identical to the infrastructure node's own
podCIDR. The result reads like a TLS problem, not a routing one:

```
x509: certificate signed by unknown authority
  (possibly because of "crypto/rsa: verification error" while trying to verify
   candidate authority certificate "kubernetes-ca")
```

Both clusters' CAs are *named* `kubernetes-ca`; the cell's pods were reaching
the **infrastructure** cluster's apiserver, because with `kube-proxy` unable to
sync (see above) the service IP escaped the VM — and once it did, nothing routed
back. `kube-router` deliberately does not masquerade pod traffic destined to a
node IP, so those packets carry a pod-CIDR source address, which is exactly the
one the infrastructure CNI swallows.

The tell is that the *host* netns works and the *pod* netns does not:

```bash
# on the cell, as root — works
curl -sk https://<service-ip>/version
# from any pod on the cell — times out
```

Pick non-overlapping ranges when you create the workload cluster (they cannot be
changed afterwards without rebuilding it):

```yaml
spec:
  network:
    podCIDR: 10.220.0.0/16      # NOT 10.244.0.0/16 if the infrastructure
    serviceCIDR: 10.221.0.0/16  # cluster uses it — check both
```

Check the infrastructure side first with
`kubectl get node <gpu-node> -o jsonpath='{.spec.podCIDR}'`, and keep the cell
NAD subnet clear of both. KubeSwift's own `br0` moved off `10.244.125.x` for the
same class of collision.

## `nodeIPFrom` is observation-only, not a readiness gate

`cell.nodeIPFrom` names a **KubeSwift interface**, not a guest device — the
operator has no way to map `node` to `ens4` inside the guest, and the address
is not known at render time anyway (DHCP/IPAM assign it after the guest
exists). Two consequences:

- **The join template must derive the node IP itself, by subnet, with a wait
  loop** — see `config/samples/cell-join-secret.yaml` for the pattern. There is
  no `{{ nodeIP }}` substitution token, because the controller does not know
  the value at render time either.
- **KubeSwift v0.13.4 reports a secondary NAD interface's MAC in
  `status.network.interfaces[]`, but not its IP** — measured; the guest
  genuinely has the address, KubeSwift just does not surface it. GPUCellPool
  therefore treats `nodeIPFrom` as an *observation* field: it reports the
  routable address once KubeSwift exposes one and falls back to the primary
  interface for the cell's own readiness gate, verifying which address the
  Node actually registered against the *workload cluster*, where the ground
  truth lives. Gating cell readiness on KubeSwift reporting the routable
  address would park every NAD-attached cell in `Booting` forever — this was a
  real bug, fixed before v0.1.0 (see `CHANGELOG.md`).

## `kubectl port-forward` cannot reach a nat-exposed VM

`kubectl port-forward` dials `localhost` **inside the pod's network
namespace**, while KubeSwift's nat primary DNAT maps `podIP -> VM`. The two do
not compose: port-forwarding to a pod fronting a nat-exposed guest does not
reach the guest. To reach a VM-hosted service (an apiserver, for debugging) from
outside its own cluster:

- expose it as a Kubernetes `Service` and reach that, or
- put the client on the same `networkRef` NAD as the VM.

This bit the operator's own development: the operator pod that drives a pool
must itself join the workload cluster's NAD (via
`podAnnotations: k8s.v1.cni.cncf.io/networks` plus a matching `nodeSelector`)
to reach a workload apiserver that is itself a KubeSwift guest with no other
path in.

## DNS inside the cell

A measured failure worth knowing before you bake an image: once the inner CNI
(k0s's kube-router, or whatever your distribution ships) programs the cell's
node, `systemd-resolved`'s stub stops resolving through the launcher pod's
dnsmasq at `192.168.99.1`, while raw egress keeps working. Image pulls and
package installs then fail at the worst possible moment — mid-join. **Pin an
upstream resolver in the cell image** (`/etc/systemd/resolved.conf.d/`) rather
than relying on the pod-netns default. See `docs/cell-image.md`.
