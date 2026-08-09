package crdcheck

import (
	"context"
	"errors"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type stubGetter struct {
	crd *apiextensionsv1.CustomResourceDefinition
	err error
}

func (s stubGetter) Get(context.Context, string, metav1.GetOptions) (*apiextensionsv1.CustomResourceDefinition, error) {
	return s.crd, s.err
}

// embedded returns the CRD this binary was built against, which is also the
// baseline every case below mutates.
func embedded(t *testing.T) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	entries, err := crdFS.ReadDir("crd")
	if err != nil || len(entries) == 0 {
		t.Fatalf("no CRD embedded (did `make manifests` run?): %v", err)
	}
	crd, err := load("crd/" + entries[0].Name())
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return crd
}

// TestTheEmbeddedCRDIsUsable guards the copy itself: an empty or unparseable embed
// would make every other check here vacuously pass.
func TestTheEmbeddedCRDIsUsable(t *testing.T) {
	crd := embedded(t)
	if crd.Name != "gpucellpools.cells.kubeswift.io" {
		t.Errorf("embedded CRD name = %q", crd.Name)
	}
	if len(crd.Spec.Versions) == 0 || crd.Spec.Versions[0].Schema == nil {
		t.Fatal("embedded CRD carries no schema")
	}
	// The two fields whose absence made this package necessary.
	props := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties
	if _, ok := props["spec"].Properties["updatePolicy"]; !ok {
		t.Error("spec.updatePolicy missing from the embedded CRD")
	}
}

func TestAServedSchemaEqualToTheBuildIsClean(t *testing.T) {
	res, err := Verify(context.Background(), stubGetter{crd: embedded(t)})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(res) != 1 || !res[0].Checked || len(res[0].Missing) != 0 {
		t.Errorf("got %+v, want one clean result", res)
	}
}

// TestADroppedFieldIsNamed is the v0.1.0 → v0.1.1 case: the served CRD predates
// spec.updatePolicy, so the apiserver would accept `RollingUpdate` and discard it.
func TestADroppedFieldIsNamed(t *testing.T) {
	old := embedded(t).DeepCopy()
	spec := old.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	delete(spec.Properties, "updatePolicy")
	old.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"] = spec

	res, err := Verify(context.Background(), stubGetter{crd: old})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !res[0].Checked {
		t.Fatal("comparison not made")
	}
	if len(res[0].Missing) != 1 || !strings.HasSuffix(res[0].Missing[0], ".spec.updatePolicy") {
		t.Errorf("Missing = %v, want exactly the dropped field", res[0].Missing)
	}
}

// TestADroppedFieldInsideAnArrayIsNamed covers status.cells[].templateHash — the
// other field the first upgrade would have dropped. It lives under array items, so
// a walk that did not descend into them would have reported nothing.
func TestADroppedFieldInsideAnArrayIsNamed(t *testing.T) {
	old := embedded(t).DeepCopy()
	root := old.Spec.Versions[0].Schema.OpenAPIV3Schema
	status := root.Properties["status"]
	cells := status.Properties["cells"]
	item := cells.Items.Schema
	if _, ok := item.Properties["templateHash"]; !ok {
		t.Skip("status.cells[].templateHash is no longer in the schema")
	}
	delete(item.Properties, "templateHash")

	res, err := Verify(context.Background(), stubGetter{crd: old})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(res[0].Missing) != 1 || !strings.Contains(res[0].Missing[0], "templateHash") {
		t.Errorf("Missing = %v, want the field under cells[]", res[0].Missing)
	}
}

// TestAMissingParentIsReportedOnce: reporting a dropped object's whole subtree would
// bury the one line an operator needs to act on.
func TestAMissingParentIsReportedOnce(t *testing.T) {
	old := embedded(t).DeepCopy()
	root := old.Spec.Versions[0].Schema.OpenAPIV3Schema
	delete(root.Properties, "status")

	res, err := Verify(context.Background(), stubGetter{crd: old})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(res[0].Missing) != 1 || !strings.HasSuffix(res[0].Missing[0], ".status") {
		t.Errorf("Missing = %v, want just the parent", res[0].Missing)
	}
}

func TestAnUnservedVersionIsReported(t *testing.T) {
	old := embedded(t).DeepCopy()
	old.Spec.Versions[0].Name = "v1beta9"

	res, err := Verify(context.Background(), stubGetter{crd: old})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(res[0].Missing) != 1 || !strings.Contains(res[0].Missing[0], "not served") {
		t.Errorf("Missing = %v, want the unserved version named", res[0].Missing)
	}
}

// TestAnUnreadableCRDIsNotAVerdict: the operator's work is not conditional on being
// able to introspect its own CRD, so a Forbidden or a missing CRD must report
// "not checked" rather than "everything is missing".
func TestAnUnreadableCRDIsNotAVerdict(t *testing.T) {
	cases := map[string]error{
		"forbidden": apierrors.NewForbidden(
			schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"},
			"gpucellpools.cells.kubeswift.io", errors.New("no rbac")),
		"absent": apierrors.NewNotFound(
			schema.GroupResource{Resource: "customresourcedefinitions"}, "gpucellpools.cells.kubeswift.io"),
	}
	for name, err := range cases {
		t.Run(name, func(t *testing.T) {
			res, vErr := Verify(context.Background(), stubGetter{err: err})
			if vErr != nil {
				t.Fatalf("Verify returned an error instead of a result: %v", vErr)
			}
			if res[0].Checked {
				t.Error("claimed to have compared a CRD it could not read")
			}
			if len(res[0].Missing) != 0 {
				t.Errorf("Missing = %v, want none: unknown is not missing", res[0].Missing)
			}
			if res[0].Err == nil {
				t.Error("no reason recorded")
			}
		})
	}
}

// TestFixCommandPointsAtAFileThatExists: the first version derived the filename
// from the CRD's name, and controller-gen does not name files that way — the
// printed command 404'd.
func TestFixCommandPointsAtAFileThatExists(t *testing.T) {
	res, err := Verify(context.Background(), stubGetter{crd: embedded(t)})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if _, err := crdFS.ReadFile("crd/" + res[0].File); err != nil {
		t.Fatalf("Result.File %q is not the embedded manifest: %v", res[0].File, err)
	}
	if got := FixCommand(res[0].File); !strings.HasSuffix(got, "/config/crd/bases/"+res[0].File) {
		t.Errorf("FixCommand = %q, want it to end in the real manifest path", got)
	}
}
