#!/usr/bin/env bash
# Bake the gpucellpool cell image locally: Ubuntu Noble + NVIDIA proprietary
# compute driver + nvidia-container-toolkit + k0s worker, cloud-init reset,
# free space trimmed, powered off.
#
# Output: $OUT/gpu-worker-noble.raw  (ready for `swiftctl image publish`)
#
# Runs under plain qemu+KVM with user-mode networking: no root, no libguestfs.
# No GPU is needed at build time — only at cell boot.
set -euo pipefail

OUT="${OUT:-$PWD/build}"
# The DISTRO qemu, deliberately: a Kata build (which may come first in PATH at
# /opt/kata/bin) is compiled without user-mode networking, so the guest gets no
# egress and the bake dies mid-apt with nothing obviously wrong.
QEMU="${QEMU:-/usr/bin/qemu-system-x86_64}"
BIOS="${BIOS:-/usr/share/seabios/bios-256k.bin}"
BASE_URL="https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img"
# Pascal (GTX 1080) needs the PROPRIETARY driver: the open kernel modules are
# Turing+ only, and 580 is the last branch that supports Pascal. The published
# reference image is on it; the manifest records the exact version installed.
NVIDIA_BRANCH="${NVIDIA_BRANCH:-580-server}"
# compute (default): the headless compute driver with Ubuntu's PREBUILT, signed
#   kernel module for this image's kernel — no DKMS, no kernel headers, no compiler
#   and no OpenGL/Vulkan/mesa stack. That is everything CUDA and HAMi-core use.
# full: the desktop driver metapackage built via DKMS, as the image was first
#   baked. Use it only if workloads need graphics or video (NVENC/NVDEC) — it adds
#   ~1.2 GiB to every cell's disk, which is copied for every cell that starts.
NVIDIA_FLAVOUR="${NVIDIA_FLAVOUR:-compute}"
NVIDIA_DRIVER_PKG="${NVIDIA_DRIVER_PKG:-nvidia-driver-${NVIDIA_BRANCH}}"   # full flavour only
# The BAKED size, not the cell's: a cell's disk comes from its SwiftGuestClass
# rootDisk, and cloud-init grows the partition to it at first boot (verified: baked
# at 12G, a 30Gi class boots with a 29G root). What the baked size does decide is
# how many fixed-size windows the data lands in when the image is published — the
# same 2.85 GiB stored 9.25 GiB at 30G and 6.50 GiB at 12G.
DISK_SIZE="${DISK_SIZE:-12G}"
MEM="${MEM:-4096}"
CPUS="${CPUS:-4}"

mkdir -p "$OUT"
cd "$OUT"

echo "[1/5] fetching base image"
[ -f noble-base.img ] || wget -q --show-progress -O noble-base.img "$BASE_URL"

echo "[2/5] preparing working disk (${DISK_SIZE})"
rm -f cell.qcow2
qemu-img convert -f qcow2 -O qcow2 noble-base.img cell.qcow2
qemu-img resize cell.qcow2 "$DISK_SIZE"

echo "[3/5] writing cloud-init seed"
cat > user-data <<EOF
#cloud-config
hostname: cell-image-build
growpart:
  mode: auto
  devices: ['/']
package_update: true
write_files:
  - path: /usr/local/bin/bake.sh
    permissions: "0755"
    content: |
      #!/bin/bash
      # Provision a GPU-capable Kubernetes worker image. Every step logs to
      # /var/log/bake.log; the marker file at the end is the success signal.
      set -xeuo pipefail
      export DEBIAN_FRONTEND=noninteractive

      apt-get update
      apt-get install -y --no-install-recommends \\
        ca-certificates curl gnupg jq \\
        cloud-init qemu-guest-agent socat conntrack iptables

      # --- NVIDIA driver (proprietary: Pascal is not supported by the open
      #     kernel modules). The module is tied to this image's kernel either
      #     way, so the cell must not upgrade its kernel without a rebake.
      #
      #     Measured on the first published image: the desktop metapackage pulls
      #     in libnvidia-gl (520 MiB), mesa and LLVM (~270 MiB) for an OpenGL/
      #     Vulkan stack a compute node never loads, and building the module via
      #     DKMS leaves its source, the kernel headers and a compiler toolchain
      #     (~390 MiB) behind for good.
      if [ "${NVIDIA_FLAVOUR}" = compute ]; then
        KMOD="linux-modules-nvidia-${NVIDIA_BRANCH}-\$(uname -r)"
        if apt-cache show "\$KMOD" >/dev/null 2>&1; then
          # The four packages nvidia-headless-no-dkms would pull in, named
          # directly: that metapackage also hard-depends on the module SOURCE
          # (118 MiB) that only a DKMS build needs. Naming them also marks them
          # manually installed, so no later autoremove can sweep them away.
          apt-get install -y --no-install-recommends "\$KMOD" \\
            nvidia-kernel-common-${NVIDIA_BRANCH} libnvidia-compute-${NVIDIA_BRANCH} \\
            nvidia-compute-utils-${NVIDIA_BRANCH} libnvidia-cfg1-${NVIDIA_BRANCH} \\
            nvidia-utils-${NVIDIA_BRANCH}
          echo prebuilt > /etc/gpucell-nvidia-module-source
        else
          # Ubuntu publishes prebuilt modules shortly after each kernel; a very
          # new base image can be ahead of them. Build it instead, and say so in
          # the manifest rather than silently producing a different image.
          echo "WARNING: no \$KMOD in the archive; building the module with DKMS"
          apt-get install -y --no-install-recommends \\
            linux-headers-\$(uname -r) dkms build-essential \\
            nvidia-dkms-${NVIDIA_BRANCH} \\
            nvidia-kernel-common-${NVIDIA_BRANCH} libnvidia-compute-${NVIDIA_BRANCH} \\
            nvidia-compute-utils-${NVIDIA_BRANCH} libnvidia-cfg1-${NVIDIA_BRANCH} \\
            nvidia-utils-${NVIDIA_BRANCH}
          echo dkms > /etc/gpucell-nvidia-module-source
        fi
      else
        apt-get install -y --no-install-recommends \\
          linux-headers-\$(uname -r) dkms build-essential
        apt-get install -y ${NVIDIA_DRIVER_PKG}
        echo dkms > /etc/gpucell-nvidia-module-source
      fi

      # --- NVIDIA container toolkit (libnvidia-container + nvidia-ctk)
      curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey \\
        | gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg
      curl -fsSL https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \\
        | sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' \\
        > /etc/apt/sources.list.d/nvidia-container-toolkit.list
      apt-get update
      apt-get install -y nvidia-container-toolkit

      # --- k0s worker binary (dev's inner cluster is k0s; the join happens at
      #     cell first boot from the pool's bootstrap secret, not here).
      curl -sSLf https://get.k0s.sh | sh
      k0s version > /etc/gpucell-k0s-version

      # --- k0s containerd drop-in: nvidia runtime as default + CDI. HAMi needs
      #     the nvidia runtime to inject its interposition library.
      mkdir -p /etc/k0s/containerd.d
      # containerd 2.x config, which is what k0s >= 1.34 ships. The v1 CRI plugin
      # form ("version = 2" with io.containerd.grpc.v1.cri) is REJECTED by k0s at
      # pre-flight -- "unsupported configuration version: expected 3, got 2" -- so
      # the worker never starts and the cell never joins. Getting this wrong is
      # silent from the outside: the VM boots, sshd runs, and nothing registers.
      cat > /etc/k0s/containerd.d/nvidia.toml <<'TOML'
      [plugins."io.containerd.cri.v1.runtime"]
        enable_cdi = true
        cdi_spec_dirs = ["/etc/cdi", "/var/run/cdi"]
      [plugins."io.containerd.cri.v1.runtime".containerd]
        default_runtime_name = "nvidia"
      [plugins."io.containerd.cri.v1.runtime".containerd.runtimes.nvidia]
        runtime_type = "io.containerd.runc.v2"
      [plugins."io.containerd.cri.v1.runtime".containerd.runtimes.nvidia.options]
        BinaryName = "/usr/bin/nvidia-container-runtime"
        SystemdCgroup = true
      TOML

      # --- CDI spec generation must happen where the GPU exists: on the cell,
      #     at first boot, before containerd starts.
      cat > /etc/systemd/system/nvidia-cdi-generate.service <<'UNIT'
      [Unit]
      Description=Generate NVIDIA CDI spec for the passthrough GPU
      After=local-fs.target
      Before=k0sworker.service containerd.service
      ConditionPathExists=/usr/bin/nvidia-ctk
      [Service]
      Type=oneshot
      RemainAfterExit=yes
      ExecStartPre=/bin/mkdir -p /var/run/cdi
      ExecStart=/bin/bash -c '/usr/bin/nvidia-ctk cdi generate --output=/var/run/cdi/nvidia.yaml || true'
      [Install]
      WantedBy=multi-user.target
      UNIT
      systemctl enable nvidia-cdi-generate.service

      # --- GPU preflight: log-only on purpose. A node that joins WITHOUT a GPU
      #     is diagnosable (the pool parks it in AwaitingGPUCapacity); a node
      #     that refuses to join is opaque.
      #     \$\${n}, not \${n}: systemd expands \${VAR} in ExecStart itself before
      #     bash runs, so the unescaped form always recorded a BLANK count —
      #     "gpu-cell preflight:  GPU(s)" — on every image before this fix.
      cat > /etc/systemd/system/gpu-cell-preflight.service <<'UNIT'
      [Unit]
      Description=Log the GPU preflight result for this cell
      After=nvidia-cdi-generate.service
      [Service]
      Type=oneshot
      RemainAfterExit=yes
      ExecStart=/bin/bash -c 'n=\$(nvidia-smi -L 2>/dev/null | grep -c "^GPU" || true); echo "gpu-cell preflight: \$\${n} GPU(s)" | tee /run/gpu-cell-preflight'
      [Install]
      WantedBy=multi-user.target
      UNIT
      systemctl enable gpu-cell-preflight.service

      systemctl enable qemu-guest-agent || true

      # --- record what went in; the image contract is only useful if pinned
      {
        echo "built: \$(date -Is)"
        echo "base: noble-server-cloudimg-amd64"
        echo "kernel: \$(uname -r)"
        echo "nvidia-flavour: ${NVIDIA_FLAVOUR}"
        echo "nvidia-branch: ${NVIDIA_BRANCH}"
        echo "nvidia-module-source: \$(cat /etc/gpucell-nvidia-module-source)"
        echo "nvidia-driver-version: \$(dpkg-query -W -f='\${Version}' nvidia-utils-${NVIDIA_BRANCH} 2>/dev/null)"
        echo "nvidia-container-toolkit: \$(dpkg-query -W -f='\${Version}' nvidia-container-toolkit 2>/dev/null)"
        echo "k0s: \$(k0s version)"
      } > /etc/gpucell-image-manifest

      # --- snapd ships in the cloud image and nothing on a cell uses it (127 MiB)
      apt-get purge -y snapd || true
      rm -rf /snap /var/snap /var/lib/snapd /var/cache/snapd
      apt-get autoremove --purge -y

      # --- reset identity so the image can be re-seeded per cell
      apt-get clean
      rm -rf /var/lib/apt/lists/*
      cloud-init clean --logs --seed || true
      rm -f /etc/machine-id /var/lib/dbus/machine-id
      touch /etc/machine-id
      rm -f /etc/ssh/ssh_host_*
      truncate -s 0 /root/.bash_history 2>/dev/null || true

      touch /var/lib/gpucell-image-ready
      sync
      # Release every freed block. Deleting a file only unlinks it: the old bytes
      # stay in the disk image and are published, imported and copied to every
      # cell exactly like live data. Measured on the first published image,
      # 2.2 GiB of its 5.7 GiB of non-zero data was freed space — apt downloads
      # and build leftovers. Needs discard=unmap on the build drive (below).
      fstrim -av
runcmd:
  - [ bash, -c, "/usr/local/bin/bake.sh > /var/log/bake.log 2>&1; echo BAKE_EXIT=\$? >> /var/log/bake.log; cp /var/log/bake.log /boot/bake.log; sync; poweroff" ]
EOF
printf 'instance-id: cell-image-build\nlocal-hostname: cell-image-build\n' > meta-data
rm -f seed.iso
genisoimage -quiet -output seed.iso -volid cidata -joliet -rock user-data meta-data

echo "[4/5] running the bake VM (serial log: $OUT/bake-console.log)"
: > bake-console.log
"$QEMU" \
  -machine q35,accel=kvm -cpu host -smp "$CPUS" -m "$MEM" \
  -bios "$BIOS" \
  -drive file=cell.qcow2,if=virtio,format=qcow2,discard=unmap,detect-zeroes=unmap \
  -drive file=seed.iso,if=virtio,format=raw,readonly=on \
  -netdev user,id=n0 -device virtio-net-pci,netdev=n0 \
  -nographic -serial file:bake-console.log -monitor none -display none

echo "[5/5] converting to raw"
qemu-img convert -p -f qcow2 -O raw cell.qcow2 gpu-worker-noble.raw
ls -lh gpu-worker-noble.raw
grep -E "^(built|kernel|nvidia|k0s)" -a bake-console.log | tail -8 || true
echo "BAKE DONE"
