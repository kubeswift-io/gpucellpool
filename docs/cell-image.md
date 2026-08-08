# The cell image

A cell boots from a prebaked disk image — a GPU-capable Kubernetes worker with
the driver, container runtime and node binaries already installed, so joining
is thin cloud-init rather than a 5-15 minute install-at-boot. This page covers
building one and what it must contain.

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
validation record. It bakes Ubuntu Noble 24.04 + the NVIDIA proprietary driver
+ `nvidia-container-toolkit` + a k0s worker binary into a raw disk, under plain
QEMU with user-mode networking (no root, no libguestfs).

```bash
NVIDIA_DRIVER_PKG=nvidia-driver-570-server \
OUT=./build \
  hack/build-cell-image.sh
```

| Variable | Default | Notes |
|---|---|---|
| `OUT` | `./build` | output directory; produces `$OUT/gpu-worker-noble.raw` |
| `QEMU` | `/usr/bin/qemu-system-x86_64` | must be the **distribution** QEMU — a Kata-bundled build (sometimes first on `PATH` at `/opt/kata/bin`) is compiled without user-mode networking, so the guest gets no egress and the bake dies mid-`apt` with nothing obviously wrong |
| `NVIDIA_DRIVER_PKG` | `nvidia-driver-570-server` | see "driver selection" below |
| `DISK_SIZE` | `30G` | the validated size; sparse output is ~5.5 GiB |
| `MEM` / `CPUS` | `4096` / `4` | build-VM resources, not the cell's |

Output: a 30 GiB raw disk (~5.5 GiB sparse on disk), plus an in-guest
`/etc/gpucell-image-manifest` recording exactly what was installed (kernel,
driver version, k0s version) — read that file rather than assuming the script
defaults match what actually landed, since some of the pinned inputs (below)
resolve to a range, not an exact version.

**No GPU is needed at build time.** The NVIDIA driver builds via DKMS against
the image's own kernel; it only needs the physical GPU present at *cell boot*,
when DKMS's kernel module actually loads.

## Driver selection

Pascal (GTX 1080, the reference-lab GPU) needs the **proprietary** driver —
NVIDIA's open kernel modules are Turing-and-later only. `nvidia-driver-<N>
-server` is correct for Pascal; if your GPU is Turing or newer, an
`-open` variant is also valid, but the script defaults to `-server` because
it is the one validated on hardware.

The metapackage tracks a **branch, not a pin**: `nvidia-driver-570-server`
resolved to `580.173.02` on the validated build. For a reproducible image, set
`NVIDIA_DRIVER_PKG` to an exact package version; otherwise treat
`/etc/gpucell-image-manifest` inside the built image as the record of what you
actually shipped, not the variable you set.

## Publishing

The image is imported as a `SwiftImage` via an OCI artifact, using KubeSwift's
`swiftctl`:

```bash
swiftctl image publish ./build/gpu-worker-noble.raw \
  --to ghcr.io/your-org/gpu-worker-noble \
  --tag noble-570 \
  --chunk-size-mib 256
```

**Set `--chunk-size-mib 256`.** The 64 MiB default issues far more requests
for a disk this size than GitHub Container Registry's per-repository
secondary rate limit tolerates, and the push fails partway through with a 429
that looks unrelated to chunk size unless you already know to look for it.

Then reference it from a `SwiftImage`:

```yaml
apiVersion: image.kubeswift.io/v1alpha1
kind: SwiftImage
metadata:
  name: gpu-worker-noble-570
spec:
  format: raw               # this is the INPUT format — the raw disk above, not qcow2
  source:
    oci:
      ref: ghcr.io/your-org/gpu-worker-noble:noble-570
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
