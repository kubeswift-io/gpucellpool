package controller

import (
	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
)

// RolloutInput is what a rolling-update decision is made from. Everything is
// observed; nothing is remembered between reconciles.
type RolloutInput struct {
	// Policy is spec.updatePolicy (nil means Manual).
	Policy *cellsv1alpha1.UpdatePolicySpec

	// DesiredHash is the hash of the CURRENT cell template.
	DesiredHash string

	// Cells is the pool's cells as observed this pass.
	Cells []cellsv1alpha1.CellStatus

	// IdleCells are the cells the capacity provider reports as holding nothing,
	// which is the same gate automatic scale-down uses. A cell whose allocations
	// could not be READ is absent from this list, and therefore never replaced.
	IdleCells []string

	// WorkloadReachable is false when the inner cluster could not be reached. No
	// cell may be replaced then: we cannot tell whether it is holding work.
	WorkloadReachable bool

	// AtDesiredSize is false while the pool is still growing or shrinking. A
	// rollout must not race a scaling decision for the same index.
	AtDesiredSize bool
}

// RolloutDecision is the outcome.
type RolloutDecision struct {
	// Drain names the cell to replace, or is empty.
	Drain string
	// UpToDate is true when every cell matches the current template.
	UpToDate bool
	// StaleCells counts cells running an older template, whether or not one is
	// being replaced right now.
	StaleCells int
	Reason     string
	Message    string
}

// PlanRollout decides whether to replace one stale cell.
//
// Replacement is deletion plus recreation — there is no in-place update of a VM's
// image — so every refusal here is protecting running work. The gates, in the order
// they are checked:
//
//  1. the operator must have asked for it (Manual is the default);
//  2. the inner cluster must be readable, or "is this cell busy?" has no answer;
//  3. the pool must already be at its desired size, so a rollout cannot race a
//     scale decision for the same index;
//  4. nothing else may be mid-flight — one cell at a time, always;
//  5. the cell must be reported IDLE. The drain gate re-checks allocations again
//     before the object is deleted, so a workload that lands in between is still
//     safe.
//
// A pool whose stale cells are all busy makes no progress, indefinitely. That is
// correct, and the reason says so instead of leaving an operator to wonder.
func PlanRollout(in RolloutInput) RolloutDecision {
	stale := staleCells(in.Cells, in.DesiredHash)
	out := RolloutDecision{StaleCells: len(stale), UpToDate: len(stale) == 0}

	if out.UpToDate {
		out.Reason = cellsv1alpha1.ReasonAllCellsCurrent
		return out
	}

	out.Reason = cellsv1alpha1.ReasonTemplateChanged
	out.Message = itoa(len(stale)) + " cell(s) were created from an older template"

	if in.Policy == nil || in.Policy.Type != cellsv1alpha1.UpdateRolling {
		// Informational only: say what is stale, change nothing.
		out.Message += "; updatePolicy.type is Manual, so they are left alone"
		return out
	}

	blocked := func(why string) RolloutDecision {
		out.Reason = cellsv1alpha1.ReasonUpdateBlocked
		out.Message = itoa(len(stale)) + " cell(s) out of date: " + why
		return out
	}

	if !in.WorkloadReachable {
		return blocked("the workload cluster is unreachable, so it is unknown whether they hold work")
	}
	if !in.AtDesiredSize {
		return blocked("the pool is still resizing; a rollout must not race a scaling decision")
	}
	if busy := midFlight(in.Cells); busy != "" {
		return blocked("cell " + busy + " is already being replaced or created")
	}

	for _, c := range stale {
		if contains(in.IdleCells, c) {
			return RolloutDecision{
				Drain:      c,
				StaleCells: len(stale),
				Reason:     cellsv1alpha1.ReasonRollingUpdate,
				Message: "replacing " + c + " to adopt the current template (" +
					itoa(len(stale)) + " out of date)",
			}
		}
	}
	return blocked("none of them is reported idle — a cell whose allocations cannot be read is never replaced either")
}

// staleCells returns the names of cells created from a different template, in
// index order, so a rollout is deterministic and reads as a rolling one.
//
// A cell whose hash is EMPTY is not stale: it predates the annotation being read
// back, and treating unknown as out-of-date would replace a whole healthy pool the
// first time this policy was switched on.
func staleCells(cells []cellsv1alpha1.CellStatus, desired string) []string {
	var out []string
	for _, c := range cells {
		switch c.Phase {
		case cellsv1alpha1.CellPhaseDraining, cellsv1alpha1.CellPhaseDeleting:
			continue
		}
		if c.TemplateHash != "" && c.TemplateHash != desired {
			out = append(out, c.Name)
		}
	}
	return out
}

// midFlight names a cell that is already coming or going, if any.
func midFlight(cells []cellsv1alpha1.CellStatus) string {
	for _, c := range cells {
		switch c.Phase {
		case cellsv1alpha1.CellPhaseDraining, cellsv1alpha1.CellPhaseDeleting,
			cellsv1alpha1.CellPhasePending, cellsv1alpha1.CellPhaseAllocatingGPU,
			cellsv1alpha1.CellPhaseGuestProvisioning, cellsv1alpha1.CellPhaseBooting,
			cellsv1alpha1.CellPhaseJoining, cellsv1alpha1.CellPhaseAwaitingGPUCapacity:
			return c.Name
		}
	}
	return ""
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
