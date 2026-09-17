package inventory

import (
	"context"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func slice(node, driver string, devices ...resourceapi.Device) *resourceapi.ResourceSlice {
	return &resourceapi.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: node + "-" + driver},
		Spec: resourceapi.ResourceSliceSpec{
			Driver:  driver,
			Pool:    resourceapi.ResourcePool{Name: node},
			Devices: devices,
		},
	}
}

func device(name string, vfioReady *bool) resourceapi.Device {
	d := resourceapi.Device{Name: name}
	if vfioReady != nil {
		d.Attributes = map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			AttrVFIOReady: {BoolValue: vfioReady},
		}
	}
	return d
}

func allocatedClaim(name, driver, pool, dev string) *resourceapi.ResourceClaim {
	return &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{
						{Driver: driver, Pool: pool, Device: dev, Request: "gpu"},
					},
				},
			},
		},
	}
}

func newClient(objs ...client.Object) client.Client {
	s := runtime.NewScheme()
	_ = resourceapi.AddToScheme(s)
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func boolPtr(b bool) *bool { return &b }

func TestFreeGPUs(t *testing.T) {
	ctx := context.Background()
	yes, no := boolPtr(true), boolPtr(false)

	t.Run("counts ready devices minus allocated", func(t *testing.T) {
		c := newClient(
			slice("boba", KubeSwiftDriver, device("gpu-0000-01-00-0", yes), device("gpu-0000-02-00-0", yes)),
			allocatedClaim("held", KubeSwiftDriver, "boba", "gpu-0000-01-00-0"),
		)
		got, err := FreeGPUs(ctx, c, "")
		if err != nil {
			t.Fatalf("FreeGPUs: %v", err)
		}
		if got.Published != 2 || got.Ready != 2 || got.Allocated != 1 || got.Free != 1 {
			t.Errorf("got %+v, want published/ready/allocated/free = 2/2/1/1", got)
		}
	})

	t.Run("a device on a node without vfio-pci is not free capacity", func(t *testing.T) {
		c := newClient(slice("boba", KubeSwiftDriver, device("gpu-0000-01-00-0", no)))
		got, _ := FreeGPUs(ctx, c, "")
		if got.Ready != 0 || got.Free != 0 {
			t.Errorf("got %+v, want a device that cannot back a VM excluded", got)
		}
		if got.Published != 1 {
			t.Errorf("Published = %d, want the device still counted as published", got.Published)
		}
	})

	t.Run("another vendor's devices are ignored", func(t *testing.T) {
		c := newClient(
			slice("boba", "gpu.nvidia.com", device("gpu-0", yes)),
			slice("boba", KubeSwiftDriver, device("gpu-0000-01-00-0", yes)),
		)
		got, _ := FreeGPUs(ctx, c, "")
		if got.Free != 1 {
			t.Errorf("Free = %d, want only KubeSwift's devices counted", got.Free)
		}
	})

	t.Run("unallocated claims do not hold devices", func(t *testing.T) {
		claim := &resourceapi.ResourceClaim{ObjectMeta: metav1.ObjectMeta{Name: "pending", Namespace: "default"}}
		c := newClient(slice("boba", KubeSwiftDriver, device("gpu-0000-01-00-0", yes)), claim)
		got, _ := FreeGPUs(ctx, c, "")
		if got.Free != 1 {
			t.Errorf("Free = %d, want 1: a claim with no allocation holds nothing", got.Free)
		}
	})

	t.Run("no slices means zero, not an error", func(t *testing.T) {
		got, err := FreeGPUs(ctx, newClient(), "")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if got.Free != 0 {
			t.Errorf("Free = %d", got.Free)
		}
	})

	t.Run("absent vfioReady attribute is treated as usable", func(t *testing.T) {
		// Inventing a fault from a missing attribute would stall a pool on a
		// driver that simply does not publish it.
		c := newClient(slice("boba", KubeSwiftDriver, device("gpu-0000-01-00-0", nil)))
		got, _ := FreeGPUs(ctx, c, "")
		if got.Free != 1 {
			t.Errorf("Free = %d, want 1", got.Free)
		}
	})
}

func gpuNode(name string, vfioReady *bool, allocated ...bool) *unstructured.Unstructured {
	gpus := make([]any, 0, len(allocated))
	for i, a := range allocated {
		gpus = append(gpus, map[string]any{
			"index":      int64(i),
			"pciAddress": "0000:0" + string(rune('1'+i)) + ":00.0",
			"allocated":  a,
		})
	}
	status := map[string]any{"gpus": gpus}
	if vfioReady != nil {
		status["vfioReady"] = *vfioReady
	}
	u := &unstructured.Unstructured{Object: map[string]any{"status": status}}
	u.SetGroupVersionKind(SwiftGPUNodeGVK)
	u.SetName(name)
	return u
}

func nativeClient(objs ...client.Object) client.Client {
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(SwiftGPUNodeGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(SwiftGPUNodeGVK.GroupVersion().WithKind(SwiftGPUNodeGVK.Kind+"List"),
		&unstructured.UnstructuredList{})
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

// The native ledger is the only place a native allocation is recorded, so this
// is the count a Native pool must gate on. Reading DRA instead reported a held
// GPU as free and the pool created a cell that could not get a device.
func TestFreeGPUsNativeCountsTheNativeLedger(t *testing.T) {
	yes := true
	c := nativeClient(
		gpuNode("a", &yes, true, false), // one held, one free
		gpuNode("b", &yes, false),       // one free
	)
	got, err := FreeGPUsNative(context.Background(), c)
	if err != nil {
		t.Fatalf("FreeGPUsNative: %v", err)
	}
	if got.Published != 3 || got.Ready != 3 || got.Allocated != 1 || got.Free != 2 || !got.Known {
		t.Errorf("counts = %+v, want published 3 ready 3 allocated 1 free 2 known", got)
	}
}

func TestFreeGPUsNativeTreatsAWholeNodeWithoutVFIOAsUnusable(t *testing.T) {
	no := false
	got, err := FreeGPUsNative(context.Background(), nativeClient(gpuNode("a", &no, false, false)))
	if err != nil {
		t.Fatalf("FreeGPUsNative: %v", err)
	}
	// Published, so the reading is KNOWN — and known to be zero free, which is
	// the point: a device on a node without vfio-pci cannot back a VM.
	if got.Published != 2 || got.Ready != 0 || got.Free != 0 || !got.Known {
		t.Errorf("counts = %+v, want 2 published, none ready, known", got)
	}
}

func TestFreeGPUsNativeAssumesUsableWhenVFIOIsNotReported(t *testing.T) {
	got, err := FreeGPUsNative(context.Background(), nativeClient(gpuNode("a", nil, false)))
	if err != nil {
		t.Fatalf("FreeGPUsNative: %v", err)
	}
	if got.Free != 1 {
		t.Errorf("free = %d, want 1: an absent field must not invent a fault", got.Free)
	}
}

// No SwiftGPUNodes at all is a configuration signal, not "the cluster is full" —
// the same contract the DRA path has for a driver that publishes nothing.
func TestFreeGPUsNativeWithNoLedgerIsUnknownNotFull(t *testing.T) {
	got, err := FreeGPUsNative(context.Background(), nativeClient())
	if err != nil {
		t.Fatalf("FreeGPUsNative: %v", err)
	}
	if got.Known {
		t.Errorf("counts = %+v, want Known false so the caller treats it as unknown", got)
	}
}
