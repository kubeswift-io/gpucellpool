package controller

import (
	"time"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
	"github.com/kubeswift-io/gpucellpool/internal/cellid"
)

// Membership limits. These are the churn controls: a pool that cannot succeed
// must stall loudly rather than burn a GPU boot every thirty seconds.
const (
	// DefaultMaxCreating bounds cells in flight. Creating a cell costs a
	// physical GPU and a VM boot; a thundering herd on a saturated cluster
	// produces N unschedulable launcher pods and no diagnosis.
	DefaultMaxCreating = 2

	// MaxFailuresPerIndex stops replacing one index forever.
	MaxFailuresPerIndex = 5

	// HopelessPoolFailures stops creating entirely when NO cell has ever reached
	// Ready. That is a configuration defect — bad image, bad token, wrong claim
	// template — not a transient, and retrying it is pure waste.
	HopelessPoolFailures = 3

	backoffBase = 30 * time.Second
	backoffCap  = 30 * time.Minute
)

// Backoff is the per-index replacement delay: 30s, 1m, 2m, 5m… capped at 30m.
func Backoff(failures int32) time.Duration {
	if failures <= 0 {
		return 0
	}
	d := backoffBase
	for i := int32(1); i < failures; i++ {
		d *= 2
		if d >= backoffCap {
			return backoffCap
		}
	}
	return d
}

// MembershipInput is the state a membership decision is made from.
type MembershipInput struct {
	Desired int32
	Cells   []cellsv1alpha1.CellStatus
	Now     time.Time

	// FreeGPUs is the outer inventory's free device count. nil means UNKNOWN —
	// which must not be treated as zero: refusing to create because we failed to
	// read is as wrong as creating blindly. Unknown proceeds and lets the
	// scheduler decide.
	FreeGPUs *int

	MaxCreating int

	// WorkloadReachable freezes every destructive path, and creation too: a cell
	// we cannot verify or label is a cell we cannot honestly report.
	WorkloadReachable bool

	// EverReady is true once any cell has reached Ready. It distinguishes "a
	// burst of failures" from "this pool has never worked".
	EverReady bool
}

// MembershipPlan is what the reconciler should do about the cell set.
type MembershipPlan struct {
	// Create are indexes to create now.
	Create []int32
	// Drain are cells to begin draining (scale-down or template teardown).
	Drain []string
	// Stalled is true when the pool wants more cells but may not create them.
	Stalled bool
	Reason  string
	Message string
	// RequeueAfter is the soonest a backoff expires, so the reconciler wakes when
	// there is something to do rather than polling.
	RequeueAfter time.Duration
}

// PlanMembership decides which cells to create and which to drain.
func PlanMembership(in MembershipInput) MembershipPlan {
	if in.MaxCreating <= 0 {
		in.MaxCreating = DefaultMaxCreating
	}

	var (
		plan       MembershipPlan
		used       []int32
		inFlight   int
		failed     []cellsv1alpha1.CellStatus
		live       int32
		totalFails int32
	)
	for _, c := range in.Cells {
		used = append(used, c.Index)
		totalFails += c.FailureCount
		switch c.Phase {
		case cellsv1alpha1.CellPhaseFailed:
			failed = append(failed, c)
		case cellsv1alpha1.CellPhaseDraining, cellsv1alpha1.CellPhaseDeleting:
			// Not part of the live set; do not count toward desired.
		case cellsv1alpha1.CellPhaseReady:
			live++
		default:
			live++
			inFlight++
		}
	}

	// Scale-down first: a shrinking pool must not also be creating.
	if in.Desired < live {
		if !in.WorkloadReachable {
			plan.Stalled = true
			plan.Reason = cellsv1alpha1.ReasonUnreachable
			plan.Message = "not scaling down while the workload cluster is unreachable"
			return plan
		}
		var candidates []int32
		for _, c := range in.Cells {
			switch c.Phase {
			case cellsv1alpha1.CellPhaseDraining, cellsv1alpha1.CellPhaseDeleting, cellsv1alpha1.CellPhaseFailed:
			default:
				candidates = append(candidates, c.Index)
			}
		}
		for _, idx := range cellid.HighestIndexes(candidates, int(live-in.Desired)) {
			for _, c := range in.Cells {
				if c.Index == idx {
					plan.Drain = append(plan.Drain, c.Name)
				}
			}
		}
		return plan
	}

	// Replacement: a failed index is retired from the live set, so the create
	// path below refills it — but only once its backoff has expired.
	for _, c := range failed {
		if c.FailureCount >= MaxFailuresPerIndex {
			plan.Stalled = true
			plan.Reason = cellsv1alpha1.ReasonCellReplacementExhausted
			plan.Message = "cell " + c.Name + " has failed " + itoa(int(c.FailureCount)) +
				" times and will not be replaced again: " + c.Message
			return plan
		}
		wait := Backoff(c.FailureCount)
		if c.LastTransitionTime != nil {
			if remaining := wait - in.Now.Sub(c.LastTransitionTime.Time); remaining > 0 {
				plan.RequeueAfter = minDuration(plan.RequeueAfter, remaining)
			}
		}
	}

	if in.Desired <= live {
		return plan
	}

	// The hopeless-pool guard. Retrying a configuration defect forever burns a
	// GPU boot per attempt and tells the operator nothing new.
	if !in.EverReady && totalFails >= HopelessPoolFailures {
		plan.Stalled = true
		plan.Reason = cellsv1alpha1.ReasonCellReplacementExhausted
		plan.Message = "no cell in this pool has ever become Ready and " + itoa(int(totalFails)) +
			" have failed; not creating more until the pool is changed"
		return plan
	}

	if !in.WorkloadReachable {
		plan.Stalled = true
		plan.Reason = cellsv1alpha1.ReasonUnreachable
		plan.Message = "not creating cells while the workload cluster is unreachable"
		return plan
	}

	want := int(in.Desired - live)
	room := in.MaxCreating - inFlight
	if room <= 0 {
		plan.Message = "waiting for in-flight cells before creating more"
		return plan
	}
	if want > room {
		want = room
	}

	// Pre-flight against the OUTER inventory. Without this the pool creates
	// guests that sit unschedulable forever, which is the brief's first failure
	// scenario — and the scheduler never explains it in terms an operator can act
	// on.
	if in.FreeGPUs != nil {
		if *in.FreeGPUs <= 0 {
			plan.Stalled = true
			plan.Reason = cellsv1alpha1.ReasonInsufficientGPUs
			plan.Message = "no free GPU in the infrastructure cluster; not creating a cell that could never be scheduled"
			return plan
		}
		if want > *in.FreeGPUs {
			want = *in.FreeGPUs
			plan.Reason = cellsv1alpha1.ReasonInsufficientGPUs
			plan.Message = "only " + itoa(*in.FreeGPUs) + " free GPU(s) available; creating what fits"
		}
	}

	for i := 0; i < want; i++ {
		idx := cellid.LowestFreeIndex(used)
		plan.Create = append(plan.Create, idx)
		used = append(used, idx)
	}
	return plan
}

// ShouldReplace reports whether a failed cell's backoff has expired, so the
// reconciler can delete it and let the next pass recreate the index.
func ShouldReplace(c cellsv1alpha1.CellStatus, now time.Time) bool {
	if c.Phase != cellsv1alpha1.CellPhaseFailed {
		return false
	}
	if c.FailureCount >= MaxFailuresPerIndex {
		return false
	}
	if c.LastTransitionTime == nil {
		return true
	}
	return now.Sub(c.LastTransitionTime.Time) >= Backoff(c.FailureCount)
}

func minDuration(a, b time.Duration) time.Duration {
	if a == 0 || (b > 0 && b < a) {
		return b
	}
	return a
}
