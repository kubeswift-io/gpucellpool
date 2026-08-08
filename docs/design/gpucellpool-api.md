# GPUCellPool — v1alpha1 API

> Design record, written before implementation (2026-07-30) and not updated
> since — `spec.autoscaling` and `cell.clusterAPI` are absent below because
> they were designed after this file was written, and the shipped status
> fields (`status.desiredReplicas`, `status.demand`, `status.cellDeviceShape`)
> are likewise missing. **For the current API, read `docs/api-reference.md`
> instead** — it is generated from the Go types and the validating webhook.
> This file remains for the field-contract *reasoning* (§4, §8).

> One CRD. Cells are owned `SwiftGuest`s, not a second kind (D1). The cell's VM
> shape is an **opaque `SwiftGuestSpec` passthrough** so this API never chases
> KubeSwift's (D4); GPU, identity, bootstrap and node enrollment are first-class
> because the operator owns their semantics.
>
> Companion: `gpucellpool-overview.md` (decisions D1–D10). Date: 2026-07-30.

---

## 1. Identity of the resource

```
group    cells.kubeswift.io          # its own group: not swift.*, not gpu.*
version  v1alpha1
kind     GPUCellPool                 # namespaced
short    cellpool
scale    .spec.replicas → .status.replicas  (HPA seam, per D2)
```

Namespaced, because everything it references is namespaced: the workload-cluster
credential Secret, the join Secret, the `SwiftGuest`s and `ResourceClaimTemplate`s
it creates or points at. A pool never crosses namespaces (§4, rule V9).

---

## 2. Minimal spec (the shape most users write)

```yaml
apiVersion: cells.kubeswift.io/v1alpha1
kind: GPUCellPool
metadata:
  name: inference-l40s
  namespace: gpu-cells
spec:
  replicas: 2

  cell:
    guestTemplate:                       # verbatim SwiftGuestSpec (opaque)
      guestClassRef: {name: gpu-worker-32c-128g}
      imageRef:      {name: gpu-worker-noble-570}
    gpu:
      count: 1
      backend: DRA
      dra:
        resourceClaimTemplateName: single-vfio-gpu

  bootstrap:
    joinSecretRef: {name: inference-cluster-join}

  workloadCluster:
    kubeconfigSecretRef: {name: inference-cluster-kubeconfig}
    node:
      labels:
        gpu: "on"                        # HAMi's gate — declared by the USER, not hardcoded
        gpu-cell.kubeswift.io/pool: inference-l40s
```

Everything else has a default. `guestClassRef` + `imageRef` carry CPU, memory,
root-disk size and the guest OS — already existing KubeSwift objects, not
re-declared here (principle 9.1).

## 3. Full spec

```yaml
spec:
  replicas: 2

  cell:
    provisioner: SwiftGuest              # SwiftGuest | ClusterAPI  [D8]
    guestTemplate:                       # opaque SwiftGuestSpec; see §4 contract
      guestClassRef: {name: gpu-worker-32c-128g}
      imageRef:      {name: gpu-worker-noble-570}
      storage: {storageClassName: longhorn-r1, accessMode: ReadWriteOnce}
      interfaces:                        # the validated multi-node worker shape
        - {name: mgmt, primary: true}
        - {name: node, networkRef: {name: cell-udn}}   # {name,namespace} only
      topologySpreadConstraints: [...]    # or use spreadPolicy below
    gpu:
      count: 1                           # v1alpha1: must be 1
      backend: DRA                       # DRA | Native
      dra:
        resourceClaimTemplateName: single-vfio-gpu   # XOR resourceClaimName
        requestName: gpu                 # default "gpu"
        tier: pcie                       # v1alpha1: must be pcie
        hugepages: "1Gi"
      # native:
      #   gpuProfileRef: {name: single-pcie-gpu}
    spreadPolicy: Spread                 # Pack | Spread (hostname) — as SwiftGuestPool
    nodeIPFrom: node                     # which template interface carries the node IP

  bootstrap:
    provider: Opaque                     # Opaque | KubeadmToken   [see -bootstrap.md]
    joinSecretRef: {name: inference-cluster-join}   # required for Opaque
    hostname: CellName                   # CellName (default) | None
    readyTimeout: 15m                    # Booting+Joining budget before Failed

  workloadCluster:
    kubeconfigSecretRef: {name: inference-cluster-kubeconfig, key: value}
    node:
      labels: {gpu: "on"}
      taints:
        - {key: gpu-cell.kubeswift.io/dedicated, value: inference, effect: NoSchedule}
      annotations: {}

  capacity:
    provider: HAMi                       # HAMi (only implementation)  [D5]
    hami:
      mode: DevicePlugin                 # DevicePlugin | DRA
      expectedDevicesPerCell: 1          # default = cell.gpu.count
      deviceClassName: hami-gpu          # DRA mode only
      # valuesRef: NOT part of this API — HAMi config belongs to HAMi (principle 9.2)
    readyTimeout: 5m                     # Joining→Ready budget for the GPU to surface

  deletion:
    policy: Drain                        # Drain | Force  (pool deletion only)
    drainTimeout: 10m
```

Deliberately **absent**: any HAMi Helm value, any HAMi resource name, any
fraction/memory/cores field, any workload field, `min`/`max` (D2), and any
duplicate of a `SwiftGuestSpec` field.

---

## 4. The `guestTemplate` contract

`cell.guestTemplate` is `x-kubernetes-preserve-unknown-fields: true` — an opaque
`SwiftGuestSpec`. Three field classes, enforced by the validating webhook so a
mistake is a rejection and never a silent override:

| class | fields | behaviour |
|---|---|---|
| **operator-owned** | `seedProfileRef`, `gpuProfileRef`, `gpuResourceClaim`, `nodeName`, `migration`, `runPolicy` | rejected if set by the user; the controller writes them |
| **denied** | `kernelRef`, `cloneFromSnapshot`, `osType: windows`, `vhostUserDevices`, `filesystems`, `dataDiskRef*` with a GPU‑incompatible mode | rejected with the KubeSwift reason quoted (GPU is disk-boot; virtiofs/vhost-user are rejected by KubeSwift itself) |
| **passthrough** | everything else (`guestClassRef`, `imageRef`, `storage`, `interfaces`, `network`, `topologySpreadConstraints`, `guestAgent`, …) | copied verbatim into every cell guest |

Rendering a cell guest = `guestTemplate` **+** operator-owned overlay:

```
spec.seedProfileRef        = <cell>-seed              (created per cell)
spec.gpuResourceClaim      = from cell.gpu.dra        (or gpuProfileRef from .native)
spec.migration.enabled     = false                    (cells are replaced, not migrated)
spec.runPolicy             = Always
metadata.name              = <pool>-<index>
metadata.labels            = cells.kubeswift.io/pool=<pool>
                             cells.kubeswift.io/cell-index=<index>
metadata.annotations       = cells.kubeswift.io/template-hash=<hash>
```

A template change bumps the hash; v1alpha1 does **not** roll cells automatically
(`Updated=False`, reason `TemplateChanged`, operator recreates) — rolling updates
of a GPU worker require inner-drain sequencing, which is Phase 4 work.

---

## 5. Go type sketch

```go
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas
// +kubebuilder:resource:scope=Namespaced,shortName=cellpool
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyCells`
// +kubebuilder:printcolumn:name="GPUs",type=integer,JSONPath=`.status.physicalCapacity.gpus`
// +kubebuilder:printcolumn:name="GPUMemFree",type=string,JSONPath=`.status.workloadCapacity.gpuMemory.available`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type GPUCellPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              GPUCellPoolSpec   `json:"spec"`
	Status            GPUCellPoolStatus `json:"status,omitempty"`
}

type GPUCellPoolSpec struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	Replicas int32 `json:"replicas"`

	Cell            CellSpec            `json:"cell"`
	Bootstrap       BootstrapSpec       `json:"bootstrap"`
	WorkloadCluster WorkloadClusterSpec `json:"workloadCluster"`
	// +optional
	Capacity CapacitySpec `json:"capacity,omitempty"`
	// +optional
	Deletion *DeletionSpec `json:"deletion,omitempty"`
}

type CellSpec struct {
	// +kubebuilder:validation:Enum=SwiftGuest;ClusterAPI
	// +kubebuilder:default=SwiftGuest
	Provisioner string `json:"provisioner,omitempty"`

	// GuestTemplate is a verbatim SwiftGuestSpec. Opaque on purpose (D4): this
	// operator must never mirror KubeSwift's API surface. See §4 for the
	// operator-owned / denied / passthrough field contract.
	// +kubebuilder:pruning:PreserveUnknownFields
	GuestTemplate runtime.RawExtension `json:"guestTemplate"`

	GPU CellGPUSpec `json:"gpu"`

	// +kubebuilder:validation:Enum=Pack;Spread
	// +kubebuilder:default=Spread
	SpreadPolicy string `json:"spreadPolicy,omitempty"`

	// NodeIPFrom names the guestTemplate interface whose address the kubelet
	// registers as --node-ip. Empty = the primary interface.
	// +optional
	NodeIPFrom string `json:"nodeIPFrom,omitempty"`
}

type CellGPUSpec struct {
	// +kubebuilder:validation:Enum=1
	// +kubebuilder:default=1
	Count int32 `json:"count,omitempty"`
	// +kubebuilder:validation:Enum=DRA;Native
	// +kubebuilder:default=DRA
	Backend string `json:"backend,omitempty"`
	// +optional
	DRA *CellGPUDRASpec `json:"dra,omitempty"`
	// +optional
	Native *CellGPUNativeSpec `json:"native,omitempty"`
}

type CellGPUDRASpec struct {
	// Exactly one of the two claim references (mirrors KubeSwift's own rule).
	// +optional
	ResourceClaimTemplateName string `json:"resourceClaimTemplateName,omitempty"`
	// +optional
	ResourceClaimName string `json:"resourceClaimName,omitempty"`
	// +kubebuilder:default=gpu
	RequestName string `json:"requestName,omitempty"`
	// +kubebuilder:validation:Enum=pcie
	// +kubebuilder:default=pcie
	Tier string `json:"tier,omitempty"`
	// +optional
	Hugepages string `json:"hugepages,omitempty"`
}

type CellGPUNativeSpec struct {
	GPUProfileRef corev1.LocalObjectReference `json:"gpuProfileRef"`
}

type BootstrapSpec struct {
	// +kubebuilder:validation:Enum=Opaque;KubeadmToken
	// +kubebuilder:default=Opaque
	Provider string `json:"provider,omitempty"`
	// JoinSecretRef holds the cloud-init user-data used verbatim (Opaque mode).
	// Required for Opaque; the token never appears in this CR.
	// +optional
	JoinSecretRef *corev1.LocalObjectReference `json:"joinSecretRef,omitempty"`
	// +kubebuilder:validation:Enum=CellName;None
	// +kubebuilder:default=CellName
	Hostname string `json:"hostname,omitempty"`
	// +kubebuilder:default="15m"
	ReadyTimeout metav1.Duration `json:"readyTimeout,omitempty"`
}

type WorkloadClusterSpec struct {
	KubeconfigSecretRef corev1.LocalObjectReference `json:"kubeconfigSecretRef"`
	// +kubebuilder:default=value
	Key string `json:"key,omitempty"`
	// +optional
	Node *WorkloadNodeSpec `json:"node,omitempty"`
}

type WorkloadNodeSpec struct {
	// Labels applied to the cell's inner Node. HAMi's gate (gpu=on) is declared
	// HERE by the user — the operator applies labels it does not interpret.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`
	// +optional
	Taints []corev1.Taint `json:"taints,omitempty"`
}

type CapacitySpec struct {
	// +kubebuilder:validation:Enum=HAMi
	// +kubebuilder:default=HAMi
	Provider string `json:"provider,omitempty"`
	// +optional
	HAMi *HAMiSpec `json:"hami,omitempty"`
	// +kubebuilder:default="5m"
	ReadyTimeout metav1.Duration `json:"readyTimeout,omitempty"`
}

type HAMiSpec struct {
	// +kubebuilder:validation:Enum=DevicePlugin;DRA
	// +kubebuilder:default=DevicePlugin
	Mode string `json:"mode,omitempty"`
	// +optional
	ExpectedDevicesPerCell *int32 `json:"expectedDevicesPerCell,omitempty"`
	// DeviceClassName is the HAMi DeviceClass to read capacity from (DRA mode).
	// +optional
	DeviceClassName string `json:"deviceClassName,omitempty"`
}

type DeletionSpec struct {
	// +kubebuilder:validation:Enum=Drain;Force
	// +kubebuilder:default=Drain
	Policy string `json:"policy,omitempty"`
	// +kubebuilder:default="10m"
	DrainTimeout metav1.Duration `json:"drainTimeout,omitempty"`
}
```

---

## 6. Status

```yaml
status:
  observedGeneration: 4
  replicas: 4                # total cells owned (scale subresource statuspath)
  readyCells: 3
  creatingCells: 1
  drainingCells: 0
  failedCells: 0

  physicalCapacity:          # OUTER — from KubeSwift/DRA inventory only
    gpus: 4                  # GPUs held by this pool's cells
    freeGPUsInCluster: 1     # free devices the pool could still claim
    model: NVIDIA-GeForce-GTX-1080

  workloadCapacity:          # INNER — from the capacity provider only
    provider: HAMi
    mode: DevicePlugin
    gpuDevices: 3
    homogeneous: true
    gpuMemory: {total: 144Gi, allocated: 96Gi, available: 48Gi}
    gpuCompute: {total: 300, allocated: 190, available: 110}   # percent; pool-homogeneous only
    byModel:
      - {model: NVIDIA-GeForce-GTX-1080, devices: 3,
         memory: {total: 24Gi, allocated: 8Gi, available: 16Gi},
         compute: {total: 300, allocated: 190, available: 110}}
    lastObserved: "2026-07-30T09:14:02Z"

  cells:                     # bounded per-cell state (D1) — the FSM lives here
    - name: inference-l40s-0
      index: 0
      phase: Ready
      guestRef: {name: inference-l40s-0}
      guestUID: 6f0e…        # identity "instance" anchor (D3)
      hostNode: boba         # outer node holding the physical GPU
      devices: ["0000:01:00.0"]
      nodeName: inference-l40s-0   # inner Node (== cell name)
      nodeReady: true
      capacityDevices: 1
      message: ""
      lastTransitionTime: "2026-07-30T09:10:41Z"
    - name: inference-l40s-3
      index: 3
      phase: AwaitingGPUCapacity
      message: 'node registered but HAMi reports 0 of 1 expected devices'

  conditions:
    - {type: Ready,                    status: "False", reason: CellsNotReady}
    - {type: Progressing,              status: "True",  reason: CellCreating}
    - {type: WorkloadClusterReachable, status: "True",  reason: Connected}
    - {type: CapacityProviderReady,    status: "True",  reason: HAMiDetected}
    - {type: CapacityAvailable,        status: "True",  reason: CapacityFree}
    - {type: PhysicalGPUsAvailable,    status: "True",  reason: FreeDevices}
```

### Condition semantics

| Type | True when | Notable False reasons |
|---|---|---|
| `Ready` | `readyCells == spec.replicas` | `CellsNotReady`, `CapacityProviderDegraded`, `WorkloadClusterUnreachable` |
| `Progressing` | a cell is being created/booting/joining/draining | `Idle` (steady state), `Stalled` |
| `WorkloadClusterReachable` | last inner API call succeeded | `Unreachable`, `CredentialInvalid`, `Forbidden` |
| `CapacityProviderReady` | HAMi detected and parseable on every ready cell's Node | `HAMiNotDetected`, `RegistrationUnparseable`, `DRAFeatureGateMissing` |
| `CapacityAvailable` | some ready cell has free memory **and** free cores | `Saturated`, `Unknown` |
| `PhysicalGPUsAvailable` | outer inventory has ≥1 free device *or* no cell is waiting for one | `InsufficientPhysicalGPU` |

`gpuCompute` is reported as a bare sum of percentages **only** while
`workloadCapacity.homogeneous == true`. Percentages of different GPU models are not
commensurable; when a pool goes mixed-model (image/claim-selector drift) the
aggregate is dropped, `homogeneous: false` is set, and `byModel` is the only
compute truth (`-capacity.md` §5).

---

## 7. Validation rules (webhook)

| # | Rule | Reason |
|---|---|---|
| V1 | `cell.gpu.count == 1` | multi-GPU/NVLink cells are a later phase |
| V2 | `cell.gpu.dra.tier == pcie` | `hgx-shared` needs QEMU + host Fabric Manager; `hgx-full` is rejected by KubeSwift at allocation |
| V3 | exactly one of `dra.resourceClaimTemplateName` / `dra.resourceClaimName` | mirrors KubeSwift's own rule; a shared claim + N cells would double-book one VFIO device |
| V4 | `backend: DRA` ⇒ `dra` set; `Native` ⇒ `native` set; never both | one allocation backend, as KubeSwift enforces |
| V5 | `guestTemplate` sets no operator-owned or denied field (§4) | no silent override |
| V6 | `guestTemplate.guestClassRef` and `.imageRef` non-empty | GPU cells are disk boot |
| V7 | `bootstrap.provider: Opaque` ⇒ `joinSecretRef` set — **unless** `cell.clusterAPI.bootstrapConfigTemplateRef` is set, where it is instead FORBIDDEN | otherwise the cell can never join; but when CAPI's own bootstrap provider owns the join data, requiring a Secret nothing reads teaches operators to ignore the schema |
| V8 | `capacity.hami.mode: DRA` ⇒ `deviceClassName` set | no way to find the slices otherwise |
| V9 | all refs resolve in the pool's namespace | no cross-namespace secret reads |
| V10 | `replicas` may only decrease when `WorkloadClusterReachable=True` | never scale down blind (D10) |
| V11a | `cell.provisioner: ClusterAPI` ⇒ `cell.clusterAPI` set; `SwiftGuest` ⇒ it is NOT set | the mode and its configuration must agree, and a cell must know which CAPI Cluster it joins |
| V11b | under `ClusterAPI`, `guestTemplate` may only use `imageRef`, `guestClassRef`, `interfaces` | a KubeSwiftMachine cannot express the rest, so anything else would be SILENTLY DROPPED — a cell booting with less than asked for |
| V11c | `clusterAPI.bootstrapConfigTemplateRef.kind` ends in `Template` | the per-cell object's kind is the template's kind minus the suffix; without it the operator would create another template |
| V12 | `cell.nodeIPFrom` names an interface present in `guestTemplate.interfaces` | otherwise kubelet binds the wrong address |

V10 is an update-only rule; V1–V9, V11a–c, V12 fire on create and update. Per
KubeSwift's discipline: each rule states which operations it fires on, and the
webhook never defaults-to-everything.

---

## 8. Compliance with the API principles

- **9.1 reference, don't reproduce** — CPU/memory/disk come from
  `SwiftGuestClass`; the image from `SwiftImage`; GPU selection from a
  `ResourceClaimTemplate`/`SwiftGPUProfile`; VM shape from an opaque
  `SwiftGuestSpec`. This CRD adds no field that already exists elsewhere.
- **9.2 no HAMi implementation detail** — one enum (`hami.mode`) plus an expected
  device count. No Helm values, no resource names, no fractions. Even HAMi's
  `gpu=on` gate is user-declared node label data, not operator knowledge.
- **9.3 shape vs policy** — `spec.cell` is the *shape* of one cell; `spec.replicas`
  (later `spec.autoscaling`) is *pool policy*. They never mix.
