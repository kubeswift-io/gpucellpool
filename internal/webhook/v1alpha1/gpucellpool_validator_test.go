package v1alpha1

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
)

func validPool() *cellsv1alpha1.GPUCellPool {
	return &cellsv1alpha1.GPUCellPool{
		ObjectMeta: metav1.ObjectMeta{Namespace: "gpu-cells", Name: "inference"},
		Spec: cellsv1alpha1.GPUCellPoolSpec{
			Replicas: 2,
			Cell: cellsv1alpha1.CellSpec{
				GuestTemplate: runtime.RawExtension{Raw: []byte(
					`{"guestClassRef":{"name":"c"},"imageRef":{"name":"i"}}`)},
				GPU: cellsv1alpha1.CellGPUSpec{
					Count:   1,
					Backend: cellsv1alpha1.GPUBackendDRA,
					DRA:     &cellsv1alpha1.CellGPUDRASpec{ResourceClaimTemplateName: "single-vfio-gpu", Tier: "pcie"},
				},
			},
			Bootstrap: cellsv1alpha1.BootstrapSpec{
				JoinSecretRef: &corev1.LocalObjectReference{Name: "join"},
			},
			WorkloadCluster: cellsv1alpha1.WorkloadClusterSpec{
				KubeconfigSecretRef: corev1.LocalObjectReference{Name: "kubeconfig"},
			},
		},
	}
}

func assertValid(t *testing.T, pool *cellsv1alpha1.GPUCellPool) {
	t.Helper()
	if errs := Validate(pool, nil); len(errs) > 0 {
		t.Fatalf("valid pool rejected: %v", errs)
	}
}

func assertRejected(t *testing.T, pool *cellsv1alpha1.GPUCellPool, want string) {
	t.Helper()
	errs := Validate(pool, nil)
	if len(errs) == 0 {
		t.Fatalf("accepted a pool that should be rejected (expected %q)", want)
	}
	if !strings.Contains(errs.ToAggregate().Error(), want) {
		t.Errorf("errors %v do not mention %q", errs, want)
	}
}

func TestValidPoolIsAccepted(t *testing.T) {
	assertValid(t, validPool())
}

func TestGPURules(t *testing.T) {
	t.Run("multi-GPU cells are not supported yet", func(t *testing.T) {
		p := validPool()
		p.Spec.Cell.GPU.Count = 2
		assertRejected(t, p, "only 1 GPU per cell")
	})

	t.Run("hgx tiers need QEMU and a host Fabric Manager", func(t *testing.T) {
		p := validPool()
		p.Spec.Cell.GPU.DRA.Tier = "hgx-shared"
		assertRejected(t, p, "tier")
	})

	t.Run("both claim references is meaningless", func(t *testing.T) {
		p := validPool()
		p.Spec.Cell.GPU.DRA.ResourceClaimName = "shared"
		assertRejected(t, p, "exactly one")
	})

	t.Run("neither claim reference", func(t *testing.T) {
		p := validPool()
		p.Spec.Cell.GPU.DRA = &cellsv1alpha1.CellGPUDRASpec{}
		assertRejected(t, p, "exactly one")
	})

	t.Run("two backends at once", func(t *testing.T) {
		p := validPool()
		p.Spec.Cell.GPU.Native = &cellsv1alpha1.CellGPUNativeSpec{
			GPUProfileRef: corev1.LocalObjectReference{Name: "profile"}}
		assertRejected(t, p, "only one allocation backend")
	})

	t.Run("native needs a profile", func(t *testing.T) {
		p := validPool()
		p.Spec.Cell.GPU.Backend = cellsv1alpha1.GPUBackendNative
		p.Spec.Cell.GPU.DRA = nil
		assertRejected(t, p, "gpuProfileRef")
	})

	t.Run("native with a profile is fine", func(t *testing.T) {
		p := validPool()
		p.Spec.Cell.GPU.Backend = cellsv1alpha1.GPUBackendNative
		p.Spec.Cell.GPU.DRA = nil
		p.Spec.Cell.GPU.Native = &cellsv1alpha1.CellGPUNativeSpec{
			GPUProfileRef: corev1.LocalObjectReference{Name: "profile"}}
		assertValid(t, p)
	})

	t.Run("unknown backend", func(t *testing.T) {
		p := validPool()
		p.Spec.Cell.GPU.Backend = "Magic"
		assertRejected(t, p, "backend")
	})
}

func TestGuestTemplateContract(t *testing.T) {
	cases := map[string]struct{ raw, want string }{
		"operator-owned seedProfileRef": {
			`{"guestClassRef":{"name":"c"},"imageRef":{"name":"i"},"seedProfileRef":{"name":"x"}}`,
			"set by the operator"},
		"operator-owned gpuResourceClaim": {
			`{"guestClassRef":{"name":"c"},"imageRef":{"name":"i"},"gpuResourceClaim":{}}`,
			"set by the operator"},
		"denied kernelRef": {
			`{"guestClassRef":{"name":"c"},"imageRef":{"name":"i"},"kernelRef":{"name":"k"}}`,
			"disk boot"},
		"denied windows": {
			`{"guestClassRef":{"name":"c"},"imageRef":{"name":"i"},"osType":"windows"}`,
			"windows"},
		"missing imageRef": {
			`{"guestClassRef":{"name":"c"}}`, "imageRef is required"},
		"not an object": {`[]`, "not a valid SwiftGuestSpec"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := validPool()
			p.Spec.Cell.GuestTemplate = runtime.RawExtension{Raw: []byte(c.raw)}
			assertRejected(t, p, c.want)
		})
	}
}

func TestNodeIPFromMustNameARealInterface(t *testing.T) {
	// Naming an interface the guest does not have makes the kubelet register no
	// address at all: the cell sits in Booting and times out with nothing to
	// point at.
	p := validPool()
	p.Spec.Cell.GuestTemplate = runtime.RawExtension{Raw: []byte(
		`{"guestClassRef":{"name":"c"},"imageRef":{"name":"i"},
		  "interfaces":[{"name":"mgmt","primary":true},{"name":"node"}]}`)}
	p.Spec.Cell.NodeIPFrom = "node"
	assertValid(t, p)

	p.Spec.Cell.NodeIPFrom = "nodee"
	assertRejected(t, p, "nodee")

	// No interfaces declared at all.
	p.Spec.Cell.GuestTemplate = runtime.RawExtension{Raw: []byte(
		`{"guestClassRef":{"name":"c"},"imageRef":{"name":"i"}}`)}
	p.Spec.Cell.NodeIPFrom = "node"
	assertRejected(t, p, "declares no interfaces")
}

func TestBootstrapAndCredentialRules(t *testing.T) {
	t.Run("Opaque needs a join secret", func(t *testing.T) {
		p := validPool()
		p.Spec.Bootstrap.JoinSecretRef = nil
		assertRejected(t, p, "joinSecretRef")
	})

	t.Run("workload credential is required", func(t *testing.T) {
		p := validPool()
		p.Spec.WorkloadCluster.KubeconfigSecretRef.Name = ""
		assertRejected(t, p, "kubeconfigSecretRef")
	})

	t.Run("HAMi DRA mode needs a DeviceClass", func(t *testing.T) {
		p := validPool()
		p.Spec.Capacity.HAMi = &cellsv1alpha1.HAMiSpec{Mode: cellsv1alpha1.HAMiModeDRA}
		assertRejected(t, p, "deviceClassName")
	})
}

func TestPoolNameMustFitAHostname(t *testing.T) {
	// The pool name becomes each cell's hostname and workload Node name.
	p := validPool()
	p.Name = strings.Repeat("a", 60)
	assertRejected(t, p, "too long")
}

func TestClusterAPIProvisionerIsRejectedForNow(t *testing.T) {
	p := validPool()
	p.Spec.Cell.Provisioner = "ClusterAPI"
	assertRejected(t, p, "ClusterAPI")
}

func TestScaleDownRequiresAReachableWorkloadCluster(t *testing.T) {
	reachable := func(status metav1.ConditionStatus) *cellsv1alpha1.GPUCellPool {
		p := validPool()
		p.Status.Conditions = []metav1.Condition{{
			Type: cellsv1alpha1.ConditionWorkloadClusterReachable, Status: status,
			Reason: "X", LastTransitionTime: metav1.Now(),
		}}
		return p
	}

	// Shrinking destroys cells, and we cannot drain what we cannot see.
	old := reachable(metav1.ConditionFalse)
	newPool := validPool()
	newPool.Spec.Replicas = 1
	if errs := Validate(newPool, old); len(errs) == 0 {
		t.Error("allowed a scale-down while the workload cluster was unreachable")
	}

	// Scaling UP is fine either way: it destroys nothing.
	newPool.Spec.Replicas = 5
	if errs := Validate(newPool, old); len(errs) > 0 {
		t.Errorf("blocked a scale-up: %v", errs)
	}

	// And with a reachable cluster, down is fine.
	newPool.Spec.Replicas = 1
	if errs := Validate(newPool, reachable(metav1.ConditionTrue)); len(errs) > 0 {
		t.Errorf("blocked a legitimate scale-down: %v", errs)
	}

	// A pool that has never reconciled has no condition yet; do not block the
	// user on an observation we never made.
	fresh := validPool()
	if errs := Validate(newPool, fresh); len(errs) > 0 {
		t.Errorf("blocked a scale-down on a pool with no observations: %v", errs)
	}
}

func TestValidatorEntryPoints(t *testing.T) {
	v := &GPUCellPoolValidator{}
	ctx := t.Context()

	if _, err := v.ValidateCreate(ctx, validPool()); err != nil {
		t.Errorf("ValidateCreate rejected a valid pool: %v", err)
	}
	bad := validPool()
	bad.Spec.Cell.GPU.Count = 4
	if _, err := v.ValidateCreate(ctx, bad); err == nil {
		t.Error("ValidateCreate accepted an invalid pool")
	}
	if _, err := v.ValidateUpdate(ctx, validPool(), bad); err == nil {
		t.Error("ValidateUpdate accepted an invalid pool")
	}
	// Deletion is the reconciler's drain gate, not admission's business.
	if _, err := v.ValidateDelete(ctx, validPool()); err != nil {
		t.Errorf("ValidateDelete blocked a deletion: %v", err)
	}
}

// A named ResourceClaim is ONE claim and a VFIO device backs one VM, so a pool that
// can hold more than one cell would double-book the device. V3's justification always
// said this; the rule never checked it.
func TestSharedClaimRequiresASingleCell(t *testing.T) {
	i32 := func(i int32) *int32 { return &i }
	shared := func() *cellsv1alpha1.GPUCellPool {
		p := validPool()
		p.Spec.Replicas = 1
		p.Spec.Cell.GPU.DRA = &cellsv1alpha1.CellGPUDRASpec{
			ResourceClaimName: "one-gpu", Tier: "pcie",
		}
		return p
	}

	assertValid(t, shared())

	p := shared()
	p.Spec.Replicas = 2
	assertRejected(t, p, "double-book")

	// The ceiling counts too: a pool that can GROW past one cell is the same bug,
	// just deferred until demand arrives.
	p = shared()
	p.Spec.Autoscaling = &cellsv1alpha1.AutoscalingSpec{
		Enabled: true, MinReplicas: i32(1), MaxReplicas: i32(3),
	}
	assertRejected(t, p, "double-book")

	// A claim TEMPLATE mints one claim per cell, so it has no such limit.
	p = validPool()
	p.Spec.Replicas = 4
	assertValid(t, p)
}
