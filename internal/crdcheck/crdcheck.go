// Package crdcheck compares the CRD schema this binary was built against with the
// one the cluster is actually serving.
//
// It exists because of how Helm treats CRDs: files in a chart's `crds/` directory
// are installed once and NEVER updated by `helm upgrade`. Upgrading a release
// therefore leaves the old schema in place, and the apiserver then silently drops
// every field the old schema does not know — no error, no event, no rejection. The
// operator writes them, the apiserver discards them, and everything reports success.
//
// Measured for real between v0.1.0 and v0.1.1: `spec.updatePolicy` and
// `status.cells[].templateHash` were both new, so on an upgraded release
// `updatePolicy.type: RollingUpdate` would have been accepted and ignored, and
// template drift would have read as up-to-date forever (an empty hash counts as
// current, deliberately — see internal/controller/rollout.go).
//
// The comparison is on the SET of property paths, not on descriptions or defaults:
// dropped fields are exactly what this is about, and anything finer would fail on
// harmless controller-gen churn.
package crdcheck

import (
	"context"
	"embed"
	"fmt"
	"path"
	"sort"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// crdFS holds the CRDs generated from this tree's api/ types, copied in by
// `make manifests`. CI diffs the copy, so it cannot drift from config/crd/bases.
//
//go:embed crd/*.yaml
var crdFS embed.FS

// Getter is the slice of the apiextensions client this package needs. Narrow on
// purpose: the check is read-only and must never be able to modify a CRD.
type Getter interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*apiextensionsv1.CustomResourceDefinition, error)
}

// Result reports one CRD's comparison.
type Result struct {
	// Name is the CRD's name.
	Name string
	// File is the manifest's filename in config/crd/bases. Carried through rather
	// than derived from Name: controller-gen names files <group>_<plural>.yaml while
	// a CRD is named <plural>.<group>, so reconstructing one from the other produced
	// a fix command pointing at a file that does not exist.
	File string
	// Missing are property paths this binary expects and the cluster does not serve,
	// sorted. Non-empty means fields are being silently dropped.
	Missing []string
	// Checked is false when the comparison could not be made — the CRD is absent, or
	// unreadable with the operator's RBAC. Not an error: the operator's job is not
	// conditional on being able to introspect its own CRD.
	Checked bool
	// Err explains a false Checked.
	Err error
}

// Verify compares every embedded CRD with the served one.
func Verify(ctx context.Context, client Getter) ([]Result, error) {
	entries, err := crdFS.ReadDir("crd")
	if err != nil {
		return nil, fmt.Errorf("reading embedded CRDs: %w", err)
	}
	var out []Result
	for _, e := range entries {
		want, err := load(path.Join("crd", e.Name()))
		if err != nil {
			return nil, err
		}
		res := Result{Name: want.Name, File: e.Name()}
		got, err := client.Get(ctx, want.Name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			res.Err = fmt.Errorf("not installed")
		case err != nil:
			res.Err = err
		default:
			res.Checked = true
			res.Missing = missingPaths(want, got)
		}
		out = append(out, res)
	}
	return out, nil
}

// FixCommand is what an operator should run to repair a stale schema. It is the
// whole point of reporting this: Helm will not do it for them.
//
// file is Result.File — the manifest's real name, not something derived from the
// CRD's name.
func FixCommand(file string) string {
	return "kubectl apply -f https://raw.githubusercontent.com/kubeswift-io/gpucellpool/" +
		Version + "/config/crd/bases/" + file
}

// Version is the release whose CRD this binary embeds, stamped at build time
// (-ldflags "-X .../crdcheck.Version=vX.Y.Z"). It defaults to main so an
// unstamped dev build still prints a URL that resolves.
var Version = "main"

func load(name string) (*apiextensionsv1.CustomResourceDefinition, error) {
	raw, err := crdFS.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", name, err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", name, err)
	}
	return &crd, nil
}

// missingPaths returns the property paths present in want and absent from got,
// per served version.
func missingPaths(want, got *apiextensionsv1.CustomResourceDefinition) []string {
	served := map[string]*apiextensionsv1.JSONSchemaProps{}
	for i := range got.Spec.Versions {
		v := &got.Spec.Versions[i]
		if v.Schema != nil {
			served[v.Name] = v.Schema.OpenAPIV3Schema
		}
	}

	var missing []string
	for i := range want.Spec.Versions {
		v := &want.Spec.Versions[i]
		if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
			continue
		}
		schema, ok := served[v.Name]
		if !ok {
			// The whole version is unserved, which is worse than a missing field and
			// worth naming as one line rather than every path beneath it.
			missing = append(missing, v.Name+" (version not served)")
			continue
		}
		missing = append(missing, walk(v.Name, v.Schema.OpenAPIV3Schema, schema)...)
	}
	sort.Strings(missing)
	return missing
}

// walk compares two schemas property by property, descending into objects and
// array items. A property the cluster does not have is recorded and not descended
// into: reporting the parent is enough to act on, and listing its whole subtree
// would bury the signal.
func walk(prefix string, want, got *apiextensionsv1.JSONSchemaProps) []string {
	var missing []string
	for name := range want.Properties {
		wantChild := want.Properties[name]
		gotChild, ok := got.Properties[name]
		if !ok {
			missing = append(missing, prefix+"."+name)
			continue
		}
		missing = append(missing, walk(prefix+"."+name, &wantChild, &gotChild)...)
	}
	if want.Items != nil && want.Items.Schema != nil && got.Items != nil && got.Items.Schema != nil {
		missing = append(missing, walk(prefix+"[]", want.Items.Schema, got.Items.Schema)...)
	}
	return missing
}
