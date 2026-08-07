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
	// ConditionCellDrainRequested is True when an outer node holding a cell is
	// being drained or cordoned. Cells are never offline-migrated, so this is a
	// signal for the operator (and, later, for automated replacement).
	ConditionCellDrainRequested = "CellDrainRequested"
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

	ReasonConnected         = "Connected"
	ReasonUnreachable       = "Unreachable"
	ReasonCredentialInvalid = "CredentialInvalid"
	ReasonForbidden         = "Forbidden"

	ReasonHAMiDetected            = "HAMiDetected"
	ReasonHAMiNotDetected         = "HAMiNotDetected"
	ReasonRegistrationUnparseable = "RegistrationUnparseable"
	ReasonDRAFeatureGateMissing   = "DRAFeatureGateMissing"

	ReasonCapacityFree     = "CapacityFree"
	ReasonSaturated        = "Saturated"
	ReasonCapacityUnknown  = "Unknown"
	ReasonHeterogeneous    = "Heterogeneous"
	ReasonModelMismatch    = "ModelMismatch"
	ReasonFreeDevices      = "FreeDevices"
	ReasonInsufficientGPUs = "InsufficientPhysicalGPU"

	// Cell-level failure reasons, surfaced in CellStatus.Message and in
	// Progressing when they dominate.
	ReasonCellUnschedulable          = "CellUnschedulable"
	ReasonCellProvisionTimeout       = "CellProvisionTimeout"
	ReasonJoinTimeout                = "JoinTimeout"
	ReasonGPUNotAdvertised           = "GPUNotAdvertised"
	ReasonNodeNameCollision          = "NodeNameCollision"
	ReasonBootstrapCredentialSuspect = "BootstrapCredentialSuspect"
	ReasonCellReplacementExhausted   = "CellReplacementExhausted"
	ReasonWaitingForAllocations      = "WaitingForAllocations"
	ReasonDrainTimedOut              = "DrainTimedOut"
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
