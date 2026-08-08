# GPUCellPool — cell bootstrap

> Design record, written before implementation (2026-07-30) and partially
> updated. It documents rationale, not current behaviour — see `docs/` for
> that, in particular `docs/cell-image.md`, `docs/networking.md` and
> `config/samples/cell-join-secret.yaml`.

> A cell must become a GPU-capable Kubernetes worker in the *workload* cluster.
> Decision: **prebaked image + thin cloud-init enrollment**, with the join
> credential delivered by Secret reference and never written into a CR.
>
> Companions: `gpucellpool-api.md` §3 (`spec.bootstrap`), `-reconciliation.md` §3
> (identity). Date: 2026-07-30.

---

## 1. What has to be true inside the cell VM

| Layer | Requirement | Who owns it |
|---|---|---|
| PCI | the passthrough GPU visible (`lspci`, one 3D controller) | KubeSwift (VFIO) |
| driver | NVIDIA driver ≥ 440 (HAMi-core's floor), version aligned with the workload images' CUDA | the cell image (or the inner cluster's driver container — §2) |
| runtime | containerd + NVIDIA container runtime + **CDI enabled** | the cell image |
| kubelet | joined, `--node-ip` = the routable address, identity labels applied | cloud-init |
| HAMi | device plugin / DRA kubelet plugin DaemonSet, node label `gpu=on` | the **workload cluster** (prerequisite, D6) |

Notes that bite:

- HAMi's `LD_PRELOAD` core also documents **glibc ≥ 2.17 and < 2.30** — that is a
  constraint on the *workload container image*, not the node, but it must be
  verified in Phase 1 or every PoC workload silently runs unlimited (`-validation-record.md` R6).
- A cell needs **no** Fabric Manager, no IOMMU config in the guest, and no
  hugepages unless `cell.gpu.dra.hugepages` is set: it is a flat single-GPU `pcie`
  guest.
- HAMi's DaemonSets must tolerate the pool's taints
  (`spec.workloadCluster.node.taints`) or the cell will never register. Document
  it; do not work around it in code.

---

## 2. Image strategy — three options, one recommendation

| | A. install at boot | **B. prebaked image (recommended)** | C. prebaked OS + driver from the inner cluster |
|---|---|---|---|
| image | generic cloud image | `SwiftImage` with driver + containerd + kubelet | `SwiftImage` with containerd + kubelet, no driver |
| driver install | cloud-init, per boot | baked | NVIDIA GPU Operator driver container in the workload cluster |
| cell startup | **5–15 min**, needs internet + package repos | **~40–90 s** (VM boot + join) | ~2–5 min (driver container build/load per boot) |
| determinism | poor (repo drift, DKMS builds) | exact | good, driver pinned by the inner cluster |
| version alignment with workloads/HAMi | accidental | must rebuild the image to move driver versions | **best** — the driver lives where HAMi and CUDA expectations live |
| offline/air-gapped | no | yes | yes (registry) |

**MVP: B.** Startup time is the input to every autoscaling decision later, and a
15-minute cell is not a scaling unit. It also removes the largest class of
"SwiftGuest is Running but the node is useless" failures from Phase 1, where the
point is to prove the GPU path, not to debug DKMS.

**Production-scale: C is the better long-term answer** and needs no API change —
it is just a different `imageRef`. Recommend it once driver-version churn across
several workload clusters costs more than the extra boot minutes. Record the
choice per pool in the image, not in the CRD.

The reference cell image (`gpu-worker-noble-<driver>`): Ubuntu Noble 24.04 (the
KubeSwift-validated disk-boot guest), NVIDIA driver + `nvidia-container-toolkit`
with CDI, containerd, the target distribution's node binaries, cloud-init, and
`qemu-guest-agent`-equivalent tooling as needed. Built once, published as a
`SwiftImage`, digest-pinned.

---

## 3. What cloud-init actually does (thin enrollment)

Ordered, and deliberately small:

```
1 set hostname                      = cell name (from meta-data local-hostname)
2 derive the node IP                = address of the NIC named by cell.nodeIPFrom
                                      (in-guest, from the interface — NOT injected
                                       by the controller: DHCP/UDN assign it after
                                       the CR is rendered)
3 write kubelet extra args           --node-ip=<derived>
                                     --node-labels=cells.kubeswift.io/pool=…,
                                                   cells.kubeswift.io/cell=…,
                                                   cells.kubeswift.io/instance=…,
                                                   <spec.workloadCluster.node.labels>
4 preflight (log only, non-fatal)    nvidia-smi -L | wc -l  ==  expected count
5 join the workload cluster          distribution-specific (§4)
```

Two deliberate choices:

- **The node IP is derived in-guest, not templated.** The controller does not know
  the address at render time; making cloud-init read it from the named interface
  removes a whole class of ordering bugs. The validated KubeSwift multi-node shape
  is a nat `mgmt` primary plus a routable `networkRef` `node` interface, and the
  kubelet must bind the latter — the same rule `capi-kubeswift` follows.
- **Preflight is non-fatal.** A node that joins *without* a GPU is diagnosable —
  the pool parks it in `AwaitingGPUCapacity` with a message and a timeout. A node
  that refuses to join is opaque. **As shipped, preflight is log-only, not the
  node-annotation surface this paragraph originally planned**: the reference
  cell image (`hack/build-cell-image.sh`) writes the result to
  `/run/gpu-cell-preflight` inside the guest, and the diagnostic path is
  `swiftctl ssh <cell> -- cat /run/gpu-cell-preflight` (see `docs/runbook.md`).
  The Go constant `cells.kubeswift.io/gpu-preflight`
  (`AnnotationGPUPreflight`, `api/v1alpha1/conditions.go`) exists for a future
  node-annotation surface but nothing writes it or reads it yet — do not
  document it as available.

Kubelet self-labelling with a third-party prefix is permitted under the
`NodeRestriction` admission plugin (it only constrains `kubernetes.io`/`k8s.io`
labels), but the controller patches the labels itself if they are missing — see
`-reconciliation.md` §3.

---

## 4. Join credentials

### `bootstrap.provider: Opaque` (default, MVP)

The user creates one Secret holding the cloud-init user-data that joins *their*
cluster. The operator treats it as a template with a tiny substitution set and
renders a **per-cell Secret** owned by the pool:

```
spec.bootstrap.joinSecretRef ──► Secret <join>            (user-owned, contains the token)
                                     │  render + substitute
                                     ▼
                                 Secret <cell>-bootstrap  (operator-owned, ownerRef → pool)
                                     │  referenced, never copied into the CR
                                     ▼
                                 SwiftSeedProfile <cell>-seed
                                   spec.datasource: NoCloud
                                   spec.userData: ""                      # required-but-empty, see below
                                   spec.userDataFrom.secretKeyRef:
                                     {name: <cell>-bootstrap, key: user-data}
                                   spec.metaData: |
                                     instance-id: <cell>
                                     local-hostname: <cell>
```

Substitutions (documented, closed set): `{{ cellName }}`, `{{ poolName }}`,
`{{ nodeLabels }}`, `{{ nodeIPInterface }}`, `{{ expectedGPUs }}`. No arbitrary
templating language, and an unknown token is an error rather than a passthrough —
leaving `{{ cellname }}` in a boot script would produce a node that joins with a
broken label and no explanation.

**Quote your tokens in value position.** `hostname: {{ cellName }}` is not valid
YAML: `{{…}}` parses as a nested flow mapping, so the template file cannot be
YAML-checked before substitution (the rendered output is fine, because the tokens
are gone by then). Write `hostname: "{{ cellName }}"` and the template validates
both before and after.

**Wait for the routable address before joining.** A thin template that derives its
node IP straight from `runcmd` races the secondary NIC's DHCP. If the address is not
up yet, `NODE_IP` comes out empty and `k0s install worker --node-ip=` (or the
kubeadm equivalent) fails — with the output going nowhere, because the serial
console is not captured. Loop until the address appears:

```bash
for i in $(seq 1 30); do
  NODE_IP=$(ip -4 -o addr show | awk '/10\.77\.0\./ {split($4,a,"/"); print a[1]; exit}')
  [ -n "$NODE_IP" ] && break || sleep 2
done
[ -n "$NODE_IP" ] || { echo "no routable address after 60s"; exit 1; }
```

Worth doing regardless, though it was NOT the cause of our own booted-but-never-
joined cell — see the next item for what actually was.

**Match the container runtime's config version.** k0s ≥ 1.34 ships containerd 2.x,
which requires the v3 config schema. A drop-in written in the v1 CRI form
(`version = 2` with `[plugins."io.containerd.grpc.v1.cri"]`) is rejected at
pre-flight:

```
Rejected: unsupported configuration version: expected 3, got 2
  property=/etc/k0s/containerd.d/nvidia.toml
Error: pre-flight checks failed
```

The worker then never starts and the cell never joins. This is silent from the
outside — the VM boots, sshd answers, both addresses come up, cloud-init reports
success and even logs `EXIT=0`, because the join *script* completed; it was the
service it installed that refused to start. Use
`[plugins."io.containerd.cri.v1.runtime"]`. Our first baked image had the v1 form
and cost two cell rebuilds to find, which is the strongest argument in this document
for giving every cell an ssh key.

**A baked image needs an ssh key in the template.**

**A baked image needs an ssh key in the template.** The bake resets cloud-init,
machine-id and host keys, so the only way into a cell is a key the join cloud-init
installs — and KubeSwift's Cloud Hypervisor drops the serial console when no client
is attached, so cloud-init output is not captured either. A thin template without
`ssh_authorized_keys` produces a cell you cannot diagnose. (Learned the hard way.)

Verified KubeSwift mechanics this relies on:

- `SwiftSeedProfile.spec.userDataFrom.secretKeyRef` **takes precedence** over
  inline `userData` (`internal/seed/resolve_refs.go:16`) — so the token exists only
  in Secrets.
- `spec.userData` is **required by the CRD schema** even when `userDataFrom` is set
  (`config/crd/bases/seed.kubeswift.io_swiftseedprofiles.yaml:218`). Set it to `""`.
  The seed webhook accepts that (`userData != "" || userDataFrom != nil`).
- The Secret must live in the **guest's namespace** (`resolveSecret` uses the
  guest namespace) — hence the API's same-namespace rule (V9).

Why `Opaque` is the default: it makes **zero assumptions** about the workload
distribution. It works for k0s, kubeadm, RKE2, k3s, or a hand-rolled kubelet
enrollment, and it requires no privileged token-minting rights in the workload
cluster.

Cost, stated honestly: the token's lifetime is the user's problem. An expired token
means new cells fail at `Joining` with a timeout — surfaced as `Failed` +
`JoinTimeout`, plus a `BootstrapCredentialSuspect` reason on `Progressing` when
*every* new cell in a row fails to join while existing cells stay healthy. That
heuristic is the difference between "your token expired" and silence.

### `bootstrap.provider: KubeadmToken` (opt-in)

For kubeadm-style clusters the operator can remove the expiry footgun itself:
create a TTL'd `bootstrap.kubernetes.io/token` Secret in the workload cluster's
`kube-system` (a plain API write — no CLI), compute the discovery CA hash from the
kubeconfig's CA bundle, render the join command, and let the token expire. One
token per cell, TTL ≈ `bootstrap.readyTimeout × 2`, deleted after the cell reaches
`Joining`.

It costs `secrets` create/delete in `kube-system` on the inner cluster
(`-reconciliation.md` §6), which is a real privilege increase — which is exactly why
it is opt-in and not the default.

### Distribution matrix

| Distribution | The join blob | `Opaque` | `KubeadmToken` |
|---|---|---|---|
| kubeadm | `kubeadm join <ep> --token … --discovery-token-ca-cert-hash sha256:…` | yes | **yes** |
| k0s | `k0s install worker --token-file <base64 join blob>` (k0s-specific token, not a bootstrap token) | yes | no — needs `k0s token create` on the control plane |
| RKE2 / k3s | `curl … \| sh -` + `K3S_URL`/`K3S_TOKEN` (long-lived node token) | yes | no |

The lab covers both relevant cases: **dev is k0s** (`Opaque` only) and **ntx is
kubeadm** (`KubeadmToken` provable). Validate `Opaque` on dev first — it is the
default and the one that must work.

---

## 5. Why not delegate bootstrap to Cluster API in the MVP (and what shipped instead)

**This section is entirely historical.** It was written to argue against
building a CAPI provisioner for the MVP, before one existed. `provisioner:
ClusterAPI` has since shipped and is hardware-validated — see
`docs/clusterapi-cells.md`. Two things below were wrong even as a plan, not
just overtaken by events: it describes the mechanism as sizing a
**`MachineDeployment`**, but the shipped provisioner creates **one `Machine` +
`KubeSwiftMachine` per cell**, named after the cell — a `MachineDeployment`
generates its own Machine names, which would have broken the
cell-name-equals-Node-name identity the whole design rests on (see D8 in
`-overview.md` and `docs/clusterapi-cells.md`). Kept for the reasoning that is
still valid: why CAPI was, and remains, a *second* provisioner rather than a
replacement for `SwiftGuest`.

CAPI solves exactly this problem — a bootstrap provider produces the cloud-init
Secret, and `capi-kubeswift` already consumes it verbatim into a `SwiftSeedProfile`.
So the honest evaluation, as it stood before implementation:

**Blocking reason (still true):** Cluster API can only add Machines to a
**CAPI-managed** cluster. The target use case is *bring your own workload cluster*
(HAMi already installed, possibly not CAPI-managed at all). A CAPI-only design
cannot serve it, so CAPI is a **second provisioner**, never the only one (D8).

**Secondary reason (resolved):** at the time of writing, `capi-kubeswift` had
no GPU surface (verified then: zero `gpu` hits across `api/`, `internal/`,
`templates/`). That blocker is gone: `capi-kubeswift` v0.2.0 ships
`KubeSwiftMachine.spec.backend.swiftGuest.gpu` and `nodeName` placement.

**What CAPI buys when the workload cluster *is* CAPI-managed** — realized, not
just planned: `Machine`↔`Node` correlation via `providerID`, and the cluster's
own bootstrap provider supplying join credentials per cell
(`bootstrapConfigTemplateRef`, instantiated once per cell the way a
`MachineSet` does — see `docs/clusterapi-cells.md`). **Not** realized: node
drain on Machine deletion and rolling updates are not automatic — see
`docs/limitations.md`, which applies identically to both provisioners. The
`CellProvisioner` seam (`-reconciliation.md` §1) is what made adding the
provisioner additive rather than a rewrite.

---

## 6. Startup budget — MEASURED (2026-08-08, baked image, dev/boba, GTX 1080)

The estimate below was optimistic by roughly 2x, and the miss is almost entirely in
one stage the table did not name: cloning the 30 GiB root disk.

| Stage | Estimated | **Measured** |
|---|---|---|
| cell created → GPU allocated + launcher scheduled | 2–15 s | ~5 s |
| launcher → VM `Running` (incl. **30 GiB root-disk clone**) | 10–30 s | **~3 min** |
| VM `Running` → address reported (`Booting`) | 20–40 s | ~40 s |
| address → workload `Node Ready` (`Joining`) | 10–30 s | ~20 s |
| `Node Ready` → HAMi advertises the device | 10–60 s | ~35 s |
| **total, guest created → `Ready`** | ~1–3 min | **4 min 45 s** |

Install-at-boot (no baked driver) measured ~14 min, so baking is worth roughly 3x —
and the remaining cost is storage, not software. Shrinking the root disk or using a
copy-on-write clone strategy is where the next minute would come from, not from the
guest.

What the operator actually waits is longer than the cell startup: demand is noticed
on the pool's resync (up to `requeueSteady`, 2 min), so request → usable capacity was
**6 min 14 s** end to end.

`gpucell_cell_startup_seconds` records the 4:45 figure — from the outer object's
creation to the FIRST time the cell is Ready, so a later Ready after a regression is
not counted as a startup.

At ~5 minutes this sits exactly on the line the design drew for itself. Reactive
autoscaling stays worthwhile (the scale-up stabilization window defaults to 10 min
for precisely this reason), but a latency-sensitive pool should keep `minReplicas`
above zero and treat scale-to-zero as a cost decision, not a default.

---

## 6a. Two things the first live run corrected (2026-08-07)

**`networkRef` takes `{name, namespace}` only.** Earlier drafts of this design and
its sample showed a `kind: NetworkAttachmentDefinition` field. KubeSwift's strict
decoding rejects it outright. Fixed in `-api.md` and `config/samples/`.

**`cell.nodeIPFrom` names a KUBESWIFT interface, not a guest device.** Cloud-init
inside the guest cannot map `node` to `ens4`, so the template derives the address
by subnet instead:

```bash
NODE_IP=$(ip -4 -o addr show | awk '/10\.77\.0\./ {split($4,a,"/"); print a[1]; exit}')
```

That works, but it means the user hardcodes their cell subnet in the template. The
alternatives are worse today: the address is not known at render time (DHCP/IPAM
assign it after the guest exists), and neither is the MAC. A future
`{{ nodeMAC }}` would need KubeSwift to expose the guest-side MAC before boot.

Related, and more consequential: **KubeSwift v0.13.4 does not report a secondary
NAD interface's IP in `status.network.interfaces[]`** — it carries the MAC only
(measured; the guest genuinely has the address). The operator therefore treats
`nodeIPFrom` as an OBSERVATION field: it reports the routable address when
KubeSwift exposes one, falls back to the primary for the readiness gate, and
verifies which address the Node actually registered with against the inner
cluster, where the truth is. Gating on the routable address would park every
bridge-NAD cell in `Booting` forever.

## 7. Open bootstrap questions

1. **k0s token minting** — is there an API-only path (a `k0s` join token is not a
   kubeadm bootstrap token)? If not, `Opaque` remains the only k0s mode and the
   docs must say so plainly. *Verify on dev.*
2. **Node deletion rights** — when a cell is deleted, who removes the inner `Node`
   object? Design says the operator (`nodes: delete`). Confirm no distribution
   auto-reaps it, and that removing it does not disturb HAMi's scheduler cache.
3. **Preflight annotation** — writing `cells.kubeswift.io/gpu-preflight` from
   cloud-init requires either kubelet self-annotation (not permitted for arbitrary
   annotations) or a small in-guest one-shot with the join credential. Cheaper
   alternative: log-only, plus the controller's own `nvidia-smi`-free inference from
   HAMi's registration annotation. *Decide in Phase 2; log-only for Phase 1.*
4. **Driver ↔ workload CUDA alignment** — with strategy B this is an image-build
   policy. Document the supported matrix per image tag; do not encode it in the API.
