package v1alpha1

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/yaml"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
)

// TestShippedSamplesPassValidation runs every GPUCellPool in config/samples through
// the real admission rules.
//
// A sample the webhook rejects is worse than no sample: it is the first thing a new
// user applies, and it fails in a way that looks like their mistake. The CRD schema is
// checked by a server dry-run in CI; this covers the half of the contract that lives in
// the webhook and cannot be expressed in OpenAPI.
func TestShippedSamplesPassValidation(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "config", "samples")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read samples dir: %v", err)
	}

	checked := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for i, doc := range strings.Split(string(raw), "\n---") {
			if !strings.Contains(doc, "kind: GPUCellPool") {
				continue
			}
			var pool cellsv1alpha1.GPUCellPool
			if err := yaml.NewYAMLOrJSONDecoder(strings.NewReader(doc), 4096).Decode(&pool); err != nil {
				t.Errorf("%s doc %d: not decodable as a GPUCellPool: %v", e.Name(), i, err)
				continue
			}
			checked++
			if errs := Validate(&pool, nil); len(errs) > 0 {
				t.Errorf("%s (%s) would be REJECTED by our own webhook: %v",
					e.Name(), pool.Name, errs.ToAggregate())
			}
		}
	}
	if checked == 0 {
		t.Fatal("no GPUCellPool samples found — this test would silently pass forever")
	}
	t.Logf("validated %d shipped GPUCellPool sample(s)", checked)
}
