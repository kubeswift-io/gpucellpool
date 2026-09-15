# The cell image

A cell boots from a prebaked disk image — a GPU-capable Kubernetes worker with
the driver, container runtime and node binaries already installed, so joining
is thin cloud-init rather than a 5-15 minute install-at-boot.

## Use the published reference image

You do not have to bake one to try this out. The image the project's own
hardware validation ran on is published as a **public** OCI artifact, so this
needs no pull secret:

```bash
kubectl apply -f config/samples/swiftimage-cell.yaml   # namespace gpu-cells
kubectl wait swiftimage/gpu-worker-noble -n gpu-cells --for=jsonpath='{.status.phase}'=Ready --timeout=15m
```

Then reference it from your pool: `imageRef: {name: gpu-worker-noble}`.

It is a reference image, not a universal one, and two things pin it:

| | |
|---|---|
| driver **580.173.02**, the *proprietary* branch | chosen for a Pascal card (GTX 1080); the open kernel modules are Turing and newer only. On newer hardware you may want `-open` instead, which means baking your own |
| node binaries are **k0s** | if your workload cluster is kubeadm, RKE2 or k3s, this image still has the driver and container toolkit you need but not your node binaries. Either install them from the join cloud-init (slower first boot, no rebake — see `config/samples/cell-join-secret.yaml`) or bake for your distribution |

It pulls ~6 GiB into a 12 GiB raw disk holding 2.85 GiB of data, and each cell
grows it to the size its `SwiftGuestClass` asks for at first boot. See "Why cell
startup is mostly a disk copy" below for what that size costs and how to keep it
down if you bake your own.

## What the image must satisfy

| Requirement | Why |
|---|---|
| NVIDIA driver | HAMi-core needs >= 440; match it to the CUDA version your workload images expect |
| `nvidia-container-toolkit` + CDI enabled | HAMi's interposition library needs the nvidia container runtime; CDI generation must happen on the cell itself, at first boot — the GPU is not present until then |
| your distribution's node binaries | kubelet + the container runtime for k0s/kubeadm/RKE2/k3s, whichever the workload cluster expects |
| a pinned upstream DNS resolver | once the inner CNI programs the node, the pod-netns resolver stops answering — see `docs/networking.md` |
| an SSH key in the join template | the bake resets `machine-id` and host keys, and KubeSwift's Cloud Hypervisor drops the serial console when nothing is attached — without SSH a broken cell is undiagnosable |

Nothing above is HAMi- or KubeSwift-specific configuration baked into the
image beyond what any GPU Kubernetes worker needs. HAMi's own DaemonSets
(device plugin / scheduler extension) run in the *workload* cluster, not on
the cell.

## `hack/build-cell-image.sh` — the reference build

The script is a first-class, documented path — not a one-off used for the
validation record. It bakes Ubuntu Noble 24.04 + the NVIDIA proprietary compute
driver + `nvidia-container-toolkit` + a k0s worker binary into a raw disk, under
plain QEMU with user-mode networking (no root, no libguestfs). A bake takes about
five minutes on a workstation with KVM.

```bash
OUT=./build hack/build-cell-image.sh
```

| Variable | Default | Notes |
|---|---|---|
| `OUT` | `./build` | output directory; produces `$OUT/gpu-worker-noble.raw` |
| `QEMU` | `/usr/bin/qemu-system-x86_64` | must be the **distribution** QEMU — a Kata-bundled build (sometimes first on `PATH` at `/opt/kata/bin`) is compiled without user-mode networking, so the guest gets no egress and the bake dies mid-`apt` with nothing obviously wrong |
| `NVIDIA_BRANCH` | `580-server` | see "driver selection" below |
| `NVIDIA_FLAVOUR` | `compute` | `compute` or `full` — see "driver selection" below |
| `NVIDIA_DRIVER_PKG` | `nvidia-driver-${NVIDIA_BRANCH}` | the metapackage the `full` flavour installs; ignored by `compute` |
| `DISK_SIZE` | `12G` | the **baked** size, not the cell's — see "Why cell startup is mostly a disk copy" |
| `MEM` / `CPUS` | `4096` / `4` | build-VM resources, not the cell's |

Output: a 12 GiB raw disk holding ~2.85 GiB of data, plus an in-guest
`/etc/gpucell-image-manifest` recording exactly what was installed (kernel,
driver flavour and version, where the kernel module came from, k0s version) —
read that file rather than assuming the script defaults match what actually
landed.

**No GPU is needed at build time.** The kernel module is Ubuntu's prebuilt one
for the image's kernel; the physical GPU only has to be present at *cell boot*,
when the module loads.

## Driver selection

Pascal (GTX 1080, the reference-lab GPU) needs the **proprietary** driver —
NVIDIA's open kernel modules are Turing-and-later only — and **580 is the last
branch that supports Pascal**, so `NVIDIA_BRANCH=580-server` is the default. On
Turing or newer you can move to a later branch.

`NVIDIA_FLAVOUR` decides what else comes with it:

| Flavour | Installs | Use it when |
|---|---|---|
| `compute` (default) | the compute libraries (`libcuda`, `libnvidia-ml`), `nvidia-smi`, and Ubuntu's **prebuilt, signed** kernel module for the image's kernel | workloads use CUDA — which is what HAMi shares |
| `full` | the desktop driver metapackage, with the module built by DKMS | workloads need graphics or video (OpenGL, Vulkan, NVENC/NVDEC) |

`full` is what the first published image used. Measured, it adds ~1.2 GiB to
every cell's disk: `libnvidia-gl` (520 MiB) and the mesa/LLVM stack a compute
node never loads, plus the module source, kernel headers and a compiler that
DKMS needs only once. If Ubuntu has not yet published a prebuilt module for a
very new base image's kernel, `compute` falls back to DKMS and records
`nvidia-module-source: dkms` in the manifest rather than silently producing a
different image.

The module is tied to the image's kernel either way, so a cell must not upgrade
its kernel without a rebake.

## Why cell startup is mostly a disk copy

Every cell gets its own copy of the image's root disk, made when the cell is
created. On the reference lab that copy is most of the time to Ready, and what
it copies is whatever the image import **allocated** — which is not the same as
the data in the image. Three things decide it, measured on the same node on the
same day:

| Image | Data | Import allocates | Per-cell clone |
|---|---|---|---|
| first published image (30 GiB disk, `full` driver, no trim) | 5.69 GiB | 12 GiB | 176 s |
| this build (12 GiB disk, `compute` driver, trimmed) | 2.85 GiB | 5.9 GiB | 109 s |
| this build, imported sparsely by KubeSwift | 2.85 GiB | 2.9 GiB | 76 s |

- **Content.** The `compute` flavour and removing `snapd` take the data from
  5.69 to 2.85 GiB.
- **Freed space.** Deleting a file inside the build only unlinks it; its old
  bytes stay in the disk and are published, imported and copied like live data.
  The first published image carried 2.2 GiB of them. The build now trims free
  space before converting the disk.
- **How the image is stored.** A published image is split into fixed-size
  windows, and a window with any data at all is stored whole. KubeSwift then
  writes each stored window out in full, zeros included, so the import
  allocates far more than the data. A smaller baked disk puts the same data in
  fewer windows — which is why `DISK_SIZE` defaults to 12G — and a KubeSwift
  that writes those zeros as holes
  ([kubeswift-io/kubeswift#599](https://github.com/kubeswift-io/kubeswift/pull/599))
  allocates only the data itself.

The copy itself does not go away: each cell still gets the whole image. Sharing
one read-only base between cells instead is tracked upstream in
[kubeswift-io/kubeswift#600](https://github.com/kubeswift-io/kubeswift/issues/600).

`DISK_SIZE` does not limit the cell: a cell's disk comes from its
`SwiftGuestClass` `rootDisk.size`, and cloud-init grows the partition to it at
first boot (verified: baked at 12G, a 30Gi class boots with a 29G root).

### `cloneStrategy: copy` vs `snapshot`

KubeSwift can also create each cell's disk from a CSI `VolumeSnapshot` of the
image instead of copying it. Whether that is faster depends entirely on the
storage driver: on a copy-on-write driver (Ceph RBD and most cloud block
storage) a snapshot clone is near-instant, but **Longhorn restores a snapshot by
copying it**. Measured on Longhorn v1.12, `snapshot` took 306 s against 196 s
for `copy` — the restore spent 4m16s before the volume would even attach, and
then paid a resize on top. Use `copy` on Longhorn; try `snapshot` on a driver
that clones by reference. Two things to know before switching: `cloneStrategy`
cannot be changed once an image has been imported (re-import it instead), and it
needs the `snapshot.storage.k8s.io` CRDs, the snapshot controller and a
`VolumeSnapshotClass` — Longhorn ships only its CSI snapshotter sidecar, not
those.

## Publishing

The image is imported as a `SwiftImage` via an OCI artifact, using KubeSwift's
`swiftctl`:

```bash
swiftctl image publish ./build/gpu-worker-noble.raw \
  --to ghcr.io/your-org/gpu-worker-noble \
  --tag noble-580
```

Leave `--chunk-size-mib` at its default: `swiftctl` scales it to the disk size
(128 MiB for the 12 GiB image). Setting it by hand only matters in two cases.
Too small for the disk, and a push to GitHub Container Registry issues enough
requests to trip its per-repository secondary rate limit, failing partway
through with a 429 that looks unrelated to chunk size. Too large, and every
window with a little data in it is stored whole — the reference image stores
5.9 GiB at 128 MiB and 6.5 GiB at 256 MiB for the same 2.85 GiB of data.

Then reference it from a `SwiftImage`:

```yaml
apiVersion: image.kubeswift.io/v1alpha1
kind: SwiftImage
metadata:
  name: gpu-worker-noble-580
spec:
  format: raw               # this is the INPUT format — the raw disk above, not qcow2
  rootDisk: {size: 12Gi}    # at least the baked DISK_SIZE; cells get their size from the guest class
  source:
    oci:
      repository: ghcr.io/your-org/gpu-worker-noble
      tag: noble-580
```

Getting `spec.format` wrong is a trap independent of this script: if you ever
import a **qcow2** cloud image (e.g. Ubuntu's stock cloud image) instead of
this script's raw output, `spec.format` must say `qcow2`. Declaring `raw` for
a qcow2 source skips conversion and hands Cloud Hypervisor a file it reads as
raw, failing fast during import ("Failed to get refcount").

## Alternatives to a prebaked image

Two other strategies exist and are documented in
`docs/design/gpucellpool-bootstrap.md` §2 for completeness, but neither is
implemented or recommended over the baked image:

- **install-at-boot** — a generic cloud image, driver installed by cloud-init
  on every boot. Measured ~14 minutes to Ready versus ~4m45s baked; not worth
  it unless you cannot maintain a custom image at all.
- **driver from the workload cluster** (a GPU-operator-style driver container,
  no driver baked into the cell) — the better long-term answer once
  driver-version churn across several workload clusters costs more than the
  extra boot minutes, but it needs no API change: it is just a different
  `imageRef`. Nothing in GPUCellPool prevents building this yourself.
