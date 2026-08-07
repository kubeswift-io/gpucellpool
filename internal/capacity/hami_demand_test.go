package capacity

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// refDevice is the shape a fresh cell of the lab's pool would bring.
func refDevice() *Device {
	return &Device{Model: "NVIDIA GeForce GTX 1080", MemoryMiB: 8192, CorePercent: 100, Healthy: true}
}

func pendingPod(name string, scheduled corev1.ConditionStatus, reason string, limits corev1.ResourceList) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Image: "x", Resources: corev1.ResourceRequirements{Limits: limits}}},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodPending,
			Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: scheduled, Reason: reason}},
		},
	}
}

func gpuLimits(devices, memMiB, cores int64) corev1.ResourceList {
	out := corev1.ResourceList{}
	if devices > 0 {
		out[ResourceGPU] = *resource.NewQuantity(devices, resource.DecimalSI)
	}
	if memMiB > 0 {
		out[ResourceGPUMemory] = *resource.NewQuantity(memMiB, resource.DecimalSI)
	}
	if cores > 0 {
		out[ResourceGPUCores] = *resource.NewQuantity(cores, resource.DecimalSI)
	}
	return out
}

func demandFor(t *testing.T, ref *Device, pods ...*corev1.Pod) Demand {
	t.Helper()
	objs := make([]runtime.Object, 0, len(pods))
	for _, p := range pods {
		objs = append(objs, p)
	}
	cs := fake.NewSimpleClientset(objs...)
	d, err := NewHAMiProvider(cs, ModeDevicePlugin).PendingDemand(context.Background(), ref)
	if err != nil {
		t.Fatalf("PendingDemand: %v", err)
	}
	return d
}

func TestPendingDemandCountsOnlyUnschedulableGPURequests(t *testing.T) {
	d := demandFor(t, refDevice(),
		pendingPod("wants-gpu", corev1.ConditionFalse, corev1.PodReasonUnschedulable, gpuLimits(1, 3000, 30)),
	)
	if d.PendingRequests != 1 || d.SatisfiableByOneCell != 1 {
		t.Errorf("got %+v, want 1 pending and 1 satisfiable", d)
	}
}

func TestPendingDemandIgnoresPodsPendingForOtherReasons(t *testing.T) {
	// THE filter that makes this safe: a pod stuck on a missing ConfigMap or an
	// image pull has already been SCHEDULED, so it carries PodScheduled=True and
	// is not demand for more GPU capacity. The design calls this out explicitly.
	d := demandFor(t, refDevice(),
		pendingPod("missing-configmap", corev1.ConditionTrue, "", gpuLimits(1, 3000, 30)),
	)
	if d.PendingRequests != 0 {
		t.Errorf("got %+v, want nothing: a scheduled-but-not-running pod is not capacity demand", d)
	}
}

func TestPendingDemandIgnoresNonGPUWork(t *testing.T) {
	d := demandFor(t, refDevice(),
		pendingPod("plain", corev1.ConditionFalse, corev1.PodReasonUnschedulable, corev1.ResourceList{
			corev1.ResourceCPU: *resource.NewQuantity(64, resource.DecimalSI),
		}),
	)
	if d.PendingRequests != 0 {
		t.Errorf("got %+v, want nothing: a CPU-starved pod is not GPU demand", d)
	}
}

func TestPendingDemandRejectsWhatACellCannotSatisfy(t *testing.T) {
	cases := map[string]corev1.ResourceList{
		// A cell holds exactly one device in v1alpha1.
		"two devices":     gpuLimits(2, 3000, 30),
		"too much memory": gpuLimits(1, 40000, 30),
		"too many cores":  gpuLimits(1, 3000, 150),
	}
	for name, limits := range cases {
		t.Run(name, func(t *testing.T) {
			d := demandFor(t, refDevice(),
				pendingPod("big", corev1.ConditionFalse, corev1.PodReasonUnschedulable, limits))
			if d.PendingRequests != 1 {
				t.Errorf("got %+v, want the request counted as pending", d)
			}
			if d.SatisfiableByOneCell != 0 {
				t.Errorf("got %+v: adding a cell would not satisfy this, so it must not be counted", d)
			}
		})
	}
}

func TestPendingDemandWithoutAReferenceShapeSatisfiesNothing(t *testing.T) {
	// An empty pool has no advertised device to learn the shape from. Reporting
	// demand as satisfiable would let the scaler act on a guess.
	d := demandFor(t, nil,
		pendingPod("wants-gpu", corev1.ConditionFalse, corev1.PodReasonUnschedulable, gpuLimits(1, 3000, 30)))
	if d.PendingRequests != 1 || d.SatisfiableByOneCell != 0 {
		t.Errorf("got %+v, want pending counted but nothing satisfiable", d)
	}
}

func TestPendingDemandIgnoresAlreadyScheduledPods(t *testing.T) {
	p := pendingPod("assigned", corev1.ConditionFalse, corev1.PodReasonUnschedulable, gpuLimits(1, 3000, 30))
	p.Spec.NodeName = "cells-0" // bound; it is starting, not waiting for capacity
	if d := demandFor(t, refDevice(), p); d.PendingRequests != 0 {
		t.Errorf("got %+v, want nothing for a bound pod", d)
	}
}

func TestPendingDemandIsUnsupportedInDRAMode(t *testing.T) {
	cs := fake.NewSimpleClientset()
	if _, err := NewHAMiProvider(cs, ModeDRA).PendingDemand(context.Background(), refDevice()); err == nil {
		t.Error("DRA mode should report ErrUnsupported rather than silently reading nothing")
	}
}
