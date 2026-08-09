package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
)

func rolling() *cellsv1alpha1.UpdatePolicySpec {
	return &cellsv1alpha1.UpdatePolicySpec{Type: cellsv1alpha1.UpdateRolling}
}

// twoStaleReadyCells is the happy path: both cells were built from an older
// template, both are Ready, both idle.
func twoStaleReadyCells() RolloutInput {
	return RolloutInput{
		Policy:      rolling(),
		DesiredHash: "new",
		Cells: []cellsv1alpha1.CellStatus{
			{Name: "pool-0", Index: 0, Phase: cellsv1alpha1.CellPhaseReady, TemplateHash: "old"},
			{Name: "pool-1", Index: 1, Phase: cellsv1alpha1.CellPhaseReady, TemplateHash: "old"},
		},
		IdleCells:         []string{"pool-0", "pool-1"},
		WorkloadReachable: true,
		AtDesiredSize:     true,
	}
}

func TestRolloutReplacesOneCellAtATime(t *testing.T) {
	got := PlanRollout(twoStaleReadyCells())
	if got.Drain != "pool-0" {
		t.Errorf("Drain = %q, want pool-0 (lowest index first, so a rollout is deterministic)", got.Drain)
	}
	if got.UpToDate || got.StaleCells != 2 {
		t.Errorf("got %+v, want 2 stale and not up to date", got)
	}
}

func TestRolloutIsOptIn(t *testing.T) {
	// Manual is the default, and a template edit is not consent to destroy work.
	in := twoStaleReadyCells()
	in.Policy = nil
	got := PlanRollout(in)
	if got.Drain != "" {
		t.Errorf("replaced a cell under the default policy: %+v", got)
	}
	if got.Reason != cellsv1alpha1.ReasonTemplateChanged {
		t.Errorf("reason = %q, want the drift still REPORTED", got.Reason)
	}
	if !strings.Contains(got.Message, "left alone") {
		t.Errorf("message should say nothing will happen: %q", got.Message)
	}

	in.Policy = &cellsv1alpha1.UpdatePolicySpec{Type: cellsv1alpha1.UpdateManual}
	if got := PlanRollout(in); got.Drain != "" {
		t.Errorf("explicit Manual still replaced a cell: %+v", got)
	}
}

func TestRolloutNeverTouchesABusyCell(t *testing.T) {
	in := twoStaleReadyCells()
	in.IdleCells = nil
	got := PlanRollout(in)
	if got.Drain != "" {
		t.Fatalf("replaced a cell holding workloads: %+v", got)
	}
	if got.Reason != cellsv1alpha1.ReasonUpdateBlocked ||
		!strings.Contains(got.Message, "reported idle") {
		t.Errorf("got %+v, want a blocked reason that says why", got)
	}

	// Only the busy one is skipped; the idle one still goes.
	in.IdleCells = []string{"pool-1"}
	if got := PlanRollout(in); got.Drain != "pool-1" {
		t.Errorf("Drain = %q, want pool-1 — the idle one", got.Drain)
	}
}

func TestRolloutFreezesWhenTheWorkloadClusterIsUnreachable(t *testing.T) {
	// "Is this cell busy?" has no answer, so the cell must not be replaced.
	in := twoStaleReadyCells()
	in.WorkloadReachable = false
	got := PlanRollout(in)
	if got.Drain != "" {
		t.Fatalf("replaced a cell while the inner cluster was unreadable: %+v", got)
	}
	if !strings.Contains(got.Message, "unreachable") {
		t.Errorf("message should name the cause: %q", got.Message)
	}
}

func TestRolloutWaitsForTheScalerAndForItself(t *testing.T) {
	// Racing a scale decision for the index about to be freed.
	in := twoStaleReadyCells()
	in.AtDesiredSize = false
	if got := PlanRollout(in); got.Drain != "" {
		t.Errorf("rolled while the pool was still resizing: %+v", got)
	}

	// One at a time: a cell already coming or going blocks the next.
	for _, phase := range []cellsv1alpha1.CellPhase{
		cellsv1alpha1.CellPhaseDraining, cellsv1alpha1.CellPhaseBooting,
		cellsv1alpha1.CellPhaseAllocatingGPU, cellsv1alpha1.CellPhaseJoining,
	} {
		in := twoStaleReadyCells()
		in.Cells[1].Phase = phase
		if got := PlanRollout(in); got.Drain != "" {
			t.Errorf("rolled a second cell while one was %s: %+v", phase, got)
		}
	}
}

func TestRolloutIgnoresCellsWithNoRecordedTemplate(t *testing.T) {
	// A cell predating the hash being read back has an empty hash. Treating unknown
	// as out-of-date would replace an entire healthy pool the first time this policy
	// was switched on.
	in := twoStaleReadyCells()
	in.Cells[0].TemplateHash = ""
	in.Cells[1].TemplateHash = ""
	got := PlanRollout(in)
	if got.Drain != "" || !got.UpToDate || got.StaleCells != 0 {
		t.Errorf("got %+v, want an unknown hash treated as current", got)
	}
	if got.Reason != cellsv1alpha1.ReasonAllCellsCurrent {
		t.Errorf("reason = %q, want AllCellsCurrent", got.Reason)
	}
}

func TestRolloutReportsUpToDate(t *testing.T) {
	in := twoStaleReadyCells()
	in.Cells[0].TemplateHash = "new"
	in.Cells[1].TemplateHash = "new"
	got := PlanRollout(in)
	if !got.UpToDate || got.Drain != "" || got.Reason != cellsv1alpha1.ReasonAllCellsCurrent {
		t.Errorf("got %+v, want up to date", got)
	}
}

// TestRollingUpdateReplacesACellEndToEnd drives the whole loop: a template change,
// the drain gate, deletion, and recreation from the NEW template at the same index.
// The pure function above cannot show that the recreated cell actually adopts the new
// shape — which is the only reason anyone turns this on.
func TestRollingUpdateReplacesACellEndToEnd(t *testing.T) {
	f := newFixture(t, func(p *cellsv1alpha1.GPUCellPool) {
		p.Spec.UpdatePolicy = &cellsv1alpha1.UpdatePolicySpec{Type: cellsv1alpha1.UpdateRolling}
	})
	f.readyCell("cells-0")

	before := f.getPool().Status.Cells[0].TemplateHash
	if before == "" {
		t.Fatal("the cell recorded no template hash, so drift can never be detected")
	}
	if c := condition(t, f.getPool(), cellsv1alpha1.ConditionUpdated); c == nil ||
		c.Reason != cellsv1alpha1.ReasonAllCellsCurrent {
		t.Fatalf("Updated = %+v, want AllCellsCurrent before any change", c)
	}

	// Change the image the cells boot from — the driver-update case.
	f.patchPool(func(p *cellsv1alpha1.GPUCellPool) {
		p.Spec.Cell.GuestTemplate = runtime.RawExtension{Raw: []byte(
			`{"guestClassRef":{"name":"gpu-worker"},"imageRef":{"name":"noble-580"}}`)}
	})
	f.reconcile()

	pool := f.getPool()
	if c := condition(t, pool, cellsv1alpha1.ConditionUpdated); c == nil ||
		c.Status != metav1.ConditionFalse {
		t.Errorf("Updated = %+v, want False once a template drifted", c)
	}

	// The cell is drained and removed, then recreated at the same index.
	for i := 0; i < 8; i++ {
		f.reconcile()
		guests := f.guests()
		if len(guests) == 1 {
			if h := guests[0].GetAnnotations()[cellsv1alpha1.AnnotationTemplateHash]; h != before {
				// Recreated from the new template.
				spec := guests[0].Object["spec"].(map[string]any)
				img, _ := spec["imageRef"].(map[string]any)
				if img["name"] != "noble-580" {
					t.Fatalf("the replacement did not adopt the new template: %v", spec["imageRef"])
				}
				return
			}
		}
	}
	t.Fatalf("the stale cell was never replaced: %+v", f.getPool().Status.Cells)
}

// A rolling update must obey the same gate scale-down does: a cell holding work is
// not replaced, however out of date it is.
func TestRollingUpdateWaitsForABusyCell(t *testing.T) {
	f := newFixture(t, func(p *cellsv1alpha1.GPUCellPool) {
		p.Spec.UpdatePolicy = &cellsv1alpha1.UpdatePolicySpec{Type: cellsv1alpha1.UpdateRolling}
	})
	f.readyCell("cells-0")
	f.gpuPod("holder", "cells-0") // a HAMi workload on the cell's GPU

	f.patchPool(func(p *cellsv1alpha1.GPUCellPool) {
		p.Spec.Cell.GuestTemplate = runtime.RawExtension{Raw: []byte(
			`{"guestClassRef":{"name":"gpu-worker"},"imageRef":{"name":"noble-580"}}`)}
	})
	for i := 0; i < 4; i++ {
		f.reconcile()
	}

	if got := len(f.guests()); got != 1 {
		t.Fatalf("got %d guests, want the busy cell left in place", got)
	}
	pool := f.getPool()
	if pool.Status.Cells[0].Phase == cellsv1alpha1.CellPhaseDraining {
		t.Error("started draining a cell that still holds a HAMi workload")
	}
	if c := condition(t, pool, cellsv1alpha1.ConditionUpdated); c == nil ||
		c.Reason != cellsv1alpha1.ReasonUpdateBlocked {
		t.Errorf("Updated = %+v, want UpdateBlocked so the stall is visible", c)
	}
}

// TestReplacedCellDoesNotInheritTeardown pins a latent bug the rolling update
// exposed, which was never about rolling updates.
//
// Cell names are reused — index 0 is always <pool>-0 — so a replacement lands on the
// name of the cell it replaced, and status rows are keyed by name. The new cell
// therefore inherited its predecessor's `Draining` phase and was deleted on the pass
// that created it: create, destroy, create, destroy, with no timeout that could ever
// break the loop. Any replacement path reaches this (a failed cell, a scale-down
// followed by a scale-up on the same index), not just a template change.
func TestReplacedCellDoesNotInheritTeardown(t *testing.T) {
	f := newFixture(t, nil)
	f.reconcile()
	first := f.guestUID("cells-0")

	// Put the row into teardown, as any removal path does, and delete the guest
	// underneath it — the state the loop used to get stuck in.
	f.patchPoolStatus(func(p *cellsv1alpha1.GPUCellPool) {
		p.Status.Cells = []cellsv1alpha1.CellStatus{{
			Name: "cells-0", Index: 0,
			Phase:    cellsv1alpha1.CellPhaseDraining,
			GuestUID: first,
		}}
	})
	f.deleteGuest("cells-0")

	// The pool recreates the index. The replacement must start clean.
	f.reconcile()
	f.reconcile()

	cells := f.getPool().Status.Cells
	if len(cells) != 1 {
		t.Fatalf("cells = %+v, want the index refilled", cells)
	}
	if cells[0].Phase == cellsv1alpha1.CellPhaseDraining {
		t.Error("the replacement inherited its predecessor's teardown")
	}
	if cells[0].GuestUID == first {
		t.Error("no new guest was created")
	}
	if len(f.guests()) != 1 {
		t.Errorf("got %d guests, want exactly one surviving replacement", len(f.guests()))
	}
}
