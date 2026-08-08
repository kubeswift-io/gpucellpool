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

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
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

	// ClusterAPI is spec.cell.clusterAPI, present only for the ClusterAPI
	// provisioner. The SwiftGuest provisioner ignores it.
	ClusterAPI *cellsv1alpha1.CellClusterAPISpec

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

	// Address is the best-known address for the VM: the NodeIPFrom interface's
	// when KubeSwift reports one, else the primary. It is a readiness signal
	// only — the operator does not control what the kubelet registers, because
	// cloud-init derives that in-guest.
	Address string

	// RoutableAddress is the NodeIPFrom interface's address, empty when KubeSwift
	// does not report one. Measured on KubeSwift v0.13.4: for a bridge NAD the
	// status carries the secondary interface's MAC but NOT its IP, so this is
	// often empty even though the guest has the address. It is used to VERIFY
	// which address a Node registered with, never to gate progress.
	RoutableAddress string

	// CreatedAt is when the outer object was created, which is the only honest
	// start point for a startup measurement.
	CreatedAt *metav1.Time

	// UID is the outer object's UID — the cell's instance anchor, used to spot a
	// workload Node left behind by a previous incarnation.
	UID string
}

// DrainFinalizerClearer is implemented by provisioners that stamp the cell-drain
// finalizer on the object they create. It is separate from Provisioner because
// only the reconciler may clear it, and only once the workload side is drained —
// keeping it off the main interface makes that hard to call by accident.
type DrainFinalizerClearer interface {
	ClearDrainFinalizer(ctx context.Context, req CellRequest) error
}

// Provisioner creates, observes and removes the outer objects for one cell.
type Provisioner interface {
	// Name is "SwiftGuest" or "ClusterAPI".
	Name() string

	// List returns the names of the cells this provisioner currently owns for a
	// pool, read from live objects in the infrastructure cluster.
	//
	// Discovery belongs here because the object that REPRESENTS a cell differs per
	// mode — a SwiftGuest in one, a Cluster API Machine in the other — and a
	// reconciler that listed only one kind would silently see no cells at all in
	// the other mode.
	List(ctx context.Context, namespace, pool string) ([]string, error)

	// Ensure is idempotent: it creates the cell's outer objects if absent and
	// reports what is observable. It never blocks and never waits.
	Ensure(ctx context.Context, req CellRequest) (OuterState, error)

	// Delete removes the outer objects. done=false means "still terminating" —
	// the caller requeues. A cell guest carries the drain finalizer, so Delete
	// will not complete until the reconciler has cleared it.
	Delete(ctx context.Context, req CellRequest) (done bool, err error)
}
