package capacity

import (
	"context"
	"strings"
	"testing"

	resourceapi "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
)

// draClassName is Project-HAMi's own DeviceClass, which is also its driver name.
const draClassName = DRADriverName

// draSlice builds what Project-HAMi's DRA driver publishes for one node: one
// device per GPU, capacity in "cores" (percent) and "memory" (BYTES, which is
// why the provider divides), and productName carrying the model.
func draSlice(node string, devices ...resourceapi.Device) *resourceapi.ResourceSlice {
	return &resourceapi.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: node + "-hami"},
		Spec: resourceapi.ResourceSliceSpec{
			Driver:   DRADriverName,
			Pool:     resourceapi.ResourcePool{Name: node},
			NodeName: ptr.To(node),
			Devices:  devices,
		},
	}
}

func draDevice(name, model string, cores, memoryMiB int64) resourceapi.Device {
	return resourceapi.Device{
		Name:                     name,
		AllowMultipleAllocations: ptr.To(true),
		Attributes: map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
			draAttrProductName: {StringValue: ptr.To(model)},
			"type":             {StringValue: ptr.To("hami-gpu")},
		},
		Capacity: map[resourceapi.QualifiedName]resourceapi.DeviceCapacity{
			draCapCores:  {Value: *resource.NewQuantity(cores, resource.DecimalSI)},
			draCapMemory: {Value: *resource.NewQuantity(memoryMiB*mib, resource.BinarySI)},
		},
	}
}

// draGatelessDevice is what the SAME driver publishes when
// DRAConsumableCapacity is off: the gated fields are dropped by the apiserver,
// so the device is there with nothing on it.
func draGatelessDevice(name string) resourceapi.Device {
	return resourceapi.Device{Name: name}
}

// draAllocatedClaim holds a share of one device. reserved=false is a claim the
// scheduler has allocated but nothing has bound yet — still a hold.
func draAllocatedClaim(name, node, device string, cores, memoryMiB int64, reserved bool) *resourceapi.ResourceClaim {
	c := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status: resourceapi.ResourceClaimStatus{
			Allocation: &resourceapi.AllocationResult{
				Devices: resourceapi.DeviceAllocationResult{
					Results: []resourceapi.DeviceRequestAllocationResult{{
						Driver:  DRADriverName,
						Pool:    node,
						Device:  device,
						Request: "gpu",
						ConsumedCapacity: map[resourceapi.QualifiedName]resource.Quantity{
							draCapCores:  *resource.NewQuantity(cores, resource.DecimalSI),
							draCapMemory: *resource.NewQuantity(memoryMiB*mib, resource.BinarySI),
						},
					}},
				},
			},
		},
	}
	if reserved {
		c.Status.ReservedFor = []resourceapi.ResourceClaimConsumerReference{{
			Resource: "pods", Name: "consumer", UID: types.UID("uid-" + name),
		}}
	}
	return c
}

// draClaim is a claim that asks for capacity, allocated or not.
func draClaim(name, class string, count, cores, memoryMiB int64, allocated bool) *resourceapi.ResourceClaim {
	c := &resourceapi.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: resourceapi.ResourceClaimSpec{
			Devices: resourceapi.DeviceClaim{
				Requests: []resourceapi.DeviceRequest{{
					Name: "gpu",
					Exactly: &resourceapi.ExactDeviceRequest{
						DeviceClassName: class,
						AllocationMode:  resourceapi.DeviceAllocationModeExactCount,
						Count:           count,
						Capacity: &resourceapi.CapacityRequirements{
							Requests: map[resourceapi.QualifiedName]resource.Quantity{
								draCapCores:  *resource.NewQuantity(cores, resource.DecimalSI),
								draCapMemory: *resource.NewQuantity(memoryMiB*mib, resource.BinarySI),
							},
						},
					},
				}},
			},
		},
	}
	if allocated {
		c.Status.Allocation = &resourceapi.AllocationResult{}
	}
	return c
}

func draDeviceClass() *resourceapi.DeviceClass {
	return &resourceapi.DeviceClass{ObjectMeta: metav1.ObjectMeta{Name: draClassName}}
}

// Capacity in DRA mode is read from the API rather than reconstructed from an
// annotation, which is the whole point of the mode.
func TestDRACapacityReadsSlicesAndConsumedCapacity(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(
		draDeviceClass(),
		draSlice("cell-0", draDevice("hami-gpu-0", "NVIDIA GeForce GTX 1080", 100, 8192)),
		draSlice("cell-1", draDevice("hami-gpu-0", "NVIDIA GeForce GTX 1080", 100, 8192)),
		// Two consumers share cell-0's device; cell-1 is idle.
		draAllocatedClaim("a", "cell-0", "hami-gpu-0", 30, 3000, true),
		draAllocatedClaim("b", "cell-0", "hami-gpu-0", 20, 1000, true),
		// Another driver's slice on the same node must not be counted.
		&resourceapi.ResourceSlice{
			ObjectMeta: metav1.ObjectMeta{Name: "cell-0-nvidia"},
			Spec: resourceapi.ResourceSliceSpec{
				Driver:  "gpu.nvidia.com",
				Pool:    resourceapi.ResourcePool{Name: "cell-0"},
				Devices: []resourceapi.Device{draDevice("gpu-0", "NVIDIA H100", 100, 81920)},
			},
		},
	)

	c, err := NewHAMiProvider(cs, ModeDRA, "").Capacity(ctx, []string{"cell-0", "cell-1"})
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	if c.Devices != 2 {
		t.Errorf("Devices = %d, want 2 (another driver's slice must not count)", c.Devices)
	}
	if !c.Homogeneous {
		t.Errorf("Homogeneous = false, got models %+v", c.ByModel)
	}
	if c.MemoryTotalMiB != 16384 || c.MemoryAllocatedMiB != 4000 {
		t.Errorf("memory = %d/%d, want 16384/4000", c.MemoryAllocatedMiB, c.MemoryTotalMiB)
	}
	if c.ComputeTotal != 200 || c.ComputeAllocated != 50 {
		t.Errorf("compute = %d/%d, want 50/200", c.ComputeAllocated, c.ComputeTotal)
	}
	if len(c.ByModel) != 1 || c.ByModel[0].Model != "NVIDIA GeForce GTX 1080" {
		t.Errorf("ByModel = %+v", c.ByModel)
	}
}

// Percentages of different models are not commensurable, so a mixed pool
// publishes per-model rows and no pool-wide compute — same rule as DevicePlugin.
func TestDRACapacityWithheldComputeOnAMixedPool(t *testing.T) {
	cs := fake.NewSimpleClientset(
		draDeviceClass(),
		draSlice("cell-0", draDevice("hami-gpu-0", "GTX 1080", 100, 8192)),
		draSlice("cell-1", draDevice("hami-gpu-0", "H100", 100, 81920)),
	)
	c, err := NewHAMiProvider(cs, ModeDRA, "").Capacity(context.Background(), []string{"cell-0", "cell-1"})
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	if c.Homogeneous || c.ComputeTotal != 0 {
		t.Errorf("mixed pool published pool-wide compute: %+v", c)
	}
	if len(c.ByModel) != 2 {
		t.Errorf("ByModel = %+v, want one row per model", c.ByModel)
	}
}

func TestDRADevicesCountsOnlyTheNodesPool(t *testing.T) {
	cs := fake.NewSimpleClientset(
		draDeviceClass(),
		draSlice("cell-0", draDevice("hami-gpu-0", "GTX 1080", 100, 8192)),
		draSlice("cell-1", draDevice("hami-gpu-0", "GTX 1080", 100, 8192),
			draDevice("hami-gpu-1", "GTX 1080", 100, 8192)),
	)
	p := NewHAMiProvider(cs, ModeDRA, "")
	for node, want := range map[string]int{"cell-0": 1, "cell-1": 2, "cell-2": 0} {
		got, err := p.Devices(context.Background(), node)
		if err != nil {
			t.Fatalf("Devices(%s): %v", node, err)
		}
		if got != want {
			t.Errorf("Devices(%s) = %d, want %d", node, got, want)
		}
	}
}

// The drain gate. An allocated claim is a hold whether or not a pod has been
// bound to it yet — treating an unreserved allocation as idle would race the
// scheduler, which is the same reason the DevicePlugin path tracks in-flight
// bindings.
func TestDRAAllocationsGateTheDrain(t *testing.T) {
	ctx := context.Background()
	class, slice := draDeviceClass(), draSlice("cell-0",
		draDevice("hami-gpu-0", "GTX 1080", 100, 8192))

	t.Run("idle", func(t *testing.T) {
		a, err := NewHAMiProvider(fake.NewSimpleClientset(class, slice), ModeDRA, "").
			Allocations(ctx, "cell-0")
		if err != nil {
			t.Fatalf("Allocations: %v", err)
		}
		if !a.Empty() {
			t.Errorf("got %+v, want empty so the cell may be removed", a)
		}
	})

	t.Run("held", func(t *testing.T) {
		cs := fake.NewSimpleClientset(class, slice,
			draAllocatedClaim("a", "cell-0", "hami-gpu-0", 30, 3000, true))
		a, err := NewHAMiProvider(cs, ModeDRA, "").Allocations(ctx, "cell-0")
		if err != nil {
			t.Fatalf("Allocations: %v", err)
		}
		if a.Empty() || a.Consumers != 1 || a.CorePercent != 30 || a.MemoryMiB != 3000 {
			t.Errorf("got %+v, want one consumer holding 30%%/3000MiB", a)
		}
	})

	t.Run("allocated but not yet bound is still a hold", func(t *testing.T) {
		cs := fake.NewSimpleClientset(class, slice,
			draAllocatedClaim("a", "cell-0", "hami-gpu-0", 30, 3000, false))
		a, err := NewHAMiProvider(cs, ModeDRA, "").Allocations(ctx, "cell-0")
		if err != nil {
			t.Fatalf("Allocations: %v", err)
		}
		if a.Empty() || !a.InFlight {
			t.Errorf("got %+v, want InFlight so the drain waits", a)
		}
	})

	t.Run("another node's claim does not hold this cell", func(t *testing.T) {
		cs := fake.NewSimpleClientset(class, slice,
			draAllocatedClaim("a", "cell-9", "hami-gpu-0", 30, 3000, true))
		a, err := NewHAMiProvider(cs, ModeDRA, "").Allocations(ctx, "cell-0")
		if err != nil {
			t.Fatalf("Allocations: %v", err)
		}
		if !a.Empty() {
			t.Errorf("got %+v, want empty", a)
		}
	})
}

// Each preflight failure imitates the next one's symptom, so each must be named
// rather than collapsed into "no capacity".
func TestDRAHealthDistinguishesItsFailures(t *testing.T) {
	ctx := context.Background()
	nodes := []string{"cell-0"}

	t.Run("no DeviceClass", func(t *testing.T) {
		h, err := NewHAMiProvider(fake.NewSimpleClientset(), ModeDRA, "").Health(ctx, nodes)
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
		if h.Ready || h.Reason != cellsv1alpha1.ReasonDRADeviceClassMissing {
			t.Errorf("got %+v", h)
		}
	})

	t.Run("a class named by the pool that does not exist", func(t *testing.T) {
		cs := fake.NewSimpleClientset(draDeviceClass())
		h, err := NewHAMiProvider(cs, ModeDRA, "mine.example.com").Health(ctx, nodes)
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
		if h.Ready || !strings.Contains(h.Message, "mine.example.com") {
			t.Errorf("got %+v, want the configured class named", h)
		}
	})

	t.Run("driver not running on the cells", func(t *testing.T) {
		cs := fake.NewSimpleClientset(draDeviceClass())
		h, err := NewHAMiProvider(cs, ModeDRA, "").Health(ctx, nodes)
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
		if h.Ready || h.Reason != cellsv1alpha1.ReasonHAMiNotDetected {
			t.Errorf("got %+v", h)
		}
	})

	t.Run("devices published with no capacity means the gate is off", func(t *testing.T) {
		cs := fake.NewSimpleClientset(draDeviceClass(),
			draSlice("cell-0", draGatelessDevice("hami-gpu-0")))
		h, err := NewHAMiProvider(cs, ModeDRA, "").Health(ctx, nodes)
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
		if h.Ready || h.Reason != cellsv1alpha1.ReasonDRAFeatureGateMissing {
			t.Errorf("got %+v, want DRAFeatureGateMissing rather than zero capacity", h)
		}
		if !strings.Contains(h.Message, "DRAConsumableCapacity") {
			t.Errorf("message does not name the gate: %q", h.Message)
		}
	})

	t.Run("healthy", func(t *testing.T) {
		cs := fake.NewSimpleClientset(draDeviceClass(),
			draSlice("cell-0", draDevice("hami-gpu-0", "GTX 1080", 100, 8192)))
		h, err := NewHAMiProvider(cs, ModeDRA, "").Health(ctx, nodes)
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
		if !h.Ready || h.Reason != cellsv1alpha1.ReasonHAMiDetected {
			t.Errorf("got %+v", h)
		}
	})

	t.Run("no cells yet is not a provider failure", func(t *testing.T) {
		cs := fake.NewSimpleClientset(draDeviceClass())
		h, err := NewHAMiProvider(cs, ModeDRA, "").Health(ctx, nil)
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
		if !h.Ready {
			t.Errorf("got %+v, want ready: an empty pool has nothing to advertise", h)
		}
	})
}

// A claim asking for every device on a node cannot be judged against one cell's
// shape, so it must never be counted as satisfiable.
func TestDRADemandNeverCallsAnAllDevicesRequestSatisfiable(t *testing.T) {
	c := draClaim("all", draClassName, 1, 30, 4096, false)
	c.Spec.Devices.Requests[0].Exactly.AllocationMode = resourceapi.DeviceAllocationModeAll
	cs := fake.NewSimpleClientset(c)

	d, err := NewHAMiProvider(cs, ModeDRA, "").PendingDemand(context.Background(), refDevice())
	if err != nil {
		t.Fatalf("PendingDemand: %v", err)
	}
	if d.PendingRequests != 1 || d.SatisfiableByOneCell != 0 {
		t.Errorf("demand = %+v, want the request counted but not satisfiable", d)
	}
}

// Guard against the fake clientset quietly accepting a shape the real API would
// not: the provider must read what Project-HAMi actually publishes.
func TestDRAModelFallsBackWhenTheDriverPublishesNoProductName(t *testing.T) {
	d := draDevice("hami-gpu-0", "", 100, 8192)
	delete(d.Attributes, draAttrProductName)
	cs := fake.NewSimpleClientset(draDeviceClass(), draSlice("cell-0", d))

	c, err := NewHAMiProvider(cs, ModeDRA, "").Capacity(context.Background(), []string{"cell-0"})
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	if len(c.ByModel) != 1 || c.ByModel[0].Model != "unknown" {
		t.Errorf("ByModel = %+v, want a placeholder rather than an empty model", c.ByModel)
	}
}
