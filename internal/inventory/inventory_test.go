package inventory

import (
	"context"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
