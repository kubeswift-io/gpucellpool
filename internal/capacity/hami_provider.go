package capacity

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
)

// HAMi modes.
const (
	ModeDevicePlugin = cellsv1alpha1.HAMiModeDevicePlugin
	ModeDRA          = cellsv1alpha1.HAMiModeDRA
)

// HAMiProvider reads capacity from HAMi in the workload cluster.
type HAMiProvider struct {
	Client kubernetes.Interface
	// Mode is DevicePlugin (implemented) or DRA (Phase 3 — reports
	// ErrUnsupported rather than reading nothing and calling it zero).
	Mode string
	// GateLabel is the node label HAMi's scheduler requires (normally "gpu").
	// The user declares its value; we only check presence.
	GateLabel string
	// DeviceClass is the DeviceClass whose ResourceSlices carry the cells'
	// capacity in DRA mode. Empty means Project-HAMi's own class.
	DeviceClass string
}

// NewHAMiProvider returns a provider over a workload-cluster client.
//
// deviceClass is only read in DRA mode; an empty value means Project-HAMi's own
// DeviceClass, which is what its chart installs.
func NewHAMiProvider(cs kubernetes.Interface, mode, deviceClass string) *HAMiProvider {
	if mode == "" {
		mode = ModeDevicePlugin
	}
	return &HAMiProvider{Client: cs, Mode: mode, GateLabel: NodeLabelGate, DeviceClass: deviceClass}
}

// Name implements Provider.
func (p *HAMiProvider) Name() string { return cellsv1alpha1.CapacityProviderHAMi }

// Health implements Provider.
func (p *HAMiProvider) Health(ctx context.Context, nodes []string) (Health, error) {
	if p.Mode == ModeDRA {
		return p.draHealth(ctx, nodes)
	}
	if len(nodes) == 0 {
		// No cells yet is not a provider failure.
		return Health{Ready: true, Reason: cellsv1alpha1.ReasonHAMiDetected}, nil
	}

	detected := 0
	for _, name := range nodes {
		node, err := p.Client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue // still joining; not a provider verdict
		}
		if err != nil {
			return Health{}, fmt.Errorf("reading node %s: %w", name, err)
		}
		_, found, err := ParseNodeRegistration(node.Annotations)
		if err != nil {
			// Loud, not silent: an unparseable registration must never be read
			// as "this node has no GPUs".
			return Health{
				Ready:   false,
				Reason:  cellsv1alpha1.ReasonRegistrationUnparseable,
				Message: err.Error(),
			}, nil
		}
		if !found {
			continue
		}
		if _, ok := node.Labels[p.GateLabel]; !ok {
			return Health{
				Ready:  false,
				Reason: cellsv1alpha1.ReasonHAMiNotDetected,
				Message: fmt.Sprintf("node %s has a HAMi registration but no %q label: "+
					"HAMi's scheduler will not place GPU workloads on it "+
					"(declare it in spec.workloadCluster.node.labels)", name, p.GateLabel),
			}, nil
		}
		detected++
	}

	if detected == 0 {
		return Health{
			Ready:  false,
			Reason: cellsv1alpha1.ReasonHAMiNotDetected,
			Message: "no cell node carries a HAMi device registration " +
				"(is HAMi installed in the workload cluster, and do its DaemonSets tolerate the pool's taints?)",
		}, nil
	}
	return Health{Ready: true, Reason: cellsv1alpha1.ReasonHAMiDetected}, nil
}

// Devices implements Provider: healthy devices advertised on one node.
//
// The count comes from the registration annotation, never from
// allocatable nvidia.com/gpu — that is devices × deviceSplitCount (10 by
// default), so a one-GPU cell would report ten.
func (p *HAMiProvider) Devices(ctx context.Context, name string) (int, error) {
	if p.Mode == ModeDRA {
		return p.draDevices(ctx, name)
	}
	node, err := p.Client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading node %s: %w", name, err)
	}
	devices, found, err := ParseNodeRegistration(node.Annotations)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, nil
	}
	return len(HealthyDevices(devices)), nil
}

// Capacity implements Provider.
func (p *HAMiProvider) Capacity(ctx context.Context, nodes []string) (Capacity, error) {
	if p.Mode == ModeDRA {
		return p.draCapacity(ctx, nodes)
	}

	byModel := map[string]*ModelCapacity{}
	var out Capacity

	for _, name := range nodes {
		node, err := p.Client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return Capacity{}, fmt.Errorf("reading node %s: %w", name, err)
		}
		devices, found, err := ParseNodeRegistration(node.Annotations)
		if err != nil {
			return Capacity{}, err
		}
		if !found {
			continue
		}

		for _, d := range HealthyDevices(devices) {
			m := byModel[d.Model]
			if m == nil {
				m = &ModelCapacity{Model: d.Model}
				byModel[d.Model] = m
			}
			m.Devices++
			m.MemoryTotalMiB += d.MemoryMiB
			m.ComputeTotal += d.CorePercent

			out.Devices++
			out.MemoryTotalMiB += d.MemoryMiB
			out.ComputeTotal += d.CorePercent
		}

		alloc, allocByUUID, err := p.nodeAllocations(ctx, name)
		if err != nil {
			return Capacity{}, err
		}
		out.MemoryAllocatedMiB += alloc.MemoryMiB
		out.ComputeAllocated += alloc.CorePercent

		// Attribute each allocation to its device's model so ByModel stays
		// correct on a mixed-model pool.
		modelOf := map[string]string{}
		for _, d := range devices {
			modelOf[d.UUID] = d.Model
		}
		for uuid, a := range allocByUUID {
			m := byModel[modelOf[uuid]]
			if m == nil {
				continue // allocation against a device we no longer see
			}
			m.MemoryAllocatedMiB += a.MemoryMiB
			m.ComputeAllocated += a.CorePercent
		}
	}

	for _, m := range byModel {
		out.ByModel = append(out.ByModel, *m)
	}
	sort.Slice(out.ByModel, func(i, j int) bool { return out.ByModel[i].Model < out.ByModel[j].Model })
	out.Homogeneous = len(byModel) <= 1

	return out, nil
}

// Allocations implements Provider.
func (p *HAMiProvider) Allocations(ctx context.Context, node string) (Allocations, error) {
	if p.Mode == ModeDRA {
		return p.draAllocations(ctx, node)
	}
	a, _, err := p.nodeAllocations(ctx, node)
	return a, err
}

// nodeAllocations sums what pods on a node hold, and returns the per-device
// breakdown for model attribution.
func (p *HAMiProvider) nodeAllocations(ctx context.Context, node string) (Allocations, map[string]Allocation, error) {
	pods, err := p.Client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + node,
	})
	if err != nil {
		return Allocations{}, nil, fmt.Errorf("listing pods on node %s: %w", node, err)
	}

	var out Allocations
	byUUID := map[string]Allocation{}

	for i := range pods.Items {
		pod := &pods.Items[i]
		// The field selector is honoured by a real API server and ignored by the
		// fake one; filter again so both behave the same.
		if pod.Spec.NodeName != node {
			continue
		}
		// A terminal pod has released its share. Counting it would block a drain
		// forever — the Phase-1 test pods went Completed while still carrying
		// their allocation annotations.
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}

		allocated, err := ParseAllocations(pod.Annotations[AnnotationDevicesAllocated])
		if err != nil {
			return Allocations{}, nil, fmt.Errorf("pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
		inFlight, err := ParseAllocations(pod.Annotations[AnnotationDevicesToAllocate])
		if err != nil {
			return Allocations{}, nil, fmt.Errorf("pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}

		if len(allocated) == 0 && len(inFlight) == 0 {
			continue
		}
		if len(inFlight) > 0 {
			out.InFlight = true
		}
		out.Consumers++

		for _, a := range append(allocated, inFlight...) {
			out.MemoryMiB += a.MemoryMiB
			out.CorePercent += a.CorePercent

			agg := byUUID[a.UUID]
			agg.UUID = a.UUID
			agg.MemoryMiB += a.MemoryMiB
			agg.CorePercent += a.CorePercent
			byUUID[a.UUID] = agg
		}
	}
	return out, byUUID, nil
}
