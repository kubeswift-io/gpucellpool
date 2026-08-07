package capacity

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
)

func cellNode(name, registration string, labels map[string]string) *corev1.Node {
	if labels == nil {
		labels = map[string]string{"gpu": "on"}
	}
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	if registration != "" {
		n.Annotations = map[string]string{AnnotationNodeRegistration: registration}
	}
	return n
}

func gpuPod(name, node, allocated, toAllocate string, phase corev1.PodPhase) *corev1.Pod {
	ann := map[string]string{}
	if allocated != "" {
		ann[AnnotationDevicesAllocated] = allocated
	}
	if toAllocate != "" {
		ann[AnnotationDevicesToAllocate] = toAllocate
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Annotations: ann},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Phase: phase},
	}
}

// registrationFor builds a one-device registration for a given model/memory.
func registrationFor(uuid, model string, memMiB int) string {
	return `[{"id":"` + uuid + `","count":10,"devmem":` + itoa(memMiB) +
		`,"devcore":100,"type":"` + model + `","mode":"hami-core","health":true}]`
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func TestDevicesUsesRegistrationNotAllocatable(t *testing.T) {
	node := cellNode("cell-0", measuredRegistration, nil)
	// The trap, reproduced from the real cell: allocatable says 10.
	node.Status.Allocatable = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("10")}

	p := NewHAMiProvider(fake.NewSimpleClientset(node), ModeDevicePlugin)
	got, err := p.Devices(context.Background(), "cell-0")
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	if got != 1 {
		t.Errorf("Devices = %d, want 1 (allocatable nvidia.com/gpu is 10 — slots, not devices)", got)
	}
}

func TestDevicesUnknownNodeIsZeroNotError(t *testing.T) {
	p := NewHAMiProvider(fake.NewSimpleClientset(), ModeDevicePlugin)
	got, err := p.Devices(context.Background(), "not-joined-yet")
	if err != nil || got != 0 {
		t.Errorf("got %d/%v, want 0/nil — a node that has not registered is not an error", got, err)
	}
}

func TestDevicesUnhealthyIsNotCapacity(t *testing.T) {
	reg := `[{"id":"GPU-a","count":10,"devmem":8192,"devcore":100,"type":"m","health":false}]`
	p := NewHAMiProvider(fake.NewSimpleClientset(cellNode("cell-0", reg, nil)), ModeDevicePlugin)
	got, err := p.Devices(context.Background(), "cell-0")
	if err != nil || got != 0 {
		t.Errorf("got %d/%v, want 0/nil for an unhealthy device", got, err)
	}
}

func TestHealth(t *testing.T) {
	ctx := context.Background()

	t.Run("registered and gated", func(t *testing.T) {
		p := NewHAMiProvider(fake.NewSimpleClientset(cellNode("cell-0", measuredRegistration, nil)), ModeDevicePlugin)
		h, err := p.Health(ctx, []string{"cell-0"})
		if err != nil || !h.Ready {
			t.Errorf("got %+v/%v", h, err)
		}
	})

	t.Run("no registration anywhere", func(t *testing.T) {
		p := NewHAMiProvider(fake.NewSimpleClientset(cellNode("cell-0", "", nil)), ModeDevicePlugin)
		h, _ := p.Health(ctx, []string{"cell-0"})
		if h.Ready || h.Reason != cellsv1alpha1.ReasonHAMiNotDetected {
			t.Errorf("got %+v", h)
		}
		if !strings.Contains(h.Message, "tolerate") {
			t.Errorf("message should point at the usual cause (taints/install): %q", h.Message)
		}
	})

	t.Run("registered but gate label missing", func(t *testing.T) {
		// HAMi registered the device, but its scheduler will never place on this
		// node. Reporting Ready here would strand workloads with no explanation.
		p := NewHAMiProvider(fake.NewSimpleClientset(
			cellNode("cell-0", measuredRegistration, map[string]string{"other": "x"})), ModeDevicePlugin)
		h, _ := p.Health(ctx, []string{"cell-0"})
		if h.Ready {
			t.Error("Ready despite the missing gate label")
		}
		if !strings.Contains(h.Message, "spec.workloadCluster.node.labels") {
			t.Errorf("message should tell the operator where to fix it: %q", h.Message)
		}
	})

	t.Run("unparseable registration is loud", func(t *testing.T) {
		p := NewHAMiProvider(fake.NewSimpleClientset(cellNode("cell-0", `[{"id":`, nil)), ModeDevicePlugin)
		h, err := p.Health(ctx, []string{"cell-0"})
		if err != nil {
			t.Fatalf("unexpected error return: %v", err)
		}
		if h.Ready || h.Reason != cellsv1alpha1.ReasonRegistrationUnparseable {
			t.Errorf("got %+v, want not-ready/RegistrationUnparseable", h)
		}
	})

	t.Run("no cells yet is not a provider failure", func(t *testing.T) {
		p := NewHAMiProvider(fake.NewSimpleClientset(), ModeDevicePlugin)
		h, _ := p.Health(ctx, nil)
		if !h.Ready {
			t.Errorf("got %+v, want ready: an empty pool says nothing about HAMi", h)
		}
	})

	t.Run("DRA mode says so instead of reading nothing", func(t *testing.T) {
		p := NewHAMiProvider(fake.NewSimpleClientset(), ModeDRA)
		h, _ := p.Health(ctx, []string{"cell-0"})
		if h.Ready || h.Reason != cellsv1alpha1.ReasonDRAFeatureGateMissing {
			t.Errorf("got %+v", h)
		}
		if _, err := p.Capacity(ctx, []string{"cell-0"}); !errors.Is(err, ErrUnsupported) {
			t.Errorf("Capacity in DRA mode = %v, want ErrUnsupported", err)
		}
	})
}

func TestCapacityAggregatesAndAttributes(t *testing.T) {
	ctx := context.Background()
	// The Phase-1 shape, doubled: two cells, one GPU each, two 3000MiB/30%
	// consumers on the first.
	cs := fake.NewSimpleClientset(
		cellNode("cell-0", registrationFor("GPU-a", "NVIDIA GeForce GTX 1080", 8192), nil),
		cellNode("cell-1", registrationFor("GPU-b", "NVIDIA GeForce GTX 1080", 8192), nil),
		gpuPod("a", "cell-0", "GPU-a,NVIDIA,3000,30:;", ";;", corev1.PodRunning),
		gpuPod("b", "cell-0", "GPU-a,NVIDIA,3000,30:;", ";;", corev1.PodRunning),
	)
	p := NewHAMiProvider(cs, ModeDevicePlugin)

	c, err := p.Capacity(ctx, []string{"cell-0", "cell-1"})
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	if c.Devices != 2 {
		t.Errorf("Devices = %d, want 2", c.Devices)
	}
	if c.MemoryTotalMiB != 16384 || c.MemoryAllocatedMiB != 6000 {
		t.Errorf("memory total/allocated = %d/%d, want 16384/6000", c.MemoryTotalMiB, c.MemoryAllocatedMiB)
	}
	if c.ComputeTotal != 200 || c.ComputeAllocated != 60 {
		t.Errorf("compute total/allocated = %d/%d, want 200/60", c.ComputeTotal, c.ComputeAllocated)
	}
	if !c.Homogeneous {
		t.Error("Homogeneous = false for a single-model pool")
	}
	if len(c.ByModel) != 1 || c.ByModel[0].Devices != 2 || c.ByModel[0].MemoryAllocatedMiB != 6000 {
		t.Errorf("ByModel = %+v", c.ByModel)
	}
}

func TestCapacityHeterogeneousPool(t *testing.T) {
	cs := fake.NewSimpleClientset(
		cellNode("cell-0", registrationFor("GPU-a", "NVIDIA GeForce GTX 1080", 8192), nil),
		cellNode("cell-1", registrationFor("GPU-b", "NVIDIA L40S", 46068), nil),
	)
	p := NewHAMiProvider(cs, ModeDevicePlugin)
	c, err := p.Capacity(context.Background(), []string{"cell-0", "cell-1"})
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	// Memory is commensurable across models; compute percentages are not, and
	// the caller drops the flat compute aggregate when this is false.
	if c.Homogeneous {
		t.Error("Homogeneous = true for a mixed-model pool")
	}
	if len(c.ByModel) != 2 {
		t.Fatalf("ByModel = %+v, want two models", c.ByModel)
	}
	if c.ByModel[0].Model != "NVIDIA GeForce GTX 1080" || c.ByModel[1].Model != "NVIDIA L40S" {
		t.Errorf("ByModel not sorted by model: %+v", c.ByModel)
	}
	if c.MemoryTotalMiB != 8192+46068 {
		t.Errorf("memory total = %d", c.MemoryTotalMiB)
	}
}

func TestAllocationsGateDrain(t *testing.T) {
	ctx := context.Background()

	t.Run("terminal pods have released their share", func(t *testing.T) {
		// Measured on the Phase-1 cell: a Completed pod still carries its
		// allocation annotations. Counting it would block a drain forever.
		cs := fake.NewSimpleClientset(
			cellNode("cell-0", measuredRegistration, nil),
			gpuPod("done", "cell-0", measuredAllocated, ";;", corev1.PodSucceeded),
			gpuPod("crashed", "cell-0", measuredAllocated, ";;", corev1.PodFailed),
		)
		a, err := NewHAMiProvider(cs, ModeDevicePlugin).Allocations(ctx, "cell-0")
		if err != nil {
			t.Fatalf("Allocations: %v", err)
		}
		if !a.Empty() {
			t.Errorf("got %+v, want empty: terminal pods hold nothing", a)
		}
	})

	t.Run("running pods hold the GPU", func(t *testing.T) {
		cs := fake.NewSimpleClientset(
			cellNode("cell-0", measuredRegistration, nil),
			gpuPod("live", "cell-0", measuredAllocated, ";;", corev1.PodRunning),
		)
		a, _ := NewHAMiProvider(cs, ModeDevicePlugin).Allocations(ctx, "cell-0")
		if a.Empty() || a.Consumers != 1 || a.MemoryMiB != 3000 || a.CorePercent != 30 {
			t.Errorf("got %+v", a)
		}
	})

	t.Run("in-flight binding blocks a drain", func(t *testing.T) {
		// The binding window IS the race: a drain that ignored it would delete a
		// cell the scheduler is mid-way through placing work on.
		cs := fake.NewSimpleClientset(
			cellNode("cell-0", measuredRegistration, nil),
			gpuPod("binding", "cell-0", "", "GPU-e71afe85-9309-864a-477e-91caa89f3932,NVIDIA,3000,30:;", corev1.PodPending),
		)
		a, _ := NewHAMiProvider(cs, ModeDevicePlugin).Allocations(ctx, "cell-0")
		if a.Empty() || !a.InFlight {
			t.Errorf("got %+v, want in-flight and non-empty", a)
		}
	})

	t.Run("pods on other nodes are not counted", func(t *testing.T) {
		cs := fake.NewSimpleClientset(
			cellNode("cell-0", measuredRegistration, nil),
			gpuPod("elsewhere", "cell-9", measuredAllocated, ";;", corev1.PodRunning),
		)
		a, _ := NewHAMiProvider(cs, ModeDevicePlugin).Allocations(ctx, "cell-0")
		if !a.Empty() {
			t.Errorf("got %+v, want empty", a)
		}
	})

	t.Run("non-GPU pods are not consumers", func(t *testing.T) {
		cs := fake.NewSimpleClientset(
			cellNode("cell-0", measuredRegistration, nil),
			gpuPod("plain", "cell-0", "", "", corev1.PodRunning),
		)
		a, _ := NewHAMiProvider(cs, ModeDevicePlugin).Allocations(ctx, "cell-0")
		if !a.Empty() {
			t.Errorf("got %+v, want empty", a)
		}
	})

	t.Run("malformed pod annotation is an error, not zero", func(t *testing.T) {
		cs := fake.NewSimpleClientset(
			cellNode("cell-0", measuredRegistration, nil),
			gpuPod("bad", "cell-0", "GPU-a,NVIDIA,notanumber,30:;", "", corev1.PodRunning),
		)
		if _, err := NewHAMiProvider(cs, ModeDevicePlugin).Allocations(ctx, "cell-0"); err == nil {
			t.Error("accepted a malformed allocation; a drain would proceed on false confidence")
		}
	})
}

func TestFakeProviderSatisfiesTheInterface(t *testing.T) {
	f := NewFakeProvider()
	f.DevicesByNode["cell-0"] = 1
	if n, _ := f.Devices(context.Background(), "cell-0"); n != 1 {
		t.Error("fake Devices")
	}
	if len(f.Calls) != 1 || f.Calls[0] != "Devices" {
		t.Errorf("Calls = %v", f.Calls)
	}
}
