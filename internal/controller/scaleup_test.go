package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
)

// unschedulableGPUPod is what a workload waiting for GPU capacity looks like.
func (f *testFixture) unschedulableGPUPod(name string, memMiB, cores int64) {
	f.t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "c", Image: "busybox",
			Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
				"nvidia.com/gpu":      *resource.NewQuantity(1, resource.DecimalSI),
				"nvidia.com/gpumem":   *resource.NewQuantity(memMiB, resource.DecimalSI),
				"nvidia.com/gpucores": *resource.NewQuantity(cores, resource.DecimalSI),
			}},
		}}},
	}
	created, err := innerClientset.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
	if err != nil {
		f.t.Fatalf("create pod: %v", err)
	}
	created.Status.Phase = corev1.PodPending
	created.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
		Reason: corev1.PodReasonUnschedulable, Message: "0/1 nodes are available: 1 NodeUnfitPod",
	}}
	if _, err := innerClientset.CoreV1().Pods("default").UpdateStatus(
		context.Background(), created, metav1.UpdateOptions{}); err != nil {
		f.t.Fatalf("set pod unschedulable: %v", err)
	}
	f.t.Cleanup(func() {
		_ = innerClientset.CoreV1().Pods("default").Delete(context.Background(), name, *metav1.NewDeleteOptions(0))
	})
}

// readyCell walks a cell all the way to Ready, so the pool has a reference device.
func (f *testFixture) readyCell(name string) {
	f.t.Helper()
	f.reconcile()
	f.stampGuestRunning(name, "10.77.0.5", "0000:01:00.0", "boba")
	f.joinNode(name, f.guestUID(name), true)
	f.reconcile()
	if p := f.getPool(); len(p.Status.Cells) == 0 || p.Status.Cells[0].Phase != cellsv1alpha1.CellPhaseReady {
		f.t.Fatalf("setup: cell not Ready: %+v", p.Status.Cells)
	}
}

func TestScaleUpOnSatisfiableDemand(t *testing.T) {
	i32 := func(i int32) *int32 { return &i }
	f := newFixture(t, func(p *cellsv1alpha1.GPUCellPool) {
		p.Spec.Autoscaling = &cellsv1alpha1.AutoscalingSpec{
			Enabled: true, MinReplicas: i32(1), MaxReplicas: i32(2),
			ScaleDown: cellsv1alpha1.ScaleDownManual,
		}
	})
	f.readyCell("cells-0")

	// No demand: the pool holds.
	f.reconcile()
	if got := len(f.guests()); got != 1 {
		t.Fatalf("got %d cells with no demand, want 1", got)
	}

	// A workload that one more cell WOULD satisfy (the cell advertises 8Gi/100).
	f.unschedulableGPUPod("waiting", 3000, 30)
	f.reconcile()

	pool := f.getPool()
	if pool.Status.Demand == nil || pool.Status.Demand.SatisfiableByOneCell != 1 {
		t.Fatalf("demand = %+v, want 1 satisfiable", pool.Status.Demand)
	}
	if pool.Status.DesiredReplicas != 2 {
		t.Errorf("desiredReplicas = %d, want 2", pool.Status.DesiredReplicas)
	}
	if got := len(f.guests()); got != 2 {
		t.Errorf("got %d cells, want a second one created", got)
	}
	if pool.Status.LastScaleUpTime == nil {
		t.Error("LastScaleUpTime not stamped, so the stabilization window will not hold")
	}
	if c := condition(t, pool, cellsv1alpha1.ConditionScalingActive); c == nil ||
		c.Reason != cellsv1alpha1.ReasonScaledUp {
		t.Errorf("ScalingActive = %+v, want ScaledUp", c)
	}

	// Immediately after: the window holds, so no third cell even though the pod
	// is still pending (it stays pending until the new cell is Ready).
	f.reconcile()
	if got := len(f.guests()); got != 2 {
		t.Errorf("got %d cells, want the stabilization window to hold at 2", got)
	}
}

func TestNoScaleUpForDemandACellCannotSatisfy(t *testing.T) {
	i32 := func(i int32) *int32 { return &i }
	f := newFixture(t, func(p *cellsv1alpha1.GPUCellPool) {
		p.Spec.Autoscaling = &cellsv1alpha1.AutoscalingSpec{
			Enabled: true, MinReplicas: i32(1), MaxReplicas: i32(4),
			ScaleDown: cellsv1alpha1.ScaleDownManual,
		}
	})
	f.readyCell("cells-0")

	// 40000 MiB against an 8Gi device: another cell changes nothing.
	f.unschedulableGPUPod("too-big", 40000, 30)
	f.reconcile()

	pool := f.getPool()
	if pool.Status.Demand == nil || pool.Status.Demand.PendingRequests != 1 {
		t.Fatalf("demand = %+v, want the request counted", pool.Status.Demand)
	}
	if pool.Status.Demand.SatisfiableByOneCell != 0 {
		t.Errorf("satisfiable = %d, want 0", pool.Status.Demand.SatisfiableByOneCell)
	}
	if got := len(f.guests()); got != 1 {
		t.Errorf("got %d cells: the pool grew for demand it cannot satisfy", got)
	}
	if c := condition(t, pool, cellsv1alpha1.ConditionScalingActive); c == nil ||
		c.Reason != cellsv1alpha1.ReasonDemandUnsatisfiable {
		t.Errorf("ScalingActive = %+v, want DemandUnsatisfiable", c)
	}
}

func TestScheduledButStuckPodIsNotDemand(t *testing.T) {
	i32 := func(i int32) *int32 { return &i }
	f := newFixture(t, func(p *cellsv1alpha1.GPUCellPool) {
		p.Spec.Autoscaling = &cellsv1alpha1.AutoscalingSpec{
			Enabled: true, MinReplicas: i32(1), MaxReplicas: i32(4),
			ScaleDown: cellsv1alpha1.ScaleDownManual,
		}
	})
	f.readyCell("cells-0")

	// The design's canonical false positive: a pod pending on something that is
	// not capacity. It has been SCHEDULED, so it must not create a cell.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "missing-configmap"},
		Spec: corev1.PodSpec{
			NodeName: "cells-0",
			Containers: []corev1.Container{{Name: "c", Image: "busybox",
				Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
					"nvidia.com/gpu":    *resource.NewQuantity(1, resource.DecimalSI),
					"nvidia.com/gpumem": *resource.NewQuantity(3000, resource.DecimalSI),
				}}}},
		},
	}
	created, err := innerClientset.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create pod: %v", err)
	}
	t.Cleanup(func() {
		_ = innerClientset.CoreV1().Pods("default").Delete(context.Background(), "missing-configmap", *metav1.NewDeleteOptions(0))
	})
	created.Status.Phase = corev1.PodPending
	created.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionTrue}}
	if _, err := innerClientset.CoreV1().Pods("default").UpdateStatus(context.Background(), created, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("status: %v", err)
	}

	f.reconcile()
	if got := len(f.guests()); got != 1 {
		t.Errorf("got %d cells: a scheduled-but-stuck pod created capacity", got)
	}
}
