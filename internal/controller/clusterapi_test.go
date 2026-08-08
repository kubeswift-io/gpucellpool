package controller

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
	"github.com/kubeswift-io/gpucellpool/internal/provisioner"
)

// capiFixture is a pool whose cells are Cluster API Machines. The guestTemplate is
// deliberately limited to the fields a KubeSwiftMachine can express — anything else
// is rejected at admission, so a harness pool that carried more would not be a real
// one.
func capiFixture(t *testing.T, mutate func(*cellsv1alpha1.GPUCellPool)) *testFixture {
	return newFixture(t, func(p *cellsv1alpha1.GPUCellPool) {
		p.Spec.Cell.Provisioner = provisioner.ProvisionerClusterAPI
		p.Spec.Cell.ClusterAPI = &cellsv1alpha1.CellClusterAPISpec{
			ClusterName: "workload",
			Version:     "v1.33.3",
		}
		p.Spec.Cell.GuestTemplate = runtime.RawExtension{Raw: []byte(
			`{"guestClassRef":{"name":"gpu-worker"},"imageRef":{"name":"noble"},` +
				`"interfaces":[{"name":"mgmt","primary":true},{"name":"node","networkRef":{"name":"cellpoc-net"}}]}`)}
		if mutate != nil {
			mutate(p)
		}
	})
}

func (f *testFixture) get(gvk schema.GroupVersionKind, name string) *unstructured.Unstructured {
	f.t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	if err := outerClient.Get(context.Background(),
		types.NamespacedName{Namespace: f.ns, Name: name}, obj); err != nil {
		f.t.Fatalf("get %s %s: %v", gvk.Kind, name, err)
	}
	return obj
}

func (f *testFixture) exists(gvk schema.GroupVersionKind, name string) bool {
	f.t.Helper()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	err := outerClient.Get(context.Background(),
		types.NamespacedName{Namespace: f.ns, Name: name}, obj)
	return err == nil
}

func TestClusterAPICellCreatesMachineAndInfra(t *testing.T) {
	f := capiFixture(t, nil)
	f.reconcile()

	// The infrastructure object carries the cell's shape.
	infra := f.get(provisioner.KubeSwiftMachineGVK, "cells-0")
	backend, _, _ := unstructured.NestedMap(infra.Object, "spec", "backend")
	if backend["type"] != "SwiftGuest" {
		t.Fatalf("backend = %v", backend)
	}
	sg, _ := backend["swiftGuest"].(map[string]any)
	if sg["imageRef"] != "noble" || sg["guestClassRef"] != "gpu-worker" ||
		sg["nodeNetworkRef"] != "cellpoc-net" {
		t.Errorf("swiftGuest = %v", sg)
	}
	if gpu, _ := sg["gpu"].(map[string]any); gpu == nil ||
		gpu["resourceClaimTemplateName"] != "single-vfio-gpu" {
		t.Errorf("gpu = %v: the cell would boot with no GPU", sg["gpu"])
	}

	// The Machine ties it to the cluster.
	machine := f.get(provisioner.MachineGVK, "cells-0")
	spec, _, _ := unstructured.NestedMap(machine.Object, "spec")
	if spec["clusterName"] != "workload" || spec["version"] != "v1.33.3" {
		t.Errorf("spec = %v", spec)
	}
	ref, _ := spec["infrastructureRef"].(map[string]any)
	if ref["kind"] != "KubeSwiftMachine" || ref["name"] != "cells-0" ||
		ref["apiGroup"] != "infrastructure.cluster.x-k8s.io" {
		t.Errorf("infrastructureRef = %v", ref)
	}
	// No bootstrap template configured: the pool's own rendered Secret is handed
	// over directly, which is why that Secret carries the "value" key too.
	boot, _ := spec["bootstrap"].(map[string]any)
	if boot["dataSecretName"] != "cells-0-bootstrap" {
		t.Errorf("bootstrap = %v", boot)
	}

	// Cluster API only sees a Machine as part of a Cluster through this label.
	if machine.GetLabels()["cluster.x-k8s.io/cluster-name"] != "workload" {
		t.Errorf("labels = %v, want the CAPI cluster label", machine.GetLabels())
	}
	// The drain finalizer must be on the Machine: it is what stops the cell being
	// removed while HAMi workloads hold its GPU.
	if len(machine.GetFinalizers()) == 0 ||
		machine.GetFinalizers()[0] != cellsv1alpha1.FinalizerCellDrain {
		t.Errorf("finalizers = %v", machine.GetFinalizers())
	}
	if len(machine.GetOwnerReferences()) != 1 ||
		machine.GetOwnerReferences()[0].Kind != "GPUCellPool" {
		t.Errorf("ownerRefs = %v", machine.GetOwnerReferences())
	}

	// And the pool has not created a SwiftGuest of its own: under this provisioner
	// the VM is capi-kubeswift's to make.
	if f.exists(provisioner.SwiftGuestGVK, "cells-0") {
		t.Error("the ClusterAPI provisioner created a SwiftGuest directly")
	}
}

func TestClusterAPICellInstantiatesTheBootstrapTemplate(t *testing.T) {
	f := capiFixture(t, func(p *cellsv1alpha1.GPUCellPool) {
		p.Spec.Cell.ClusterAPI.BootstrapConfigTemplateRef = &cellsv1alpha1.ClusterAPIObjectRef{
			APIGroup: "bootstrap.cluster.x-k8s.io",
			Kind:     "KubeadmConfigTemplate",
			Name:     "gpu-workers",
		}
	})

	// The template a CAPI user already has.
	tmpl := &unstructured.Unstructured{}
	tmpl.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "bootstrap.cluster.x-k8s.io", Version: "v1beta2", Kind: "KubeadmConfigTemplate"})
	tmpl.SetNamespace(f.ns)
	tmpl.SetName("gpu-workers")
	tmpl.Object["spec"] = map[string]any{"template": map[string]any{"spec": map[string]any{
		"joinConfiguration": map[string]any{"nodeRegistration": map[string]any{
			"kubeletExtraArgs": map[string]any{"node-labels": "gpu=on"},
		}},
	}}}
	if err := outerClient.Create(context.Background(), tmpl); err != nil {
		t.Fatalf("create template: %v", err)
	}

	f.reconcile()

	cfgGVK := schema.GroupVersionKind{
		Group: "bootstrap.cluster.x-k8s.io", Version: "v1beta2", Kind: "KubeadmConfig"}
	cfg := f.get(cfgGVK, "cells-0")
	// spec.template.spec of the template becomes spec of the per-cell object, which
	// is how a MachineSet does it.
	if _, ok, _ := unstructured.NestedMap(cfg.Object, "spec", "joinConfiguration"); !ok {
		t.Errorf("instantiated config lost the template spec: %v", cfg.Object["spec"])
	}

	machine := f.get(provisioner.MachineGVK, "cells-0")
	boot, _, _ := unstructured.NestedMap(machine.Object, "spec", "bootstrap")
	ref, _ := boot["configRef"].(map[string]any)
	if ref == nil || ref["kind"] != "KubeadmConfig" || ref["name"] != "cells-0" {
		t.Errorf("bootstrap = %v, want a configRef to the per-cell config", boot)
	}
	if _, set := boot["dataSecretName"]; set {
		t.Error("both configRef and dataSecretName set: the bootstrap provider would be bypassed")
	}
}

// TestClusterAPICellReachesReady walks the full cross-cluster path with Cluster API
// in the middle: the Machine and infra object are ours, the VM and its providerID
// are capi-kubeswift's, and the GPU is only visible by following that providerID to
// the backing SwiftGuest.
func TestClusterAPICellReachesReady(t *testing.T) {
	f := capiFixture(t, nil)
	f.reconcile()

	// Nothing provisioned yet: no GPU is known, so the cell waits.
	if p := f.getPool(); len(p.Status.Cells) != 1 ||
		p.Status.Cells[0].Phase != cellsv1alpha1.CellPhaseAllocatingGPU {
		t.Fatalf("cells = %+v, want one AllocatingGPU", f.getPool().Status.Cells)
	}

	// What capi-kubeswift does: create the SwiftGuest, then publish the providerID.
	f.createBackingGuest("cells-0")
	f.stampGuestRunning("cells-0", "10.77.0.21", "0000:01:00.0", "boba")
	f.setProviderID("cells-0", "kubeswift://"+f.ns+"/cells-0")
	f.setMachinePhase("cells-0", "Provisioned")

	f.reconcile()
	cell := f.getPool().Status.Cells[0]
	if len(cell.Devices) != 1 || cell.Devices[0] != "0000:01:00.0" {
		t.Fatalf("devices = %v: the GPU was not found through the providerID", cell.Devices)
	}
	if cell.Phase != cellsv1alpha1.CellPhaseJoining {
		t.Errorf("phase = %s, want Joining", cell.Phase)
	}

	// The node registers and HAMi advertises: both layers now agree.
	f.joinNode("cells-0", "", true)
	f.reconcile()
	if got := f.getPool().Status.Cells[0].Phase; got != cellsv1alpha1.CellPhaseReady {
		t.Errorf("phase = %s, want Ready", got)
	}
}

func TestClusterAPICellIsRemovedByDeletingTheMachine(t *testing.T) {
	f := capiFixture(t, nil)
	f.reconcile()
	if !f.exists(provisioner.MachineGVK, "cells-0") {
		t.Fatal("setup: no Machine")
	}

	f.patchPool(func(p *cellsv1alpha1.GPUCellPool) { p.Spec.Replicas = 0 })
	for i := 0; i < 6; i++ {
		f.reconcile()
		if !f.exists(provisioner.MachineGVK, "cells-0") {
			break
		}
	}
	if f.exists(provisioner.MachineGVK, "cells-0") {
		t.Fatal("Machine survived the scale-down, so the drain finalizer was never cleared")
	}
	// Cluster API deletes the infra object it adopted; with no CAPI controller in
	// the harness the provisioner must remove it itself, or the cell's GPU claim
	// would leak.
	if f.exists(provisioner.KubeSwiftMachineGVK, "cells-0") {
		t.Error("KubeSwiftMachine left behind")
	}
}

// createBackingGuest stands in for capi-kubeswift: the SwiftGuest is created by the
// provider, not by this operator.
func (f *testFixture) createBackingGuest(name string) {
	f.t.Helper()
	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(provisioner.SwiftGuestGVK)
	g.SetNamespace(f.ns)
	g.SetName(name)
	g.Object["spec"] = map[string]any{"guestClassRef": map[string]any{"name": "gpu-worker"}}
	if err := outerClient.Create(context.Background(), g); err != nil {
		f.t.Fatalf("create backing guest: %v", err)
	}
}

func (f *testFixture) setProviderID(cell, id string) {
	f.t.Helper()
	infra := f.get(provisioner.KubeSwiftMachineGVK, cell)
	spec, _, _ := unstructured.NestedMap(infra.Object, "spec")
	spec["providerID"] = id
	infra.Object["spec"] = spec
	if err := outerClient.Update(context.Background(), infra); err != nil {
		f.t.Fatalf("set providerID: %v", err)
	}
}

func (f *testFixture) setMachinePhase(cell, phase string) {
	f.t.Helper()
	machine := f.get(provisioner.MachineGVK, cell)
	machine.Object["status"] = map[string]any{"phase": phase}
	if err := outerClient.Status().Update(context.Background(), machine); err != nil {
		f.t.Fatalf("set machine phase: %v", err)
	}
}

// TestProvisionerSelection covers the reconciler's own defence. The CRD enum keeps
// a bad value out at admission, so this branch is only reachable with the webhook
// disabled — which is exactly when a silent fallback to SwiftGuest would create the
// wrong objects in the infrastructure cluster without anyone being told.
func TestProvisionerSelection(t *testing.T) {
	r := &GPUCellPoolReconciler{}
	for _, tc := range []struct {
		name, spec, want string
		wantErr          bool
	}{
		{name: "default", spec: "", want: provisioner.ProvisionerSwiftGuest},
		{name: "explicit SwiftGuest", spec: provisioner.ProvisionerSwiftGuest, want: provisioner.ProvisionerSwiftGuest},
		{name: "ClusterAPI", spec: provisioner.ProvisionerClusterAPI, want: provisioner.ProvisionerClusterAPI},
		{name: "unknown", spec: "Terraform", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := &cellsv1alpha1.GPUCellPool{}
			pool.Spec.Cell.Provisioner = tc.spec
			got, err := r.provisioner(pool)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("accepted provisioner %q as %s", tc.spec, got.Name())
				}
				return
			}
			if err != nil {
				t.Fatalf("provisioner(%q): %v", tc.spec, err)
			}
			if got.Name() != tc.want {
				t.Errorf("provisioner(%q) = %s, want %s", tc.spec, got.Name(), tc.want)
			}
		})
	}
}
