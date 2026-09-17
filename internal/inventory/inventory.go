// Package inventory reads the OUTER cluster's physical GPU inventory.
//
// This is what turns "no free GPU" from a scheduler timeout into a pre-flight
// determination: without it the pool creates guests that sit unschedulable
// forever, and the scheduler never explains that in terms an operator can act on.
package inventory

import (
	"context"
	"fmt"

	resourceapi "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// KubeSwiftDriver is the DRA driver KubeSwift publishes VFIO GPUs under. Devices
// are named gpu-<bdf> and the pool is the node name, so an allocation result
// alone identifies the device.
const KubeSwiftDriver = "gpu.kubeswift.io"

// AttrVFIOReady is the driver attribute that reports whether vfio-pci is loaded
// on the node. A device on a node without it cannot back a VM, so it is not free
// capacity for our purposes.
const AttrVFIOReady = "vfioReady"

// Counts is the outer inventory reading.
type Counts struct {
	// Published is every device the driver advertises.
	Published int
	// Ready is those on nodes where vfio-pci is loaded.
	Ready int
	// Allocated is those held by an allocated ResourceClaim.
	Allocated int
	// Free is Ready minus Allocated, floored at zero.
	Free int
	// Known is false when this driver publishes no devices at all, in which case
	// Free says nothing and must be treated as unknown by the caller.
	Known bool
}

// FreeGPUs counts allocatable VFIO GPUs in the infrastructure cluster.
//
// It returns (nil, err) on failure — never a zero count. The caller treats nil as
// UNKNOWN and proceeds, because refusing to create a cell because we failed to
// read is as wrong as creating one blindly.
//
// Counts.Known is the same idea one level down: a driver that publishes NOTHING
// tells us nothing about capacity. That is a configuration signal (the native
// SwiftGPU backend publishes no ResourceSlices at all, and a cluster may use a
// different driver name), not "the cluster is full" — so it must not stall the
// pool. Only devices we can actually see can be counted as taken.
func FreeGPUs(ctx context.Context, c client.Client, driver string) (*Counts, error) {
	if driver == "" {
		driver = KubeSwiftDriver
	}

	var slices resourceapi.ResourceSliceList
	if err := c.List(ctx, &slices); err != nil {
		return nil, fmt.Errorf("listing ResourceSlices: %w", err)
	}

	// device key: "<pool>/<device>", which is how an allocation result names it.
	ready := map[string]bool{}
	counts := &Counts{}
	for i := range slices.Items {
		s := &slices.Items[i]
		if s.Spec.Driver != driver {
			continue
		}
		for _, d := range s.Spec.Devices {
			counts.Published++
			key := s.Spec.Pool.Name + "/" + d.Name
			vfio := true // absent attribute: assume usable rather than invent a fault
			if attr, ok := d.Attributes[resourceapi.QualifiedName(AttrVFIOReady)]; ok && attr.BoolValue != nil {
				vfio = *attr.BoolValue
			}
			if vfio {
				ready[key] = true
				counts.Ready++
			}
		}
	}

	var claims resourceapi.ResourceClaimList
	if err := c.List(ctx, &claims, &client.ListOptions{Namespace: metav1.NamespaceAll}); err != nil {
		return nil, fmt.Errorf("listing ResourceClaims: %w", err)
	}
	held := map[string]bool{}
	for i := range claims.Items {
		cl := &claims.Items[i]
		if cl.Status.Allocation == nil {
			continue
		}
		for _, res := range cl.Status.Allocation.Devices.Results {
			if res.Driver != driver {
				continue
			}
			key := res.Pool + "/" + res.Device
			if ready[key] && !held[key] {
				held[key] = true
				counts.Allocated++
			}
		}
	}

	counts.Free = counts.Ready - counts.Allocated
	if counts.Free < 0 {
		counts.Free = 0
	}
	counts.Known = counts.Published > 0
	return counts, nil
}

// SwiftGPUNodeGVK is the per-node GPU inventory the native SwiftGPU backend
// allocates from. It is cluster-scoped and owned by KubeSwift's discovery
// DaemonSet.
var SwiftGPUNodeGVK = schema.GroupVersionKind{
	Group:   "gpu.kubeswift.io",
	Version: "v1alpha1",
	Kind:    "SwiftGPUNode",
}

// FreeGPUsNative counts allocatable GPUs from the NATIVE ledger.
//
// The two backends keep separate books for the same physical devices, and a
// pool's spec.cell.gpu.backend decides which one is authoritative for it. A
// native allocation is recorded on SwiftGPUNode.status.gpus[].allocated and
// never appears in a ResourceClaim, so counting DRA claims for a Native pool
// reports held GPUs as free: observed on a single-GPU cluster where the device
// was held by a native allocation while the pool announced "1 free GPU(s)" and
// created a cell per bootstrap timeout, each one a root-disk clone and a boot
// that could never succeed.
//
// Same contract as FreeGPUs: (nil, err) never means "full", and a ledger that
// publishes nothing at all leaves Known false, because a cluster with no GPU
// discovery installed tells us nothing about capacity. A missing CRD is exactly
// that case, not an error — the backend may simply not be deployed.
func FreeGPUsNative(ctx context.Context, c client.Client) (*Counts, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(SwiftGPUNodeGVK.GroupVersion().WithKind(SwiftGPUNodeGVK.Kind + "List"))
	if err := c.List(ctx, list); err != nil {
		if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
			return &Counts{}, nil // no discovery installed: UNKNOWN, not full
		}
		return nil, fmt.Errorf("listing %ss: %w", SwiftGPUNodeGVK.Kind, err)
	}

	counts := &Counts{}
	for i := range list.Items {
		node := &list.Items[i]
		// vfioReady is per node here, not per device (it reports whether the
		// module is loaded). Absent: assume usable rather than invent a fault,
		// exactly as the DRA path treats a missing attribute.
		vfio := true
		if v, found, err := unstructured.NestedBool(node.Object, "status", "vfioReady"); err == nil && found {
			vfio = v
		}
		gpus, found, err := unstructured.NestedSlice(node.Object, "status", "gpus")
		if err != nil || !found {
			continue
		}
		for _, raw := range gpus {
			gpu, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			counts.Published++
			if !vfio {
				continue
			}
			counts.Ready++
			if allocated, found, err := unstructured.NestedBool(gpu, "allocated"); err == nil && found && allocated {
				counts.Allocated++
			}
		}
	}

	counts.Free = counts.Ready - counts.Allocated
	if counts.Free < 0 {
		counts.Free = 0
	}
	counts.Known = counts.Published > 0
	return counts, nil
}
