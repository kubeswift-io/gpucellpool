// Package cellid owns cell naming, index allocation and cross-cluster identity.
//
// The identity model (docs/design/gpucellpool-reconciliation.md §3) is
// name-derived on purpose, so it survives an operator restart with no bookkeeping:
//
//	index          0..N-1, lowest free index wins; highest index drains first
//	cell name      <pool>-<index>
//	SwiftGuest     <cell>            seed: <cell>-seed   bootstrap: <cell>-bootstrap
//	guest hostname <cell>
//	inner Node     <cell>            (the kubelet registers under the hostname)
//
// Correlation is Node.name == cell name, VERIFIED by identity labels. The
// instance label carries the outer guest UID so a Node left behind by a replaced
// cell is detected as stale rather than adopted.
package cellid

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
)

// maxIndexDigits bounds the index contribution to a cell name. A pool with more
// than 99999 cells is not a thing we support, and the bound is what makes the
// hostname-length check below decidable.
const maxIndexDigits = 5

// hostnameMaxLen is the DNS label limit. A cell name becomes the guest hostname
// AND the workload Node name, so it must fit in one label — not the 253-character
// subdomain limit that applies to Kubernetes object names generally. This is the
// tighter constraint and the one that actually bites.
const hostnameMaxLen = validation.DNS1035LabelMaxLength

// Name returns the cell name for a pool and index.
func Name(pool string, index int32) string {
	return fmt.Sprintf("%s-%d", pool, index)
}

// SeedName returns the SwiftSeedProfile name for a cell.
func SeedName(cell string) string { return cell + "-seed" }

// BootstrapSecretName returns the per-cell rendered bootstrap Secret name. The
// join credential lives here, never in a custom resource.
func BootstrapSecretName(cell string) string { return cell + "-bootstrap" }

// ValidatePool reports whether every cell name this pool can produce is a usable
// hostname and Node name. Checked at admission so the failure is a rejection at
// pool-creation time rather than a cell that boots and cannot register.
func ValidatePool(pool string) error {
	if errs := validation.IsDNS1123Subdomain(pool); len(errs) > 0 {
		return fmt.Errorf("pool name %q is not a valid DNS subdomain: %s", pool, strings.Join(errs, "; "))
	}
	// Longest possible cell name: pool + "-" + maxIndexDigits.
	if got := len(pool) + 1 + maxIndexDigits; got > hostnameMaxLen {
		return fmt.Errorf("pool name %q is too long: a cell name may be up to %d characters "+
			"(pool + %q + index) but the hostname limit is %d — shorten the pool name to at most %d characters",
			pool, got, "-", hostnameMaxLen, hostnameMaxLen-1-maxIndexDigits)
	}
	return nil
}

// ParseIndex extracts a cell's index from its name, given the owning pool. It
// rejects anything that is not exactly "<pool>-<digits>" so a foreign object that
// happens to carry our label cannot be mistaken for a cell.
func ParseIndex(pool, cell string) (int32, error) {
	prefix := pool + "-"
	if !strings.HasPrefix(cell, prefix) {
		return 0, fmt.Errorf("cell %q does not belong to pool %q", cell, pool)
	}
	suffix := strings.TrimPrefix(cell, prefix)
	if suffix == "" || strings.ContainsFunc(suffix, func(r rune) bool { return r < '0' || r > '9' }) {
		return 0, fmt.Errorf("cell %q has no numeric index suffix", cell)
	}
	n, err := strconv.ParseInt(suffix, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("cell %q index: %w", cell, err)
	}
	return int32(n), nil
}

// LowestFreeIndex returns the lowest index not present in used. Reusing a freed
// index keeps names dense and stable, which matters because the name is also the
// Node name an operator reads in the workload cluster.
func LowestFreeIndex(used []int32) int32 {
	seen := make(map[int32]struct{}, len(used))
	for _, u := range used {
		seen[u] = struct{}{}
	}
	for i := int32(0); ; i++ {
		if _, ok := seen[i]; !ok {
			return i
		}
	}
}

// HighestIndexes returns count indexes from used, highest first — the scale-down
// victim order. Highest-first matches SwiftGuestPool and keeps the surviving set
// contiguous from 0.
func HighestIndexes(used []int32, count int) []int32 {
	sorted := make([]int32, len(used))
	copy(sorted, used)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] > sorted[j] })
	if count > len(sorted) {
		count = len(sorted)
	}
	if count < 0 {
		count = 0
	}
	return sorted[:count]
}

// GuestLabels returns the labels stamped on a cell's OUTER objects (guest, seed,
// bootstrap secret) — the label selector the reconciler uses to rediscover cells
// after a restart.
func GuestLabels(pool string, index int32) map[string]string {
	return map[string]string{
		cellsv1alpha1.LabelPool:      pool,
		cellsv1alpha1.LabelCell:      Name(pool, index),
		cellsv1alpha1.LabelCellIndex: strconv.FormatInt(int64(index), 10),
	}
}

// PoolSelector selects every outer object owned by a pool.
func PoolSelector(pool string) labels.Selector {
	return labels.SelectorFromSet(labels.Set{cellsv1alpha1.LabelPool: pool})
}

// NodeLabels returns the labels a cell's workload Node must carry: the identity
// triple plus the user's own labels from spec.workloadCluster.node.labels (which
// is where a capacity provider's gate, e.g. HAMi's gpu=on, is declared).
//
// User labels are applied first so an identity label can never be overridden by
// one — identity is not negotiable.
func NodeLabels(pool string, index int32, instance string, extra map[string]string) map[string]string {
	out := make(map[string]string, len(extra)+4)
	for k, v := range extra {
		out[k] = v
	}
	out[cellsv1alpha1.LabelPool] = pool
	out[cellsv1alpha1.LabelCell] = Name(pool, index)
	out[cellsv1alpha1.LabelCellIndex] = strconv.FormatInt(int64(index), 10)
	if instance != "" {
		out[cellsv1alpha1.LabelInstance] = instance
	}
	return out
}

// IsStaleNode reports whether a Node carrying this pool's labels belongs to a
// PREVIOUS incarnation of the cell — same name, different guest UID. Such a Node
// must be reaped before its replacement joins, or the pool reports a cell as Ready
// while HAMi advertises capacity for a GPU that no longer exists.
//
// A Node with no instance label is NOT stale: the kubelet may simply not have
// applied it yet, and the controller patches it in that case.
func IsStaleNode(nodeLabels map[string]string, wantInstance string) bool {
	got, ok := nodeLabels[cellsv1alpha1.LabelInstance]
	if !ok || got == "" || wantInstance == "" {
		return false
	}
	return got != wantInstance
}

// TemplateHash is a stable short hash of the cell template, recorded on each
// guest so a template change is observable (Updated=False, TemplateChanged).
// v1alpha1 does not roll cells automatically: replacing a GPU worker needs inner
// drain sequencing, which is a later phase.
func TemplateHash(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		// Length-prefix each part so ("ab","c") and ("a","bc") differ.
		fmt.Fprintf(h, "%d:", len(p))
		h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))[:10]
}
