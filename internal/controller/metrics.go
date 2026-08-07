package controller

import (
	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
	"github.com/kubeswift-io/gpucellpool/internal/capacity"
	poolmetrics "github.com/kubeswift-io/gpucellpool/internal/metrics"
)

// allCellPhases is written on every pass so a phase that empties reports 0 rather
// than going stale at whatever it last held.
var allCellPhases = []cellsv1alpha1.CellPhase{
	cellsv1alpha1.CellPhasePending, cellsv1alpha1.CellPhaseAllocatingGPU,
	cellsv1alpha1.CellPhaseGuestProvisioning, cellsv1alpha1.CellPhaseBooting,
	cellsv1alpha1.CellPhaseJoining, cellsv1alpha1.CellPhaseAwaitingGPUCapacity,
	cellsv1alpha1.CellPhaseReady, cellsv1alpha1.CellPhaseDraining,
	cellsv1alpha1.CellPhaseDeleting, cellsv1alpha1.CellPhaseFailed,
}

const mib = 1024 * 1024

// publishMetrics mirrors the pool's observed state into Prometheus. Everything
// here is already in status; the point is that both LAYERS stay separately named,
// so an alert can distinguish "no physical GPU left" from "the shared GPU is full".
func (r *GPUCellPoolReconciler) publishMetrics(
	pool *cellsv1alpha1.GPUCellPool, cells []cellsv1alpha1.CellStatus,
	scale ScaleDecision, freeGPUs *int, cap0 capacity.Capacity, capacityKnown bool,
) {
	name, ns := pool.Name, pool.Namespace

	poolmetrics.CellsDesired.WithLabelValues(name, ns).Set(float64(scale.Desired))

	byPhase := map[cellsv1alpha1.CellPhase]int{}
	held := 0
	for _, c := range cells {
		byPhase[c.Phase]++
		held += len(c.Devices)
	}
	for _, p := range allCellPhases {
		poolmetrics.Cells.WithLabelValues(name, ns, string(p)).Set(float64(byPhase[p]))
	}

	poolmetrics.PhysicalGPUs.WithLabelValues(name, ns, "held").Set(float64(held))
	if freeGPUs != nil {
		poolmetrics.PhysicalGPUs.WithLabelValues(name, ns, "free").Set(float64(*freeGPUs))
	}

	// A failed read retains the previous status values on purpose, so publishing
	// nothing here is right: the scrape-error counter is what says it went stale.
	if !capacityKnown {
		return
	}
	poolmetrics.CapacityGPUDevices.WithLabelValues(name, ns).Set(float64(cap0.Devices))
	poolmetrics.CapacityGPUMemoryBytes.WithLabelValues(name, ns, "total").Set(float64(cap0.MemoryTotalMiB * mib))
	poolmetrics.CapacityGPUMemoryBytes.WithLabelValues(name, ns, "allocated").Set(float64(cap0.MemoryAllocatedMiB * mib))
	poolmetrics.CapacityGPUMemoryBytes.WithLabelValues(name, ns, "available").
		Set(float64((cap0.MemoryTotalMiB - cap0.MemoryAllocatedMiB) * mib))

	// Percentages of different GPU models are not commensurable, so a mixed-model
	// pool publishes no pool-wide compute figure at all.
	if cap0.Homogeneous {
		poolmetrics.CapacityGPUComputePercent.WithLabelValues(name, ns, "total").Set(float64(cap0.ComputeTotal))
		poolmetrics.CapacityGPUComputePercent.WithLabelValues(name, ns, "allocated").Set(float64(cap0.ComputeAllocated))
		poolmetrics.CapacityGPUComputePercent.WithLabelValues(name, ns, "available").
			Set(float64(cap0.ComputeTotal - cap0.ComputeAllocated))
	}
}

func boolGauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
