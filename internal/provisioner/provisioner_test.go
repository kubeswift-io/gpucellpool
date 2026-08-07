package provisioner

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
)

const validTemplate = `{"guestClassRef":{"name":"gpu-worker"},"imageRef":{"name":"noble"}}`

func testRequest() CellRequest {
	return CellRequest{
		Pool:                "inference",
		Namespace:           "gpu-cells",
		Index:               0,
		CellName:            "inference-0",
		GuestTemplate:       []byte(validTemplate),
		GPU:                 GPUBinding{Backend: cellsv1alpha1.GPUBackendDRA, ResourceClaimTemplateName: "single-vfio-gpu", RequestName: "gpu", Tier: "pcie"},
		BootstrapSecretName: "inference-0-bootstrap",
		Hostname:            "inference-0",
		SpreadPolicy:        "Spread",
		TemplateHash:        "abc1234567",
		OwnerRefs:           []metav1.OwnerReference{{APIVersion: "cells.kubeswift.io/v1alpha1", Kind: "GPUCellPool", Name: "inference", UID: "pool-uid"}},
	}
}

func TestValidateTemplate(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{"valid", validTemplate, ""},
		{"empty", "", "guestClassRef and imageRef are required"},
		{"not json", "{nope", "not a valid SwiftGuestSpec"},
		{"missing imageRef", `{"guestClassRef":{"name":"c"}}`, "imageRef is required"},
		{"missing guestClassRef", `{"imageRef":{"name":"i"}}`, "guestClassRef is required"},
		// Operator-owned: silently overriding these is how a user's intent gets
		// lost, so they are rejected.
		{"owned seedProfileRef", `{"guestClassRef":{"name":"c"},"imageRef":{"name":"i"},"seedProfileRef":{"name":"x"}}`, "set by the operator"},
		{"owned gpuResourceClaim", `{"guestClassRef":{"name":"c"},"imageRef":{"name":"i"},"gpuResourceClaim":{}}`, "set by the operator"},
		{"owned nodeName", `{"guestClassRef":{"name":"c"},"imageRef":{"name":"i"},"nodeName":"boba"}`, "set by the operator"},
		{"owned migration", `{"guestClassRef":{"name":"c"},"imageRef":{"name":"i"},"migration":{"enabled":true}}`, "set by the operator"},
		// Denied: incompatible with a GPU cell.
		{"denied kernelRef", `{"guestClassRef":{"name":"c"},"imageRef":{"name":"i"},"kernelRef":{"name":"k"}}`, "disk boot"},
		{"denied clone", `{"guestClassRef":{"name":"c"},"imageRef":{"name":"i"},"cloneFromSnapshot":{}}`, "VFIO state"},
		{"denied filesystems", `{"guestClassRef":{"name":"c"},"imageRef":{"name":"i"},"filesystems":[]}`, "virtio-fs"},
		{"denied vhost-user", `{"guestClassRef":{"name":"c"},"imageRef":{"name":"i"},"vhostUserDevices":[]}`, "vhost-user"},
		{"denied windows", `{"guestClassRef":{"name":"c"},"imageRef":{"name":"i"},"osType":"windows"}`, "windows"},
		{"linux ok", `{"guestClassRef":{"name":"c"},"imageRef":{"name":"i"},"osType":"linux"}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateTemplate([]byte(c.raw))
			switch {
			case c.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case c.wantErr != "" && err == nil:
				t.Fatalf("want error containing %q, got nil", c.wantErr)
			case c.wantErr != "" && !strings.Contains(err.Error(), c.wantErr):
				t.Fatalf("error %q does not contain %q", err, c.wantErr)
			}
		})
	}
}

func TestValidateGPU(t *testing.T) {
	dra := cellsv1alpha1.GPUBackendDRA
	cases := []struct {
		name    string
		g       GPUBinding
		wantErr string
	}{
		{"dra template", GPUBinding{Backend: dra, ResourceClaimTemplateName: "t"}, ""},
		{"dra claim", GPUBinding{Backend: dra, ResourceClaimName: "c"}, ""},
		// A VFIO device backs exactly one running VM: both refs, or neither, is
		// a configuration that cannot mean anything sensible.
		{"dra both", GPUBinding{Backend: dra, ResourceClaimTemplateName: "t", ResourceClaimName: "c"}, "exactly one"},
		{"dra neither", GPUBinding{Backend: dra}, "exactly one"},
		{"dra hgx tier", GPUBinding{Backend: dra, ResourceClaimTemplateName: "t", Tier: "hgx-shared"}, "only pcie"},
		{"native ok", GPUBinding{Backend: cellsv1alpha1.GPUBackendNative, GPUProfileName: "p"}, ""},
		{"native missing profile", GPUBinding{Backend: cellsv1alpha1.GPUBackendNative}, "gpuProfileRef is required"},
		{"unknown backend", GPUBinding{Backend: "Magic"}, "want"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateGPU(c.g)
			if c.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)) {
				t.Fatalf("want error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

func TestRenderGuestSpecOverlay(t *testing.T) {
	spec, err := renderGuestSpec(testRequest())
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	if got := spec["seedProfileRef"].(map[string]any)["name"]; got != "inference-0-seed" {
		t.Errorf("seedProfileRef = %v, want inference-0-seed", got)
	}
	if spec["runPolicy"] != "Always" {
		t.Errorf("runPolicy = %v, want Always (a cell that exits must come back)", spec["runPolicy"])
	}
	// Cells are replaced, never migrated: an offline migration of a VFIO guest
	// restarts the VM under a live Kubernetes node.
	if enabled := spec["migration"].(map[string]any)["enabled"]; enabled != false {
		t.Errorf("migration.enabled = %v, want false", enabled)
	}
	claim := spec["gpuResourceClaim"].(map[string]any)
	if claim["resourceClaimTemplateName"] != "single-vfio-gpu" || claim["tier"] != "pcie" {
		t.Errorf("gpuResourceClaim = %v", claim)
	}
	if _, set := claim["resourceClaimName"]; set {
		t.Error("both claim references rendered")
	}
	// User fields survive untouched.
	if got := spec["imageRef"].(map[string]any)["name"]; got != "noble" {
		t.Errorf("user imageRef lost: %v", got)
	}
	if _, ok := spec["topologySpreadConstraints"]; !ok {
		t.Error("SpreadPolicy=Spread did not inject a topology spread constraint")
	}
}

func TestRenderGuestSpecRespectsUserTopologyAndPack(t *testing.T) {
	req := testRequest()
	req.GuestTemplate = []byte(`{"guestClassRef":{"name":"c"},"imageRef":{"name":"i"},
		"topologySpreadConstraints":[{"maxSkew":3,"topologyKey":"zone","whenUnsatisfiable":"DoNotSchedule"}]}`)
	spec, err := renderGuestSpec(req)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := spec["topologySpreadConstraints"].([]any)
	if len(got) != 1 || got[0].(map[string]any)["topologyKey"] != "zone" {
		t.Errorf("user topology constraints overridden: %v", got)
	}

	req = testRequest()
	req.SpreadPolicy = "Pack"
	spec, err = renderGuestSpec(req)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if _, ok := spec["topologySpreadConstraints"]; ok {
		t.Error("Pack injected a spread constraint")
	}
}

func TestRenderSeedKeepsTokenOutOfTheCR(t *testing.T) {
	spec := renderSeedSpec(testRequest())
	if spec["userData"] != "" {
		t.Errorf("userData = %q, want empty: the token must live only in the Secret", spec["userData"])
	}
	ref := spec["userDataFrom"].(map[string]any)["secretKeyRef"].(map[string]any)
	if ref["name"] != "inference-0-bootstrap" || ref["key"] != "user-data" {
		t.Errorf("secretKeyRef = %v", ref)
	}
	if !strings.Contains(spec["metaData"].(string), "local-hostname: inference-0") {
		t.Errorf("metaData missing local-hostname: %v", spec["metaData"])
	}
}

func newFakeClient(objs ...client.Object) client.Client {
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(SwiftGuestGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(SwiftGuestGVK.GroupVersion().WithKind("SwiftGuestList"), &unstructured.UnstructuredList{})
	s.AddKnownTypeWithName(SwiftSeedProfileGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(SwiftSeedProfileGVK.GroupVersion().WithKind("SwiftSeedProfileList"), &unstructured.UnstructuredList{})
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func getGuest(t *testing.T, c client.Client, ns, name string) *unstructured.Unstructured {
	t.Helper()
	u := newUnstructured(SwiftGuestGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, u); err != nil {
		t.Fatalf("get guest: %v", err)
	}
	return u
}

func TestEnsureCreatesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient()
	p := NewSwiftGuestProvisioner(c)
	req := testRequest()

	st, err := p.Ensure(ctx, req)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !st.Exists || st.Provisioned {
		t.Errorf("fresh cell: Exists=%v Provisioned=%v, want true/false", st.Exists, st.Provisioned)
	}

	guest := getGuest(t, c, req.Namespace, req.CellName)
	if guest.GetLabels()[cellsv1alpha1.LabelPool] != "inference" {
		t.Errorf("pool label missing: %v", guest.GetLabels())
	}
	if guest.GetAnnotations()[cellsv1alpha1.AnnotationTemplateHash] != "abc1234567" {
		t.Error("template hash annotation missing")
	}
	if len(guest.GetOwnerReferences()) != 1 {
		t.Error("owner reference missing")
	}
	// Without this finalizer a cell can be deleted out from under running HAMi
	// workloads.
	found := false
	for _, f := range guest.GetFinalizers() {
		if f == cellsv1alpha1.FinalizerCellDrain {
			found = true
		}
	}
	if !found {
		t.Errorf("drain finalizer missing: %v", guest.GetFinalizers())
	}

	seed := newUnstructured(SwiftSeedProfileGVK)
	if err := c.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: "inference-0-seed"}, seed); err != nil {
		t.Fatalf("seed profile not created: %v", err)
	}

	// A second Ensure must not error and must not duplicate anything.
	if _, err := p.Ensure(ctx, req); err != nil {
		t.Fatalf("second Ensure: %v", err)
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(SwiftGuestGVK.GroupVersion().WithKind("SwiftGuestList"))
	if err := c.List(ctx, list); err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 1 {
		t.Errorf("got %d guests, want 1", len(list.Items))
	}
}

func TestEnsureRejectsBadTemplateBeforeCreating(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient()
	p := NewSwiftGuestProvisioner(c)
	req := testRequest()
	req.GuestTemplate = []byte(`{"guestClassRef":{"name":"c"},"imageRef":{"name":"i"},"kernelRef":{"name":"k"}}`)

	if _, err := p.Ensure(ctx, req); err == nil {
		t.Fatal("Ensure accepted a denied template")
	}
	u := newUnstructured(SwiftGuestGVK)
	err := c.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.CellName}, u)
	if !apierrors.IsNotFound(err) {
		t.Error("a guest was created despite the template being rejected")
	}
}

func TestObserve(t *testing.T) {
	base := func(status map[string]any) *unstructured.Unstructured {
		u := newUnstructured(SwiftGuestGVK)
		u.SetName("inference-0")
		u.SetUID("uid-1")
		u.Object["status"] = status
		return u
	}

	t.Run("running with primary IP is provisioned", func(t *testing.T) {
		st := observe(base(map[string]any{
			"phase":   "Running",
			"network": map[string]any{"primaryIP": "192.168.99.10"},
			"gpu":     map[string]any{"devices": []any{"0000:01:00.0"}, "nodeName": "boba"},
		}), "")
		if !st.Provisioned || st.Address != "192.168.99.10" || st.HostNode != "boba" {
			t.Errorf("got %+v", st)
		}
		if len(st.GPUDevices) != 1 || st.GPUDevices[0] != "0000:01:00.0" {
			t.Errorf("devices = %v", st.GPUDevices)
		}
		if st.UID != "uid-1" {
			t.Errorf("UID = %q", st.UID)
		}
	})

	t.Run("running without an address is not provisioned", func(t *testing.T) {
		st := observe(base(map[string]any{"phase": "Running"}), "")
		if st.Provisioned {
			t.Error("provisioned with no address: the kubelet would have nothing to register")
		}
	})

	t.Run("nodeIPFrom never falls back to the primary", func(t *testing.T) {
		// Falling back would register the node-local, non-routable nat address —
		// the exact failure cell.nodeIPFrom exists to prevent.
		st := observe(base(map[string]any{
			"phase": "Running",
			"network": map[string]any{
				"primaryIP":  "192.168.99.10",
				"interfaces": []any{map[string]any{"name": "mgmt", "ip": "192.168.99.10"}},
			},
		}), "node")
		if st.Address != "" || st.Provisioned {
			t.Errorf("fell back to the primary address: %+v", st)
		}
	})

	t.Run("nodeIPFrom selects its interface", func(t *testing.T) {
		st := observe(base(map[string]any{
			"phase": "Running",
			"network": map[string]any{
				"primaryIP": "192.168.99.10",
				"interfaces": []any{
					map[string]any{"name": "mgmt", "ip": "192.168.99.10"},
					map[string]any{"name": "node", "ip": "10.77.0.5"},
				},
			},
		}), "node")
		if st.Address != "10.77.0.5" || !st.Provisioned {
			t.Errorf("got %+v", st)
		}
	})

	t.Run("failed carries KubeSwift's own message", func(t *testing.T) {
		st := observe(base(map[string]any{
			"phase": "Failed",
			"conditions": []any{
				map[string]any{"type": "StorageReady", "status": "True"},
				map[string]any{"type": "Resolved", "status": "False", "reason": "ResolutionFailed", "message": "SwiftImage not Ready"},
			},
		}), "")
		if !st.Failed {
			t.Fatal("phase Failed not reported")
		}
		if !strings.Contains(st.Message, "SwiftImage not Ready") {
			t.Errorf("message = %q, want KubeSwift's explanation", st.Message)
		}
	})
}

func TestDeleteOrderAndFinalizerGate(t *testing.T) {
	ctx := context.Background()
	c := newFakeClient()
	p := NewSwiftGuestProvisioner(c)
	req := testRequest()
	if _, err := p.Ensure(ctx, req); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// The drain finalizer holds the guest: deletion cannot complete.
	done, err := p.Delete(ctx, req)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if done {
		t.Fatal("Delete reported done while the drain finalizer still held the guest")
	}
	seed := newUnstructured(SwiftSeedProfileGVK)
	if err := c.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: "inference-0-seed"}, seed); err != nil {
		t.Error("seed profile removed before the guest was gone")
	}

	// Only once the workload side is drained does the reconciler clear it.
	if err := p.ClearDrainFinalizer(ctx, req); err != nil {
		t.Fatalf("ClearDrainFinalizer: %v", err)
	}
	done, err = p.Delete(ctx, req)
	if err != nil {
		t.Fatalf("Delete after finalizer cleared: %v", err)
	}
	if !done {
		t.Fatal("Delete did not complete after the guest was gone")
	}
	err = c.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: "inference-0-seed"}, seed)
	if !apierrors.IsNotFound(err) {
		t.Error("seed profile not removed after the guest was gone")
	}
}
