// Package v1alpha1 holds the GPUCellPool admission webhook.
//
// Every rule here exists so a mistake is a REJECTION rather than a cell that
// boots and quietly does the wrong thing. The rules are also enforced again at
// render time (internal/provisioner), so a pool that slipped past a disabled
// webhook still cannot misconfigure a cell.
package v1alpha1

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
	"github.com/kubeswift-io/gpucellpool/internal/cellid"
	"github.com/kubeswift-io/gpucellpool/internal/provisioner"
)

// +kubebuilder:webhook:path=/validate-cells-kubeswift-io-v1alpha1-gpucellpool,mutating=false,failurePolicy=fail,sideEffects=None,groups=cells.kubeswift.io,resources=gpucellpools,verbs=create;update,versions=v1alpha1,name=vgpucellpool.kb.io,admissionReviewVersions=v1

// GPUCellPoolValidator validates GPUCellPool objects.
//
// failurePolicy is Fail on purpose: the guestTemplate denylist is a security
// control (a cell's launcher pod is privileged in the infrastructure cluster), and
// a bypassed security control is a privilege escalation, not a degraded feature.
type GPUCellPoolValidator struct{}

// The validator is typed: controller-runtime hands us a *GPUCellPool, so "this is
// not the kind I expected" is a compile error rather than a runtime one.
var _ admission.Validator[*cellsv1alpha1.GPUCellPool] = &GPUCellPoolValidator{}

// SetupWebhookWithManager registers the validator.
func SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &cellsv1alpha1.GPUCellPool{}).
		WithValidator(&GPUCellPoolValidator{}).
		Complete()
}

// ValidateCreate implements admission.Validator.
func (v *GPUCellPoolValidator) ValidateCreate(_ context.Context, pool *cellsv1alpha1.GPUCellPool) (admission.Warnings, error) {
	return nil, invalidOrNil(pool, Validate(pool, nil))
}

// ValidateUpdate implements admission.Validator.
func (v *GPUCellPoolValidator) ValidateUpdate(_ context.Context, old, pool *cellsv1alpha1.GPUCellPool) (admission.Warnings, error) {
	return nil, invalidOrNil(pool, Validate(pool, old))
}

// ValidateDelete implements admission.Validator. Deletion is always allowed:
// teardown safety is the reconciler's drain gate, not admission's business.
func (v *GPUCellPoolValidator) ValidateDelete(_ context.Context, _ *cellsv1alpha1.GPUCellPool) (admission.Warnings, error) {
	return nil, nil
}

// Validate applies every rule. old is nil on create; rules that only make sense
// on update state so explicitly, per the house discipline that validation must
// enumerate which operations it fires on rather than defaulting to everything.
func Validate(pool, old *cellsv1alpha1.GPUCellPool) field.ErrorList {
	var errs field.ErrorList
	spec := field.NewPath("spec")

	// The pool name becomes each cell's hostname AND workload Node name, so it
	// must fit a DNS label with room for the index — not the 253-character object
	// name limit.
	if err := cellid.ValidatePool(pool.Name); err != nil {
		errs = append(errs, field.Invalid(field.NewPath("metadata", "name"), pool.Name, err.Error()))
	}

	errs = append(errs, validateProvisioner(spec.Child("cell"), pool)...)

	errs = append(errs, validateGPU(spec.Child("cell", "gpu"), pool.Spec.Cell.GPU)...)
	errs = append(errs, validateTemplate(spec.Child("cell"), pool)...)
	errs = append(errs, validateBootstrap(spec.Child("bootstrap"), pool.Spec.Bootstrap)...)
	errs = append(errs, validateWorkloadCluster(spec.Child("workloadCluster"), pool.Spec.WorkloadCluster)...)
	errs = append(errs, validateCapacity(spec.Child("capacity"), pool.Spec.Capacity)...)
	errs = append(errs, validateAutoscaling(spec.Child("autoscaling"), pool)...)

	// V10 (update only): never scale down blind. Shrinking a pool destroys cells,
	// and we cannot drain what we cannot see.
	if old != nil && pool.Spec.Replicas < old.Spec.Replicas && !workloadReachable(old) {
		errs = append(errs, field.Forbidden(spec.Child("replicas"),
			"cannot scale down while WorkloadClusterReachable is not True: "+
				"cells cannot be drained without access to the workload cluster"))
	}

	return errs
}

// validateGPU covers V1-V4: one whole pcie GPU, one backend, exactly one claim ref.
func validateGPU(p *field.Path, gpu cellsv1alpha1.CellGPUSpec) field.ErrorList {
	var errs field.ErrorList

	if gpu.Count != 0 && gpu.Count != 1 {
		errs = append(errs, field.Invalid(p.Child("count"), gpu.Count,
			"only 1 GPU per cell is supported in v1alpha1 (multi-GPU cells need NVLink topology work)"))
	}

	backend := gpu.Backend
	if backend == "" {
		backend = cellsv1alpha1.GPUBackendDRA
	}
	switch backend {
	case cellsv1alpha1.GPUBackendDRA:
		if gpu.Native != nil {
			errs = append(errs, field.Forbidden(p.Child("native"),
				"set only one allocation backend: backend is DRA"))
		}
		if gpu.DRA == nil {
			errs = append(errs, field.Required(p.Child("dra"), "required when backend is DRA"))
			break
		}
		hasTemplate := gpu.DRA.ResourceClaimTemplateName != ""
		hasClaim := gpu.DRA.ResourceClaimName != ""
		if hasTemplate == hasClaim {
			// A VFIO device backs exactly one running VM, so a shared claim is
			// only ever correct for a single-cell pool — and neither reference at
			// all cannot mean anything.
			errs = append(errs, field.Invalid(p.Child("dra"), gpu.DRA,
				"set exactly one of resourceClaimTemplateName or resourceClaimName"))
		}
		if t := gpu.DRA.Tier; t != "" && t != "pcie" {
			errs = append(errs, field.NotSupported(p.Child("dra", "tier"), t, []string{"pcie"}))
		}
	case cellsv1alpha1.GPUBackendNative:
		if gpu.DRA != nil {
			errs = append(errs, field.Forbidden(p.Child("dra"),
				"set only one allocation backend: backend is Native"))
		}
		if gpu.Native == nil || gpu.Native.GPUProfileRef.Name == "" {
			errs = append(errs, field.Required(p.Child("native", "gpuProfileRef"),
				"required when backend is Native"))
		}
	default:
		errs = append(errs, field.NotSupported(p.Child("backend"), backend,
			[]string{cellsv1alpha1.GPUBackendDRA, cellsv1alpha1.GPUBackendNative}))
	}
	return errs
}

// capiExpressibleTemplateFields are the guestTemplate fields a KubeSwiftMachine
// can carry. Everything else has nowhere to go under the ClusterAPI provisioner.
var capiExpressibleTemplateFields = []string{"imageRef", "guestClassRef", "interfaces"}

// validateProvisioner covers V11: the provisioner and its per-mode configuration
// must agree, and a guestTemplate must be expressible by the mode chosen.
//
// The last part is the important one. A KubeSwiftMachine exposes a curated subset
// of SwiftGuestSpec (image, class, two networks, GPU, storage class), so mapping a
// rich guestTemplate onto it would have to DROP fields — data disks, extra
// interfaces, topology, storage — and a cell that boots with less than the operator
// asked for is exactly the silent failure this project refuses. So the fields are
// rejected here, at admission, where the operator can see them named.
func validateProvisioner(p *field.Path, pool *cellsv1alpha1.GPUCellPool) field.ErrorList {
	var errs field.ErrorList
	name := pool.Spec.Cell.Provisioner
	if name == "" {
		name = provisioner.ProvisionerSwiftGuest
	}
	capi := pool.Spec.Cell.ClusterAPI

	switch name {
	case provisioner.ProvisionerSwiftGuest:
		if capi != nil {
			errs = append(errs, field.Forbidden(p.Child("clusterAPI"),
				"only valid with provisioner: ClusterAPI"))
		}
		return errs
	case provisioner.ProvisionerClusterAPI:
		// fall through to the checks below
	default:
		return append(errs, field.NotSupported(p.Child("provisioner"), name,
			[]string{provisioner.ProvisionerSwiftGuest, provisioner.ProvisionerClusterAPI}))
	}

	if capi == nil {
		return append(errs, field.Required(p.Child("clusterAPI"),
			"required with provisioner: ClusterAPI — cells must name the CAPI Cluster they join"))
	}
	if ref := capi.BootstrapConfigTemplateRef; ref != nil && !strings.HasSuffix(ref.Kind, "Template") {
		// The per-cell object's kind is the template's kind minus "Template"; without
		// the suffix the operator would try to create another template.
		errs = append(errs, field.Invalid(p.Child("clusterAPI", "bootstrapConfigTemplateRef", "kind"),
			ref.Kind, `must be a template kind ending in "Template", e.g. KubeadmConfigTemplate`))
	}

	// Which guestTemplate fields are actually present?
	var tmpl map[string]json.RawMessage
	if raw := pool.Spec.Cell.GuestTemplate.Raw; len(raw) > 0 {
		if err := json.Unmarshal(raw, &tmpl); err != nil {
			return errs // validateTemplate reports the parse failure
		}
	}
	for f := range tmpl {
		if !contains(capiExpressibleTemplateFields, f) {
			errs = append(errs, field.Forbidden(p.Child("guestTemplate", f),
				"not expressible by a KubeSwiftMachine, so it would be silently dropped; "+
					"provisioner ClusterAPI supports only "+strings.Join(capiExpressibleTemplateFields, ", ")))
		}
	}
	return errs
}

// validateTemplate covers V5, V6 and V12.
func validateTemplate(p *field.Path, pool *cellsv1alpha1.GPUCellPool) field.ErrorList {
	var errs field.ErrorList
	raw := pool.Spec.Cell.GuestTemplate.Raw

	if err := provisioner.ValidateTemplate(raw); err != nil {
		errs = append(errs, field.Invalid(p.Child("guestTemplate"), "<SwiftGuestSpec>", err.Error()))
		return errs // the remaining checks need a parseable template
	}

	// V12: an interface name that the guest does not have would make the kubelet
	// register no address at all, and the cell would sit in Booting until it timed
	// out with nothing to point at.
	if want := pool.Spec.Cell.NodeIPFrom; want != "" {
		names, err := interfaceNames(raw)
		if err != nil {
			errs = append(errs, field.Invalid(p.Child("guestTemplate", "interfaces"), "<interfaces>", err.Error()))
			return errs
		}
		if len(names) == 0 {
			errs = append(errs, field.Invalid(p.Child("nodeIPFrom"), want,
				"guestTemplate declares no interfaces, so this name cannot be resolved"))
		} else if !contains(names, want) {
			errs = append(errs, field.NotSupported(p.Child("nodeIPFrom"), want, names))
		}
	}
	return errs
}

// validateBootstrap covers V7.
func validateBootstrap(p *field.Path, b cellsv1alpha1.BootstrapSpec) field.ErrorList {
	var errs field.ErrorList
	provider := b.Provider
	if provider == "" {
		provider = cellsv1alpha1.BootstrapProviderOpaque
	}
	if provider == cellsv1alpha1.BootstrapProviderOpaque &&
		(b.JoinSecretRef == nil || b.JoinSecretRef.Name == "") {
		errs = append(errs, field.Required(p.Child("joinSecretRef"),
			"required for the Opaque provider: without it a cell boots with no cloud-init and never joins"))
	}
	return errs
}

func validateWorkloadCluster(p *field.Path, w cellsv1alpha1.WorkloadClusterSpec) field.ErrorList {
	var errs field.ErrorList
	if w.KubeconfigSecretRef.Name == "" {
		errs = append(errs, field.Required(p.Child("kubeconfigSecretRef", "name"),
			"a credential for the workload cluster is required"))
	}
	return errs
}

// validateAutoscaling guards the one feature that can consume GPUs on its own.
func validateAutoscaling(p *field.Path, pool *cellsv1alpha1.GPUCellPool) field.ErrorList {
	var errs field.ErrorList
	as := pool.Spec.Autoscaling
	if as == nil || !as.Enabled {
		return errs
	}

	// An unbounded pool that misreads demand can consume every GPU in the cluster.
	if as.MaxReplicas == nil {
		errs = append(errs, field.Required(p.Child("maxReplicas"),
			"required when autoscaling is enabled: an unbounded pool can consume every GPU in the cluster"))
	}
	if as.MinReplicas != nil && as.MaxReplicas != nil && *as.MinReplicas > *as.MaxReplicas {
		errs = append(errs, field.Invalid(p.Child("minReplicas"), *as.MinReplicas,
			"must not exceed maxReplicas"))
	}
	// Auto is implemented, but it only ever removes cells the capacity provider
	// reports as idle — so it is pointless without a floor that leaves the pool
	// able to serve anything at all.
	if as.ScaleDown == cellsv1alpha1.ScaleDownAuto && as.MinReplicas == nil {
		errs = append(errs, field.Required(p.Child("minReplicas"),
			"required with scaleDown: Auto — without a floor the pool can shrink to zero cells "+
				"and every later request pays a full cell boot"))
	}
	return errs
}

// validateCapacity covers V8.
func validateCapacity(p *field.Path, c cellsv1alpha1.CapacitySpec) field.ErrorList {
	var errs field.ErrorList
	if c.HAMi != nil && c.HAMi.Mode == cellsv1alpha1.HAMiModeDRA && c.HAMi.DeviceClassName == "" {
		errs = append(errs, field.Required(p.Child("hami", "deviceClassName"),
			"required in DRA mode: without it there is no way to find the ResourceSlices carrying capacity"))
	}
	return errs
}

// interfaceNames extracts spec.interfaces[].name from the opaque template.
func interfaceNames(raw []byte) ([]string, error) {
	var tmpl struct {
		Interfaces []struct {
			Name string `json:"name"`
		} `json:"interfaces"`
	}
	if err := json.Unmarshal(raw, &tmpl); err != nil {
		return nil, fmt.Errorf("guestTemplate.interfaces is not a list of interfaces: %w", err)
	}
	names := make([]string, 0, len(tmpl.Interfaces))
	for _, i := range tmpl.Interfaces {
		if i.Name != "" {
			names = append(names, i.Name)
		}
	}
	return names, nil
}

func workloadReachable(pool *cellsv1alpha1.GPUCellPool) bool {
	for _, c := range pool.Status.Conditions {
		if c.Type == cellsv1alpha1.ConditionWorkloadClusterReachable {
			return c.Status == metav1.ConditionTrue
		}
	}
	// No observation yet (a pool that has never reconciled): do not block the
	// user on a condition we have not had a chance to set.
	return true
}

func invalidOrNil(pool *cellsv1alpha1.GPUCellPool, errs field.ErrorList) error {
	if len(errs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(
		cellsv1alpha1.GroupVersion.WithKind("GPUCellPool").GroupKind(), pool.Name, errs)
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
