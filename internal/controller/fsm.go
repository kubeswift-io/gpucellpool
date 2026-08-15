// Package controller reconciles GPUCellPool objects.
//
// The decision logic lives in pure functions (fsm.go, membership.go, status.go)
// and the reconciler wires them to clients. That split is deliberate: the
// behaviours that lose workloads or lie to operators are exactly the ones that
// must be provable without a cluster.
package controller

import (
	"time"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
	"github.com/kubeswift-io/gpucellpool/internal/capacity"
	"github.com/kubeswift-io/gpucellpool/internal/provisioner"
	"github.com/kubeswift-io/gpucellpool/internal/workload"
)

// Observation is everything known about one cell at one moment, from BOTH
// clusters. A cell's phase is a function of this and nothing else — no timers, no
// memory, no controller state — so a restart changes no decision.
type Observation struct {
	Current        cellsv1alpha1.CellPhase
	LastTransition time.Time
	Now            time.Time

	// Outer cluster.
	Outer provisioner.OuterState

	// Inner cluster. WorkloadReachable=false means these are unknown, not empty.
	WorkloadReachable bool
	Node              workload.NodeState
	StaleNode         bool

	// Capacity provider.
	ProviderReady   bool
	CapacityDevices int
	ExpectedDevices int

	// Timeouts.
	BootstrapTimeout time.Duration
	CapacityTimeout  time.Duration
}

// Decision is the outcome of advancing one cell.
type Decision struct {
	Phase   cellsv1alpha1.CellPhase
	Reason  string
	Message string
	// ReapStaleNode asks the caller to delete a workload Node left behind by a
	// previous incarnation of this cell, BEFORE the replacement can join.
	ReapStaleNode bool
}

// AdvanceCell computes a cell's next phase.
//
// Three rules dominate and are worth stating before the code:
//
//  1. Doubt freezes. While the workload cluster is unreachable the phase does not
//     move — not forward, and above all not to Failed. We do not know anything
//     new, so we conclude nothing new.
//  2. Regression is not failure. A cell whose Node went NotReady, or whose GPU
//     stopped being advertised, goes back to an earlier phase and can recover. A
//     restarted HAMi plugin must not condemn a healthy VM.
//  3. Ready needs both layers AND the GPU. A running VM whose GPU never surfaced
//     is the exact failure this pool exists to catch, so it waits, and then fails
//     loudly with a reason — it never counts as capacity.
func AdvanceCell(obs Observation) Decision {
	// Terminal-ish phases are owned by the reconciler's teardown path.
	switch obs.Current {
	case cellsv1alpha1.CellPhaseDraining, cellsv1alpha1.CellPhaseDeleting:
		return Decision{Phase: obs.Current}
	}

	// Rule 1: freeze on inner doubt. An outer terminal failure is still worth
	// reporting — that knowledge is not in question.
	if !obs.WorkloadReachable {
		if obs.Outer.Failed {
			return failed(obs.Outer.Reason, obs.Outer.Message)
		}
		return Decision{
			Phase:   obs.Current,
			Reason:  cellsv1alpha1.ReasonUnreachable,
			Message: "workload cluster unreachable; cell state is frozen at its last observation",
		}
	}

	// A terminal outer failure needs no timeout: KubeSwift has already concluded.
	if obs.Outer.Failed {
		return failed(obs.Outer.Reason, obs.Outer.Message)
	}

	if obs.Current == cellsv1alpha1.CellPhaseFailed {
		// Failed is terminal for the cell; replacement is a membership decision.
		return Decision{Phase: cellsv1alpha1.CellPhaseFailed}
	}

	switch {
	case !obs.Outer.Exists:
		return Decision{Phase: cellsv1alpha1.CellPhasePending}

	case len(obs.Outer.GPUDevices) == 0:
		// Waiting on the scheduler (DRA) or the SwiftGPU controller (Native).
		return expireOr(obs, cellsv1alpha1.CellPhaseAllocatingGPU, obs.BootstrapTimeout,
			cellsv1alpha1.ReasonCellProvisionTimeout,
			"no GPU allocated for this cell yet (is a device free, and is vfio-pci loaded on a GPU node?)")

	case !obs.Outer.Provisioned:
		// The VM exists with a device but is not running with an address yet.
		phase := cellsv1alpha1.CellPhaseGuestProvisioning
		msg := "waiting for the VM to run"
		if obs.Outer.Phase == "Running" {
			// Running with no address: booting, or bound to the wrong interface.
			phase = cellsv1alpha1.CellPhaseBooting
			msg = "VM is running; waiting for the node address " +
				"(if this persists, check cell.nodeIPFrom names an interface the guest actually has)"
		}
		return expireOr(obs, phase, obs.BootstrapTimeout, cellsv1alpha1.ReasonCellProvisionTimeout, msg)

	case obs.StaleNode:
		// A Node with this cell's name, a previous incarnation's UID, and NO kubelet
		// heartbeating for it. Left in place it would be reported as this cell's
		// Node — a phantom Ready cell, or capacity advertised for a GPU that no
		// longer exists. A Node whose kubelet IS live is never stale, however old
		// its identity label: that is the replacement adopting the object, and it
		// gets re-labelled instead (see the observation site).
		return Decision{
			Phase:         cellsv1alpha1.CellPhaseJoining,
			Reason:        cellsv1alpha1.ReasonNodeNameCollision,
			Message:       "a workload Node left by a previous incarnation of this cell (no kubelet heartbeat) is being removed before the replacement joins",
			ReapStaleNode: true,
		}

	case !obs.Node.Exists || !obs.Node.Ready:
		// Two different faults wear the same timeout. If the Node REGISTERED, the
		// bootstrap credential was accepted and the problem is downstream. If no
		// Node ever appeared, the credential is one of the few things that can
		// explain it — so they get different reasons, and only the latter feeds
		// the suspect-credential guard.
		msg := "waiting for the workload Node to register"
		reason := cellsv1alpha1.ReasonNodeNeverRegistered
		if obs.Node.Exists {
			msg = "workload Node registered but not Ready"
			reason = cellsv1alpha1.ReasonJoinTimeout
		}
		return expireOr(obs, cellsv1alpha1.CellPhaseJoining, obs.BootstrapTimeout, reason, msg)

	case !obs.ProviderReady || obs.CapacityDevices < obs.ExpectedDevices:
		msg := preflightMessage(obs)
		return expireOr(obs, cellsv1alpha1.CellPhaseAwaitingGPUCapacity, obs.CapacityTimeout,
			cellsv1alpha1.ReasonGPUNotAdvertised, msg)

	default:
		return Decision{Phase: cellsv1alpha1.CellPhaseReady}
	}
}

// preflightMessage explains why a joined Node is not yet advertising its GPU,
// preferring the guest's own preflight result when the image reported one.
func preflightMessage(obs Observation) string {
	if !obs.ProviderReady {
		return "workload Node is Ready but the capacity provider is not usable yet"
	}
	msg := "workload Node is Ready but the capacity provider advertises " +
		itoa(obs.CapacityDevices) + " of " + itoa(obs.ExpectedDevices) + " expected GPU(s)"
	if pf, ok := obs.Node.Annotations[cellsv1alpha1.AnnotationGPUPreflight]; ok && pf != "" {
		msg += "; in-guest preflight reported " + pf
	}
	return msg
}

// expireOr returns the in-progress phase, or Failed once the phase has been held
// past its budget. A cell that cannot make progress must say so — a silent
// permanent Pending is the failure mode this whole design rejects.
func expireOr(obs Observation, phase cellsv1alpha1.CellPhase, budget time.Duration, reason, msg string) Decision {
	if budget > 0 && obs.Current == phase && !obs.LastTransition.IsZero() &&
		obs.Now.Sub(obs.LastTransition) > budget {
		return failed(reason, msg+" (timed out after "+budget.String()+")")
	}
	return Decision{Phase: phase, Reason: reason, Message: msg}
}

func failed(reason, msg string) Decision {
	if reason == "" {
		reason = cellsv1alpha1.ReasonCellProvisionTimeout
	}
	return Decision{Phase: cellsv1alpha1.CellPhaseFailed, Reason: reason, Message: msg}
}

// DrainDecision describes whether a draining cell may be removed.
type DrainDecision struct {
	// Proceed is true when the cell can be deleted.
	Proceed bool
	Reason  string
	Message string
}

// AdvanceDrain decides whether a draining cell may be deleted.
//
// The asymmetry between explicit and inferred intent is the point: a user who
// deleted the pool gets a bounded wait and then a rough teardown, because leaving
// VMs and GPUs pinned forever after a delete request is worse. An automatic
// scale-down gets no such licence — it waits indefinitely and says so, because
// destroying someone's running work on a heuristic is unrecoverable.
func AdvanceDrain(
	allocs capacity.Allocations, allocsKnown bool,
	poolDeleting bool, force bool,
	drainingSince, now time.Time, drainTimeout time.Duration,
) DrainDecision {
	if force && poolDeleting {
		return DrainDecision{Proceed: true, Reason: cellsv1alpha1.ReasonCellDraining,
			Message: "deletion policy Force: removing the cell without draining"}
	}

	if allocsKnown && allocs.Empty() {
		return DrainDecision{Proceed: true, Reason: cellsv1alpha1.ReasonCellDraining,
			Message: "no workloads hold this cell's GPU"}
	}

	var msg string
	if allocsKnown {
		msg = itoa(allocs.Consumers) + " workload(s) still hold this cell's GPU"
		if allocs.InFlight {
			msg += " (a binding is in flight)"
		}
	} else {
		msg = "cannot read allocations from the workload cluster; not removing the cell"
	}

	if poolDeleting && drainTimeout > 0 && !drainingSince.IsZero() && now.Sub(drainingSince) > drainTimeout {
		return DrainDecision{Proceed: true, Reason: cellsv1alpha1.ReasonDrainTimedOut,
			Message: msg + "; drain timed out on pool deletion, proceeding anyway"}
	}
	return DrainDecision{Proceed: false, Reason: cellsv1alpha1.ReasonWaitingForAllocations, Message: msg}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
