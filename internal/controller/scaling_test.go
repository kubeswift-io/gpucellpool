package controller

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
	"github.com/kubeswift-io/gpucellpool/internal/capacity"
)

func autoscaling(min, max int32) *cellsv1alpha1.AutoscalingSpec {
	return &cellsv1alpha1.AutoscalingSpec{
		Enabled:     true,
		MinReplicas: &min,
		MaxReplicas: &max,
		ScaleDown:   cellsv1alpha1.ScaleDownManual,
	}
}

func scaleInput(live int32, satisfiable, pending int) ScaleInput {
	free := 4
	return ScaleInput{
		Replicas:    1,
		Autoscaling: autoscaling(1, 4),
		LiveCells:   live,
		Demand:      capacity.Demand{PendingRequests: pending, SatisfiableByOneCell: satisfiable},
		DemandKnown: true,
		// A pool with live cells knows what a cell brings; the shape-unknown case is
		// exercised on its own below.
		ShapeKnown: true,
		FreeGPUs:   &free,
		Now:        now,
	}
}

func TestDecideScaleDisabledHoldsReplicas(t *testing.T) {
	in := scaleInput(3, 5, 5)
	in.Autoscaling = nil
	if got := DecideScale(in); got.Desired != 1 || got.ScaledUp {
		t.Errorf("got %+v, want spec.replicas with no scaling", got)
	}
	in.Autoscaling = &cellsv1alpha1.AutoscalingSpec{Enabled: false, MaxReplicas: ptr32(9)}
	if got := DecideScale(in); got.Desired != 1 {
		t.Errorf("got %+v, want spec.replicas when disabled", got)
	}
}

func TestDecideScaleGrowsOneAtATime(t *testing.T) {
	// A cell costs a physical GPU and minutes of boot, so five pending requests
	// still buy exactly one cell this pass.
	got := DecideScale(scaleInput(1, 5, 5))
	if got.Desired != 2 || !got.ScaledUp || got.Reason != cellsv1alpha1.ReasonScaledUp {
		t.Errorf("got %+v, want 2/ScaledUp", got)
	}
}

func TestDecideScaleRefusesWhatACellCannotFix(t *testing.T) {
	// Pending, but nothing a cell of this shape satisfies: a two-device request, or
	// one asking for more memory than a single device has. Creating a cell would
	// burn a GPU to change nothing.
	got := DecideScale(scaleInput(1, 0, 3))
	if got.ScaledUp || got.Reason != cellsv1alpha1.ReasonDemandUnsatisfiable {
		t.Errorf("got %+v, want no scale-up with DemandUnsatisfiable", got)
	}
	if !strings.Contains(got.Message, "none fits one cell") {
		t.Errorf("message should say why: %q", got.Message)
	}
}

func TestDecideScaleNeverActsOnUnknownDemand(t *testing.T) {
	in := scaleInput(1, 5, 5)
	in.DemandKnown = false
	got := DecideScale(in)
	if got.ScaledUp || got.Reason != cellsv1alpha1.ReasonCapacityUnknown {
		t.Errorf("got %+v, want no scale-up when demand could not be read", got)
	}
}

func TestDecideScaleRespectsTheCeiling(t *testing.T) {
	got := DecideScale(scaleInput(4, 5, 5))
	if got.Desired != 4 || got.ScaledUp || got.Reason != cellsv1alpha1.ReasonAtMaxReplicas {
		t.Errorf("got %+v, want to stay at max", got)
	}
	if !strings.Contains(got.Message, "still pending") {
		t.Errorf("operator needs to know demand is unmet: %q", got.Message)
	}
}

func TestDecideScaleRespectsTheFloorWithoutShrinking(t *testing.T) {
	// Below the floor: come up to it even with no demand.
	in := scaleInput(0, 0, 0)
	if got := DecideScale(in); got.Desired != 1 {
		t.Errorf("got %+v, want the floor", got)
	}
	// Above the floor with no demand: hold, do not shrink. Shrinking is
	// operator-triggered in v1alpha1.
	in = scaleInput(3, 0, 0)
	if got := DecideScale(in); got.Desired != 3 {
		t.Errorf("got %+v, want to hold at 3 rather than shrink to the floor", got)
	}
}

func TestDecideScaleWaitsOutTheStabilizationWindow(t *testing.T) {
	in := scaleInput(1, 5, 5)
	in.Autoscaling.StabilizationWindow = &metav1.Duration{Duration: 10 * time.Minute}

	just := metav1.NewTime(now.Add(-2 * time.Minute))
	in.LastScaleUp = &just
	got := DecideScale(in)
	if got.ScaledUp || got.Reason != cellsv1alpha1.ReasonStabilizing {
		t.Errorf("got %+v, want to wait: demand does not clear until the new cell is Ready", got)
	}

	old := metav1.NewTime(now.Add(-11 * time.Minute))
	in.LastScaleUp = &old
	if got := DecideScale(in); !got.ScaledUp {
		t.Errorf("got %+v, want a scale-up after the window", got)
	}
}

func TestDecideScaleWillNotPromiseAGPUThatDoesNotExist(t *testing.T) {
	in := scaleInput(1, 5, 5)
	zero := 0
	in.FreeGPUs = &zero
	got := DecideScale(in)
	if got.ScaledUp || got.Reason != cellsv1alpha1.ReasonInsufficientGPUs {
		t.Errorf("got %+v, want no scale-up with no free GPU", got)
	}

	// Unknown is not zero: proceed and let the scheduler decide.
	in.FreeGPUs = nil
	if got := DecideScale(in); !got.ScaledUp {
		t.Errorf("got %+v, want a scale-up when the free count is unknown", got)
	}
}

func TestReferenceDevice(t *testing.T) {
	ref := ReferenceDevice(capacity.Capacity{ByModel: []capacity.ModelCapacity{{
		Model: "NVIDIA GeForce GTX 1080", Devices: 2,
		MemoryTotalMiB: 16384, ComputeTotal: 200,
	}}})
	if ref == nil || ref.MemoryMiB != 8192 || ref.CorePercent != 100 {
		t.Errorf("ReferenceDevice = %+v, want per-device 8192/100", ref)
	}
	// An empty pool has nothing to learn the shape from, and guessing would let
	// the scaler act on a fiction.
	if got := ReferenceDevice(capacity.Capacity{}); got != nil {
		t.Errorf("ReferenceDevice = %+v, want nil for an empty pool", got)
	}
}

func ptr32(i int32) *int32 { return &i }
