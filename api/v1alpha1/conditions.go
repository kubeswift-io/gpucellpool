package v1alpha1

// Condition types on GPUCellPool. They split the two layers deliberately, so a
// single False localises the fault: outer (PhysicalGPUsAvailable), transport
// (WorkloadClusterReachable), inner provider (CapacityProviderReady), inner
// capacity (CapacityAvailable), aggregate (Ready).
const (
	// ConditionReady is True when readyCells == spec.replicas.
	ConditionReady = "Ready"
	// ConditionProgressing is True while cells are being created, booting,
	// joining or draining.
	ConditionProgressing = "Progressing"
	// ConditionWorkloadClusterReachable is True when the last workload-cluster
	// API call succeeded. While it is False, every destructive path is frozen.
	ConditionWorkloadClusterReachable = "WorkloadClusterReachable"
	// ConditionCapacityProviderReady is True when the inner capacity provider
	// (HAMi) is detected and parseable on the pool's cells.
	ConditionCapacityProviderReady = "CapacityProviderReady"
	// ConditionCapacityAvailable is True when at least one ready cell has free
	// GPU memory and free GPU compute.
	ConditionCapacityAvailable = "CapacityAvailable"
	// ConditionPhysicalGPUsAvailable is False only when a cell is waiting for a
	// physical GPU and the outer inventory has none free.
	ConditionPhysicalGPUsAvailable = "PhysicalGPUsAvailable"
	// ConditionScalingActive is True when demand-driven scale-up is enabled and
	// the pool is reading demand successfully. Its reason carries the last
	// decision, which is where an operator looks to understand why the pool did
	// or did not grow.
	ConditionScalingActive = "ScalingActive"
	// ConditionCellDrainRequested is True when an outer node holding a cell is
	// being drained or cordoned. Cells are never offline-migrated, so this is a
	// signal for the operator (and, later, for automated replacement).
	ConditionCellDrainRequested = "CellDrainRequested"

	// ConditionUpdated reports whether every cell was created from the CURRENT
	// cell template. False means at least one cell is running an older shape — a
	// previous image, guest class or interface set.
	//
	// It is only ever informational until spec.updatePolicy.type is RollingUpdate,
	// because replacing a cell destroys whatever the old one was still running
	// unless it is drained first. Knowing you have drifted is useful on its own;
	// acting on it is a decision the operator opts into.
	ConditionUpdated = "Updated"
)

// Condition reasons. Every reason names a distinct operator action: "your token
// expired" and "you removed my Node patch rights" must not share one.
const (
	ReasonIdle    = "Idle"
	ReasonStalled = "Stalled"

	ReasonCellsNotReady   = "CellsNotReady"
	ReasonAllCellsReady   = "AllCellsReady"
	ReasonCellCreating    = "CellCreating"
	ReasonCellDraining    = "CellDraining"
	ReasonTemplateChanged = "TemplateChanged"
	ReasonAllCellsCurrent = "AllCellsCurrent"
	ReasonRollingUpdate   = "RollingUpdate"
	ReasonUpdateBlocked   = "UpdateBlocked"

	ReasonConnected         = "Connected"
	ReasonUnreachable       = "Unreachable"
	ReasonCredentialInvalid = "CredentialInvalid"
	ReasonForbidden         = "Forbidden"

	ReasonHAMiDetected            = "HAMiDetected"
	ReasonHAMiNotDetected         = "HAMiNotDetected"
	ReasonRegistrationUnparseable = "RegistrationUnparseable"
	ReasonDRAFeatureGateMissing   = "DRAFeatureGateMissing"
	ReasonDRADeviceClassMissing   = "DRADeviceClassMissing"

	ReasonCapacityFree     = "CapacityFree"
	ReasonSaturated        = "Saturated"
	ReasonCapacityUnknown  = "Unknown"
	ReasonHeterogeneous    = "Heterogeneous"
	ReasonModelMismatch    = "ModelMismatch"
	ReasonFreeDevices      = "FreeDevices"
	ReasonInsufficientGPUs = "InsufficientPhysicalGPU"

	// Cell-level failure reasons, surfaced in CellStatus.Message and in
	// Progressing when they dominate.
	ReasonCellUnschedulable    = "CellUnschedulable"
	ReasonCellProvisionTimeout = "CellProvisionTimeout"
	ReasonJoinTimeout          = "JoinTimeout"
	// ReasonNodeNeverRegistered is a join timeout in which NO Node object ever
	// appeared, as opposed to one that registered and never went Ready. The
	// distinction matters: if the Node registered, the bootstrap credential
	// WORKED and the fault is downstream (kubelet, CNI). Only the never-appeared
	// case is evidence about the credential — see PlanMembership's suspect guard.
	ReasonNodeNeverRegistered        = "NodeNeverRegistered"
	ReasonGPUNotAdvertised           = "GPUNotAdvertised"
	ReasonNodeNameCollision          = "NodeNameCollision"
	ReasonBootstrapCredentialSuspect = "BootstrapCredentialSuspect"
	ReasonCellReplacementExhausted   = "CellReplacementExhausted"
	ReasonFaultNotInTheCell          = "FaultNotInTheCell"
	ReasonWaitingForAllocations      = "WaitingForAllocations"
	ReasonDrainTimedOut              = "DrainTimedOut"

	// Scaling reasons (Phase 3, scale-up).
	ReasonScaledUp            = "ScaledUp"
	ReasonStabilizing         = "Stabilizing"
	ReasonAtMaxReplicas       = "AtMaxReplicas"
	ReasonDemandUnsatisfiable = "DemandUnsatisfiable"
	ReasonScaledDown          = "ScaledDown"
	ReasonAtMinReplicas       = "AtMinReplicas"
	ReasonNoIdleCell          = "NoIdleCell"

	// ReasonUnknownProvisioner means spec.cell.provisioner names something this
	// build cannot create. It is an event rather than a silent fallback: the field
	// decides what objects appear in the infrastructure cluster.
	ReasonUnknownProvisioner = "UnknownProvisioner"

	// ReasonScaledToZero marks a pool that deliberately holds no cells, so that an
	// empty-by-design pool is not reported as a broken one.
	ReasonScaledToZero = "ScaledToZero"

	// ReasonCellShapeUnknown means we cannot judge whether a fresh cell would
	// satisfy a pending request, because the pool has never advertised a device
	// and so has no shape to compare against. Distinct from DemandUnsatisfiable
	// on purpose: "it would not fit" and "I do not know what a cell brings" call
	// for different operator actions.
	ReasonCellShapeUnknown = "CellShapeUnknown"
)

// Labels the operator stamps on the objects it owns, in both clusters. They are
// the cross-cluster identity mechanism: cell name correlates the outer guest with
// the inner Node, and Instance distinguishes this incarnation from a previous one.
const (
	// LabelPool is the pool name, on cell guests and on workload Nodes.
	LabelPool = "cells.kubeswift.io/pool"
	// LabelCell is the cell name (<pool>-<index>).
	LabelCell = "cells.kubeswift.io/cell"
	// LabelCellIndex is the cell's stable ordinal.
	LabelCellIndex = "cells.kubeswift.io/cell-index"
	// LabelInstance carries the outer guest UID, so a workload Node left behind
	// by a replaced cell is detected as stale instead of adopted.
	LabelInstance = "cells.kubeswift.io/instance"
)

// Annotations.
const (
	// AnnotationTemplateHash records the cell template a guest was created from.
	AnnotationTemplateHash = "cells.kubeswift.io/template-hash"
	// AnnotationGPUPreflight carries the in-guest GPU preflight result
	// ("<found>/<expected>"), surfaced verbatim in CellStatus.Message.
	AnnotationGPUPreflight = "cells.kubeswift.io/gpu-preflight"
	// AnnotationReleaseGuard clears the pool-level "hopeless pool" guard.
	AnnotationReleaseGuard = "cells.kubeswift.io/release-guard"
)

// Finalizers.
const (
	// FinalizerPool keeps the pool object alive until its cells are torn down
	// and their workload Nodes reaped.
	FinalizerPool = "cells.kubeswift.io/pool"
	// FinalizerCellDrain is placed on each cell guest so it cannot be deleted —
	// by this operator, by kubectl, or by garbage collection — while HAMi
	// workloads still hold its GPU.
	FinalizerCellDrain = "cells.kubeswift.io/cell-drain"
)
