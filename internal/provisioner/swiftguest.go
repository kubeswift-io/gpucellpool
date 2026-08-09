package provisioner

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
	"github.com/kubeswift-io/gpucellpool/internal/cellid"
)

// KubeSwift kinds, reached by GVK through an unstructured client so this
// Apache-2.0 project never imports KubeSwift's AGPL Go packages.
var (
	SwiftGuestGVK = schema.GroupVersionKind{
		Group: "swift.kubeswift.io", Version: "v1alpha1", Kind: "SwiftGuest",
	}
	SwiftSeedProfileGVK = schema.GroupVersionKind{
		Group: "seed.kubeswift.io", Version: "v1alpha1", Kind: "SwiftSeedProfile",
	}
)

// SwiftGuestProvisioner backs a cell with a KubeSwift SwiftGuest VM.
type SwiftGuestProvisioner struct {
	Client client.Client
}

// NewSwiftGuestProvisioner returns a provisioner using the given outer-cluster client.
func NewSwiftGuestProvisioner(c client.Client) *SwiftGuestProvisioner {
	return &SwiftGuestProvisioner{Client: c}
}

// Name implements Provisioner.
func (p *SwiftGuestProvisioner) Name() string { return ProvisionerSwiftGuest }

// List implements Provisioner: the cell guests labelled for this pool.
func (p *SwiftGuestProvisioner) List(ctx context.Context, namespace, pool string) ([]string, error) {
	return listCellNames(ctx, p.Client, SwiftGuestGVK, namespace, pool)
}

// Ensure implements Provisioner.
func (p *SwiftGuestProvisioner) Ensure(ctx context.Context, req CellRequest) (OuterState, error) {
	if err := p.ensureSeedProfile(ctx, req); err != nil {
		return OuterState{}, fmt.Errorf("ensuring SwiftSeedProfile: %w", err)
	}
	guest, err := p.ensureGuest(ctx, req)
	if err != nil {
		return OuterState{}, fmt.Errorf("ensuring SwiftGuest: %w", err)
	}
	return observe(guest, req.NodeIPFrom), nil
}

// Delete implements Provisioner: remove the guest, then — once it is really gone —
// the seed profile. Returning done=false asks the caller to requeue, which is also
// what happens while the drain finalizer still holds the guest.
func (p *SwiftGuestProvisioner) Delete(ctx context.Context, req CellRequest) (bool, error) {
	guest := newUnstructured(SwiftGuestGVK)
	err := p.Client.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.CellName}, guest)
	switch {
	case err == nil:
		if guest.GetDeletionTimestamp().IsZero() {
			if delErr := p.Client.Delete(ctx, guest); delErr != nil && !apierrors.IsNotFound(delErr) {
				return false, fmt.Errorf("deleting SwiftGuest %s: %w", req.CellName, delErr)
			}
		}
		return false, nil // still terminating
	case !apierrors.IsNotFound(err):
		return false, fmt.Errorf("reading SwiftGuest %s: %w", req.CellName, err)
	}

	seed := newUnstructured(SwiftSeedProfileGVK)
	seed.SetNamespace(req.Namespace)
	seed.SetName(cellid.SeedName(req.CellName))
	if err := p.Client.Delete(ctx, seed); err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("deleting SwiftSeedProfile for %s: %w", req.CellName, err)
	}
	return true, nil
}

// ClearDrainFinalizer removes the cell-drain finalizer from a cell guest. Only
// the reconciler may call this, and only once the workload side is drained: the
// finalizer is what stops a cell from being deleted — by us, by kubectl, or by
// garbage collection — while HAMi workloads still hold its GPU.
func (p *SwiftGuestProvisioner) ClearDrainFinalizer(ctx context.Context, req CellRequest) error {
	guest := newUnstructured(SwiftGuestGVK)
	err := p.Client.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.CellName}, guest)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading SwiftGuest %s: %w", req.CellName, err)
	}

	kept, found := withoutDrainFinalizer(guest.GetFinalizers())
	if !found {
		return nil
	}
	guest.SetFinalizers(kept)
	if err := p.Client.Update(ctx, guest); err != nil {
		return fmt.Errorf("clearing drain finalizer on %s: %w", req.CellName, err)
	}
	return nil
}

func (p *SwiftGuestProvisioner) ensureSeedProfile(ctx context.Context, req CellRequest) error {
	name := cellid.SeedName(req.CellName)
	existing := newUnstructured(SwiftSeedProfileGVK)
	err := p.Client.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: name}, existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	seed := newUnstructured(SwiftSeedProfileGVK)
	seed.SetNamespace(req.Namespace)
	seed.SetName(name)
	seed.SetLabels(cellid.GuestLabels(req.Pool, req.Index))
	seed.SetOwnerReferences(req.OwnerRefs)
	seed.Object["spec"] = renderSeedSpec(req)
	return p.Client.Create(ctx, seed)
}

func (p *SwiftGuestProvisioner) ensureGuest(ctx context.Context, req CellRequest) (*unstructured.Unstructured, error) {
	existing := newUnstructured(SwiftGuestGVK)
	err := p.Client.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.CellName}, existing)
	if err == nil {
		return existing, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}

	spec, err := renderGuestSpec(req)
	if err != nil {
		return nil, err
	}
	guest := newUnstructured(SwiftGuestGVK)
	guest.SetNamespace(req.Namespace)
	guest.SetName(req.CellName)
	guest.SetLabels(cellid.GuestLabels(req.Pool, req.Index))
	guest.SetAnnotations(map[string]string{cellsv1alpha1.AnnotationTemplateHash: req.TemplateHash})
	guest.SetOwnerReferences(req.OwnerRefs)
	guest.SetFinalizers([]string{cellsv1alpha1.FinalizerCellDrain})
	guest.Object["spec"] = spec

	if err := p.Client.Create(ctx, guest); err != nil {
		return nil, err
	}
	return guest, nil
}

// observe maps a SwiftGuest's status onto OuterState. Every field read here is
// part of the KubeSwift status contract; nothing is inferred.
func observe(guest *unstructured.Unstructured, nodeIPFrom string) OuterState {
	st := OuterState{Exists: true, UID: string(guest.GetUID())}
	if ts := guest.GetCreationTimestamp(); !ts.IsZero() {
		created := ts
		st.CreatedAt = &created
	}

	st.TemplateHash = guest.GetAnnotations()[cellsv1alpha1.AnnotationTemplateHash]
	st.Phase, _, _ = unstructured.NestedString(guest.Object, "status", "phase")
	st.HostNode, _, _ = unstructured.NestedString(guest.Object, "status", "gpu", "nodeName")
	if st.HostNode == "" {
		st.HostNode, _, _ = unstructured.NestedString(guest.Object, "status", "nodeName")
	}
	if devs, ok, _ := unstructured.NestedStringSlice(guest.Object, "status", "gpu", "devices"); ok {
		st.GPUDevices = devs
	}
	st.RoutableAddress = address(guest, nodeIPFrom)
	st.Address = st.RoutableAddress
	if st.Address == "" {
		// KubeSwift may know an interface without reporting its address (bridge
		// NADs on v0.13.4 report the MAC only). Falling back is correct here
		// because this address is a readiness signal, not something the kubelet
		// consumes: the guest derives its own node IP. Verification of WHICH
		// address the Node actually registered with happens against the inner
		// cluster, where the truth is.
		st.Address, _, _ = unstructured.NestedString(guest.Object, "status", "network", "primaryIP")
	}

	switch st.Phase {
	case "Failed":
		st.Failed = true
		st.Reason = cellsv1alpha1.ReasonCellProvisionTimeout
		st.Message = conditionMessage(guest)
		if st.Message == "" {
			st.Message = "SwiftGuest reported phase Failed"
		}
	case "Running":
		// Provisioned means "the VM is up AND reachable enough to join": an
		// address is what the kubelet needs, so a Running guest with no address
		// is not yet provisioned.
		st.Provisioned = st.Address != ""
	}
	return st
}

// address returns the address the kubelet should register. When nodeIPFrom names
// an interface, only that interface counts — falling back to the primary would
// silently register the wrong (node-local, non-routable) address, which is the
// failure this field exists to prevent.
func address(guest *unstructured.Unstructured, nodeIPFrom string) string {
	if nodeIPFrom != "" {
		ifaces, ok, _ := unstructured.NestedSlice(guest.Object, "status", "network", "interfaces")
		if !ok {
			return ""
		}
		for _, raw := range ifaces {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if name, _ := m["name"].(string); name == nodeIPFrom {
				ip, _ := m["ip"].(string)
				return ip
			}
		}
		return ""
	}
	ip, _, _ := unstructured.NestedString(guest.Object, "status", "network", "primaryIP")
	return ip
}

// conditionMessage returns the message of the first non-True condition, so a
// cell's status carries KubeSwift's own explanation rather than a generic one.
func conditionMessage(guest *unstructured.Unstructured) string {
	conds, ok, _ := unstructured.NestedSlice(guest.Object, "status", "conditions")
	if !ok {
		return ""
	}
	for _, raw := range conds {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if status, _ := m["status"].(string); status == "True" {
			continue
		}
		msg, _ := m["message"].(string)
		reason, _ := m["reason"].(string)
		switch {
		case msg != "" && reason != "":
			return reason + ": " + msg
		case msg != "":
			return msg
		case reason != "":
			return reason
		}
	}
	return ""
}

func newUnstructured(gvk schema.GroupVersionKind) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	return u
}
