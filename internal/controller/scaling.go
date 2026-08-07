package controller

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
	"github.com/kubeswift-io/gpucellpool/internal/capacity"
)

// ScaleInput is what a scaling decision is made from. Everything here is
// observed; nothing is remembered between reconciles except LastScaleUp, which
// lives in status.
type ScaleInput struct {
	// Replicas is spec.replicas — the desired count when autoscaling is off, and
	// the default floor when it is on.
	Replicas int32
	// Autoscaling is spec.autoscaling (nil when absent).
	Autoscaling *cellsv1alpha1.AutoscalingSpec

	// LiveCells counts cells that exist and are not on their way out.
	LiveCells int32

	// Demand is the last reading; DemandKnown is false when it could not be read.
	Demand      capacity.Demand
	DemandKnown bool

	// FreeGPUs from the outer inventory; nil means unknown.
	FreeGPUs *int

	// IdleCells are Ready cells the capacity provider reports as holding nothing.
	// Only these may be removed: an automatic action must not destroy running work.
	// Ordered by the caller (emptiest first).
	IdleCells []string

	Now             time.Time
	LastScaleUp     *metav1.Time
	LastScaleDown   *metav1.Time
	DemandFreeSince *metav1.Time
}

// ScaleDecision is the outcome.
type ScaleDecision struct {
	// Desired is the cell count the membership planner should aim for.
	Desired int32
	// ScaledUp is true when this decision GREW the pool, so the caller stamps
	// LastScaleUpTime and the stabilization window starts.
	ScaledUp bool
	// ScaledDown is true when this decision SHRANK the pool.
	ScaledDown bool
	// DrainCandidates is the preferred removal order when shrinking — idle cells
	// only. Empty means "the caller picks", which is highest-index-first.
	DrainCandidates []string
	Reason          string
	Message         string
}

// DecideScale computes the desired cell count.
//
// Scale-UP only. Four things gate every increase, and together they are the whole
// safety of the feature:
//
//  1. the demand must be READ, not assumed (an unreadable provider scales nothing);
//  2. the demand must be satisfiable by one fresh cell of THIS pool's shape — a
//     pod asking for two devices, or more memory than one device has, is not
//     capacity demand this pool can answer;
//  3. one cell at a time, because a cell costs a physical GPU and minutes of boot;
//  4. a stabilization window, because demand does not clear until the new cell is
//     Ready — without it a single burst creates a cell per reconcile.
func DecideScale(in ScaleInput) ScaleDecision {
	as := in.Autoscaling
	if as == nil || !as.Enabled {
		return ScaleDecision{Desired: in.Replicas, Reason: cellsv1alpha1.ReasonIdle}
	}

	floor := in.Replicas
	if as.MinReplicas != nil {
		floor = *as.MinReplicas
	}
	ceiling := floor
	if as.MaxReplicas != nil {
		ceiling = *as.MaxReplicas
	}
	if ceiling < floor {
		ceiling = floor
	}

	// The baseline is what exists, clamped into range: autoscaling must not
	// silently shrink a pool that is already larger than the floor. Shrinking is
	// operator-triggered in v1alpha1 (ScaleDown: Manual).
	base := in.LiveCells
	if base < floor {
		base = floor
	}
	if base > ceiling {
		base = ceiling
	}

	switch {
	case !in.DemandKnown:
		return ScaleDecision{Desired: base, Reason: cellsv1alpha1.ReasonCapacityUnknown,
			Message: "GPU demand could not be read from the workload cluster; not scaling"}

	case in.Demand.SatisfiableByOneCell == 0 && in.Demand.PendingRequests > 0:
		// Something is pending, but adding a cell of this shape would not help.
		return ScaleDecision{Desired: base, Reason: cellsv1alpha1.ReasonDemandUnsatisfiable,
			Message: itoa(in.Demand.PendingRequests) +
				" GPU request(s) are pending but none fits one cell of this pool's shape; not scaling"}

	case in.Demand.SatisfiableByOneCell == 0:
		// No demand this pool can answer. That is the only situation in which
		// shrinking is even considered.
		return decideScaleDown(in, as, base, floor)

	case base >= ceiling:
		return ScaleDecision{Desired: ceiling, Reason: cellsv1alpha1.ReasonAtMaxReplicas,
			Message: "at maxReplicas (" + itoa(int(ceiling)) + ") with " +
				itoa(in.Demand.SatisfiableByOneCell) + " satisfiable request(s) still pending"}

	case in.FreeGPUs != nil && *in.FreeGPUs <= 0:
		return ScaleDecision{Desired: base, Reason: cellsv1alpha1.ReasonInsufficientGPUs,
			Message: "demand is satisfiable but no free GPU remains in the infrastructure cluster"}

	case stabilizing(as, in.LastScaleUp, in.Now):
		return ScaleDecision{Desired: base, Reason: cellsv1alpha1.ReasonStabilizing,
			Message: "waiting out the stabilization window before scaling up again " +
				"(demand does not clear until the previous cell is Ready)"}

	default:
		// One at a time.
		return ScaleDecision{Desired: base + 1, ScaledUp: true, Reason: cellsv1alpha1.ReasonScaledUp,
			Message: "scaling up by one for " + itoa(in.Demand.SatisfiableByOneCell) +
				" satisfiable GPU request(s)"}
	}
}

// decideScaleDown is only reached with demand at zero. Everything here is a reason
// NOT to shrink, because the failure mode is asymmetric: holding an idle GPU costs
// money, removing a wanted one costs a full boot AND the work that was about to run
// on it.
func decideScaleDown(in ScaleInput, as *cellsv1alpha1.AutoscalingSpec, base, floor int32) ScaleDecision {
	hold := func(reason, msg string) ScaleDecision {
		return ScaleDecision{Desired: base, Reason: reason, Message: msg}
	}

	if as.ScaleDown != cellsv1alpha1.ScaleDownAuto {
		return hold(cellsv1alpha1.ReasonIdle, "")
	}
	if base <= floor {
		return hold(cellsv1alpha1.ReasonAtMinReplicas, "")
	}
	if len(in.IdleCells) == 0 {
		// Every cell is holding work. This is the common case in a busy pool and
		// not a fault.
		return hold(cellsv1alpha1.ReasonNoIdleCell,
			"demand is absent but every cell still holds workloads")
	}

	window := scaleDownWindow(as)

	// Demand must have been absent for the WHOLE window, not merely absent right
	// now: a pool that shrinks between two bursts is worse than one that waits.
	if in.DemandFreeSince == nil || in.DemandFreeSince.IsZero() ||
		in.Now.Sub(in.DemandFreeSince.Time) < window {
		return hold(cellsv1alpha1.ReasonStabilizing,
			"waiting for demand to stay absent for "+window.String()+" before removing a cell")
	}
	// And not immediately after any scale action, in either direction.
	if within(in.LastScaleDown, in.Now, window) || within(in.LastScaleUp, in.Now, window) {
		return hold(cellsv1alpha1.ReasonStabilizing,
			"waiting out the scale-down window after the last scaling action")
	}

	return ScaleDecision{
		Desired:         base - 1,
		ScaledDown:      true,
		DrainCandidates: in.IdleCells,
		Reason:          cellsv1alpha1.ReasonScaledDown,
		Message: "removing one idle cell after " + window.String() +
			" with no GPU demand (" + itoa(len(in.IdleCells)) + " idle)",
	}
}

func scaleDownWindow(as *cellsv1alpha1.AutoscalingSpec) time.Duration {
	if as.ScaleDownStabilizationWindow != nil && as.ScaleDownStabilizationWindow.Duration > 0 {
		return as.ScaleDownStabilizationWindow.Duration
	}
	return 30 * time.Minute
}

func within(t *metav1.Time, now time.Time, window time.Duration) bool {
	return t != nil && !t.IsZero() && now.Sub(t.Time) < window
}

func stabilizing(as *cellsv1alpha1.AutoscalingSpec, last *metav1.Time, now time.Time) bool {
	if last == nil || last.IsZero() {
		return false
	}
	window := 10 * time.Minute
	if as.StabilizationWindow != nil && as.StabilizationWindow.Duration > 0 {
		window = as.StabilizationWindow.Duration
	}
	return now.Sub(last.Time) < window
}

// ReferenceDevice derives the device a fresh cell would bring, from what the pool
// already advertises. A pool with no advertised device returns nil: we cannot
// judge satisfiability, and an automatic action must not run on a guess.
func ReferenceDevice(c capacity.Capacity) *capacity.Device {
	for _, m := range c.ByModel {
		if m.Devices <= 0 {
			continue
		}
		return &capacity.Device{
			Model:       m.Model,
			MemoryMiB:   m.MemoryTotalMiB / int64(m.Devices),
			CorePercent: m.ComputeTotal / int64(m.Devices),
			Healthy:     true,
		}
	}
	return nil
}
