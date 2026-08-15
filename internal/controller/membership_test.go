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

// #17. A pool that HAS worked and whose bootstrap credential then expires is
// invisible to both existing guards: EverReady defeats the hopeless check, and
// no Node ever registers so the capacity veto (which requires NodeReady) never
// fires. Without this guard every index burns MaxFailuresPerIndex rebuilds —
// each a GPU allocation, a root-disk clone, a boot and a full join timeout.

func neverRegistered(index int32) cellsv1alpha1.CellStatus {
	c := cell(index, cellsv1alpha1.CellPhaseFailed)
	c.Reason = cellsv1alpha1.ReasonNodeNeverRegistered
	c.FailureCount = 1
	return c
}

func TestPlanStallsWhenNoCellEverRegistersANode(t *testing.T) {
	in := input(3, neverRegistered(0), neverRegistered(1))
	in.ProviderUsable = true // the capacity veto must not be what stops this
	plan := PlanMembership(in)

	if !plan.Stalled || plan.Reason != cellsv1alpha1.ReasonBootstrapCredentialSuspect {
		t.Fatalf("plan = %+v, want a stall naming the suspect credential", plan)
	}
	if len(plan.Create) != 0 {
		t.Errorf("stalled plan still wants to create %v — that is the GPU boot this guard exists to stop", plan.Create)
	}
	// The message has to be actionable: it must name the field an operator edits.
	if !strings.Contains(plan.Message, "spec.bootstrap.joinSecretRef") {
		t.Errorf("message does not name the secret to fix: %q", plan.Message)
	}
	// And honest about being a heuristic — a blocked route looks identical.
	if !strings.Contains(plan.Message, "route") {
		t.Errorf("message claims more certainty than it has: %q", plan.Message)
	}
}

// One cell can be unlucky — a slow image pull, a node that rebooted mid-join.
// Stalling on the first is how a guard ends up firing on a transient.
func TestPlanStillReplacesAfterASingleUnregisteredFailure(t *testing.T) {
	in := input(3, neverRegistered(0))
	in.ProviderUsable = true
	plan := PlanMembership(in)

	if plan.Stalled && plan.Reason == cellsv1alpha1.ReasonBootstrapCredentialSuspect {
		t.Fatal("stalled on ONE unregistered failure; a single cell may simply be unlucky")
	}
	if len(plan.Create) == 0 {
		t.Error("a single failure must still be replaced")
	}
}

// A Node that registered proves the credential WORKED. Whatever killed the cell
// afterwards is a different fault, and is #16's business, not this guard's.
func TestPlanDoesNotSuspectTheCredentialWhenNodesRegistered(t *testing.T) {
	joined := func(index int32) cellsv1alpha1.CellStatus {
		c := cell(index, cellsv1alpha1.CellPhaseFailed)
		c.Reason = cellsv1alpha1.ReasonJoinTimeout // registered, never went Ready
		c.NodeName = c.Name
		c.FailureCount = 1
		return c
	}
	in := input(3, joined(0), joined(1))
	in.ProviderUsable = true
	plan := PlanMembership(in)

	if plan.Stalled && plan.Reason == cellsv1alpha1.ReasonBootstrapCredentialSuspect {
		t.Fatalf("blamed the credential for cells whose Nodes registered: %+v", plan)
	}
}

// The suspicion must clear the moment anything joins — otherwise a pool that
// recovered stays stalled on stale evidence.
func TestPlanClearsSuspicionForACellThatWasEverReady(t *testing.T) {
	a, b := neverRegistered(0), neverRegistered(1)
	b.ReadyOnce = true // this one demonstrably joined at some point
	in := input(3, a, b)
	in.ProviderUsable = true

	if plan := PlanMembership(in); plan.Stalled &&
		plan.Reason == cellsv1alpha1.ReasonBootstrapCredentialSuspect {
		t.Fatalf("a cell that was ever Ready is not evidence against the credential: %+v", plan)
	}
}

// The trap this design had to route around: a tombstone clears NodeName, so
// "no Node ever appeared" and "the Node went away with the cell" become
// indistinguishable one reconcile later. Reason survives; NodeName does not.
// If the predicate ever keys on NodeName again, this fails.
func TestSuspectCountSurvivesTheTombstoneReset(t *testing.T) {
	tombstone := neverRegistered(0)
	tombstone.GuestUID = "" // tombstoned
	tombstone.NodeName = "" // cleared by the reset, as the real path does

	if n := suspectCredentialFailures([]cellsv1alpha1.CellStatus{tombstone}); n != 1 {
		t.Fatalf("count = %d, want 1: the tombstone must still carry why it failed", n)
	}

	// The case that actually forces Reason-keying rather than NodeName-keying: a
	// cell whose Node DID register, then tombstoned. Its NodeName is now empty
	// too, so anything keying on NodeName counts it as "never registered" and
	// blames a credential that demonstrably worked.
	registeredThenTombstoned := cell(1, cellsv1alpha1.CellPhaseFailed)
	registeredThenTombstoned.Reason = cellsv1alpha1.ReasonJoinTimeout
	registeredThenTombstoned.GuestUID = ""
	registeredThenTombstoned.NodeName = ""

	if n := suspectCredentialFailures([]cellsv1alpha1.CellStatus{registeredThenTombstoned}); n != 0 {
		t.Fatalf("count = %d, want 0: this cell's Node registered — the credential worked", n)
	}
}
