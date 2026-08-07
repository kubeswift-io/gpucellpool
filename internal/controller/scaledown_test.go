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
