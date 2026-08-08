// Package bootstrap renders a cell's cloud-init.
//
// The join credential lives only in Secrets: the user's Secret holds the template,
// this package renders a per-cell copy, and the SwiftSeedProfile references that.
// No token ever enters a custom resource, a log line, an Event or a status field.
package bootstrap

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The substitution set is CLOSED on purpose — a template language in a
// privileged boot path is a liability, and a typo must be an error rather than a
// silently unsubstituted node label.
const (
	KeyCellName        = "cellName"
	KeyPoolName        = "poolName"
	KeyNodeLabels      = "nodeLabels"
	KeyNodeIPInterface = "nodeIPInterface"
	KeyExpectedGPUs    = "expectedGPUs"
)

// SecretKey is the key the rendered cloud-init is stored under, which is what
// KubeSwift's SwiftSeedProfile reads.
const SecretKey = "user-data"

// CAPISecretKey is the key Cluster API reads bootstrap data from when a Machine
// names a Secret directly (spec.bootstrap.dataSecretName). The per-cell Secret
// carries the same bytes under both keys so either provisioner can consume it.
const CAPISecretKey = "value"

// Values are the per-cell substitutions.
//
// Note what is absent: the guest UID. It cannot be known before the guest exists,
// so the identity "instance" label is patched onto the Node by the controller
// rather than self-applied by the kubelet. Pretending otherwise would produce a
// template that renders an empty label.
type Values struct {
	CellName string
	PoolName string
	// NodeLabels is a ready-to-use --node-labels value (k=v,k=v), sorted so the
	// rendered output is stable and a no-op re-render does not churn the Secret.
	NodeLabels string
	// NodeIPInterface is the interface whose address the kubelet should register.
	NodeIPInterface string
	ExpectedGPUs    int
}

var tokenRE = regexp.MustCompile(`\{\{\s*([A-Za-z][A-Za-z0-9]*)\s*\}\}`)

// Render substitutes the closed token set into a cloud-init template.
//
// An unknown token is an ERROR: leaving `{{ cellname }}` in a boot script would
// produce a node that joins with a broken label and no explanation, which is
// exactly the class of silent failure this operator exists to prevent.
func Render(template []byte, v Values) ([]byte, error) {
	if len(template) == 0 {
		return nil, fmt.Errorf("bootstrap template is empty")
	}

	subs := map[string]string{
		KeyCellName:        v.CellName,
		KeyPoolName:        v.PoolName,
		KeyNodeLabels:      v.NodeLabels,
		KeyNodeIPInterface: v.NodeIPInterface,
		KeyExpectedGPUs:    strconv.Itoa(v.ExpectedGPUs),
	}

	var unknown []string
	out := tokenRE.ReplaceAllStringFunc(string(template), func(match string) string {
		name := tokenRE.FindStringSubmatch(match)[1]
		val, ok := subs[name]
		if !ok {
			unknown = append(unknown, name)
			return match
		}
		return val
	})

	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("unknown substitution(s) %s in the bootstrap template; supported: %s",
			strings.Join(dedupe(unknown), ", "), strings.Join(SupportedKeys(), ", "))
	}
	return []byte(out), nil
}

// SupportedKeys lists the substitutions, for error messages and docs.
func SupportedKeys() []string {
	return []string{KeyCellName, KeyExpectedGPUs, KeyNodeIPInterface, KeyNodeLabels, KeyPoolName}
}

// NodeLabelArg formats labels as a kubelet --node-labels value. Sorted for
// stability: an unstable render would rewrite the Secret on every reconcile.
func NodeLabelArg(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+labels[k])
	}
	return strings.Join(parts, ",")
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
