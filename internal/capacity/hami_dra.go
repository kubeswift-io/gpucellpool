package capacity

// HAMi DRA mode: capacity read from resource.k8s.io instead of from node
// annotations.
//
// The shape below is not inferred from the API alone — it is what
// Project-HAMi/k8s-dra-driver actually publishes (cmd/hami-kubelet-plugin):
// one device per physical GPU, named "hami-gpu-<minor>", carrying capacity
// "cores" (100, a percentage of the device) and "memory" (bytes), attributes
// including "productName" (the model) and "type" ("hami-gpu"), and
// allowMultipleAllocations so several claims can share one device. A claim
// records what it took per device in status.allocation.devices[].consumedCapacity
// under the same two keys.
//
// Why this mode is worth having: capacity is a first-class API field here, so
// "how much of this GPU is left" is read rather than reconstructed from an
// annotation protocol that HAMi owns and may change.

import (
	"context"
	"fmt"
	"sort"

	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
)

const (
	// DRADriverName is the driver Project-HAMi's DRA driver registers, and also
	// the name of the DeviceClass its chart ships.
	DRADriverName = "hami-core-gpu.project-hami.io"

	// Capacity keys the driver publishes per device.
	draCapCores  resourceapi.QualifiedName = "cores"
	draCapMemory resourceapi.QualifiedName = "memory"

	// draAttrProductName carries the GPU model, which is what homogeneity and
	// ByModel are computed from.
	draAttrProductName resourceapi.QualifiedName = "productName"

	mib = int64(1024 * 1024)
)

// deviceClassName is the DeviceClass whose slices carry the cells' capacity.
func (p *HAMiProvider) deviceClassName() string {
	if p.DeviceClass != "" {
		return p.DeviceClass
	}
	return DRADriverName
}

// draSlices returns this driver's ResourceSlices, indexed by pool name — which
// the driver sets to the node name, so a pool name IS a cell node.
func (p *HAMiProvider) draSlices(ctx context.Context) (map[string][]resourceapi.ResourceSlice, error) {
	list, err := p.Client.ResourceV1().ResourceSlices().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing ResourceSlices: %w", err)
	}
	byPool := map[string][]resourceapi.ResourceSlice{}
	for i := range list.Items {
		s := &list.Items[i]
		if s.Spec.Driver != DRADriverName {
			continue
		}
		byPool[s.Spec.Pool.Name] = append(byPool[s.Spec.Pool.Name], *s)
	}
	return byPool, nil
}

// draHealth preflights the things that make DRA capacity readable at all.
//
// The order matters, because each failure imitates the next one's symptom: no
// resource.k8s.io means no slices, no DeviceClass means nothing can claim, and a
// device published WITHOUT capacity means the DRAConsumableCapacity gate is off —
// which reads exactly like a GPU with nothing left on it. Reporting "zero
// capacity" for any of these would be the silent failure this operator exists to
// avoid.
func (p *HAMiProvider) draHealth(ctx context.Context, nodes []string) (Health, error) {
	// Listing slices first is what separates "the API is not served" from "the
	// DeviceClass is absent": both surface as 404s, but only the first makes a
	// whole-collection list fail.
	byPool, err := p.draSlices(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return Health{
				Reason: cellsv1alpha1.ReasonDRAFeatureGateMissing,
				Message: "the resource.k8s.io/v1 API is not served by the workload cluster; " +
					"HAMi DRA mode needs Kubernetes >= 1.34 with DynamicResourceAllocation enabled",
			}, nil
		}
		return Health{}, err
	}

	class := p.deviceClassName()
	if _, err := p.Client.ResourceV1().DeviceClasses().Get(ctx, class, metav1.GetOptions{}); err != nil {
		if apierrors.IsNotFound(err) {
			return Health{
				Reason: cellsv1alpha1.ReasonDRADeviceClassMissing,
				Message: "DeviceClass " + class + " does not exist in the workload cluster; " +
					"install Project-HAMi's DRA driver, or set capacity.hami.deviceClassName " +
					"to the class your driver ships",
			}, nil
		}
		return Health{}, fmt.Errorf("reading DeviceClass %s: %w", class, err)
	}

	if len(nodes) == 0 {
		// No cells yet is not a provider failure, exactly as in DevicePlugin mode.
		return Health{Ready: true, Reason: cellsv1alpha1.ReasonHAMiDetected}, nil
	}

	detected, withCapacity := 0, 0
	for _, node := range nodes {
		for _, s := range byPool[node] {
			for i := range s.Spec.Devices {
				detected++
				if len(s.Spec.Devices[i].Capacity) > 0 {
					withCapacity++
				}
			}
		}
	}

	switch {
	case detected == 0:
		return Health{
			Reason: cellsv1alpha1.ReasonHAMiNotDetected,
			Message: "no ResourceSlices published by driver " + DRADriverName +
				" for any cell node; is the DRA driver's DaemonSet running on the cells?",
		}, nil
	case withCapacity == 0:
		// Gated fields are dropped by the apiserver, not rejected, so this is what
		// a missing feature gate looks like from here.
		return Health{
			Reason: cellsv1alpha1.ReasonDRAFeatureGateMissing,
			Message: "the DRA driver publishes devices with no capacity, which means the " +
				"DRAConsumableCapacity feature gate is off (it is beta and on by default " +
				"from Kubernetes 1.36; enable it explicitly on 1.34 and 1.35)",
		}, nil
	}
	return Health{Ready: true, Reason: cellsv1alpha1.ReasonHAMiDetected}, nil
}

// draDevices counts the devices published for one cell node.
func (p *HAMiProvider) draDevices(ctx context.Context, node string) (int, error) {
	byPool, err := p.draSlices(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, s := range byPool[node] {
		n += len(s.Spec.Devices)
	}
	return n, nil
}

// draConsumed is what allocated claims have taken, per "<pool>/<device>".
type draConsumed struct {
	cores     int64
	memoryMiB int64
	// claims is how many distinct claims hold a share of this device.
	claims int
	// unreserved is a claim allocated to this device that nothing has bound yet:
	// a hold, and a binding we could race if we treated the device as idle.
	unreserved int
}

func (p *HAMiProvider) draAllocated(ctx context.Context) (map[string]*draConsumed, error) {
	claims, err := p.Client.ResourceV1().ResourceClaims(metav1.NamespaceAll).
		List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing ResourceClaims: %w", err)
	}

	out := map[string]*draConsumed{}
	for i := range claims.Items {
		c := &claims.Items[i]
		if c.Status.Allocation == nil {
			continue
		}
		for _, res := range c.Status.Allocation.Devices.Results {
			if res.Driver != DRADriverName {
				continue
			}
			key := res.Pool + "/" + res.Device
			e := out[key]
			if e == nil {
				e = &draConsumed{}
				out[key] = e
			}
			e.claims++
			if len(c.Status.ReservedFor) == 0 {
				e.unreserved++
			}
			if q, ok := res.ConsumedCapacity[draCapCores]; ok {
				e.cores += q.Value()
			}
			if q, ok := res.ConsumedCapacity[draCapMemory]; ok {
				e.memoryMiB += q.Value() / mib
			}
		}
	}
	return out, nil
}

// draCapacity aggregates the pool-wide view from slices and claims.
func (p *HAMiProvider) draCapacity(ctx context.Context, nodes []string) (Capacity, error) {
	byPool, err := p.draSlices(ctx)
	if err != nil {
		return Capacity{}, err
	}
	consumed, err := p.draAllocated(ctx)
	if err != nil {
		return Capacity{}, err
	}

	byModel := map[string]*ModelCapacity{}
	var out Capacity
	for _, node := range nodes {
		for _, s := range byPool[node] {
			for i := range s.Spec.Devices {
				d := &s.Spec.Devices[i]
				model := draModel(d)
				m := byModel[model]
				if m == nil {
					m = &ModelCapacity{Model: model}
					byModel[model] = m
				}

				m.Devices++
				out.Devices++
				if q, ok := d.Capacity[draCapCores]; ok {
					m.ComputeTotal += q.Value.Value()
				}
				if q, ok := d.Capacity[draCapMemory]; ok {
					m.MemoryTotalMiB += q.Value.Value() / mib
				}
				if e := consumed[s.Spec.Pool.Name+"/"+d.Name]; e != nil {
					m.ComputeAllocated += e.cores
					m.MemoryAllocatedMiB += e.memoryMiB
				}
			}
		}
	}

	for _, m := range byModel {
		out.ByModel = append(out.ByModel, *m)
	}
	sort.Slice(out.ByModel, func(i, j int) bool { return out.ByModel[i].Model < out.ByModel[j].Model })
	out.Homogeneous = len(out.ByModel) <= 1
	for _, m := range out.ByModel {
		out.MemoryTotalMiB += m.MemoryTotalMiB
		out.MemoryAllocatedMiB += m.MemoryAllocatedMiB
	}
	// Percentages of different models are not commensurable, so compute totals
	// are only published for a homogeneous pool — the same rule as DevicePlugin.
	if out.Homogeneous && len(out.ByModel) == 1 {
		out.ComputeTotal = out.ByModel[0].ComputeTotal
		out.ComputeAllocated = out.ByModel[0].ComputeAllocated
	}
	return out, nil
}

// draAllocations reports what still holds one cell's GPU.
func (p *HAMiProvider) draAllocations(ctx context.Context, node string) (Allocations, error) {
	byPool, err := p.draSlices(ctx)
	if err != nil {
		return Allocations{}, err
	}
	consumed, err := p.draAllocated(ctx)
	if err != nil {
		return Allocations{}, err
	}

	var out Allocations
	for _, s := range byPool[node] {
		for i := range s.Spec.Devices {
			e := consumed[s.Spec.Pool.Name+"/"+s.Spec.Devices[i].Name]
			if e == nil {
				continue
			}
			out.Consumers += e.claims
			out.CorePercent += e.cores
			out.MemoryMiB += e.memoryMiB
			if e.unreserved > 0 {
				out.InFlight = true
			}
		}
	}
	return out, nil
}

// draPendingDemand reports GPU demand that no current cell can satisfy.
//
// The signal here is better than DevicePlugin's: an unallocated ResourceClaim
// naming this pool's DeviceClass is unambiguous GPU-capacity demand, rather than
// a pending Pod that might be pending for any of a dozen unrelated reasons. Both
// filters still apply — it must be GPU demand AND a fresh cell must actually
// satisfy it — because a claim asking for more than one whole device would
// otherwise drive an endless scale-up.
func (p *HAMiProvider) draPendingDemand(ctx context.Context, ref *Device) (Demand, error) {
	claims, err := p.Client.ResourceV1().ResourceClaims(metav1.NamespaceAll).
		List(ctx, metav1.ListOptions{})
	if err != nil {
		return Demand{}, fmt.Errorf("listing ResourceClaims: %w", err)
	}

	class := p.deviceClassName()
	var out Demand
	for i := range claims.Items {
		c := &claims.Items[i]
		if c.Status.Allocation != nil {
			continue // already placed
		}
		req, wants := draRequest(c, class)
		if !wants {
			continue
		}
		out.PendingRequests++
		if ref != nil && fitsOneCell(req, *ref) {
			out.SatisfiableByOneCell++
		}
	}
	return out, nil
}

// draRequest reduces a claim's device requests to the same shape the
// DevicePlugin path judges satisfiability with, so one rule covers both modes.
func draRequest(c *resourceapi.ResourceClaim, class string) (gpuReq, bool) {
	var (
		req   gpuReq
		wants bool
	)
	for _, r := range c.Spec.Devices.Requests {
		ex := r.Exactly
		if ex == nil || ex.DeviceClassName != class {
			continue
		}
		wants = true

		count := ex.Count
		if ex.AllocationMode == resourceapi.DeviceAllocationModeAll {
			// "all devices on a node" cannot be judged against one cell's shape.
			return gpuReq{devices: 2}, true
		}
		if count <= 0 {
			count = 1
		}
		req.devices += count

		if ex.Capacity == nil {
			continue
		}
		if q, ok := ex.Capacity.Requests[draCapCores]; ok {
			req.cores += q.Value()
		}
		if q, ok := ex.Capacity.Requests[draCapMemory]; ok {
			req.memoryMiB += q.Value() / mib
		}
	}
	return req, wants
}

// draModel is the device's model, or a stable placeholder. An unnamed model must
// not silently merge devices of different kinds into one ModelCapacity row.
func draModel(d *resourceapi.Device) string {
	if a, ok := d.Attributes[draAttrProductName]; ok && a.StringValue != nil && *a.StringValue != "" {
		return *a.StringValue
	}
	return "unknown"
}
