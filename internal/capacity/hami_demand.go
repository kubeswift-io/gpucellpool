package capacity

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HAMi's request surface. gpumem is MiB, gpucores is a percentage of one device.
const (
	ResourceGPU       corev1.ResourceName = "nvidia.com/gpu"
	ResourceGPUMemory corev1.ResourceName = "nvidia.com/gpumem"
	ResourceGPUCores  corev1.ResourceName = "nvidia.com/gpucores"
)

// PendingDemand implements Provider: GPU requests the workload cluster cannot
// place right now.
//
// The discriminator that makes this safe is `PodScheduled=False` with reason
// Unschedulable. A pod stuck for an unrelated reason — a missing ConfigMap, an
// image that will not pull — has already been SCHEDULED, so it carries
// PodScheduled=True and is invisible here. That is precisely the failure mode the
// design warns about ("a Pod pending because a ConfigMap is missing must not create
// a GPU Cell"), and it falls out of the condition rather than needing a heuristic.
//
// Measured on the lab: HAMi refuses an over-large request at scheduling time with
// "0/1 nodes are available: 1 NodeUnfitPod", which is an Unschedulable pod — so
// over-large requests DO show up here and must be filtered by the second gate.
func (p *HAMiProvider) PendingDemand(ctx context.Context, ref *Device) (Demand, error) {
	if p.Mode == ModeDRA {
		return p.draPendingDemand(ctx, ref)
	}

	pods, err := p.Client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase=Pending",
	})
	if err != nil {
		return Demand{}, fmt.Errorf("listing pending pods: %w", err)
	}

	var out Demand
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Spec.NodeName != "" || !unschedulable(pod) {
			continue
		}
		req, wantsGPU := gpuRequest(pod)
		if !wantsGPU {
			continue
		}
		out.PendingRequests++
		if ref != nil && fitsOneCell(req, *ref) {
			out.SatisfiableByOneCell++
		}
	}
	return out, nil
}

// unschedulable reports whether the scheduler could not place this pod. A pod that
// was scheduled and is merely not running yet is NOT demand for more capacity.
func unschedulable(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled {
			return c.Status == corev1.ConditionFalse &&
				(c.Reason == corev1.PodReasonUnschedulable || c.Reason == "SchedulerError")
		}
	}
	// No PodScheduled condition yet: the scheduler has not looked. Not demand.
	return false
}

// gpuReq is what one pod asks of a GPU.
type gpuReq struct {
	devices   int64
	memoryMiB int64
	cores     int64
}

// gpuRequest sums a pod's HAMi GPU asks across its containers.
func gpuRequest(pod *corev1.Pod) (gpuReq, bool) {
	var out gpuReq
	found := false
	for i := range pod.Spec.Containers {
		lim := pod.Spec.Containers[i].Resources.Limits
		req := pod.Spec.Containers[i].Resources.Requests
		for _, rl := range []corev1.ResourceList{lim, req} {
			if q, ok := rl[ResourceGPU]; ok {
				out.devices += q.Value()
				found = true
			}
			if q, ok := rl[ResourceGPUMemory]; ok {
				out.memoryMiB += q.Value()
				found = true
			}
			if q, ok := rl[ResourceGPUCores]; ok {
				out.cores += q.Value()
				found = true
			}
			if found {
				break // limits win; do not double-count requests
			}
		}
	}
	return out, found
}

// fitsOneCell reports whether one fresh cell could satisfy this request.
//
// v1alpha1 cells hold exactly ONE device, so a multi-device request can never be
// satisfied by adding a cell — and neither can a request for more memory or
// compute than a single device has. Creating a cell for either would burn a GPU
// and a VM boot to change nothing.
func fitsOneCell(req gpuReq, ref Device) bool {
	if req.devices > 1 {
		return false
	}
	if req.memoryMiB > ref.MemoryMiB {
		return false
	}
	if req.cores > ref.CorePercent {
		return false
	}
	return true
}

// String is for messages and events.
func (r gpuReq) String() string {
	return "devices=" + strconv.FormatInt(r.devices, 10) +
		" memoryMiB=" + strconv.FormatInt(r.memoryMiB, 10) +
		" cores=" + strconv.FormatInt(r.cores, 10)
}
