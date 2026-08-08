package provisioner

import (
	"testing"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
)

func capiRequest() CellRequest {
	return CellRequest{
		Pool:      "cells",
		Namespace: "cellpoc",
		Index:     0,
		CellName:  "cells-0",
		GuestTemplate: []byte(`{
			"guestClassRef": {"name": "gpu-worker"},
			"imageRef": {"name": "noble"},
			"interfaces": [
				{"name": "mgmt", "primary": true},
				{"name": "node", "networkRef": {"name": "cellpoc-net"}}
			]
		}`),
		GPU: GPUBinding{
			Backend:                   cellsv1alpha1.GPUBackendDRA,
			ResourceClaimTemplateName: "single-vfio-gpu",
			Tier:                      "pcie",
		},
		ClusterAPI: &cellsv1alpha1.CellClusterAPISpec{ClusterName: "workload", Version: "v1.33.3"},
	}
}

func TestRenderMachineBackendMapsTheExpressibleFields(t *testing.T) {
	backend, err := renderMachineBackend(capiRequest())
	if err != nil {
		t.Fatalf("renderMachineBackend: %v", err)
	}
	if backend["type"] != "SwiftGuest" {
		t.Errorf("type = %v, want SwiftGuest", backend["type"])
	}
	sg, ok := backend["swiftGuest"].(map[string]any)
	if !ok {
		t.Fatalf("swiftGuest = %v", backend["swiftGuest"])
	}
	// Names, not object references: KubeSwiftMachine takes plain strings.
	if sg["imageRef"] != "noble" || sg["guestClassRef"] != "gpu-worker" {
		t.Errorf("refs = %v/%v, want noble/gpu-worker", sg["imageRef"], sg["guestClassRef"])
	}
	// The routable secondary is what the kubelet registers, so it must land on
	// nodeNetworkRef — putting it on networkRef would make it the guest's PRIMARY
	// and cut the control-plane path.
	if sg["nodeNetworkRef"] != "cellpoc-net" {
		t.Errorf("nodeNetworkRef = %v, want cellpoc-net", sg["nodeNetworkRef"])
	}
	if _, set := sg["networkRef"]; set {
		t.Errorf("networkRef set from a primary that names no network: %v", sg["networkRef"])
	}
	gpu, ok := sg["gpu"].(map[string]any)
	if !ok {
		t.Fatalf("gpu = %v", sg["gpu"])
	}
	if gpu["resourceClaimTemplateName"] != "single-vfio-gpu" || gpu["tier"] != "pcie" {
		t.Errorf("gpu = %v", gpu)
	}
}

func TestRenderMachineBackendNativeGPU(t *testing.T) {
	req := capiRequest()
	req.GPU = GPUBinding{Backend: cellsv1alpha1.GPUBackendNative, GPUProfileName: "one-1080", Hugepages: "1Gi"}
	backend, err := renderMachineBackend(req)
	if err != nil {
		t.Fatalf("renderMachineBackend: %v", err)
	}
	gpu := backend["swiftGuest"].(map[string]any)["gpu"].(map[string]any)
	if gpu["gpuProfileRef"] != "one-1080" || gpu["hugepages"] != "1Gi" {
		t.Errorf("gpu = %v", gpu)
	}
	if _, set := gpu["resourceClaimTemplateName"]; set {
		t.Error("a Native cell must not carry a DRA claim reference")
	}
}

func TestRenderMachineBackendRejectsUnparseableTemplate(t *testing.T) {
	req := capiRequest()
	req.GuestTemplate = []byte(`{not json`)
	if _, err := renderMachineBackend(req); err == nil {
		t.Error("accepted an unparseable guestTemplate")
	}
}

func TestPrimaryNetworkGoesToNetworkRef(t *testing.T) {
	req := capiRequest()
	req.GuestTemplate = []byte(`{"interfaces":[
		{"name":"mgmt","primary":true,"networkRef":{"name":"udn"}},
		{"name":"node","networkRef":{"name":"second"}}
	]}`)
	backend, err := renderMachineBackend(req)
	if err != nil {
		t.Fatalf("renderMachineBackend: %v", err)
	}
	sg := backend["swiftGuest"].(map[string]any)
	if sg["networkRef"] != "udn" || sg["nodeNetworkRef"] != "second" {
		t.Errorf("networks = %v/%v, want udn/second", sg["networkRef"], sg["nodeNetworkRef"])
	}
}

func TestParseKubeSwiftProviderID(t *testing.T) {
	for _, tc := range []struct{ in, ns, name string }{
		{"kubeswift://cellpoc/cells-0", "cellpoc", "cells-0"},
		{"kubeswift://cellpoc/", "", ""},
		{"kubeswift:///cells-0", "", ""},
		{"kubeswift://cells-0", "", ""},
		{"docker://x/y", "", ""},
		{"", "", ""},
	} {
		ns, name := parseKubeSwiftProviderID(tc.in)
		if ns != tc.ns || name != tc.name {
			t.Errorf("parse(%q) = %q/%q, want %q/%q", tc.in, ns, name, tc.ns, tc.name)
		}
	}
}

func TestBootstrapConfigGVKStripsTemplate(t *testing.T) {
	gvk := bootstrapConfigGVK(&cellsv1alpha1.ClusterAPIObjectRef{
		APIGroup: "bootstrap.cluster.x-k8s.io", Kind: "KubeadmConfigTemplate", Name: "workers",
	})
	if gvk.Kind != "KubeadmConfig" || gvk.Group != "bootstrap.cluster.x-k8s.io" {
		t.Errorf("gvk = %v, want KubeadmConfig in the bootstrap group", gvk)
	}
}
