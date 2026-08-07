package provisioner

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
	"github.com/kubeswift-io/gpucellpool/internal/cellid"
)

// The guestTemplate field contract (docs/design/gpucellpool-api.md §4).
//
// ownedFields are written by this operator; a user setting them would be silently
// overridden, so they are rejected instead. deniedFields are incompatible with a
// GPU cell — most of them are rejected by KubeSwift itself, and we would rather
// fail at admission with an explanation than at VM boot with a webhook error.
var (
	ownedFields = []string{
		"seedProfileRef",
		"gpuProfileRef",
		"gpuResourceClaim",
		"nodeName",
		"migration",
		"runPolicy",
	}

	deniedFields = map[string]string{
		"kernelRef":         "a GPU cell is a disk boot; KubeSwift makes kernelRef and GPU passthrough mutually exclusive",
		"cloneFromSnapshot": "VFIO state cannot be restored from a snapshot",
		"vhostUserDevices":  "KubeSwift rejects vhost-user devices on the GPU path",
		"filesystems":       "KubeSwift rejects virtio-fs shares on the GPU path",
	}

	requiredFields = []string{"guestClassRef", "imageRef"}
)

// ValidateTemplate enforces the field contract. It is used by the admission
// webhook and again at render time, so a template that slipped past admission
// (an older object, a disabled webhook) still cannot silently misconfigure a cell.
func ValidateTemplate(raw []byte) error {
	if len(raw) == 0 {
		return fmt.Errorf("cell.guestTemplate is empty: guestClassRef and imageRef are required")
	}
	var tmpl map[string]any
	if err := json.Unmarshal(raw, &tmpl); err != nil {
		return fmt.Errorf("cell.guestTemplate is not a valid SwiftGuestSpec object: %w", err)
	}

	var problems []string
	for _, f := range ownedFields {
		if _, ok := tmpl[f]; ok {
			problems = append(problems, fmt.Sprintf(
				"cell.guestTemplate.%s is set by the operator and must not be set here", f))
		}
	}
	for f, why := range deniedFields {
		if _, ok := tmpl[f]; ok {
			problems = append(problems, fmt.Sprintf("cell.guestTemplate.%s is not supported: %s", f, why))
		}
	}
	if os, ok := tmpl["osType"].(string); ok && strings.EqualFold(os, "windows") {
		problems = append(problems, "cell.guestTemplate.osType: windows is not supported for GPU cells")
	}
	for _, f := range requiredFields {
		if v, ok := tmpl[f]; !ok || v == nil {
			problems = append(problems, fmt.Sprintf("cell.guestTemplate.%s is required", f))
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("%s", strings.Join(problems, "; "))
	}
	return nil
}

// ValidateGPU checks the GPU binding is internally consistent. Same reasoning as
// ValidateTemplate: enforced at admission and again at render.
func ValidateGPU(g GPUBinding) error {
	switch g.Backend {
	case cellsv1alpha1.GPUBackendDRA:
		hasTemplate := g.ResourceClaimTemplateName != ""
		hasClaim := g.ResourceClaimName != ""
		if hasTemplate == hasClaim {
			return fmt.Errorf("cell.gpu.dra: set exactly one of resourceClaimTemplateName or resourceClaimName")
		}
		if g.Tier != "" && g.Tier != "pcie" {
			return fmt.Errorf("cell.gpu.dra.tier: only pcie is supported (hgx tiers need QEMU and a host Fabric Manager)")
		}
	case cellsv1alpha1.GPUBackendNative:
		if g.GPUProfileName == "" {
			return fmt.Errorf("cell.gpu.native.gpuProfileRef is required for the Native backend")
		}
	default:
		return fmt.Errorf("cell.gpu.backend %q: want %q or %q",
			g.Backend, cellsv1alpha1.GPUBackendDRA, cellsv1alpha1.GPUBackendNative)
	}
	return nil
}

// renderGuestSpec applies the operator-owned overlay to the user's template and
// returns the SwiftGuest spec to create.
//
// Everything the operator writes here is a field the user is forbidden to set
// (see ownedFields), so the overlay can never clobber user intent.
func renderGuestSpec(req CellRequest) (map[string]any, error) {
	if err := ValidateTemplate(req.GuestTemplate); err != nil {
		return nil, err
	}
	if err := ValidateGPU(req.GPU); err != nil {
		return nil, err
	}

	spec := map[string]any{}
	if err := json.Unmarshal(req.GuestTemplate, &spec); err != nil {
		return nil, fmt.Errorf("cell.guestTemplate: %w", err)
	}

	// Cloud-init comes from the per-cell seed profile, which references the
	// bootstrap Secret. The token is never inlined.
	spec["seedProfileRef"] = map[string]any{"name": cellid.SeedName(req.CellName)}

	// The pool's contract is N running cells, so a VM that exits — crash or
	// in-guest poweroff — must come back. KubeSwift applies its own backoff on
	// top of the pool's replacement backoff.
	spec["runPolicy"] = "Always"

	// Cells are replaced, never migrated: a VFIO guest can only be moved by an
	// offline migration, which is a VM restart under a live Kubernetes node with
	// workloads on it. Pin the guest and let the pool own replacement.
	spec["migration"] = map[string]any{"enabled": false}

	switch req.GPU.Backend {
	case cellsv1alpha1.GPUBackendDRA:
		claim := map[string]any{}
		if req.GPU.ResourceClaimTemplateName != "" {
			claim["resourceClaimTemplateName"] = req.GPU.ResourceClaimTemplateName
		} else {
			claim["resourceClaimName"] = req.GPU.ResourceClaimName
		}
		if req.GPU.RequestName != "" {
			claim["requestName"] = req.GPU.RequestName
		}
		if req.GPU.Tier != "" {
			claim["tier"] = req.GPU.Tier
		}
		if req.GPU.Hugepages != "" {
			claim["hugepages"] = req.GPU.Hugepages
		}
		spec["gpuResourceClaim"] = claim
	case cellsv1alpha1.GPUBackendNative:
		spec["gpuProfileRef"] = map[string]any{"name": req.GPU.GPUProfileName}
	}

	// Spread cells across outer hosts unless the template says otherwise. An
	// explicit topologySpreadConstraints in the template wins, matching how
	// SwiftGuestPool resolves the same conflict.
	if req.SpreadPolicy == "Spread" {
		if _, set := spec["topologySpreadConstraints"]; !set {
			spec["topologySpreadConstraints"] = []any{
				map[string]any{
					"maxSkew":           int64(1),
					"topologyKey":       "kubernetes.io/hostname",
					"whenUnsatisfiable": "ScheduleAnyway",
					"labelSelector": map[string]any{
						"matchLabels": map[string]any{cellsv1alpha1.LabelPool: req.Pool},
					},
				},
			}
		}
	}

	return spec, nil
}

// renderSeedSpec builds the per-cell SwiftSeedProfile.
//
// userData is required by the SwiftSeedProfile CRD schema but userDataFrom takes
// precedence when both are present, so the empty string here is deliberate: the
// cloud-init — and the join token inside it — exists only in the Secret.
func renderSeedSpec(req CellRequest) map[string]any {
	key := req.BootstrapSecretKey
	if key == "" {
		key = "user-data"
	}
	spec := map[string]any{
		"datasource": "NoCloud",
		"userData":   "",
		"userDataFrom": map[string]any{
			"secretKeyRef": map[string]any{
				"name": req.BootstrapSecretName,
				"key":  key,
			},
		},
	}
	if req.Hostname != "" {
		spec["metaData"] = fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", req.CellName, req.Hostname)
	}
	return spec
}
