// Package metrics publishes what an operator needs to see about a pool without
// reading status by hand.
//
// Both layers are observable, deliberately with separate names: physical GPUs are
// whole devices from the infrastructure cluster, capacity_* is fractional capacity
// from the workload cluster. Collapsing them into one "gpu" metric would recreate
// exactly the confusion this design exists to avoid.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const prefix = "gpucell_"

var (
	// CellsDesired is what the scaling policy asked for — spec.replicas, or the
	// autoscaler's decision.
	CellsDesired = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: prefix + "cells_desired",
		Help: "Desired number of cells in the pool.",
	}, []string{"pool", "namespace"})

	// Cells counts cells by phase. Every phase is written on every pass, so a
	// phase that empties reports 0 rather than going stale at its last value.
	Cells = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: prefix + "cells",
		Help: "Number of cells in each phase.",
	}, []string{"pool", "namespace", "phase"})

	// CellStartupSeconds is the time from a cell's creation to Ready.
	//
	// This is the number that decides how autoscaling should be framed: a cell
	// that takes fifteen minutes is a capacity-planning unit, not something a
	// reactive autoscaler can chase. Buckets span "baked image, thin enrollment"
	// through "installs a driver at first boot", which measured ~14 minutes.
	CellStartupSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    prefix + "cell_startup_seconds",
		Help:    "Seconds from cell creation to Ready.",
		Buckets: []float64{30, 60, 120, 180, 300, 600, 900, 1800},
	}, []string{"pool", "namespace"})

	// CellTransitionsTotal records every phase change, so a cell that oscillates
	// (Ready -> AwaitingGPUCapacity -> Ready) is visible even though its current
	// phase looks healthy at any instant.
	CellTransitionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: prefix + "cell_transitions_total",
		Help: "Cell phase transitions.",
	}, []string{"pool", "namespace", "from", "to"})

	// PhysicalGPUs is OUTER capacity: whole devices. state is held|free.
	PhysicalGPUs = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: prefix + "physical_gpus",
		Help: "Whole physical GPUs, by state (held by this pool, or free in the infrastructure cluster).",
	}, []string{"pool", "namespace", "state"})

	// CapacityGPUDevices is INNER: how many devices the capacity provider
	// advertises across the pool's cells.
	CapacityGPUDevices = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: prefix + "capacity_gpu_devices",
		Help: "GPU devices advertised by the capacity provider across the pool's cells.",
	}, []string{"pool", "namespace"})

	// CapacityGPUMemoryBytes is INNER fractional memory. state is total|allocated|available.
	CapacityGPUMemoryBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: prefix + "capacity_gpu_memory_bytes",
		Help: "GPU memory known to the capacity provider, by state.",
	}, []string{"pool", "namespace", "state"})

	// CapacityGPUComputePercent is INNER fractional compute, in percent of a
	// device. Only published for a homogeneous pool: percentages of different GPU
	// models are not commensurable.
	CapacityGPUComputePercent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: prefix + "capacity_gpu_compute_percent",
		Help: "GPU compute known to the capacity provider, by state (homogeneous pools only).",
	}, []string{"pool", "namespace", "state"})

	// WorkloadClusterReachable is 1 or 0. While it is 0 the operator freezes every
	// destructive path, so an alert on it explains why a pool stopped changing.
	WorkloadClusterReachable = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: prefix + "workload_cluster_reachable",
		Help: "1 when the workload cluster answered on the last reconcile, 0 otherwise.",
	}, []string{"pool", "namespace"})

	// CapacityScrapeErrorsTotal counts failures to READ capacity. It exists
	// because a failed read is reported as Unknown and the previous numbers are
	// retained — which is right, but means the gauges alone cannot tell you the
	// data went stale.
	CapacityScrapeErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: prefix + "capacity_scrape_errors_total",
		Help: "Failures to read capacity or demand from the workload cluster.",
	}, []string{"pool", "namespace", "reason"})

	// ScaleDecisionsTotal records what the autoscaler decided and why, including
	// the refusals — "did not scale, and here is the reason" is the interesting
	// case.
	ScaleDecisionsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: prefix + "scale_decisions_total",
		Help: "Scaling decisions by reason.",
	}, []string{"pool", "namespace", "reason", "scaled_up"})

	// ReconcileErrorsTotal counts reconciles that returned an error.
	ReconcileErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: prefix + "reconcile_errors_total",
		Help: "Reconcile errors.",
	}, []string{"pool", "namespace"})
)

func init() {
	metrics.Registry.MustRegister(
		CellsDesired, Cells, CellStartupSeconds, CellTransitionsTotal,
		PhysicalGPUs, CapacityGPUDevices, CapacityGPUMemoryBytes, CapacityGPUComputePercent,
		WorkloadClusterReachable, CapacityScrapeErrorsTotal, ScaleDecisionsTotal,
		ReconcileErrorsTotal,
	)
}

// ForgetPool drops a deleted pool's series, so a torn-down pool does not linger in
// the metrics forever reporting its last state as if it were current.
func ForgetPool(namespace, name string) {
	labels := prometheus.Labels{"pool": name, "namespace": namespace}
	for _, v := range []interface {
		DeletePartialMatch(prometheus.Labels) int
	}{
		CellsDesired, Cells, CellTransitionsTotal, PhysicalGPUs, CapacityGPUDevices,
		CapacityGPUMemoryBytes, CapacityGPUComputePercent, WorkloadClusterReachable,
		CapacityScrapeErrorsTotal, ScaleDecisionsTotal, ReconcileErrorsTotal,
		CellStartupSeconds,
	} {
		v.DeletePartialMatch(labels)
	}
}
