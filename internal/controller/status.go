package controller

import (
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
	"github.com/kubeswift-io/gpucellpool/internal/capacity"
)

// CountCells tallies the cell set for status.
func CountCells(cells []cellsv1alpha1.CellStatus) (total, ready, creating, draining, failed int32) {
	for _, c := range cells {
		total++
		switch c.Phase {
		case cellsv1alpha1.CellPhaseReady:
			ready++
		case cellsv1alpha1.CellPhaseFailed:
			failed++
		case cellsv1alpha1.CellPhaseDraining, cellsv1alpha1.CellPhaseDeleting:
			draining++
		default:
			creating++
		}
	}
	return
}

// WorkloadCapacityStatus converts a provider reading into API status.
//
// Two rules are enforced here rather than in the provider, because they are about
// what we are willing to PUBLISH:
//
//   - Memory is always aggregated: bytes are commensurable across GPU models.
//   - Compute is a percentage OF A DEVICE, so the flat aggregate is published
//     only while the pool is homogeneous. 100 cores of a GTX 1080 and 100 of an
//     H200 are not 200 of anything, and ByModel is the only compute truth.
func WorkloadCapacityStatus(provider, mode string, c capacity.Capacity, observed metav1.Time) *cellsv1alpha1.WorkloadCapacityStatus {
	out := &cellsv1alpha1.WorkloadCapacityStatus{
		Provider:     provider,
		Mode:         mode,
		GPUDevices:   int32(c.Devices),
		Homogeneous:  c.Homogeneous,
		GPUMemory:    memoryCapacity(c.MemoryTotalMiB, c.MemoryAllocatedMiB),
		LastObserved: &observed,
	}
	if c.Homogeneous {
		out.GPUCompute = &cellsv1alpha1.ComputeCapacity{
			Total:     int32(c.ComputeTotal),
			Allocated: int32(c.ComputeAllocated),
			Available: int32(c.ComputeTotal - c.ComputeAllocated),
		}
	}
	for _, m := range c.ByModel {
		out.ByModel = append(out.ByModel, cellsv1alpha1.ModelCapacity{
			Model:   m.Model,
			Devices: int32(m.Devices),
			Memory:  memoryCapacity(m.MemoryTotalMiB, m.MemoryAllocatedMiB),
			Compute: &cellsv1alpha1.ComputeCapacity{
				Total:     int32(m.ComputeTotal),
				Allocated: int32(m.ComputeAllocated),
				Available: int32(m.ComputeTotal - m.ComputeAllocated),
			},
		})
	}
	return out
}

func memoryCapacity(totalMiB, allocatedMiB int64) *cellsv1alpha1.MemoryCapacity {
	const mib = 1024 * 1024
	return &cellsv1alpha1.MemoryCapacity{
		Total:     *resource.NewQuantity(totalMiB*mib, resource.BinarySI),
		Allocated: *resource.NewQuantity(allocatedMiB*mib, resource.BinarySI),
		Available: *resource.NewQuantity((totalMiB-allocatedMiB)*mib, resource.BinarySI),
	}
}

// ConditionInput is what the pool's conditions are computed from.
type ConditionInput struct {
	Generation int64

	Desired int32
	Ready   int32

	WorkloadReachable bool
	WorkloadReason    string
	WorkloadMessage   string

	ProviderHealth capacity.Health
	// CapacityKnown is false when the provider could not be read; capacity is
	// then Unknown, never zero.
	CapacityKnown bool
	Capacity      capacity.Capacity

	// FreeGPUs is the outer inventory reading; nil means unknown.
	FreeGPUs           *int
	CellsWaitingForGPU int

	Membership  MembershipPlan
	Progressing bool

	// Autoscaling reports whether demand-driven scale-up is enabled, and Scale is
	// the last decision — the reason is where an operator looks to understand why
	// the pool did or did not grow.
	Autoscaling bool
	Scale       ScaleDecision
	DemandKnown bool
}

// ComputeConditions returns the pool's conditions. They split the layers on
// purpose, so a single False localises the fault: outer (PhysicalGPUsAvailable),
// transport (WorkloadClusterReachable), provider (CapacityProviderReady), inner
// capacity (CapacityAvailable), aggregate (Ready).
func ComputeConditions(in ConditionInput) []metav1.Condition {
	var out []metav1.Condition
	set := func(t string, ok bool, reason, msg string) {
		status := metav1.ConditionFalse
		if ok {
			status = metav1.ConditionTrue
		}
		out = append(out, metav1.Condition{
			Type:               t,
			Status:             status,
			Reason:             reason,
			Message:            msg,
			ObservedGeneration: in.Generation,
		})
	}

	// Transport first: everything below is less trustworthy when this is False.
	reachReason := in.WorkloadReason
	if reachReason == "" {
		reachReason = cellsv1alpha1.ReasonConnected
	}
	set(cellsv1alpha1.ConditionWorkloadClusterReachable, in.WorkloadReachable, reachReason, in.WorkloadMessage)

	// Provider.
	providerReason := in.ProviderHealth.Reason
	if providerReason == "" {
		providerReason = cellsv1alpha1.ReasonHAMiDetected
	}
	set(cellsv1alpha1.ConditionCapacityProviderReady, in.ProviderHealth.Ready, providerReason, in.ProviderHealth.Message)

	// Outer inventory. False only when the pool actually wants a GPU and none is
	// free — a saturated cluster with a satisfied pool is not a fault. "Wants"
	// includes a pool that could not even create a cell object yet: that is the
	// commonest shape of this failure and the earliest moment we can name it.
	wantsGPU := in.CellsWaitingForGPU > 0 || in.Membership.Reason == cellsv1alpha1.ReasonInsufficientGPUs
	switch {
	case in.FreeGPUs == nil:
		set(cellsv1alpha1.ConditionPhysicalGPUsAvailable, true, cellsv1alpha1.ReasonCapacityUnknown,
			"free GPU count could not be read from the infrastructure cluster")
	case *in.FreeGPUs <= 0 && wantsGPU:
		msg := "the pool wants more cells and no free GPU is available in the infrastructure cluster"
		if in.CellsWaitingForGPU > 0 {
			msg = itoa(in.CellsWaitingForGPU) + " cell(s) waiting and no free GPU in the infrastructure cluster"
		}
		set(cellsv1alpha1.ConditionPhysicalGPUsAvailable, false, cellsv1alpha1.ReasonInsufficientGPUs, msg)
	default:
		set(cellsv1alpha1.ConditionPhysicalGPUsAvailable, true, cellsv1alpha1.ReasonFreeDevices,
			itoa(deref(in.FreeGPUs))+" free GPU(s) in the infrastructure cluster")
	}

	// Inner capacity.
	switch {
	case !in.CapacityKnown:
		set(cellsv1alpha1.ConditionCapacityAvailable, false, cellsv1alpha1.ReasonCapacityUnknown,
			"capacity could not be read; the last known values are retained")
	case !in.Capacity.Homogeneous:
		set(cellsv1alpha1.ConditionCapacityAvailable,
			in.Capacity.MemoryAllocatedMiB < in.Capacity.MemoryTotalMiB,
			cellsv1alpha1.ReasonHeterogeneous,
			"pool holds more than one GPU model; per-model capacity is authoritative and the pool-wide compute aggregate is omitted")
	case in.Capacity.Devices == 0 && emptyByDesign(in):
		// A pool holding no cells advertises no capacity, and that is not a fault or
		// an unknown. Distinguishing it matters: the case below is the failure this
		// pool exists to catch (a booted VM whose GPU never surfaced).
		set(cellsv1alpha1.ConditionCapacityAvailable, false, cellsv1alpha1.ReasonScaledToZero,
			"the pool holds no cells, so it advertises no GPU capacity; "+
				"a pending request that one cell can satisfy will create one")
	case in.Capacity.Devices == 0:
		set(cellsv1alpha1.ConditionCapacityAvailable, false, cellsv1alpha1.ReasonCapacityUnknown,
			"no GPU devices are advertised by the capacity provider yet")
	case in.Capacity.MemoryAllocatedMiB >= in.Capacity.MemoryTotalMiB ||
		in.Capacity.ComputeAllocated >= in.Capacity.ComputeTotal:
		set(cellsv1alpha1.ConditionCapacityAvailable, false, cellsv1alpha1.ReasonSaturated,
			"every advertised GPU is fully allocated")
	default:
		set(cellsv1alpha1.ConditionCapacityAvailable, true, cellsv1alpha1.ReasonCapacityFree, "")
	}

	// Progress.
	progressing := in.Progressing && !in.Membership.Stalled
	progReason := cellsv1alpha1.ReasonIdle
	progMsg := ""
	switch {
	case in.Membership.Stalled:
		progReason = in.Membership.Reason
		progMsg = in.Membership.Message
	case progressing:
		progReason = cellsv1alpha1.ReasonCellCreating
		progMsg = in.Membership.Message
	}
	set(cellsv1alpha1.ConditionProgressing, progressing, progReason, progMsg)

	// Scaling. Only present when enabled, so a pool that does not autoscale is not
	// cluttered with a condition about it.
	if in.Autoscaling {
		set(cellsv1alpha1.ConditionScalingActive, in.DemandKnown, in.Scale.Reason, in.Scale.Message)
	}

	// Aggregate.
	ready := in.Ready == in.Desired
	readyReason := cellsv1alpha1.ReasonAllCellsReady
	readyMsg := ""
	switch {
	case !in.WorkloadReachable:
		ready = false
		readyReason = cellsv1alpha1.ReasonUnreachable
		readyMsg = "workload cluster unreachable"
	case !in.ProviderHealth.Ready:
		ready = false
		readyReason = in.ProviderHealth.Reason
		readyMsg = in.ProviderHealth.Message
	case emptyByDesign(in):
		// Nothing is wrong: the pool was asked for no cells and holds none.
		readyReason = cellsv1alpha1.ReasonScaledToZero
		readyMsg = "the pool holds no cells by design (desired 0)"
	case !ready:
		readyReason = cellsv1alpha1.ReasonCellsNotReady
		readyMsg = itoa(int(in.Ready)) + " of " + itoa(int(in.Desired)) + " cells are Ready"
	}
	set(cellsv1alpha1.ConditionReady, ready, readyReason, readyMsg)

	return out
}

// emptyByDesign reports whether the pool is holding no cells because none were
// asked for, as opposed to holding none because it cannot create them.
func emptyByDesign(in ConditionInput) bool {
	return in.Desired == 0 && in.Ready == 0
}

// ApplyConditions merges computed conditions into an existing slice, preserving
// LastTransitionTime for conditions whose status did not change.
func ApplyConditions(existing []metav1.Condition, computed []metav1.Condition) []metav1.Condition {
	out := existing
	for _, c := range computed {
		meta.SetStatusCondition(&out, c)
	}
	return out
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
