package provisioner

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
	"github.com/kubeswift-io/gpucellpool/internal/cellid"
)

// Cluster API kinds, reached by GVK through an unstructured client. Both Cluster
// API and capi-kubeswift are Apache-2.0, so importing their Go types would be
// licence-clean — this stays unstructured to avoid pulling the whole Cluster API
// module in for four object shapes, and to keep one discipline across the package.
var (
	MachineGVK = schema.GroupVersionKind{
		Group: "cluster.x-k8s.io", Version: "v1beta2", Kind: "Machine",
	}
	KubeSwiftMachineGVK = schema.GroupVersionKind{
		Group: "infrastructure.cluster.x-k8s.io", Version: "v1alpha1", Kind: "KubeSwiftMachine",
	}
)

// clusterNameLabel is how Cluster API finds the Machines of a Cluster. A Machine
// without it is orphaned from its cluster's controllers.
const clusterNameLabel = "cluster.x-k8s.io/cluster-name"

// ClusterAPIProvisioner backs a cell with a Cluster API Machine plus its
// KubeSwiftMachine infrastructure object.
//
// The point is membership, not a different VM: the same KubeSwift SwiftGuest ends
// up running, but the workload cluster's own controllers know about it — it has a
// Machine, a providerID, and it participates in the cluster's node lifecycle
// instead of being a node somebody attached out of band.
//
// One Machine per cell, named after the cell, NOT a MachineDeployment sized to the
// replica count. That is a deliberate departure from the original sketch: a
// MachineDeployment generates Machine names, and capi-kubeswift derives the guest
// hostname (and therefore the workload Node name) from the Machine name. Random
// names would break the cell-name == Node-name identity the whole operator rests
// on, and there would be no way to drain or replace one specific cell.
type ClusterAPIProvisioner struct {
	Client client.Client
}

// NewClusterAPIProvisioner returns a provisioner using the given outer-cluster client.
func NewClusterAPIProvisioner(c client.Client) *ClusterAPIProvisioner {
	return &ClusterAPIProvisioner{Client: c}
}

// Name implements Provisioner.
func (p *ClusterAPIProvisioner) Name() string { return ProvisionerClusterAPI }

// List implements Provisioner: the cell Machines labelled for this pool.
func (p *ClusterAPIProvisioner) List(ctx context.Context, namespace, pool string) ([]string, error) {
	return listCellNames(ctx, p.Client, MachineGVK, namespace, pool)
}

// listCellNames returns the names of a pool's cell objects of one kind.
func listCellNames(
	ctx context.Context, c client.Client, gvk schema.GroupVersionKind, namespace, pool string,
) ([]string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
	if err := c.List(ctx, list,
		client.InNamespace(namespace),
		client.MatchingLabels{cellsv1alpha1.LabelPool: pool},
	); err != nil {
		return nil, fmt.Errorf("listing cell %ss: %w", gvk.Kind, err)
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].GetName())
	}
	return names, nil
}

// Ensure implements Provisioner: bootstrap config (if templated), then the
// infrastructure object, then the Machine that ties them together.
//
// Order matters. The Machine references both, and Cluster API starts acting on it
// the moment it exists, so creating it last means it never points at something
// that is not there yet.
func (p *ClusterAPIProvisioner) Ensure(ctx context.Context, req CellRequest) (OuterState, error) {
	cfg := req.ClusterAPI
	if cfg == nil {
		return OuterState{}, fmt.Errorf("cell %s: provisioner ClusterAPI needs spec.cell.clusterAPI", req.CellName)
	}

	bootstrapRef, err := p.ensureBootstrapConfig(ctx, req)
	if err != nil {
		return OuterState{}, fmt.Errorf("ensuring bootstrap config for %s: %w", req.CellName, err)
	}
	if err := p.ensureInfraMachine(ctx, req); err != nil {
		return OuterState{}, fmt.Errorf("ensuring KubeSwiftMachine %s: %w", req.CellName, err)
	}
	machine, err := p.ensureMachine(ctx, req, bootstrapRef)
	if err != nil {
		return OuterState{}, fmt.Errorf("ensuring Machine %s: %w", req.CellName, err)
	}
	return p.observeMachine(ctx, req, machine)
}

// Delete implements Provisioner: ask for every object of the cell to go, and
// report done only once the Machine is really gone.
//
// All three deletions are requested in the SAME pass, deliberately. Deleting the
// Machine and waiting to clean up afterwards looks tidier but leaks: once the
// Machine is gone the cell is no longer discovered, so nothing ever comes back for
// the infrastructure object — and an unadopted KubeSwiftMachine still holds a GPU
// claim. Cluster API tolerates an infrastructure object that has already
// disappeared while it deletes a Machine, so ordering buys nothing.
//
// The drain gate is not weakened by this: the reconciler clears the drain
// finalizer only after the workload side is drained, and it does that BEFORE
// calling Delete. By the time we are here the decision to destroy the cell has
// already been made.
func (p *ClusterAPIProvisioner) Delete(ctx context.Context, req CellRequest) (bool, error) {
	machine := newUnstructured(MachineGVK)
	err := p.Client.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.CellName}, machine)
	switch {
	case err == nil:
		if machine.GetDeletionTimestamp().IsZero() {
			if delErr := p.Client.Delete(ctx, machine); delErr != nil && !apierrors.IsNotFound(delErr) {
				return false, fmt.Errorf("deleting Machine %s: %w", req.CellName, delErr)
			}
		}
	case !apierrors.IsNotFound(err):
		return false, fmt.Errorf("reading Machine %s: %w", req.CellName, err)
	}
	machineGone := apierrors.IsNotFound(err)

	infra := newUnstructured(KubeSwiftMachineGVK)
	infra.SetNamespace(req.Namespace)
	infra.SetName(req.CellName)
	if err := p.Client.Delete(ctx, infra); err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("deleting KubeSwiftMachine %s: %w", req.CellName, err)
	}

	if ref := bootstrapTemplateRef(req); ref != nil {
		cfgObj := newUnstructured(bootstrapConfigGVK(ref))
		cfgObj.SetNamespace(req.Namespace)
		cfgObj.SetName(req.CellName)
		if err := p.Client.Delete(ctx, cfgObj); err != nil && !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("deleting bootstrap config %s: %w", req.CellName, err)
		}
	}
	return machineGone, nil
}

// ClearDrainFinalizer removes the cell-drain finalizer from the cell's Machine.
// The finalizer sits on the Machine because that is the object whose deletion must
// wait: while HAMi workloads still hold the cell's GPU, neither we nor kubectl nor
// Cluster API's own machinery may take it away.
func (p *ClusterAPIProvisioner) ClearDrainFinalizer(ctx context.Context, req CellRequest) error {
	machine := newUnstructured(MachineGVK)
	err := p.Client.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.CellName}, machine)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading Machine %s: %w", req.CellName, err)
	}
	kept, found := withoutDrainFinalizer(machine.GetFinalizers())
	if !found {
		return nil
	}
	machine.SetFinalizers(kept)
	if err := p.Client.Update(ctx, machine); err != nil {
		return fmt.Errorf("clearing drain finalizer on Machine %s: %w", req.CellName, err)
	}
	return nil
}

// ensureBootstrapConfig instantiates a per-cell bootstrap config from the user's
// template, exactly as a MachineSet would, and returns the reference to put on the
// Machine. A nil return means "use spec.bootstrap's rendered Secret instead".
func (p *ClusterAPIProvisioner) ensureBootstrapConfig(
	ctx context.Context, req CellRequest,
) (*cellsv1alpha1.ClusterAPIObjectRef, error) {
	tmplRef := bootstrapTemplateRef(req)
	if tmplRef == nil {
		return nil, nil
	}
	gvk := bootstrapConfigGVK(tmplRef)

	existing := newUnstructured(gvk)
	err := p.Client.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.CellName}, existing)
	if err == nil {
		return &cellsv1alpha1.ClusterAPIObjectRef{APIGroup: gvk.Group, Kind: gvk.Kind, Name: req.CellName}, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}

	tmpl := newUnstructured(schema.GroupVersionKind{
		Group: tmplRef.APIGroup, Version: bootstrapTemplateVersion, Kind: tmplRef.Kind,
	})
	if err := p.Client.Get(ctx,
		types.NamespacedName{Namespace: req.Namespace, Name: tmplRef.Name}, tmpl); err != nil {
		return nil, fmt.Errorf("reading bootstrap template %s/%s: %w", tmplRef.Kind, tmplRef.Name, err)
	}
	// Cluster API's own template contract: the concrete spec lives at
	// spec.template.spec. Refusing to guess anything else means a template shape we
	// do not understand fails loudly instead of producing an empty bootstrap config
	// that would leave the cell Pending forever with no explanation.
	spec, ok, err := unstructured.NestedMap(tmpl.Object, "spec", "template", "spec")
	if err != nil || !ok {
		return nil, fmt.Errorf("bootstrap template %s/%s has no spec.template.spec",
			tmplRef.Kind, tmplRef.Name)
	}

	obj := newUnstructured(gvk)
	obj.SetNamespace(req.Namespace)
	obj.SetName(req.CellName)
	obj.SetLabels(p.labels(req))
	obj.SetOwnerReferences(coOwnerRefs(req.OwnerRefs))
	obj.Object["spec"] = spec
	if err := p.Client.Create(ctx, obj); err != nil {
		return nil, err
	}
	return &cellsv1alpha1.ClusterAPIObjectRef{APIGroup: gvk.Group, Kind: gvk.Kind, Name: req.CellName}, nil
}

func (p *ClusterAPIProvisioner) ensureInfraMachine(ctx context.Context, req CellRequest) error {
	existing := newUnstructured(KubeSwiftMachineGVK)
	err := p.Client.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.CellName}, existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	backend, err := renderMachineBackend(req)
	if err != nil {
		return err
	}
	infra := newUnstructured(KubeSwiftMachineGVK)
	infra.SetNamespace(req.Namespace)
	infra.SetName(req.CellName)
	infra.SetLabels(p.labels(req))
	infra.SetAnnotations(map[string]string{cellsv1alpha1.AnnotationTemplateHash: req.TemplateHash})
	infra.SetOwnerReferences(coOwnerRefs(req.OwnerRefs))
	infra.Object["spec"] = map[string]any{"backend": backend}
	return p.Client.Create(ctx, infra)
}

func (p *ClusterAPIProvisioner) ensureMachine(
	ctx context.Context, req CellRequest, bootstrapRef *cellsv1alpha1.ClusterAPIObjectRef,
) (*unstructured.Unstructured, error) {
	existing := newUnstructured(MachineGVK)
	err := p.Client.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.CellName}, existing)
	if err == nil {
		return existing, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}

	bootstrap := map[string]any{}
	if bootstrapRef != nil {
		bootstrap["configRef"] = map[string]any{
			"apiGroup": bootstrapRef.APIGroup,
			"kind":     bootstrapRef.Kind,
			"name":     bootstrapRef.Name,
		}
	} else {
		bootstrap["dataSecretName"] = req.BootstrapSecretName
	}

	spec := map[string]any{
		"clusterName": req.ClusterAPI.ClusterName,
		"bootstrap":   bootstrap,
		"infrastructureRef": map[string]any{
			"apiGroup": KubeSwiftMachineGVK.Group,
			"kind":     KubeSwiftMachineGVK.Kind,
			"name":     req.CellName,
		},
	}
	if v := req.ClusterAPI.Version; v != "" {
		spec["version"] = v
	}

	machine := newUnstructured(MachineGVK)
	machine.SetNamespace(req.Namespace)
	machine.SetName(req.CellName)
	machine.SetLabels(p.labels(req))
	machine.SetAnnotations(map[string]string{cellsv1alpha1.AnnotationTemplateHash: req.TemplateHash})
	machine.SetOwnerReferences(coOwnerRefs(req.OwnerRefs))
	machine.SetFinalizers([]string{cellsv1alpha1.FinalizerCellDrain})
	machine.Object["spec"] = spec

	if err := p.Client.Create(ctx, machine); err != nil {
		return nil, err
	}
	return machine, nil
}

// observeMachine maps the Machine onto OuterState, then follows its providerID to
// the backing SwiftGuest for the things only KubeSwift knows: which physical GPU
// the cell holds, which host it is on, and which interface's address the kubelet
// would register.
//
// Without that second read the pool could never leave AllocatingGPU, because a
// Machine says nothing about PCI devices. The providerID format
// "kubeswift://<namespace>/<name>" is capi-kubeswift's documented contract.
func (p *ClusterAPIProvisioner) observeMachine(
	ctx context.Context, req CellRequest, machine *unstructured.Unstructured,
) (OuterState, error) {
	st := OuterState{Exists: true, UID: string(machine.GetUID())}
	if ts := machine.GetCreationTimestamp(); !ts.IsZero() {
		created := ts
		st.CreatedAt = &created
	}
	st.TemplateHash = machine.GetAnnotations()[cellsv1alpha1.AnnotationTemplateHash]
	st.Phase, _, _ = unstructured.NestedString(machine.Object, "status", "phase")

	switch st.Phase {
	case "Failed":
		st.Failed = true
		st.Reason = cellsv1alpha1.ReasonCellProvisionTimeout
		st.Message = conditionMessage(machine)
		if st.Message == "" {
			st.Message = "Cluster API Machine reported phase Failed"
		}
		return st, nil
	case "Provisioned", "Running", "Updating":
		// Keep going: these are the phases in which a backing guest exists.
	default:
		// Pending/Provisioning: no infrastructure yet, so nothing more to read.
		return st, nil
	}

	guestNS, guestName := parseKubeSwiftProviderID(providerID(ctx, p.Client, req))
	if guestName == "" {
		return st, nil
	}
	guest := newUnstructured(SwiftGuestGVK)
	if err := p.Client.Get(ctx, types.NamespacedName{Namespace: guestNS, Name: guestName}, guest); err != nil {
		if apierrors.IsNotFound(err) {
			return st, nil
		}
		return st, fmt.Errorf("reading backing SwiftGuest %s/%s: %w", guestNS, guestName, err)
	}

	// The guest's own view supplies GPU, host and address; the Machine keeps
	// ownership of lifecycle and identity.
	guestState := observe(guest, req.NodeIPFrom)
	// Deliberately NOT guestState.TemplateHash: under this provisioner the object
	// this operator creates and versions is the Machine, and the backing guest is
	// capi-kubeswift's to annotate however it likes.
	st.GPUDevices = guestState.GPUDevices
	st.HostNode = guestState.HostNode
	st.Address = guestState.Address
	st.RoutableAddress = guestState.RoutableAddress
	st.Provisioned = guestState.Provisioned
	if guestState.Failed {
		st.Failed, st.Reason, st.Message = true, guestState.Reason, guestState.Message
	}
	return st, nil
}

// providerID reads the providerID from the infrastructure object, falling back to
// the Machine's copy of it. Reading the infra object first is deliberate: it is
// where the value originates, so it is correct one reconcile earlier.
func providerID(ctx context.Context, c client.Client, req CellRequest) string {
	infra := newUnstructured(KubeSwiftMachineGVK)
	if err := c.Get(ctx,
		types.NamespacedName{Namespace: req.Namespace, Name: req.CellName}, infra); err == nil {
		if id, ok, _ := unstructured.NestedString(infra.Object, "spec", "providerID"); ok && id != "" {
			return id
		}
	}
	machine := newUnstructured(MachineGVK)
	if err := c.Get(ctx,
		types.NamespacedName{Namespace: req.Namespace, Name: req.CellName}, machine); err == nil {
		id, _, _ := unstructured.NestedString(machine.Object, "spec", "providerID")
		return id
	}
	return ""
}

// parseKubeSwiftProviderID splits "kubeswift://<namespace>/<name>". Anything else
// yields empty strings, so an unexpected format degrades to "no GPU information
// yet" rather than a lookup for a nonsense object.
func parseKubeSwiftProviderID(id string) (namespace, name string) {
	const prefix = "kubeswift://"
	if !strings.HasPrefix(id, prefix) {
		return "", ""
	}
	parts := strings.SplitN(strings.TrimPrefix(id, prefix), "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", ""
	}
	return parts[0], parts[1]
}

// renderMachineBackend maps the cell's guestTemplate and GPU onto the curated
// fields a KubeSwiftMachine can express.
//
// The mapping is small because the backend's surface is small — and that is the
// whole reason the webhook rejects any other guestTemplate field under this
// provisioner. Dropping fields quietly here would mean a cell that boots with less
// storage, fewer disks or the wrong network than the operator asked for.
func renderMachineBackend(req CellRequest) (map[string]any, error) {
	var tmpl map[string]any
	if len(req.GuestTemplate) > 0 {
		if err := json.Unmarshal(req.GuestTemplate, &tmpl); err != nil {
			return nil, fmt.Errorf("parsing guestTemplate: %w", err)
		}
	}

	sg := map[string]any{}
	if name := nestedRefName(tmpl, "imageRef"); name != "" {
		sg["imageRef"] = name
	}
	if name := nestedRefName(tmpl, "guestClassRef"); name != "" {
		sg["guestClassRef"] = name
	}
	primary, secondary := interfaceNetworks(tmpl)
	if primary != "" {
		sg["networkRef"] = primary
	}
	if secondary != "" {
		sg["nodeNetworkRef"] = secondary
	}
	if gpu := machineGPU(req.GPU); gpu != nil {
		sg["gpu"] = gpu
	}

	return map[string]any{"type": "SwiftGuest", "swiftGuest": sg}, nil
}

// machineGPU renders the cell's whole-device request into KubeSwiftMachine's gpu
// block. Exactly one backend is set, which the pool's own validation already
// guarantees.
func machineGPU(g GPUBinding) map[string]any {
	out := map[string]any{}
	switch g.Backend {
	case cellsv1alpha1.GPUBackendDRA:
		switch {
		case g.ResourceClaimTemplateName != "":
			out["resourceClaimTemplateName"] = g.ResourceClaimTemplateName
		case g.ResourceClaimName != "":
			out["resourceClaimName"] = g.ResourceClaimName
		default:
			return nil
		}
		if g.RequestName != "" {
			out["requestName"] = g.RequestName
		}
	case cellsv1alpha1.GPUBackendNative:
		if g.GPUProfileName == "" {
			return nil
		}
		out["gpuProfileRef"] = g.GPUProfileName
	default:
		return nil
	}
	if g.Tier != "" {
		out["tier"] = g.Tier
	}
	if g.Hugepages != "" {
		out["hugepages"] = g.Hugepages
	}
	return out
}

// nestedRefName reads a {name: ...} reference's name.
func nestedRefName(tmpl map[string]any, field string) string {
	ref, ok := tmpl[field].(map[string]any)
	if !ok {
		return ""
	}
	name, _ := ref["name"].(string)
	return name
}

// interfaceNetworks picks the primary and the first secondary networkRef out of a
// guestTemplate's interfaces. A KubeSwiftMachine expresses exactly these two.
func interfaceNetworks(tmpl map[string]any) (primary, secondary string) {
	ifaces, ok := tmpl["interfaces"].([]any)
	if !ok {
		return "", ""
	}
	for _, raw := range ifaces {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		net := nestedRefName(m, "networkRef")
		if net == "" {
			continue
		}
		isPrimary, _ := m["primary"].(bool)
		switch {
		case isPrimary && primary == "":
			primary = net
		case !isPrimary && secondary == "":
			secondary = net
		}
	}
	return primary, secondary
}

// bootstrapConfigGVK turns a template reference into the GVK of the CONCRETE
// object to create: KubeadmConfigTemplate -> KubeadmConfig.
func bootstrapConfigGVK(ref *cellsv1alpha1.ClusterAPIObjectRef) schema.GroupVersionKind {
	return schema.GroupVersionKind{
		Group:   ref.APIGroup,
		Version: bootstrapTemplateVersion,
		Kind:    strings.TrimSuffix(ref.Kind, "Template"),
	}
}

// bootstrapTemplateVersion is the version used for bootstrap-provider objects.
// Cluster API resolves versions from CRD contract labels for its own references,
// but an unstructured client has to name one. v1beta2 is the contract version that
// ships with Cluster API v1.11+.
const bootstrapTemplateVersion = "v1beta2"

// coOwnerRefs turns the pool's ownership into NON-controller owner references.
//
// Cluster API's own controllers must be the controller of the objects they manage —
// the Machine controller sets itself on the infrastructure object, and the Cluster on
// the Machine — and Kubernetes allows exactly ONE controller reference per object.
// Claiming it makes CAPI fail every reconcile with "already owned by another
// controller", which is a hard stop: the Machine never gets bootstrap data and no VM
// is ever created.
//
// A plain owner reference is enough for what this operator actually needs, which is
// garbage collection: when the pool goes, its cells go with it.
//
// Not caught by the harness — a CRD stub accepts any ownerRef, because the thing that
// objects is the controller that is not running there.
func coOwnerRefs(refs []metav1.OwnerReference) []metav1.OwnerReference {
	out := make([]metav1.OwnerReference, 0, len(refs))
	for _, r := range refs {
		r.Controller = nil
		r.BlockOwnerDeletion = nil
		out = append(out, r)
	}
	return out
}

// labels are the pool's cell identity labels plus the Cluster API cluster label,
// which is how Cluster API's controllers find a Cluster's Machines at all.
func (p *ClusterAPIProvisioner) labels(req CellRequest) map[string]string {
	out := cellid.GuestLabels(req.Pool, req.Index)
	out[clusterNameLabel] = req.ClusterAPI.ClusterName
	return out
}

// bootstrapTemplateRef returns the configured template reference, tolerating a
// pool whose clusterAPI block has since been removed — Delete still has to finish.
func bootstrapTemplateRef(req CellRequest) *cellsv1alpha1.ClusterAPIObjectRef {
	if req.ClusterAPI == nil {
		return nil
	}
	return req.ClusterAPI.BootstrapConfigTemplateRef
}

func withoutDrainFinalizer(in []string) (kept []string, found bool) {
	kept = make([]string, 0, len(in))
	for _, f := range in {
		if f == cellsv1alpha1.FinalizerCellDrain {
			found = true
			continue
		}
		kept = append(kept, f)
	}
	return kept, found
}

var _ Provisioner = (*ClusterAPIProvisioner)(nil)
