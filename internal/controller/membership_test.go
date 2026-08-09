package controller

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
	"github.com/kubeswift-io/gpucellpool/internal/capacity"
)

func cell(index int32, phase cellsv1alpha1.CellPhase) cellsv1alpha1.CellStatus {
	return cellsv1alpha1.CellStatus{
		Name:               "pool-" + itoa(int(index)),
		Index:              index,
		Phase:              phase,
		LastTransitionTime: &metav1.Time{Time: now},
	}
}

func input(desired int32, cells ...cellsv1alpha1.CellStatus) MembershipInput {
	return MembershipInput{
		Desired:           desired,
		Cells:             cells,
		Now:               now,
		MaxCreating:       DefaultMaxCreating,
		WorkloadReachable: true,
		EverReady:         true,
	}
}

func TestPlanMembershipCreatesUpToTheInFlightCap(t *testing.T) {
	// Creating a cell costs a physical GPU and a VM boot; a thundering herd
	// produces N unschedulable pods and no diagnosis.
	plan := PlanMembership(input(5))
	if len(plan.Create) != DefaultMaxCreating {
		t.Errorf("created %d cells, want the cap of %d", len(plan.Create), DefaultMaxCreating)
	}
	if plan.Create[0] != 0 || plan.Create[1] != 1 {
		t.Errorf("indexes = %v, want [0 1]", plan.Create)
	}

	// With cells already in flight there is no room for more.
	plan = PlanMembership(input(5, cell(0, cellsv1alpha1.CellPhaseBooting), cell(1, cellsv1alpha1.CellPhaseJoining)))
	if len(plan.Create) != 0 {
		t.Errorf("created %v while two cells were already in flight", plan.Create)
	}
}

func TestPlanMembershipReusesFreedIndexes(t *testing.T) {
	plan := PlanMembership(input(3, cell(0, cellsv1alpha1.CellPhaseReady), cell(2, cellsv1alpha1.CellPhaseReady)))
	if len(plan.Create) != 1 || plan.Create[0] != 1 {
		t.Errorf("Create = %v, want the hole at index 1", plan.Create)
	}
}

func TestPreflightAgainstThePhysicalInventory(t *testing.T) {
	zero, one := 0, 1

	t.Run("no free GPU stalls instead of queueing unschedulable guests", func(t *testing.T) {
		in := input(2)
		in.FreeGPUs = &zero
		plan := PlanMembership(in)
		if len(plan.Create) != 0 {
			t.Error("created a cell that could never be scheduled")
		}
		if !plan.Stalled || plan.Reason != cellsv1alpha1.ReasonInsufficientGPUs {
			t.Errorf("got %+v, want stalled/InsufficientPhysicalGPU", plan)
		}
	})

	t.Run("creates only what fits", func(t *testing.T) {
		in := input(2)
		in.FreeGPUs = &one
		plan := PlanMembership(in)
		if len(plan.Create) != 1 {
			t.Errorf("Create = %v, want one cell for one free GPU", plan.Create)
		}
	})

	t.Run("unknown is not zero", func(t *testing.T) {
		// Refusing to create because we failed to READ is as wrong as creating
		// blindly. Unknown proceeds and lets the scheduler decide.
		in := input(1)
		in.FreeGPUs = nil
		if plan := PlanMembership(in); len(plan.Create) != 1 {
			t.Errorf("Create = %v, want one: unknown free-GPU count must not block", plan.Create)
		}
	})
}

func TestUnreachableWorkloadClusterFreezesBothDirections(t *testing.T) {
	in := input(3, cell(0, cellsv1alpha1.CellPhaseReady))
	in.WorkloadReachable = false
	plan := PlanMembership(in)
	if len(plan.Create) != 0 {
		t.Error("created a cell we could not verify or label")
	}
	if !plan.Stalled || plan.Reason != cellsv1alpha1.ReasonUnreachable {
		t.Errorf("got %+v", plan)
	}

	// And scale-down, which is destructive, is refused outright.
	in = input(0, cell(0, cellsv1alpha1.CellPhaseReady), cell(1, cellsv1alpha1.CellPhaseReady))
	in.WorkloadReachable = false
	plan = PlanMembership(in)
	if len(plan.Drain) != 0 {
		t.Errorf("Drain = %v while the workload cluster was unreachable", plan.Drain)
	}
}

func TestScaleDownTakesHighestIndexFirst(t *testing.T) {
	plan := PlanMembership(input(1,
		cell(0, cellsv1alpha1.CellPhaseReady),
		cell(1, cellsv1alpha1.CellPhaseReady),
		cell(2, cellsv1alpha1.CellPhaseReady)))
	if len(plan.Drain) != 2 {
		t.Fatalf("Drain = %v, want two cells", plan.Drain)
	}
	if plan.Drain[0] != "pool-2" || plan.Drain[1] != "pool-1" {
		t.Errorf("Drain = %v, want [pool-2 pool-1]", plan.Drain)
	}
	if len(plan.Create) != 0 {
		t.Error("a shrinking pool also created cells")
	}
}

func TestHopelessPoolGuard(t *testing.T) {
	// Three failures and nothing has ever worked: a bad image, a bad token or the
	// wrong claim template. Retrying burns a GPU boot per attempt.
	in := input(2)
	in.EverReady = false
	in.Cells = []cellsv1alpha1.CellStatus{
		{Name: "pool-0", Index: 0, Phase: cellsv1alpha1.CellPhaseFailed, FailureCount: 3},
	}
	plan := PlanMembership(in)
	if len(plan.Create) != 0 {
		t.Error("kept creating cells in a pool that has never worked")
	}
	if !plan.Stalled || !strings.Contains(plan.Message, "ever become Ready") {
		t.Errorf("got %+v", plan)
	}

	// A pool that HAS worked keeps replacing — a burst of failures is different
	// from a configuration defect.
	in.EverReady = true
	if plan := PlanMembership(in); plan.Stalled {
		t.Errorf("stalled a pool that has worked before: %+v", plan)
	}
}

func TestExhaustedIndexStopsBeingReplaced(t *testing.T) {
	in := input(1)
	in.Cells = []cellsv1alpha1.CellStatus{{
		Name: "pool-0", Index: 0, Phase: cellsv1alpha1.CellPhaseFailed,
		FailureCount: MaxFailuresPerIndex, Message: "GPU never advertised",
	}}
	plan := PlanMembership(in)
	if !plan.Stalled || plan.Reason != cellsv1alpha1.ReasonCellReplacementExhausted {
		t.Errorf("got %+v", plan)
	}
	// The operator needs the original cause, not just the count.
	if !strings.Contains(plan.Message, "GPU never advertised") {
		t.Errorf("message lost the cell's own failure: %q", plan.Message)
	}
}

func TestBackoffGrowsAndCaps(t *testing.T) {
	if Backoff(0) != 0 {
		t.Error("backoff for a healthy cell should be zero")
	}
	if Backoff(1) != 30*time.Second {
		t.Errorf("first backoff = %s, want 30s", Backoff(1))
	}
	if Backoff(2) != time.Minute {
		t.Errorf("second backoff = %s, want 1m", Backoff(2))
	}
	if Backoff(50) != backoffCap {
		t.Errorf("backoff = %s, want the %s cap", Backoff(50), backoffCap)
	}
}

func TestShouldReplaceHonoursBackoff(t *testing.T) {
	failedAt := metav1.Time{Time: now.Add(-10 * time.Second)}
	c := cellsv1alpha1.CellStatus{Phase: cellsv1alpha1.CellPhaseFailed, FailureCount: 1, LastTransitionTime: &failedAt}
	if ShouldReplace(c, now, true) {
		t.Error("replaced before the 30s backoff expired")
	}
	c.LastTransitionTime = &metav1.Time{Time: now.Add(-31 * time.Second)}
	if !ShouldReplace(c, now, true) {
		t.Error("did not replace after the backoff expired")
	}
	c.FailureCount = MaxFailuresPerIndex
	if ShouldReplace(c, now, true) {
		t.Error("replaced an exhausted index")
	}
	if ShouldReplace(cellsv1alpha1.CellStatus{Phase: cellsv1alpha1.CellPhaseReady}, now, true) {
		t.Error("replaced a healthy cell")
	}
}

// TestShouldNotRebuildAgainstAPoolWideFault: a cell whose Node joined and
// advertises nothing, in a pool where nothing advertises anything, is not a broken
// cell — it is a broken workload cluster. Rebuilding it costs a GPU allocation, a
// full root-disk clone, a boot and a join per attempt and cannot succeed. Measured
// on hardware: HAMi could not register and the pool's answer was to rebuild the VM.
func TestShouldNotRebuildAgainstAPoolWideFault(t *testing.T) {
	failedAt := metav1.Time{Time: now.Add(-10 * time.Minute)}
	joined := cellsv1alpha1.CellStatus{
		Name: "cells-0", Phase: cellsv1alpha1.CellPhaseFailed, FailureCount: 1,
		LastTransitionTime: &failedAt, NodeReady: true, CapacityDevices: 0,
	}

	if ShouldReplace(joined, now, false) {
		t.Error("rebuilt a cell against a pool-wide provider fault")
	}
	if !ShouldReplace(joined, now, true) {
		t.Error("a single cell advertising nothing in an otherwise healthy pool must still be replaced")
	}

	// A cell that never joined tells us nothing about the provider, so the veto
	// must not catch it — that failure really may be the cell's.
	neverJoined := joined
	neverJoined.NodeReady = false
	if !ShouldReplace(neverJoined, now, false) {
		t.Error("a cell that never joined was not replaced; its fault is not the provider's")
	}

	// And the stall is reported, not silent.
	plan := PlanMembership(MembershipInput{
		Desired: 1, Cells: []cellsv1alpha1.CellStatus{joined}, Now: now,
		WorkloadReachable: true, EverReady: true, ProviderUsable: false,
	})
	if !plan.Stalled || plan.Reason != cellsv1alpha1.ReasonFaultNotInTheCell {
		t.Errorf("plan = %+v, want a stall naming the pool-wide fault", plan)
	}
	if len(plan.Create) != 0 || len(plan.Drain) != 0 {
		t.Errorf("plan acted on a fault it cannot fix: %+v", plan)
	}
}

func TestPlanRequeuesWhenABackoffIsPending(t *testing.T) {
	failedAt := metav1.Time{Time: now.Add(-10 * time.Second)}
	in := input(1, cellsv1alpha1.CellStatus{
		Name: "pool-0", Index: 0, Phase: cellsv1alpha1.CellPhaseFailed,
		FailureCount: 1, LastTransitionTime: &failedAt,
	})
	plan := PlanMembership(in)
	if plan.RequeueAfter <= 0 || plan.RequeueAfter > 30*time.Second {
		t.Errorf("RequeueAfter = %s, want the remaining backoff (~20s)", plan.RequeueAfter)
	}
}

func TestDrainingCellsDoNotCountTowardDesired(t *testing.T) {
	// A cell on its way out is not capacity; the pool must refill behind it.
	plan := PlanMembership(input(1, cell(0, cellsv1alpha1.CellPhaseDraining)))
	if len(plan.Create) != 1 {
		t.Errorf("Create = %v, want a replacement for the draining cell", plan.Create)
	}
}

func TestAllocationsEmptyHelper(t *testing.T) {
	if !(capacity.Allocations{}).Empty() {
		t.Error("zero allocations should be empty")
	}
	if (capacity.Allocations{InFlight: true}).Empty() {
		t.Error("an in-flight binding is not empty")
	}
	if (capacity.Allocations{Consumers: 1}).Empty() {
		t.Error("a consumer is not empty")
	}
}
