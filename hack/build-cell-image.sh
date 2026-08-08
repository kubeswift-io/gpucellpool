#!/usr/bin/env bash
# Bake the gpucellpool cell image locally: Ubuntu Noble + NVIDIA proprietary
# driver + nvidia-container-toolkit + k0s worker, cloud-init reset, powered off.
#
# Output: $OUT/gpu-worker-noble.raw  (ready for `swiftctl image publish`)
#
# Runs under plain qemu+KVM with user-mode networking: no root, no libguestfs.
# The NVIDIA driver builds via DKMS against the image's own kernel, so no GPU is
# needed at build time — only at cell boot.
set -euo pipefail

OUT="${OUT:-/tmp/claude-1000/-home-wrkode-code-vmm-kubeswift-kubeswift/c6001214-37be-44cd-ae40-c65efde23337/scratchpad/cellpoc/build}"
# The DISTRO qemu, deliberately: a Kata build (which may come first in PATH at
# /opt/kata/bin) is compiled without user-mode networking, so the guest gets no
# egress and the bake dies mid-apt with nothing obviously wrong.
QEMU="${QEMU:-/usr/bin/qemu-system-x86_64}"
BIOS="${BIOS:-/usr/share/seabios/bios-256k.bin}"
BASE_URL="https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img"
# Pascal (GTX 1080) needs the PROPRIETARY driver: the open kernel modules are
# Turing+ only. Keep this pinned and recorded — it is part of the image contract.
NVIDIA_DRIVER_PKG="${NVIDIA_DRIVER_PKG:-nvidia-driver-570-server}"
DISK_SIZE="${DISK_SIZE:-30G}"
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
        linux-headers-\$(uname -r) dkms build-essential \\
        cloud-init qemu-guest-agent socat conntrack iptables

      # --- NVIDIA driver (proprietary: Pascal is not supported by the open
      #     kernel modules). DKMS builds against this image's kernel, so the
      #     cell must not upgrade its kernel without a rebake.
      apt-get install -y ${NVIDIA_DRIVER_PKG}

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
      cat > /etc/systemd/system/gpu-cell-preflight.service <<'UNIT'
      [Unit]
      Description=Log the GPU preflight result for this cell
      After=nvidia-cdi-generate.service
      [Service]
      Type=oneshot
      RemainAfterExit=yes
      ExecStart=/bin/bash -c 'n=\$(nvidia-smi -L 2>/dev/null | grep -c "^GPU" || true); echo "gpu-cell preflight: \${n} GPU(s)" | tee /run/gpu-cell-preflight'
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
        echo "nvidia-driver-pkg: ${NVIDIA_DRIVER_PKG}"
        echo "nvidia-driver-version: \$(dpkg -l | awk '/nvidia-dkms/ {print \$3; exit}')"
        echo "nvidia-container-toolkit: \$(dpkg -query -W -f='\${Version}' nvidia-container-toolkit 2>/dev/null)"
        echo "k0s: \$(k0s version)"
      } > /etc/gpucell-image-manifest

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
  -drive file=cell.qcow2,if=virtio,format=qcow2 \
  -drive file=seed.iso,if=virtio,format=raw,readonly=on \
  -netdev user,id=n0 -device virtio-net-pci,netdev=n0 \
  -nographic -serial file:bake-console.log -monitor none -display none

echo "[5/5] converting to raw"
qemu-img convert -p -f qcow2 -O raw cell.qcow2 gpu-worker-noble.raw
ls -lh gpu-worker-noble.raw
grep -E "^(built|kernel|nvidia|k0s)" -a bake-console.log | tail -8 || true
echo "BAKE DONE"
