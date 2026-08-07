package cellid

import (
	"strings"
	"testing"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
)

func TestNameAndParseIndexRoundTrip(t *testing.T) {
	for _, idx := range []int32{0, 1, 7, 42, 99999} {
		name := Name("inference", idx)
		got, err := ParseIndex("inference", name)
		if err != nil {
			t.Fatalf("ParseIndex(%q): %v", name, err)
		}
		if got != idx {
			t.Errorf("round trip: got %d, want %d", got, idx)
		}
	}
}

func TestParseIndexRejectsForeignNames(t *testing.T) {
	// A foreign object carrying our pool label must not be mistaken for a cell.
	cases := []string{
		"other-0",           // different pool
		"inference",         // no index
		"inference-",        // empty index
		"inference-abc",     // non-numeric
		"inference-0-spare", // trailing junk
		"inference-1x",      // partially numeric
	}
	for _, c := range cases {
		if _, err := ParseIndex("inference", c); err == nil {
			t.Errorf("ParseIndex(%q) = nil error, want rejection", c)
		}
	}
}

func TestValidatePoolEnforcesHostnameBudget(t *testing.T) {
	if err := ValidatePool("inference-l40s"); err != nil {
		t.Fatalf("valid pool rejected: %v", err)
	}
	// 63 - 1 ("-") - 5 (index digits) = 57 is the longest usable pool name.
	if err := ValidatePool(strings.Repeat("a", 57)); err != nil {
		t.Errorf("57-char pool rejected: %v", err)
	}
	if err := ValidatePool(strings.Repeat("a", 58)); err == nil {
		t.Error("58-char pool accepted; a cell name would exceed the hostname limit")
	}
	if err := ValidatePool("Not_A_Subdomain"); err == nil {
		t.Error("invalid DNS subdomain accepted")
	}
}

func TestLowestFreeIndexFillsGaps(t *testing.T) {
	cases := []struct {
		used []int32
		want int32
	}{
		{nil, 0},
		{[]int32{0, 1, 2}, 3},
		{[]int32{0, 2, 3}, 1}, // reuse the hole left by a deleted cell
		{[]int32{1, 2}, 0},
		{[]int32{5}, 0},
	}
	for _, c := range cases {
		if got := LowestFreeIndex(c.used); got != c.want {
			t.Errorf("LowestFreeIndex(%v) = %d, want %d", c.used, got, c.want)
		}
	}
}

func TestHighestIndexesIsScaleDownOrder(t *testing.T) {
	got := HighestIndexes([]int32{0, 3, 1, 2}, 2)
	if len(got) != 2 || got[0] != 3 || got[1] != 2 {
		t.Errorf("HighestIndexes = %v, want [3 2]", got)
	}
	if got := HighestIndexes([]int32{0}, 5); len(got) != 1 {
		t.Errorf("count larger than the set: got %v, want 1 element", got)
	}
	if got := HighestIndexes(nil, 3); len(got) != 0 {
		t.Errorf("empty set: got %v, want none", got)
	}
	// Must not reorder the caller's slice.
	used := []int32{0, 3, 1}
	_ = HighestIndexes(used, 3)
	if used[0] != 0 || used[1] != 3 || used[2] != 1 {
		t.Errorf("input slice mutated: %v", used)
	}
}

func TestNodeLabelsIdentityWinsOverUserLabels(t *testing.T) {
	extra := map[string]string{
		"gpu":                       "on", // HAMi's gate, declared by the user
		cellsv1alpha1.LabelCell:     "someone-elses-cell",
		cellsv1alpha1.LabelInstance: "forged",
	}
	got := NodeLabels("inference", 2, "uid-abc", extra)

	if got["gpu"] != "on" {
		t.Error("user label dropped")
	}
	if got[cellsv1alpha1.LabelCell] != "inference-2" {
		t.Errorf("identity label overridden by user input: %q", got[cellsv1alpha1.LabelCell])
	}
	if got[cellsv1alpha1.LabelInstance] != "uid-abc" {
		t.Errorf("instance label overridden by user input: %q", got[cellsv1alpha1.LabelInstance])
	}
	if got[cellsv1alpha1.LabelCellIndex] != "2" {
		t.Errorf("index label = %q, want 2", got[cellsv1alpha1.LabelCellIndex])
	}
}

func TestIsStaleNode(t *testing.T) {
	cases := []struct {
		name    string
		labels  map[string]string
		want    string
		isStale bool
	}{
		{"same instance", map[string]string{cellsv1alpha1.LabelInstance: "uid-1"}, "uid-1", false},
		{"previous incarnation", map[string]string{cellsv1alpha1.LabelInstance: "uid-0"}, "uid-1", true},
		// Not yet labelled: the kubelet may not have applied it. The controller
		// patches it; treating this as stale would delete a healthy Node.
		{"label absent", map[string]string{}, "uid-1", false},
		{"label empty", map[string]string{cellsv1alpha1.LabelInstance: ""}, "uid-1", false},
		// We do not know our own UID yet — never delete on that basis.
		{"want unknown", map[string]string{cellsv1alpha1.LabelInstance: "uid-0"}, "", false},
	}
	for _, c := range cases {
		if got := IsStaleNode(c.labels, c.want); got != c.isStale {
			t.Errorf("%s: IsStaleNode = %v, want %v", c.name, got, c.isStale)
		}
	}
}

func TestTemplateHashIsStableAndUnambiguous(t *testing.T) {
	a := TemplateHash([]byte(`{"imageRef":{"name":"x"}}`))
	if a != TemplateHash([]byte(`{"imageRef":{"name":"x"}}`)) {
		t.Error("hash is not stable across calls")
	}
	if a == TemplateHash([]byte(`{"imageRef":{"name":"y"}}`)) {
		t.Error("different templates hashed equal")
	}
	// Length prefixing must make part boundaries significant.
	if TemplateHash([]byte("ab"), []byte("c")) == TemplateHash([]byte("a"), []byte("bc")) {
		t.Error("concatenation collision: parts are not length-prefixed")
	}
	if len(a) != 10 {
		t.Errorf("hash length = %d, want 10", len(a))
	}
}
