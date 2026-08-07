package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// GPUCellPoolSpec is the desired state of a pool of GPU cells.
//
// A cell is one KubeSwift SwiftGuest VM holding one whole passthrough GPU, joined
// to a workload Kubernetes cluster as a node whose GPU HAMi shares. The spec is
// split deliberately: spec.cell is the SHAPE of one cell, spec.replicas is POOL
// POLICY. They never mix (design principle 9.3).
type GPUCellPoolSpec struct {
	// Replicas is the desired number of cells. Scaled via the scale subresource,
	// so `kubectl scale` and an HPA both work. Autoscaling (min/max, demand
	// signals) is a later phase and will arrive as a separate `autoscaling` block
	// rather than by changing this field.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	Replicas int32 `json:"replicas"`

	// Cell describes one cell: the VM shape and its GPU.
	Cell CellSpec `json:"cell"`

	// Bootstrap describes how a cell becomes a worker of the workload cluster.
	Bootstrap BootstrapSpec `json:"bootstrap"`

	// WorkloadCluster is the cluster the cells join and where HAMi runs.
	WorkloadCluster WorkloadClusterSpec `json:"workloadCluster"`

	// Capacity selects and configures the inner capacity provider (HAMi).
	// +optional
	Capacity CapacitySpec `json:"capacity,omitempty"`

	// Deletion controls teardown behaviour for the pool.
	// +optional
	Deletion *DeletionSpec `json:"deletion,omitempty"`
}

// CellSpec is the shape of a single cell.
type CellSpec struct {
	// Provisioner creates the outer objects for a cell.
	//   SwiftGuest: create a KubeSwift SwiftGuest directly (the only mode today).
	//   ClusterAPI: size a MachineDeployment (later phase; requires a CAPI-managed
	//               workload cluster, so it is an additional mode, never a
	//               replacement).
	// +kubebuilder:validation:Enum=SwiftGuest;ClusterAPI
	// +kubebuilder:default=SwiftGuest
	// +optional
	Provisioner string `json:"provisioner,omitempty"`

	// GuestTemplate is a verbatim KubeSwift SwiftGuestSpec, passed through
	// unmodified apart from the fields this operator owns. It is opaque on
	// purpose: mirroring KubeSwift's API surface here would guarantee drift, and
	// every KubeSwift VM feature (storage, interfaces, data disks, topology
	// spread) is available for free.
	//
	// Operator-owned fields (rejected if set here; written by the controller):
	// seedProfileRef, gpuProfileRef, gpuResourceClaim, nodeName, migration,
	// runPolicy.
	//
	// Denied fields (incompatible with a GPU cell): kernelRef,
	// cloneFromSnapshot, osType: windows, vhostUserDevices, filesystems.
	//
	// guestClassRef and imageRef are required — a GPU cell is a disk boot.
	// +kubebuilder:pruning:PreserveUnknownFields
	GuestTemplate runtime.RawExtension `json:"guestTemplate"`

	// GPU is the cell's physical GPU request.
	GPU CellGPUSpec `json:"gpu"`

	// SpreadPolicy is shorthand for hostname topology spread of the cell VMs
	// across outer nodes. Spread is the default: two cells on one host share a
	// failure domain and, usually, a NUMA/PCIe path.
	// +kubebuilder:validation:Enum=Pack;Spread
	// +kubebuilder:default=Spread
	// +optional
	SpreadPolicy string `json:"spreadPolicy,omitempty"`

	// NodeIPFrom names the guestTemplate interface whose address the kubelet
	// registers as --node-ip. Empty means the primary interface.
	//
	// A cell almost always needs a ROUTABLE interface here, not KubeSwift's
	// node-local nat primary: the workload apiserver must be able to dial the
	// kubelet for logs, exec, port-forward and metrics.
	// +optional
	NodeIPFrom string `json:"nodeIPFrom,omitempty"`
}

// GPU allocation backends.
const (
	// GPUBackendDRA allocates the device at pod-schedule time via Dynamic
	// Resource Allocation (KubeSwift spec.gpuResourceClaim).
	GPUBackendDRA = "DRA"
	// GPUBackendNative allocates the device in the KubeSwift controller
	// (spec.gpuProfileRef + SwiftGPUProfile/SwiftGPUNode).
	GPUBackendNative = "Native"
)

// CellGPUSpec is the cell's physical GPU request. It selects a WHOLE device:
// fractional allocation is HAMi's job, inside the cell.
type CellGPUSpec struct {
	// Count is the number of whole GPUs per cell. Only 1 is supported in
	// v1alpha1; multi-GPU cells need NVLink/topology work.
	// +kubebuilder:validation:Enum=1
	// +kubebuilder:default=1
	// +optional
	Count int32 `json:"count,omitempty"`

	// Backend selects the KubeSwift GPU allocation backend.
	// +kubebuilder:validation:Enum=DRA;Native
	// +kubebuilder:default=DRA
	// +optional
	Backend string `json:"backend,omitempty"`

	// DRA configures the DRA backend. Required when backend is DRA.
	// +optional
	DRA *CellGPUDRASpec `json:"dra,omitempty"`

	// Native configures the native SwiftGPU backend. Required when backend is
	// Native.
	// +optional
	Native *CellGPUNativeSpec `json:"native,omitempty"`
}

// CellGPUDRASpec points at the ResourceClaim(Template) the cell's launcher pod
// carries. Exactly one of the two references must be set.
type CellGPUDRASpec struct {
	// ResourceClaimTemplateName mints a per-cell ResourceClaim. Recommended.
	// +optional
	ResourceClaimTemplateName string `json:"resourceClaimTemplateName,omitempty"`

	// ResourceClaimName references one pre-created, shared ResourceClaim. A
	// VFIO device can back only ONE running VM, so this is only correct for a
	// single-cell pool.
	// +optional
	ResourceClaimName string `json:"resourceClaimName,omitempty"`

	// RequestName is the device-request name inside the claim to read the
	// allocation result back from.
	// +kubebuilder:default=gpu
	// +optional
	RequestName string `json:"requestName,omitempty"`

	// Tier selects hypervisor and firmware in KubeSwift. Only pcie (Cloud
	// Hypervisor) is supported for cells: hgx-shared needs QEMU plus a host
	// Fabric Manager, and hgx-full is rejected by KubeSwift at allocation.
	// +kubebuilder:validation:Enum=pcie
	// +kubebuilder:default=pcie
	// +optional
	Tier string `json:"tier,omitempty"`

	// Hugepages sizes the GPU memory hugepage backing ("1Gi", "2Mi", or empty).
	// +kubebuilder:validation:Enum="";"1Gi";"2Mi"
	// +optional
	Hugepages string `json:"hugepages,omitempty"`
}

// CellGPUNativeSpec selects the native SwiftGPU allocation backend.
type CellGPUNativeSpec struct {
	// GPUProfileRef references a SwiftGPUProfile in the pool's namespace.
	GPUProfileRef corev1.LocalObjectReference `json:"gpuProfileRef"`
}

// Bootstrap providers.
const (
	// BootstrapProviderOpaque uses user-supplied cloud-init verbatim (with a
	// small, closed substitution set). Makes no assumption about the workload
	// distribution — k0s, kubeadm, RKE2 and k3s all work.
	BootstrapProviderOpaque = "Opaque"
	// BootstrapProviderKubeadmToken mints a TTL'd bootstrap token in the
	// workload cluster per cell. Kubeadm-style clusters only; needs Secret
	// write rights in kube-system.
	BootstrapProviderKubeadmToken = "KubeadmToken"
)

// BootstrapSpec describes how a cell VM joins the workload cluster.
type BootstrapSpec struct {
	// Provider selects the join-credential mechanism.
	// +kubebuilder:validation:Enum=Opaque;KubeadmToken
	// +kubebuilder:default=Opaque
	// +optional
	Provider string `json:"provider,omitempty"`

	// JoinSecretRef holds the cloud-init user-data that joins the workload
	// cluster, under key "user-data" unless JoinSecretKey says otherwise.
	// Required for the Opaque provider.
	//
	// The credential is only ever referenced: the controller renders a per-cell
	// Secret and points the SwiftSeedProfile at it, so no token is written into
	// any custom resource.
	// +optional
	JoinSecretRef *corev1.LocalObjectReference `json:"joinSecretRef,omitempty"`

	// JoinSecretKey is the key inside JoinSecretRef holding the user-data.
	// +kubebuilder:default=user-data
	// +optional
	JoinSecretKey string `json:"joinSecretKey,omitempty"`

	// Hostname controls the guest hostname. CellName (default) makes the guest
	// hostname, and therefore the workload Node name, equal to the cell name —
	// which is how cells are correlated across the two clusters.
	// +kubebuilder:validation:Enum=CellName;None
	// +kubebuilder:default=CellName
	// +optional
	Hostname string `json:"hostname,omitempty"`

	// ReadyTimeout bounds Booting+Joining. A cell that has not produced a Ready
	// workload Node within it is marked Failed with a reason, never left silent.
	// +kubebuilder:default="15m"
	// +optional
	ReadyTimeout *metav1.Duration `json:"readyTimeout,omitempty"`
}

// WorkloadClusterSpec locates the workload cluster and describes how its Nodes
// should be labelled.
type WorkloadClusterSpec struct {
	// KubeconfigSecretRef references a Secret in the pool's namespace holding a
	// kubeconfig for the workload cluster. Scope the credential: see
	// docs/design/gpucellpool-reconciliation.md for the minimum RBAC.
	KubeconfigSecretRef corev1.LocalObjectReference `json:"kubeconfigSecretRef"`

	// Key is the Secret key holding the kubeconfig.
	// +kubebuilder:default=value
	// +optional
	Key string `json:"key,omitempty"`

	// Node describes labels, annotations and taints applied to each cell's
	// workload Node.
	// +optional
	Node *WorkloadNodeSpec `json:"node,omitempty"`
}

// WorkloadNodeSpec is the metadata a cell's workload Node should carry.
type WorkloadNodeSpec struct {
	// Labels are applied to the cell's Node. The capacity provider's own gate
	// (for HAMi: gpu=on) is declared HERE, by the user: this operator applies
	// labels it does not interpret.
	// +optional
	Labels map[string]string `json:"labels,omitempty"`

	// Annotations are applied to the cell's Node.
	// +optional
	Annotations map[string]string `json:"annotations,omitempty"`

	// Taints are applied to the cell's Node. Note that the capacity provider's
	// own DaemonSets must tolerate them or the GPU is never advertised.
	// +optional
	Taints []corev1.Taint `json:"taints,omitempty"`
}

// Capacity providers and modes.
const (
	// CapacityProviderHAMi reads GPU capacity from HAMi.
	CapacityProviderHAMi = "HAMi"
	// HAMiModeDevicePlugin reads the node registration annotation and pod
	// allocation annotations (HAMi's default deployment).
	HAMiModeDevicePlugin = "DevicePlugin"
	// HAMiModeDRA reads ResourceSlice capacity and ResourceClaim
	// consumedCapacity. Needs Kubernetes >= 1.34 with DRAConsumableCapacity.
	HAMiModeDRA = "DRA"
)

// CapacitySpec selects the inner capacity provider.
type CapacitySpec struct {
	// Provider is the inner GPU capacity provider.
	// +kubebuilder:validation:Enum=HAMi
	// +kubebuilder:default=HAMi
	// +optional
	Provider string `json:"provider,omitempty"`

	// HAMi configures the HAMi provider.
	// +optional
	HAMi *HAMiSpec `json:"hami,omitempty"`

	// ReadyTimeout bounds how long a joined Node may go without advertising the
	// expected GPU before the cell is marked Failed.
	// +kubebuilder:default="5m"
	// +optional
	ReadyTimeout *metav1.Duration `json:"readyTimeout,omitempty"`
}

// HAMiSpec configures HAMi capacity discovery. It deliberately exposes no HAMi
// deployment settings: HAMi's configuration belongs to HAMi.
type HAMiSpec struct {
	// Mode selects which HAMi accounting model to read.
	// +kubebuilder:validation:Enum=DevicePlugin;DRA
	// +kubebuilder:default=DevicePlugin
	// +optional
	Mode string `json:"mode,omitempty"`

	// ExpectedDevicesPerCell is how many GPU devices HAMi must advertise on a
	// cell's Node before that cell counts as Ready. Defaults to cell.gpu.count.
	// +kubebuilder:validation:Minimum=1
	// +optional
	ExpectedDevicesPerCell *int32 `json:"expectedDevicesPerCell,omitempty"`

	// DeviceClassName is the HAMi DeviceClass whose ResourceSlices carry the
	// cells' capacity. Required in DRA mode.
	// +optional
	DeviceClassName string `json:"deviceClassName,omitempty"`
}

// Deletion policies.
const (
	// DeletionPolicyDrain cordons the workload Node and waits for HAMi
	// allocations to clear before removing a cell.
	DeletionPolicyDrain = "Drain"
	// DeletionPolicyForce removes cells without draining. Destroys running
	// workloads.
	DeletionPolicyForce = "Force"
)

// DeletionSpec controls pool teardown.
type DeletionSpec struct {
	// Policy controls whether cells are drained before deletion.
	// +kubebuilder:validation:Enum=Drain;Force
	// +kubebuilder:default=Drain
	// +optional
	Policy string `json:"policy,omitempty"`

	// DrainTimeout bounds draining during EXPLICIT pool deletion, after which
	// teardown proceeds anyway (leaving VMs and GPUs pinned forever after a
	// delete request is worse). Implicit scale-down never forces.
	// +kubebuilder:default="10m"
	// +optional
	DrainTimeout *metav1.Duration `json:"drainTimeout,omitempty"`
}

// CellPhase is the observed state of one cell. A cell's phase is derived from
// BOTH layers: a running VM is not a ready cell.
// +kubebuilder:validation:Enum=Pending;AllocatingGPU;GuestProvisioning;Booting;Joining;AwaitingGPUCapacity;Ready;Draining;Deleting;Failed
type CellPhase string

const (
	// CellPhasePending means the cell's outer objects do not exist yet.
	CellPhasePending CellPhase = "Pending"
	// CellPhaseAllocatingGPU means the cell exists and is waiting for a
	// physical GPU (scheduler in DRA mode, controller in Native mode).
	CellPhaseAllocatingGPU CellPhase = "AllocatingGPU"
	// CellPhaseGuestProvisioning means the GPU is allocated and the VM is
	// starting.
	CellPhaseGuestProvisioning CellPhase = "GuestProvisioning"
	// CellPhaseBooting means the VM is running and its node address is not
	// known yet.
	CellPhaseBooting CellPhase = "Booting"
	// CellPhaseJoining means the VM is up and its workload Node has not
	// registered Ready yet.
	CellPhaseJoining CellPhase = "Joining"
	// CellPhaseAwaitingGPUCapacity means the workload Node is Ready but the
	// capacity provider does not advertise the expected GPU yet.
	CellPhaseAwaitingGPUCapacity CellPhase = "AwaitingGPUCapacity"
	// CellPhaseReady means both layers agree: VM running, Node Ready, GPU
	// advertised.
	CellPhaseReady CellPhase = "Ready"
	// CellPhaseDraining means the cell is being evacuated before removal.
	CellPhaseDraining CellPhase = "Draining"
	// CellPhaseDeleting means the outer objects are being removed.
	CellPhaseDeleting CellPhase = "Deleting"
	// CellPhaseFailed means the cell hit a terminal failure or a timeout.
	CellPhaseFailed CellPhase = "Failed"
)

// CellStatus is the observed state of one cell, spanning both clusters.
type CellStatus struct {
	// Name is the cell name: <pool>-<index>. It is also the SwiftGuest name,
	// the guest hostname and the workload Node name.
	Name string `json:"name"`

	// Index is the cell's stable ordinal within the pool.
	Index int32 `json:"index"`

	// Phase is the cell's state (see CellPhase).
	Phase CellPhase `json:"phase"`

	// GuestUID is the outer guest's UID. It distinguishes this incarnation of
	// the cell from a previous one, which is how a stale workload Node left by
	// a replaced cell is detected instead of being adopted.
	// +optional
	GuestUID string `json:"guestUID,omitempty"`

	// HostNode is the outer Kubernetes node whose physical GPU this cell holds.
	// +optional
	HostNode string `json:"hostNode,omitempty"`

	// Devices are the PCI addresses passed into the cell VM.
	// +optional
	Devices []string `json:"devices,omitempty"`

	// NodeName is the cell's workload Node (equal to Name once it registers).
	// +optional
	NodeName string `json:"nodeName,omitempty"`

	// NodeReady reports the workload Node's Ready condition.
	// +optional
	NodeReady bool `json:"nodeReady,omitempty"`

	// CapacityDevices is how many GPU devices the capacity provider advertises
	// on this cell's Node.
	// +optional
	CapacityDevices int32 `json:"capacityDevices,omitempty"`

	// Message explains a non-Ready phase in operator-actionable terms.
	// +optional
	Message string `json:"message,omitempty"`

	// FailureCount is how many times this index has failed; it drives the
	// replacement backoff and the exhaustion guard.
	// +optional
	FailureCount int32 `json:"failureCount,omitempty"`

	// LastTransitionTime is when Phase last changed.
	// +optional
	LastTransitionTime *metav1.Time `json:"lastTransitionTime,omitempty"`
}

// PhysicalCapacityStatus is OUTER capacity: whole devices, from KubeSwift and
// the outer DRA inventory. It is never inferred from inner state.
type PhysicalCapacityStatus struct {
	// GPUs is the number of physical GPUs this pool's cells hold.
	GPUs int32 `json:"gpus"`

	// FreeGPUsInCluster is how many further devices the pool could claim, from
	// the outer inventory. Nil means unknown (never report unknown as zero).
	// +optional
	FreeGPUsInCluster *int32 `json:"freeGPUsInCluster,omitempty"`

	// Model is the GPU model the pool holds, as reported by the outer
	// inventory.
	// +optional
	Model string `json:"model,omitempty"`
}

// MemoryCapacity is total/allocated/available GPU memory.
type MemoryCapacity struct {
	Total     resource.Quantity `json:"total"`
	Allocated resource.Quantity `json:"allocated"`
	Available resource.Quantity `json:"available"`
}

// ComputeCapacity is total/allocated/available GPU compute, in percent of a
// device (HAMi's unit). Percentages of DIFFERENT GPU models are not
// commensurable, so pool-wide aggregates are only published for a homogeneous
// pool; ByModel is always authoritative.
type ComputeCapacity struct {
	Total     int32 `json:"total"`
	Allocated int32 `json:"allocated"`
	Available int32 `json:"available"`
}

// ModelCapacity is per-GPU-model capacity within the pool.
type ModelCapacity struct {
	Model   string           `json:"model"`
	Devices int32            `json:"devices"`
	Memory  *MemoryCapacity  `json:"memory,omitempty"`
	Compute *ComputeCapacity `json:"compute,omitempty"`
}

// WorkloadCapacityStatus is INNER capacity: the fractional GPU capacity the
// provider (HAMi) advertises on this pool's cells.
type WorkloadCapacityStatus struct {
	// Provider and Mode record which accounting model produced these numbers.
	// +optional
	Provider string `json:"provider,omitempty"`
	// +optional
	Mode string `json:"mode,omitempty"`

	// GPUDevices is the number of GPU devices the provider advertises across
	// the pool's ready cells.
	// +optional
	GPUDevices int32 `json:"gpuDevices,omitempty"`

	// Homogeneous is true when every advertised device is the same model. When
	// false, the pool-wide Compute aggregate is omitted (see ComputeCapacity).
	// +optional
	Homogeneous bool `json:"homogeneous,omitempty"`

	// GPUMemory is pool-wide GPU memory. Always valid: bytes are comparable
	// across models.
	// +optional
	GPUMemory *MemoryCapacity `json:"gpuMemory,omitempty"`

	// GPUCompute is pool-wide GPU compute in percent. Only published while
	// Homogeneous is true.
	// +optional
	GPUCompute *ComputeCapacity `json:"gpuCompute,omitempty"`

	// ByModel is per-model capacity and is always populated.
	// +optional
	ByModel []ModelCapacity `json:"byModel,omitempty"`

	// LastObserved is when these numbers were last computed successfully. A
	// failed read retains the previous values and this timestamp rather than
	// reporting zero.
	// +optional
	LastObserved *metav1.Time `json:"lastObserved,omitempty"`
}

// GPUCellPoolStatus is the observed state of a GPUCellPool.
type GPUCellPoolStatus struct {
	// ObservedGeneration is the .metadata.generation this status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Replicas is the number of cells the pool owns (scale subresource).
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// ReadyCells is the number of cells where both layers agree.
	// +optional
	ReadyCells int32 `json:"readyCells,omitempty"`
	// +optional
	CreatingCells int32 `json:"creatingCells,omitempty"`
	// +optional
	DrainingCells int32 `json:"drainingCells,omitempty"`
	// +optional
	FailedCells int32 `json:"failedCells,omitempty"`

	// PhysicalCapacity is outer capacity (whole GPUs).
	// +optional
	PhysicalCapacity *PhysicalCapacityStatus `json:"physicalCapacity,omitempty"`

	// WorkloadCapacity is inner capacity (HAMi fractions).
	// +optional
	WorkloadCapacity *WorkloadCapacityStatus `json:"workloadCapacity,omitempty"`

	// Cells is per-cell state, one entry per owned cell. It is a projection of
	// live cluster objects, so it is fully recoverable after a restart.
	// +optional
	// +listType=map
	// +listMapKey=name
	Cells []CellStatus `json:"cells,omitempty"`

	// Conditions separate the layers on purpose, so a single False localises the
	// fault: see the Condition* constants.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// GPUCellPool is a pool of VM-isolated, fractionally-shared GPU worker nodes.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.replicas,statuspath=.status.replicas
// +kubebuilder:resource:scope=Namespaced,shortName=cellpool
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.spec.replicas`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyCells`
// +kubebuilder:printcolumn:name="GPUs",type=integer,JSONPath=`.status.physicalCapacity.gpus`
// +kubebuilder:printcolumn:name="GPUMemFree",type=string,JSONPath=`.status.workloadCapacity.gpuMemory.available`
// +kubebuilder:printcolumn:name="Available",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type GPUCellPool struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GPUCellPoolSpec   `json:"spec"`
	Status GPUCellPoolStatus `json:"status,omitempty"`
}

// GPUCellPoolList contains a list of GPUCellPool.
// +kubebuilder:object:root=true
type GPUCellPoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GPUCellPool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GPUCellPool{}, &GPUCellPoolList{})
}
