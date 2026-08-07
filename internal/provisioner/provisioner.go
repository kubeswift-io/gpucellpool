// Package provisioner owns the OUTER objects for a cell.
//
// It is the seam that keeps this operator's cell lifecycle independent of how a
// cell VM is actually created: today a KubeSwift SwiftGuest, later (for a
// CAPI-managed workload cluster) a Cluster API Machine. The interface is
// deliberately narrow — create-or-observe, and delete — because everything that
// spans the two clusters belongs to the reconciler, not here.
//
// Nothing in this package imports KubeSwift Go packages: SwiftGuest and
// SwiftSeedProfile are reached with unstructured clients and hard-coded GVKs, so
// this Apache-2.0 project never links AGPL code.
package provisioner

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Provisioner names.
const (
	ProvisionerSwiftGuest = "SwiftGuest"
	ProvisionerClusterAPI = "ClusterAPI"
)

// CellRequest is everything the provisioner needs to materialise one cell. It
// carries no pool object and no clients: the reconciler resolves policy, the
// provisioner renders objects.
type CellRequest struct {
	Pool      string
	Namespace string
	Index     int32
	CellName  string

	// GuestTemplate is the user's verbatim SwiftGuestSpec as JSON. It is opaque
	// to this operator apart from the field contract in template.go.
	GuestTemplate []byte

	// GPU is the cell's whole-device request, which the operator owns.
	GPU GPUBinding

	// BootstrapSecretName/Key point at the per-cell rendered cloud-init Secret.
	// The provisioner only ever REFERENCES it — the join credential never enters
	// a custom resource.
	BootstrapSecretName string
	BootstrapSecretKey  string

	// Hostname, when set, is written into the seed profile's meta-data as
	// local-hostname. It is what makes the workload Node name equal the cell
	// name, which is the whole cross-cluster identity mechanism.
	Hostname string

	// NodeIPFrom names the template interface whose address the kubelet
	// registers. Empty means the primary interface.
	NodeIPFrom string

	// SpreadPolicy is "Spread" or "Pack" (see CellSpec.SpreadPolicy).
	SpreadPolicy string

	TemplateHash string
	OwnerRefs    []metav1.OwnerReference
}

// GPUBinding is the cell's physical GPU request, resolved from spec.cell.gpu.
type GPUBinding struct {
	// Backend is "DRA" or "Native".
	Backend string

	// DRA fields (exactly one of the two claim references is set).
	ResourceClaimTemplateName string
	ResourceClaimName         string
	RequestName               string
	Tier                      string
	Hugepages                 string

	// Native field.
	GPUProfileName string
}

// OuterState is what the provisioner can observe about a cell in the outer
// cluster. It says nothing about the workload cluster — that is the reconciler's
// other half, and conflating them is how a "running" VM gets mistaken for a
// usable node.
type OuterState struct {
	// Exists is false before the outer objects have been created.
	Exists bool

	// Phase is the provisioner-native phase, carried for messages only. Never
	// branch on it outside the provisioner.
	Phase string

	// Provisioned is true once the VM is running AND has an address.
	Provisioned bool

	// Failed marks a terminal outer failure; Reason/Message explain it.
	Failed  bool
	Reason  string
	Message string

	// GPUDevices are the PCI addresses actually passed into the VM.
	GPUDevices []string

	// HostNode is the outer node whose physical GPU this cell holds.
	HostNode string

	// Address is the address the kubelet should register (from NodeIPFrom, or
	// the primary interface).
	Address string

	// UID is the outer object's UID — the cell's instance anchor, used to spot a
	// workload Node left behind by a previous incarnation.
	UID string
}

// Provisioner creates, observes and removes the outer objects for one cell.
type Provisioner interface {
	// Name is "SwiftGuest" or "ClusterAPI".
	Name() string

	// Ensure is idempotent: it creates the cell's outer objects if absent and
	// reports what is observable. It never blocks and never waits.
	Ensure(ctx context.Context, req CellRequest) (OuterState, error)

	// Delete removes the outer objects. done=false means "still terminating" —
	// the caller requeues. A cell guest carries the drain finalizer, so Delete
	// will not complete until the reconciler has cleared it.
	Delete(ctx context.Context, req CellRequest) (done bool, err error)
}
