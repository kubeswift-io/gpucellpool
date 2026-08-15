package controller

import (
	"strings"
	"testing"
	"time"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
	"github.com/kubeswift-io/gpucellpool/internal/capacity"
	"github.com/kubeswift-io/gpucellpool/internal/provisioner"
	"github.com/kubeswift-io/gpucellpool/internal/workload"
)

var now = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

// readyObs is a cell where both layers agree; tests degrade it field by field.
func readyObs() Observation {
	return Observation{
		Current:           cellsv1alpha1.CellPhaseReady,
		LastTransition:    now.Add(-time.Hour),
		Now:               now,
		Outer:             provisioner.OuterState{Exists: true, Phase: "Running", Provisioned: true, GPUDevices: []string{"0000:01:00.0"}, Address: "10.77.0.5", UID: "uid-1"},
		WorkloadReachable: true,
		Node:              workload.NodeState{Exists: true, Ready: true},
		ProviderReady:     true,
		CapacityDevices:   1,
		ExpectedDevices:   1,
		BootstrapTimeout:  15 * time.Minute,
		CapacityTimeout:   5 * time.Minute,
	}
}

func TestAdvanceCellHappyPath(t *testing.T) {
	steps := []struct {
		name   string
		mutate func(*Observation)
		want   cellsv1alpha1.CellPhase
	}{
		{"no outer objects", func(o *Observation) { o.Outer = provisioner.OuterState{} }, cellsv1alpha1.CellPhasePending},
		{"awaiting GPU", func(o *Observation) {
			o.Outer = provisioner.OuterState{Exists: true}
		}, cellsv1alpha1.CellPhaseAllocatingGPU},
		{"GPU allocated, VM starting", func(o *Observation) {
			o.Outer = provisioner.OuterState{Exists: true, GPUDevices: []string{"0000:01:00.0"}}
		}, cellsv1alpha1.CellPhaseGuestProvisioning},
		{"VM running, no address yet", func(o *Observation) {
			o.Outer = provisioner.OuterState{Exists: true, GPUDevices: []string{"0000:01:00.0"}, Phase: "Running"}
		}, cellsv1alpha1.CellPhaseBooting},
		{"node not registered", func(o *Observation) { o.Node = workload.NodeState{} }, cellsv1alpha1.CellPhaseJoining},
		{"node not ready", func(o *Observation) { o.Node = workload.NodeState{Exists: true} }, cellsv1alpha1.CellPhaseJoining},
		{"GPU not advertised", func(o *Observation) { o.CapacityDevices = 0 }, cellsv1alpha1.CellPhaseAwaitingGPUCapacity},
		{"provider degraded", func(o *Observation) { o.ProviderReady = false }, cellsv1alpha1.CellPhaseAwaitingGPUCapacity},
		{"all three agree", func(o *Observation) {}, cellsv1alpha1.CellPhaseReady},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			obs := readyObs()
			obs.Current = cellsv1alpha1.CellPhasePending // fresh, so no timeout fires
			obs.LastTransition = now
			s.mutate(&obs)
			if got := AdvanceCell(obs).Phase; got != s.want {
				t.Errorf("phase = %s, want %s", got, s.want)
			}
		})
	}
}

func TestReadyRequiresAllThreeLayers(t *testing.T) {
	// A running VM whose GPU never surfaced is the exact failure this pool
	// exists to catch. It must never count as capacity.
	obs := readyObs()
	obs.CapacityDevices = 0
	if got := AdvanceCell(obs).Phase; got == cellsv1alpha1.CellPhaseReady {
		t.Error("Ready with no GPU advertised")
	}
	obs = readyObs()
	obs.Node.Ready = false
	if got := AdvanceCell(obs).Phase; got == cellsv1alpha1.CellPhaseReady {
		t.Error("Ready with a NotReady node")
	}
	obs = readyObs()
	obs.Outer.Provisioned = false
	if got := AdvanceCell(obs).Phase; got == cellsv1alpha1.CellPhaseReady {
		t.Error("Ready with an unprovisioned VM")
	}
}

func TestDoubtFreezesRatherThanFails(t *testing.T) {
	// Rule 1. While the workload cluster is unreachable we know nothing new, so
	// we conclude nothing new — above all, we do not condemn a healthy cell.
	for _, phase := range []cellsv1alpha1.CellPhase{
		cellsv1alpha1.CellPhaseReady,
		cellsv1alpha1.CellPhaseJoining,
		cellsv1alpha1.CellPhaseAwaitingGPUCapacity,
	} {
		obs := readyObs()
		obs.Current = phase
		obs.WorkloadReachable = false
		obs.Node = workload.NodeState{} // unknown, not absent
		obs.CapacityDevices = 0
		obs.LastTransition = now.Add(-24 * time.Hour) // well past every timeout

		got := AdvanceCell(obs)
		if got.Phase != phase {
			t.Errorf("phase moved from %s to %s while the workload cluster was unreachable", phase, got.Phase)
		}
		if got.Phase == cellsv1alpha1.CellPhaseFailed {
			t.Error("failed a cell on inner doubt")
		}
	}
}

func TestOuterFailureIsReportedEvenWhenInnerIsUnreachable(t *testing.T) {
	// KubeSwift has already concluded; that knowledge is not in question.
	obs := readyObs()
	obs.WorkloadReachable = false
	obs.Outer.Failed = true
	obs.Outer.Reason = cellsv1alpha1.ReasonCellUnschedulable
	obs.Outer.Message = "no node fits"

	got := AdvanceCell(obs)
	if got.Phase != cellsv1alpha1.CellPhaseFailed || got.Reason != cellsv1alpha1.ReasonCellUnschedulable {
		t.Errorf("got %+v, want Failed/CellUnschedulable", got)
	}
	if !strings.Contains(got.Message, "no node fits") {
		t.Errorf("message lost KubeSwift's explanation: %q", got.Message)
	}
}

func TestRegressionIsNotFailure(t *testing.T) {
	// A restarted HAMi plugin or a briefly NotReady node must not condemn a
	// healthy VM — the cell goes back a phase and can recover.
	obs := readyObs()
	obs.CapacityDevices = 0
	obs.LastTransition = now // just regressed
	got := AdvanceCell(obs)
	if got.Phase != cellsv1alpha1.CellPhaseAwaitingGPUCapacity {
		t.Errorf("phase = %s, want AwaitingGPUCapacity", got.Phase)
	}

	obs = readyObs()
	obs.Node.Ready = false
	obs.LastTransition = now
	if got := AdvanceCell(obs).Phase; got != cellsv1alpha1.CellPhaseJoining {
		t.Errorf("phase = %s, want Joining", got)
	}
}

func TestTimeouts(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Observation)
		current cellsv1alpha1.CellPhase
		elapsed time.Duration
		want    string
	}{
		// The two join timeouts are distinguished on purpose (#17). A Node that
		// REGISTERED proves the bootstrap credential was accepted, so the fault is
		// downstream; a Node that never appeared is the only one of the two that is
		// evidence about the credential, and only it feeds the suspect guard.
		{"join timeout, node never registered", func(o *Observation) { o.Node = workload.NodeState{} },
			cellsv1alpha1.CellPhaseJoining, 16 * time.Minute, cellsv1alpha1.ReasonNodeNeverRegistered},
		{"join timeout, node registered but never Ready",
			func(o *Observation) { o.Node = workload.NodeState{Exists: true} },
			cellsv1alpha1.CellPhaseJoining, 16 * time.Minute, cellsv1alpha1.ReasonJoinTimeout},
		{"gpu never advertised", func(o *Observation) { o.CapacityDevices = 0 },
			cellsv1alpha1.CellPhaseAwaitingGPUCapacity, 6 * time.Minute, cellsv1alpha1.ReasonGPUNotAdvertised},
		{"provision timeout", func(o *Observation) { o.Outer = provisioner.OuterState{Exists: true} },
			cellsv1alpha1.CellPhaseAllocatingGPU, 16 * time.Minute, cellsv1alpha1.ReasonCellProvisionTimeout},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			obs := readyObs()
			c.mutate(&obs)
			obs.Current = c.current
			obs.LastTransition = now.Add(-c.elapsed)

			got := AdvanceCell(obs)
			if got.Phase != cellsv1alpha1.CellPhaseFailed {
				t.Fatalf("phase = %s, want Failed after %s", got.Phase, c.elapsed)
			}
			if got.Reason != c.want {
				t.Errorf("reason = %s, want %s", got.Reason, c.want)
			}
			if !strings.Contains(got.Message, "timed out") {
				t.Errorf("message should say it timed out: %q", got.Message)
			}
		})
	}
}

func TestTimeoutOnlyCountsTimeInTheSamePhase(t *testing.T) {
	// A cell that just entered AwaitingGPUCapacity after an hour in Joining must
	// get its full capacity budget, not inherit the elapsed time.
	obs := readyObs()
	obs.CapacityDevices = 0
	obs.Current = cellsv1alpha1.CellPhaseJoining // different phase
	obs.LastTransition = now.Add(-time.Hour)

	if got := AdvanceCell(obs); got.Phase == cellsv1alpha1.CellPhaseFailed {
		t.Error("timed out on elapsed time from a different phase")
	}
}

func TestStaleNodeIsReapedBeforeJoining(t *testing.T) {
	// A Node with this cell's name but a previous incarnation's UID would be
	// reported as this cell's Node: a phantom Ready cell, or capacity advertised
	// for a GPU that no longer exists.
	obs := readyObs()
	obs.StaleNode = true
	obs.LastTransition = now

	got := AdvanceCell(obs)
	if got.Phase != cellsv1alpha1.CellPhaseJoining {
		t.Errorf("phase = %s, want Joining while the stale Node is removed", got.Phase)
	}
	if !got.ReapStaleNode {
		t.Error("ReapStaleNode not requested")
	}
}

func TestPreflightResultSurfacesInTheMessage(t *testing.T) {
	obs := readyObs()
	obs.CapacityDevices = 0
	obs.LastTransition = now
	obs.Node.Annotations = map[string]string{cellsv1alpha1.AnnotationGPUPreflight: "0/1"}

	got := AdvanceCell(obs)
	if !strings.Contains(got.Message, "0/1") {
		t.Errorf("message should carry the in-guest preflight result: %q", got.Message)
	}
}

func TestBootingMessagePointsAtNodeIPFrom(t *testing.T) {
	// Running with no address is usually a nodeIPFrom that names an interface the
	// guest does not have — worth saying, because the symptom is silence.
	obs := readyObs()
	obs.Outer = provisioner.OuterState{Exists: true, GPUDevices: []string{"0000:01:00.0"}, Phase: "Running"}
	obs.Current = cellsv1alpha1.CellPhaseBooting
	obs.LastTransition = now

	got := AdvanceCell(obs)
	if !strings.Contains(got.Message, "nodeIPFrom") {
		t.Errorf("message = %q, want a pointer at cell.nodeIPFrom", got.Message)
	}
}

func TestDrainingAndDeletingAreOwnedByTeardown(t *testing.T) {
	for _, phase := range []cellsv1alpha1.CellPhase{cellsv1alpha1.CellPhaseDraining, cellsv1alpha1.CellPhaseDeleting} {
		obs := readyObs()
		obs.Current = phase
		if got := AdvanceCell(obs).Phase; got != phase {
			t.Errorf("%s was moved to %s by the FSM", phase, got)
		}
	}
}

func TestAdvanceDrain(t *testing.T) {
	held := capacity.Allocations{Consumers: 1, MemoryMiB: 3000, CorePercent: 30}
	empty := capacity.Allocations{}
	drainStart := now.Add(-time.Hour)

	t.Run("workloads still hold the GPU", func(t *testing.T) {
		d := AdvanceDrain(held, true, false, false, drainStart, now, 10*time.Minute)
		if d.Proceed {
			t.Error("proceeded while a workload held the GPU")
		}
		if !strings.Contains(d.Message, "1 workload(s)") {
			t.Errorf("message = %q", d.Message)
		}
	})

	t.Run("in-flight binding blocks", func(t *testing.T) {
		d := AdvanceDrain(capacity.Allocations{InFlight: true}, true, false, false, drainStart, now, 10*time.Minute)
		if d.Proceed {
			t.Error("proceeded during a binding window")
		}
	})

	t.Run("unknown allocations block", func(t *testing.T) {
		// Not knowing is not the same as knowing there are none.
		d := AdvanceDrain(empty, false, false, false, drainStart, now, 10*time.Minute)
		if d.Proceed {
			t.Error("proceeded without being able to read allocations")
		}
	})

	t.Run("empty proceeds", func(t *testing.T) {
		if d := AdvanceDrain(empty, true, false, false, drainStart, now, 10*time.Minute); !d.Proceed {
			t.Errorf("did not proceed with no allocations: %+v", d)
		}
	})

	t.Run("explicit deletion may force through after the timeout", func(t *testing.T) {
		d := AdvanceDrain(held, true, true, false, drainStart, now, 10*time.Minute)
		if !d.Proceed || d.Reason != cellsv1alpha1.ReasonDrainTimedOut {
			t.Errorf("got %+v, want proceed/DrainTimedOut", d)
		}
	})

	t.Run("inferred scale-down NEVER forces through", func(t *testing.T) {
		// The asymmetry that matters: an automatic action must not destroy
		// running work because a timer expired.
		d := AdvanceDrain(held, true, false, false, drainStart, now, 10*time.Minute)
		if d.Proceed {
			t.Error("a scale-down forced through a drain timeout")
		}
	})

	t.Run("force skips the drain entirely, but only on pool deletion", func(t *testing.T) {
		if d := AdvanceDrain(held, true, true, true, now, now, time.Hour); !d.Proceed {
			t.Error("Force did not proceed on pool deletion")
		}
		if d := AdvanceDrain(held, true, false, true, now, now, time.Hour); d.Proceed {
			t.Error("Force proceeded outside pool deletion")
		}
	})
}
