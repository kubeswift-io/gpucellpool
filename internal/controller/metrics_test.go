package controller

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
	"github.com/kubeswift-io/gpucellpool/internal/capacity"
	poolmetrics "github.com/kubeswift-io/gpucellpool/internal/metrics"
)

func metricsPool(name string) *cellsv1alpha1.GPUCellPool {
	return &cellsv1alpha1.GPUCellPool{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name},
	}
}

func TestPublishMetricsBothLayersStaySeparate(t *testing.T) {
	r := &GPUCellPoolReconciler{}
	pool := metricsPool("m1")
	t.Cleanup(func() { poolmetrics.ForgetPool("ns", "m1") })

	free := 3
	cells := []cellsv1alpha1.CellStatus{
		{Name: "m1-0", Phase: cellsv1alpha1.CellPhaseReady, Devices: []string{"0000:01:00.0"}},
		{Name: "m1-1", Phase: cellsv1alpha1.CellPhaseBooting},
	}
	cap0 := capacity.Capacity{
		Devices: 1, Homogeneous: true,
		MemoryTotalMiB: 8192, MemoryAllocatedMiB: 3000,
		ComputeTotal: 100, ComputeAllocated: 30,
	}
	r.publishMetrics(pool, cells, ScaleDecision{Desired: 2}, &free, cap0, true)

	if got := testutil.ToFloat64(poolmetrics.CellsDesired.WithLabelValues("m1", "ns")); got != 2 {
		t.Errorf("cells_desired = %v, want 2", got)
	}
	if got := testutil.ToFloat64(poolmetrics.Cells.WithLabelValues("m1", "ns", "Ready")); got != 1 {
		t.Errorf("cells{Ready} = %v, want 1", got)
	}
	// A phase with no cells must report 0, not go stale.
	if got := testutil.ToFloat64(poolmetrics.Cells.WithLabelValues("m1", "ns", "Failed")); got != 0 {
		t.Errorf("cells{Failed} = %v, want 0", got)
	}

	// Whole devices (outer) and fractional capacity (inner) are different metrics
	// on purpose: an alert must be able to tell "no GPU left in the cluster" from
	// "the shared GPU is full".
	if got := testutil.ToFloat64(poolmetrics.PhysicalGPUs.WithLabelValues("m1", "ns", "held")); got != 1 {
		t.Errorf("physical_gpus{held} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(poolmetrics.PhysicalGPUs.WithLabelValues("m1", "ns", "free")); got != 3 {
		t.Errorf("physical_gpus{free} = %v, want 3", got)
	}
	if got := testutil.ToFloat64(poolmetrics.CapacityGPUMemoryBytes.WithLabelValues("m1", "ns", "available")); got != 5192*mib {
		t.Errorf("capacity memory available = %v, want %v", got, 5192*mib)
	}
	if got := testutil.ToFloat64(poolmetrics.CapacityGPUComputePercent.WithLabelValues("m1", "ns", "allocated")); got != 30 {
		t.Errorf("capacity compute allocated = %v, want 30", got)
	}
}

func TestPublishMetricsOmitsComputeForAMixedModelPool(t *testing.T) {
	r := &GPUCellPoolReconciler{}
	pool := metricsPool("m2")
	t.Cleanup(func() { poolmetrics.ForgetPool("ns", "m2") })

	// 100 cores of one model and 100 of another are not 200 of anything.
	r.publishMetrics(pool, nil, ScaleDecision{Desired: 2}, nil, capacity.Capacity{
		Devices: 2, Homogeneous: false, MemoryTotalMiB: 54260, ComputeTotal: 200,
	}, true)

	if got := testutil.ToFloat64(poolmetrics.CapacityGPUComputePercent.WithLabelValues("m2", "ns", "total")); got != 0 {
		t.Errorf("published a pool-wide compute figure for a mixed-model pool: %v", got)
	}
	// Memory stays valid: bytes are commensurable.
	if got := testutil.ToFloat64(poolmetrics.CapacityGPUMemoryBytes.WithLabelValues("m2", "ns", "total")); got != 54260*mib {
		t.Errorf("capacity memory total = %v", got)
	}
}

func TestPublishMetricsPublishesNothingWhenCapacityIsUnknown(t *testing.T) {
	r := &GPUCellPoolReconciler{}
	pool := metricsPool("m3")
	t.Cleanup(func() { poolmetrics.ForgetPool("ns", "m3") })

	// Status retains the previous values on an unreadable provider; the gauges must
	// not invent a zero, because a zero reads as "measured, and it is empty".
	r.publishMetrics(pool, nil, ScaleDecision{Desired: 1}, nil, capacity.Capacity{}, false)
	if got := testutil.ToFloat64(poolmetrics.CapacityGPUDevices.WithLabelValues("m3", "ns")); got != 0 {
		t.Errorf("capacity_gpu_devices = %v, want the series absent/zero", got)
	}
}

func TestForgetPoolDropsSeries(t *testing.T) {
	r := &GPUCellPoolReconciler{}
	pool := metricsPool("m4")
	r.publishMetrics(pool, nil, ScaleDecision{Desired: 7}, nil, capacity.Capacity{}, false)
	if got := testutil.ToFloat64(poolmetrics.CellsDesired.WithLabelValues("m4", "ns")); got != 7 {
		t.Fatalf("setup: cells_desired = %v", got)
	}
	// A torn-down pool must stop reporting its last state as if it were current.
	poolmetrics.ForgetPool("ns", "m4")
	if got := testutil.ToFloat64(poolmetrics.CellsDesired.WithLabelValues("m4", "ns")); got != 0 {
		t.Errorf("cells_desired = %v after ForgetPool, want the series gone", got)
	}
}
