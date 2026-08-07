package controller

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
	"github.com/kubeswift-io/gpucellpool/internal/capacity"
)

func TestCountCells(t *testing.T) {
	total, ready, creating, draining, failed := CountCells([]cellsv1alpha1.CellStatus{
		{Phase: cellsv1alpha1.CellPhaseReady},
		{Phase: cellsv1alpha1.CellPhaseReady},
		{Phase: cellsv1alpha1.CellPhaseBooting},
		{Phase: cellsv1alpha1.CellPhaseDraining},
		{Phase: cellsv1alpha1.CellPhaseFailed},
	})
	if total != 5 || ready != 2 || creating != 1 || draining != 1 || failed != 1 {
		t.Errorf("got %d/%d/%d/%d/%d", total, ready, creating, draining, failed)
	}
}

func TestWorkloadCapacityStatusPublishesComputeOnlyWhenHomogeneous(t *testing.T) {
	obs := metav1.Now()

	homo := WorkloadCapacityStatus("HAMi", "DevicePlugin", capacity.Capacity{
		Devices: 2, Homogeneous: true,
		MemoryTotalMiB: 16384, MemoryAllocatedMiB: 6000,
		ComputeTotal: 200, ComputeAllocated: 60,
		ByModel: []capacity.ModelCapacity{{Model: "NVIDIA GeForce GTX 1080", Devices: 2,
			MemoryTotalMiB: 16384, MemoryAllocatedMiB: 6000, ComputeTotal: 200, ComputeAllocated: 60}},
	}, obs)

	if homo.GPUCompute == nil || homo.GPUCompute.Available != 140 {
		t.Errorf("GPUCompute = %+v, want available 140", homo.GPUCompute)
	}
	// 16384 MiB - 6000 MiB = 10384 MiB available.
	if got := homo.GPUMemory.Available.Value(); got != 10384*1024*1024 {
		t.Errorf("memory available = %d bytes", got)
	}
	if len(homo.ByModel) != 1 || homo.ByModel[0].Compute.Total != 200 {
		t.Errorf("ByModel = %+v", homo.ByModel)
	}

	hetero := WorkloadCapacityStatus("HAMi", "DevicePlugin", capacity.Capacity{
		Devices: 2, Homogeneous: false,
		MemoryTotalMiB: 54260, MemoryAllocatedMiB: 0,
		ComputeTotal: 200,
		ByModel: []capacity.ModelCapacity{
			{Model: "NVIDIA GeForce GTX 1080", Devices: 1, MemoryTotalMiB: 8192, ComputeTotal: 100},
			{Model: "NVIDIA L40S", Devices: 1, MemoryTotalMiB: 46068, ComputeTotal: 100},
		},
	}, obs)

	// 100 cores of a GTX 1080 and 100 of an L40S are not 200 of anything.
	if hetero.GPUCompute != nil {
		t.Errorf("published a pool-wide compute aggregate for a mixed-model pool: %+v", hetero.GPUCompute)
	}
	// Memory stays valid: bytes are commensurable.
	if hetero.GPUMemory == nil || hetero.GPUMemory.Total.Value() != 54260*1024*1024 {
		t.Errorf("GPUMemory = %+v", hetero.GPUMemory)
	}
	if len(hetero.ByModel) != 2 {
		t.Errorf("ByModel = %+v, want per-model detail to remain authoritative", hetero.ByModel)
	}
}

func conditionOf(conds []metav1.Condition, t string) *metav1.Condition {
	return meta.FindStatusCondition(conds, t)
}

func TestComputeConditionsLocalisesTheFault(t *testing.T) {
	base := ConditionInput{
		Generation:        3,
		Desired:           2,
		Ready:             2,
		WorkloadReachable: true,
		ProviderHealth:    capacity.Health{Ready: true, Reason: cellsv1alpha1.ReasonHAMiDetected},
		CapacityKnown:     true,
		Capacity: capacity.Capacity{
			Devices: 2, Homogeneous: true,
			MemoryTotalMiB: 16384, MemoryAllocatedMiB: 6000,
			ComputeTotal: 200, ComputeAllocated: 60,
		},
	}
	free := 1
	base.FreeGPUs = &free

	t.Run("all good", func(t *testing.T) {
		conds := ComputeConditions(base)
		for _, want := range []string{
			cellsv1alpha1.ConditionReady,
			cellsv1alpha1.ConditionWorkloadClusterReachable,
			cellsv1alpha1.ConditionCapacityProviderReady,
			cellsv1alpha1.ConditionCapacityAvailable,
			cellsv1alpha1.ConditionPhysicalGPUsAvailable,
		} {
			c := conditionOf(conds, want)
			if c == nil || c.Status != metav1.ConditionTrue {
				t.Errorf("%s = %+v, want True", want, c)
			}
			if c != nil && c.ObservedGeneration != 3 {
				t.Errorf("%s observedGeneration = %d", want, c.ObservedGeneration)
			}
		}
	})

	t.Run("unreachable drags Ready down and names itself", func(t *testing.T) {
		in := base
		in.WorkloadReachable = false
		in.WorkloadReason = cellsv1alpha1.ReasonCredentialInvalid
		conds := ComputeConditions(in)

		if c := conditionOf(conds, cellsv1alpha1.ConditionWorkloadClusterReachable); c.Status != metav1.ConditionFalse ||
			c.Reason != cellsv1alpha1.ReasonCredentialInvalid {
			t.Errorf("reachable = %+v, want False/CredentialInvalid", c)
		}
		if c := conditionOf(conds, cellsv1alpha1.ConditionReady); c.Status != metav1.ConditionFalse {
			t.Error("Ready stayed True while the workload cluster was unreachable")
		}
	})

	t.Run("provider degraded blocks Ready even with all cells counted", func(t *testing.T) {
		in := base
		in.ProviderHealth = capacity.Health{Ready: false, Reason: cellsv1alpha1.ReasonRegistrationUnparseable, Message: "bad json"}
		conds := ComputeConditions(in)
		ready := conditionOf(conds, cellsv1alpha1.ConditionReady)
		if ready.Status != metav1.ConditionFalse || ready.Reason != cellsv1alpha1.ReasonRegistrationUnparseable {
			t.Errorf("Ready = %+v, want False carrying the provider's reason", ready)
		}
	})

	t.Run("capacity unknown is not zero", func(t *testing.T) {
		in := base
		in.CapacityKnown = false
		c := conditionOf(ComputeConditions(in), cellsv1alpha1.ConditionCapacityAvailable)
		if c.Status != metav1.ConditionFalse || c.Reason != cellsv1alpha1.ReasonCapacityUnknown {
			t.Errorf("got %+v, want False/Unknown", c)
		}
	})

	t.Run("saturated", func(t *testing.T) {
		in := base
		in.Capacity.MemoryAllocatedMiB = in.Capacity.MemoryTotalMiB
		c := conditionOf(ComputeConditions(in), cellsv1alpha1.ConditionCapacityAvailable)
		if c.Status != metav1.ConditionFalse || c.Reason != cellsv1alpha1.ReasonSaturated {
			t.Errorf("got %+v, want False/Saturated", c)
		}
	})

	t.Run("no free GPU is only a fault when a cell wants one", func(t *testing.T) {
		zero := 0
		in := base
		in.FreeGPUs = &zero

		// A satisfied pool on a saturated cluster is not broken.
		if c := conditionOf(ComputeConditions(in), cellsv1alpha1.ConditionPhysicalGPUsAvailable); c.Status != metav1.ConditionTrue {
			t.Errorf("flagged a fault with no cell waiting: %+v", c)
		}

		in.CellsWaitingForGPU = 1
		c := conditionOf(ComputeConditions(in), cellsv1alpha1.ConditionPhysicalGPUsAvailable)
		if c.Status != metav1.ConditionFalse || c.Reason != cellsv1alpha1.ReasonInsufficientGPUs {
			t.Errorf("got %+v, want False/InsufficientPhysicalGPU", c)
		}
	})

	t.Run("unknown free count does not invent a fault", func(t *testing.T) {
		in := base
		in.FreeGPUs = nil
		in.CellsWaitingForGPU = 1
		c := conditionOf(ComputeConditions(in), cellsv1alpha1.ConditionPhysicalGPUsAvailable)
		if c.Status != metav1.ConditionTrue || c.Reason != cellsv1alpha1.ReasonCapacityUnknown {
			t.Errorf("got %+v, want True/Unknown", c)
		}
	})

	t.Run("a stalled pool is not Progressing and says why", func(t *testing.T) {
		in := base
		in.Progressing = true
		in.Membership = MembershipPlan{Stalled: true, Reason: cellsv1alpha1.ReasonInsufficientGPUs, Message: "no free GPU"}
		c := conditionOf(ComputeConditions(in), cellsv1alpha1.ConditionProgressing)
		if c.Status != metav1.ConditionFalse || c.Reason != cellsv1alpha1.ReasonInsufficientGPUs || c.Message != "no free GPU" {
			t.Errorf("got %+v", c)
		}
	})
}

func TestApplyConditionsPreservesTransitionTimes(t *testing.T) {
	old := metav1.NewTime(now.Add(-time.Hour))
	existing := []metav1.Condition{{
		Type: cellsv1alpha1.ConditionReady, Status: metav1.ConditionTrue,
		Reason: cellsv1alpha1.ReasonAllCellsReady, LastTransitionTime: old,
	}}
	merged := ApplyConditions(existing, []metav1.Condition{{
		Type: cellsv1alpha1.ConditionReady, Status: metav1.ConditionTrue,
		Reason: cellsv1alpha1.ReasonAllCellsReady, Message: "still fine",
	}})
	got := conditionOf(merged, cellsv1alpha1.ConditionReady)
	if !got.LastTransitionTime.Equal(&old) {
		t.Errorf("LastTransitionTime moved on an unchanged status: %v -> %v", old, got.LastTransitionTime)
	}
	if got.Message != "still fine" {
		t.Errorf("message not updated: %q", got.Message)
	}
}
