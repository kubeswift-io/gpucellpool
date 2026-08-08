package controller

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
	"github.com/kubeswift-io/gpucellpool/internal/capacity"
)

// idleForAges is the happy path: three cells, demand gone for an hour, two idle.
func idleForAges() ScaleInput {
	min, max := int32(1), int32(4)
	free := 2
	longAgo := metav1.NewTime(now.Add(-time.Hour))
	return ScaleInput{
		Replicas: 1,
		Autoscaling: &cellsv1alpha1.AutoscalingSpec{
			Enabled: true, MinReplicas: &min, MaxReplicas: &max,
			ScaleDown: cellsv1alpha1.ScaleDownAuto,
		},
		LiveCells:       3,
		Demand:          capacity.Demand{},
		DemandKnown:     true,
		ShapeKnown:      true,
		FreeGPUs:        &free,
		IdleCells:       []string{"pool-2", "pool-1"},
		Now:             now,
		DemandFreeSince: &longAgo,
	}
}

func TestScaleDownRemovesOneIdleCell(t *testing.T) {
	got := DecideScale(idleForAges())
	if got.Desired != 2 || !got.ScaledDown || got.Reason != cellsv1alpha1.ReasonScaledDown {
		t.Fatalf("got %+v, want 2/ScaledDown", got)
	}
	// The victims must be the idle cells, in the order given.
	if len(got.DrainCandidates) == 0 || got.DrainCandidates[0] != "pool-2" {
		t.Errorf("DrainCandidates = %v, want the idle cells", got.DrainCandidates)
	}
}

func TestScaleDownRequiresAuto(t *testing.T) {
	in := idleForAges()
	in.Autoscaling.ScaleDown = cellsv1alpha1.ScaleDownManual
	if got := DecideScale(in); got.ScaledDown || got.Desired != 3 {
		t.Errorf("got %+v, want no automatic shrink under ScaleDown: Manual", got)
	}
}

func TestScaleDownNeverTouchesABusyCell(t *testing.T) {
	// Demand is gone, but every cell still holds work. Removing one would destroy
	// it, so the pool holds and says why.
	in := idleForAges()
	in.IdleCells = nil
	got := DecideScale(in)
	if got.ScaledDown || got.Reason != cellsv1alpha1.ReasonNoIdleCell {
		t.Errorf("got %+v, want no shrink with NoIdleCell", got)
	}
	if !strings.Contains(got.Message, "still holds workloads") {
		t.Errorf("message should say why: %q", got.Message)
	}
}

func TestScaleDownHonoursTheFloor(t *testing.T) {
	in := idleForAges()
	in.LiveCells = 1 // already at minReplicas
	got := DecideScale(in)
	if got.ScaledDown || got.Reason != cellsv1alpha1.ReasonAtMinReplicas {
		t.Errorf("got %+v, want to hold at the floor", got)
	}
}

func TestScaleDownWaitsForDemandToStayAbsent(t *testing.T) {
	// Absent right now is not the same as absent for the window: a pool that
	// shrinks between two bursts is worse than one that waits.
	in := idleForAges()
	justNow := metav1.NewTime(now.Add(-2 * time.Minute))
	in.DemandFreeSince = &justNow
	got := DecideScale(in)
	if got.ScaledDown || got.Reason != cellsv1alpha1.ReasonStabilizing {
		t.Errorf("got %+v, want to wait", got)
	}

	// Never observed free at all: also a wait, not a shrink.
	in.DemandFreeSince = nil
	if got := DecideScale(in); got.ScaledDown {
		t.Error("shrank without ever observing demand go absent")
	}
}

func TestScaleDownWaitsAfterAnyScalingAction(t *testing.T) {
	for _, name := range []string{"up", "down"} {
		in := idleForAges()
		recent := metav1.NewTime(now.Add(-5 * time.Minute))
		if name == "up" {
			in.LastScaleUp = &recent
		} else {
			in.LastScaleDown = &recent
		}
		if got := DecideScale(in); got.ScaledDown {
			t.Errorf("scaled down %s minutes after a scale-%s", "5", name)
		}
	}
}

func TestScaleDownUsesTheLongerWindow(t *testing.T) {
	in := idleForAges()
	// 45m default-beating window: demand free for 30m is not yet enough.
	in.Autoscaling.ScaleDownStabilizationWindow = &metav1.Duration{Duration: 45 * time.Minute}
	freeFor30 := metav1.NewTime(now.Add(-30 * time.Minute))
	in.DemandFreeSince = &freeFor30
	if got := DecideScale(in); got.ScaledDown {
		t.Error("shrank inside the configured scale-down window")
	}
	freeFor50 := metav1.NewTime(now.Add(-50 * time.Minute))
	in.DemandFreeSince = &freeFor50
	if got := DecideScale(in); !got.ScaledDown {
		t.Error("did not shrink after the configured window")
	}
}

func TestScaleDownNeverRunsOnUnknownDemand(t *testing.T) {
	in := idleForAges()
	in.DemandKnown = false
	if got := DecideScale(in); got.ScaledDown {
		t.Error("shrank while demand was unreadable")
	}
}

func TestMembershipPrefersIdleCellsWhenShrinking(t *testing.T) {
	// The autoscaler names idle cells; membership must remove THOSE, not simply
	// the highest index, or it would destroy running work on cell 2 while cell 0
	// sat empty.
	in := input(2,
		cell(0, cellsv1alpha1.CellPhaseReady),
		cell(1, cellsv1alpha1.CellPhaseReady),
		cell(2, cellsv1alpha1.CellPhaseReady))
	in.DrainPreference = []string{"pool-0"}
	plan := PlanMembership(in)
	if len(plan.Drain) != 1 || plan.Drain[0] != "pool-0" {
		t.Errorf("Drain = %v, want the idle cell pool-0", plan.Drain)
	}
}

func TestMembershipFallsBackToHighestIndex(t *testing.T) {
	// An operator-driven scale-down has no preference list; "make it smaller"
	// means highest index first.
	in := input(1,
		cell(0, cellsv1alpha1.CellPhaseReady),
		cell(1, cellsv1alpha1.CellPhaseReady))
	plan := PlanMembership(in)
	if len(plan.Drain) != 1 || plan.Drain[0] != "pool-1" {
		t.Errorf("Drain = %v, want pool-1", plan.Drain)
	}
}

// The tests below cover the trap that made scale-to-zero a one-way door: with no
// live cell there is no shape to compare a pending request against, so the pool
// reported "nothing fits" and never grew again.

func TestScaleToZeroThenBackUp(t *testing.T) {
	min, max := int32(0), int32(2)
	longAgo := metav1.NewTime(now.Add(-time.Hour))
	in := ScaleInput{
		Replicas: 1,
		Autoscaling: &cellsv1alpha1.AutoscalingSpec{
			Enabled: true, MinReplicas: &min, MaxReplicas: &max,
			ScaleDown: cellsv1alpha1.ScaleDownAuto,
		},
		LiveCells:       1,
		DemandKnown:     true,
		ShapeKnown:      true,
		IdleCells:       []string{"pool-0"},
		Now:             now,
		DemandFreeSince: &longAgo,
	}

	// Down to zero: minReplicas 0 means the last idle cell may go.
	got := DecideScale(in)
	if got.Desired != 0 || !got.ScaledDown {
		t.Fatalf("got %+v, want 0/ScaledDown", got)
	}

	// Now a request arrives at an empty pool. The shape is REMEMBERED, so the pool
	// can still judge it and grow back.
	in.LiveCells = 0
	in.IdleCells = nil
	in.Demand = capacity.Demand{PendingRequests: 1, SatisfiableByOneCell: 1}
	in.DemandFreeSince = nil
	in.LastScaleDown = nil // the window has passed
	if got := DecideScale(in); got.Desired != 1 || !got.ScaledUp {
		t.Fatalf("got %+v, want 1/ScaledUp — an empty pool must be able to grow back", got)
	}
}

func TestUnknownShapeIsNotReportedAsUnsatisfiable(t *testing.T) {
	// A pool that has never advertised a device compared nothing, so blaming the
	// request would send the operator to the wrong place.
	in := scaleInput(0, 0, 2)
	in.ShapeKnown = false
	got := DecideScale(in)
	if got.ScaledUp || got.Reason != cellsv1alpha1.ReasonCellShapeUnknown {
		t.Fatalf("got %+v, want CellShapeUnknown", got)
	}
	if !strings.Contains(got.Message, "learns its cell shape") {
		t.Errorf("message should tell the operator what to do: %q", got.Message)
	}
}

func TestCellShapePrefersLiveOverRemembered(t *testing.T) {
	live := capacity.Capacity{ByModel: []capacity.ModelCapacity{{
		Model: "GTX 1080", Devices: 1, MemoryTotalMiB: 8192, ComputeTotal: 100,
	}}}
	remembered := &cellsv1alpha1.CellDeviceShape{Model: "stale", MemoryMiB: 40960, CorePercent: 100}

	if got := CellShape(live, remembered); got == nil || got.MemoryMiB != 8192 {
		t.Errorf("CellShape = %+v, want the live 8192MiB device", got)
	}
	// No live cell: fall back, which is the whole point.
	if got := CellShape(capacity.Capacity{}, remembered); got == nil || got.MemoryMiB != 40960 {
		t.Errorf("CellShape = %+v, want the remembered device", got)
	}
	// Nothing either way stays nil rather than becoming a zero-sized device that
	// every request would "fit".
	if got := CellShape(capacity.Capacity{}, nil); got != nil {
		t.Errorf("CellShape = %+v, want nil", got)
	}
	if got := CellShape(capacity.Capacity{}, &cellsv1alpha1.CellDeviceShape{}); got != nil {
		t.Errorf("CellShape = %+v, want nil for an empty remembered shape", got)
	}
}

func TestRememberShapeIgnoresAnEmptyReading(t *testing.T) {
	// A failed capacity read must not overwrite a good memory with zeros.
	if got := RememberShape(nil, now); got != nil {
		t.Errorf("RememberShape(nil) = %+v, want nil", got)
	}
	if got := RememberShape(&capacity.Device{Model: "x"}, now); got != nil {
		t.Errorf("RememberShape(0MiB) = %+v, want nil", got)
	}
	got := RememberShape(&capacity.Device{Model: "GTX 1080", MemoryMiB: 8192, CorePercent: 100}, now)
	if got == nil || got.MemoryMiB != 8192 || got.LastObserved == nil {
		t.Errorf("RememberShape = %+v, want a stamped shape", got)
	}
}

// TestPoolScalesToZeroAndBackUp is the round trip through the reconciler, not just
// the decision function: a pool empties itself, forgets nothing, and grows again on
// the next request. Before status.cellDeviceShape existed the second half failed —
// an empty pool had no shape to judge a request against and stayed empty forever.
func TestPoolScalesToZeroAndBackUp(t *testing.T) {
	i32 := func(i int32) *int32 { return &i }
	f := newFixture(t, func(p *cellsv1alpha1.GPUCellPool) {
		p.Spec.Autoscaling = &cellsv1alpha1.AutoscalingSpec{
			Enabled: true, MinReplicas: i32(1), MaxReplicas: i32(2),
			ScaleDown:                    cellsv1alpha1.ScaleDownAuto,
			ScaleDownStabilizationWindow: &metav1.Duration{Duration: time.Minute},
		}
	})
	f.readyCell("cells-0")

	shape := f.getPool().Status.CellDeviceShape
	if shape == nil || shape.MemoryMiB != 8192 || shape.CorePercent != 100 {
		t.Fatalf("cellDeviceShape = %+v, want the 8192MiB device the cell advertises", shape)
	}

	// Allow the floor to reach zero, then let the quiet window pass.
	f.patchPool(func(p *cellsv1alpha1.GPUCellPool) { p.Spec.Autoscaling.MinReplicas = i32(0) })
	f.reconcile()
	f.now = f.now.Add(2 * time.Minute)

	// Drain and removal take a few passes: mark draining, gate on allocations, delete
	// the guest, then clear the status row on the next discovery.
	for i := 0; i < 6; i++ {
		f.reconcile()
		if len(f.guests()) == 0 && len(f.getPool().Status.Cells) == 0 {
			break
		}
	}
	if got := len(f.guests()); got != 0 {
		t.Fatalf("got %d cells, want the idle cell removed", got)
	}
	if rows := f.getPool().Status.Cells; len(rows) != 0 {
		t.Fatalf("status still lists %+v after the guest was removed", rows)
	}
	if p := f.getPool(); p.Status.CellDeviceShape == nil {
		t.Fatal("the pool forgot its cell shape when the last cell went, so it can never grow back")
	}

	// A request arrives at an empty pool. It must be judged against the REMEMBERED
	// shape and answered with a cell.
	f.now = f.now.Add(2 * time.Minute)
	f.unschedulableGPUPod("waiting", 3000, 30)
	f.reconcile()

	pool := f.getPool()
	if pool.Status.Demand == nil || pool.Status.Demand.SatisfiableByOneCell != 1 {
		t.Fatalf("demand = %+v, want 1 satisfiable against the remembered shape", pool.Status.Demand)
	}
	if got := len(f.guests()); got != 1 {
		t.Errorf("got %d cells, want the pool to grow back from zero", got)
	}
}
