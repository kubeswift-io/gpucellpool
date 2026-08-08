package v1alpha1

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
	"github.com/kubeswift-io/gpucellpool/internal/provisioner"
)

func capiPool() *cellsv1alpha1.GPUCellPool {
	p := validPool()
	p.Spec.Cell.Provisioner = provisioner.ProvisionerClusterAPI
	p.Spec.Cell.ClusterAPI = &cellsv1alpha1.CellClusterAPISpec{ClusterName: "workload"}
	return p
}

func TestClusterAPIPoolIsAccepted(t *testing.T) {
	assertValid(t, capiPool())
}

func TestClusterAPIRequiresItsConfig(t *testing.T) {
	p := capiPool()
	p.Spec.Cell.ClusterAPI = nil
	assertRejected(t, p, "required with provisioner: ClusterAPI")
}

func TestClusterAPIConfigIsForbiddenOnSwiftGuest(t *testing.T) {
	p := validPool()
	p.Spec.Cell.ClusterAPI = &cellsv1alpha1.CellClusterAPISpec{ClusterName: "workload"}
	assertRejected(t, p, "only valid with provisioner: ClusterAPI")
}

func TestUnknownProvisionerIsRejected(t *testing.T) {
	p := validPool()
	p.Spec.Cell.Provisioner = "Terraform"
	assertRejected(t, p, "Unsupported value")
}

// This is the rule that stops the CAPI mode from lying about what it built: a
// KubeSwiftMachine cannot express most of a SwiftGuestSpec, so a template field
// with nowhere to go must be a rejection, not a quiet omission.
func TestClusterAPIRejectsInexpressibleTemplateFields(t *testing.T) {
	for _, tc := range []struct{ name, tmpl, want string }{
		{"dataDisks", `{"imageRef":{"name":"i"},"dataDisks":[{"name":"d","sizeGiB":10}]}`, "dataDisks"},
		{"snapshot clone", `{"imageRef":{"name":"i"},"cloneFromSnapshot":{"name":"s"}}`, "cloneFromSnapshot"},
		{"topology", `{"imageRef":{"name":"i"},"topologySpreadConstraints":[]}`, "topologySpreadConstraints"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := capiPool()
			p.Spec.Cell.GuestTemplate = runtime.RawExtension{Raw: []byte(tc.tmpl)}
			assertRejected(t, p, tc.want)
		})
	}
}

func TestClusterAPIAcceptsTheExpressibleTemplate(t *testing.T) {
	p := capiPool()
	p.Spec.Cell.GuestTemplate = runtime.RawExtension{Raw: []byte(
		`{"imageRef":{"name":"i"},"guestClassRef":{"name":"c"},` +
			`"interfaces":[{"name":"mgmt","primary":true},{"name":"node","networkRef":{"name":"n"}}]}`)}
	p.Spec.Cell.NodeIPFrom = "node"
	assertValid(t, p)
}

// A pool whose join data comes from the cluster's own bootstrap provider must not be
// forced to invent a join Secret that nothing reads.
func TestCAPIBootstrapTemplateReplacesTheJoinSecret(t *testing.T) {
	p := capiPool()
	p.Spec.Cell.ClusterAPI.BootstrapConfigTemplateRef = &cellsv1alpha1.ClusterAPIObjectRef{
		APIGroup: "bootstrap.cluster.x-k8s.io", Kind: "KubeadmConfigTemplate", Name: "workers",
	}
	p.Spec.Bootstrap.JoinSecretRef = nil
	assertValid(t, p)

	// Setting one anyway is a rejection, not a warning: it would look like the
	// source of the cells' cloud-init while being ignored.
	p.Spec.Bootstrap.JoinSecretRef = &corev1.LocalObjectReference{Name: "join"}
	assertRejected(t, p, "unused when cell.clusterAPI.bootstrapConfigTemplateRef is set")
}

func TestBootstrapTemplateRefMustBeATemplateKind(t *testing.T) {
	p := capiPool()
	p.Spec.Cell.ClusterAPI.BootstrapConfigTemplateRef = &cellsv1alpha1.ClusterAPIObjectRef{
		APIGroup: "bootstrap.cluster.x-k8s.io", Kind: "KubeadmConfig", Name: "workers",
	}
	assertRejected(t, p, `must be a template kind`)
}
