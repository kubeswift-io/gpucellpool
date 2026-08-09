package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	resourceapi "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
	"github.com/kubeswift-io/gpucellpool/internal/provisioner"
	"github.com/kubeswift-io/gpucellpool/internal/workload"
)

// The two-cluster harness. The whole point of this operator is that it spans two
// independent clusters, so the tests do too: a real outer apiserver holding
// GPUCellPools and (stub) KubeSwift kinds, and a real inner apiserver holding
// Nodes and Pods, reached only through a kubeconfig in a Secret.
//
// Everything the one-GPU lab cannot exercise — two cells, aggregation, drain
// gating, credential failure — is proven here.
var (
	outerEnv, innerEnv *envtest.Environment
	outerCfg, innerCfg *rest.Config
	outerClient        client.Client
	innerClientset     kubernetes.Interface
	testScheme         = runtime.NewScheme()
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Fprintln(os.Stderr, "KUBEBUILDER_ASSETS is unset; skipping envtest suites (run: make test)")
		os.Exit(m.Run())
	}

	utilMust(cellsv1alpha1.AddToScheme(testScheme))
	utilMust(corev1.AddToScheme(testScheme))
	utilMust(resourceapi.AddToScheme(testScheme))

	root, err := filepath.Abs("../..")
	utilMust(err)

	outerEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join(root, "config", "crd", "bases"),
			filepath.Join(root, "test", "crd"),
		},
		ErrorIfCRDPathMissing: true,
	}
	outerCfg, err = outerEnv.Start()
	utilMust(err)

	innerEnv = &envtest.Environment{}
	innerCfg, err = innerEnv.Start()
	utilMust(err)

	outerClient, err = client.New(outerCfg, client.Options{Scheme: testScheme})
	utilMust(err)
	innerClientset, err = kubernetes.NewForConfig(innerCfg)
	utilMust(err)

	code := m.Run()

	_ = outerEnv.Stop()
	_ = innerEnv.Stop()
	os.Exit(code)
}

func utilMust(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "test setup: %v\n", err)
		os.Exit(1)
	}
}

func requireEnvtest(t *testing.T) {
	t.Helper()
	if outerCfg == nil {
		t.Skip("envtest not available (KUBEBUILDER_ASSETS unset)")
	}
}

// kubeconfigFor renders a kubeconfig for an envtest apiserver, so the reconciler
// reaches the inner cluster exactly as it would in production: through a Secret.
func kubeconfigFor(cfg *rest.Config) []byte {
	c := clientcmdapi.NewConfig()
	c.Clusters["inner"] = &clientcmdapi.Cluster{
		Server:                   cfg.Host,
		CertificateAuthorityData: cfg.CAData,
	}
	c.AuthInfos["inner"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cfg.CertData,
		ClientKeyData:         cfg.KeyData,
	}
	c.Contexts["inner"] = &clientcmdapi.Context{Cluster: "inner", AuthInfo: "inner"}
	c.CurrentContext = "inner"
	raw, err := clientcmd.Write(*c)
	utilMust(err)
	return raw
}

// testFixture is one isolated pool: its own namespace in both clusters.
type testFixture struct {
	t          *testing.T
	ns         string
	pool       *cellsv1alpha1.GPUCellPool
	reconciler *GPUCellPoolReconciler
	now        time.Time
}

func newFixture(t *testing.T, mutate func(*cellsv1alpha1.GPUCellPool)) *testFixture {
	t.Helper()
	requireEnvtest(t)
	ctx := context.Background()

	ns := "pool-" + randSuffix(t.Name())
	utilMust(outerClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))

	// The credential the pool uses to reach the inner cluster.
	utilMust(outerClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "workload-kubeconfig"},
		Data:       map[string][]byte{"value": kubeconfigFor(innerCfg)},
	}))
	utilMust(outerClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "join"},
		Data:       map[string][]byte{"user-data": []byte("#cloud-config\n")},
	}))

	pool := &cellsv1alpha1.GPUCellPool{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "cells"},
		Spec: cellsv1alpha1.GPUCellPoolSpec{
			Replicas: 1,
			Cell: cellsv1alpha1.CellSpec{
				GuestTemplate: runtime.RawExtension{Raw: []byte(
					`{"guestClassRef":{"name":"gpu-worker"},"imageRef":{"name":"noble"}}`)},
				GPU: cellsv1alpha1.CellGPUSpec{
					Count:   1,
					Backend: cellsv1alpha1.GPUBackendDRA,
					DRA:     &cellsv1alpha1.CellGPUDRASpec{ResourceClaimTemplateName: "single-vfio-gpu", Tier: "pcie"},
				},
			},
			Bootstrap: cellsv1alpha1.BootstrapSpec{
				Provider:      cellsv1alpha1.BootstrapProviderOpaque,
				JoinSecretRef: &corev1.LocalObjectReference{Name: "join"},
				Hostname:      "CellName",
			},
			WorkloadCluster: cellsv1alpha1.WorkloadClusterSpec{
				KubeconfigSecretRef: corev1.LocalObjectReference{Name: "workload-kubeconfig"},
				Key:                 "value",
				Node:                &cellsv1alpha1.WorkloadNodeSpec{Labels: map[string]string{"gpu": "on"}},
			},
		},
	}
	if mutate != nil {
		mutate(pool)
	}
	utilMust(outerClient.Create(ctx, pool))

	f := &testFixture{t: t, ns: ns, pool: pool, now: time.Now()}
	f.reconciler = &GPUCellPoolReconciler{
		Client:  outerClient,
		Scheme:  testScheme,
		Clients: workload.NewClientCache(),
		Clock:   func() time.Time { return f.now },
	}
	t.Cleanup(func() { f.cleanup() })
	return f
}

func (f *testFixture) cleanup() {
	ctx := context.Background()
	// Drop finalizers so the namespace can go away even mid-drain.
	guests := &unstructured.UnstructuredList{}
	guests.SetGroupVersionKind(provisioner.SwiftGuestGVK.GroupVersion().WithKind("SwiftGuestList"))
	if err := outerClient.List(ctx, guests, client.InNamespace(f.ns)); err == nil {
		for i := range guests.Items {
			g := &guests.Items[i]
			g.SetFinalizers(nil)
			_ = outerClient.Update(ctx, g)
			_ = outerClient.Delete(ctx, g)
		}
	}
	var pool cellsv1alpha1.GPUCellPool
	if err := outerClient.Get(ctx, types.NamespacedName{Namespace: f.ns, Name: "cells"}, &pool); err == nil {
		pool.Finalizers = nil
		_ = outerClient.Update(ctx, &pool)
		_ = outerClient.Delete(ctx, &pool)
	}
	nodes, err := innerClientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: cellsv1alpha1.LabelPool + "=cells",
	})
	if err == nil {
		for i := range nodes.Items {
			_ = innerClientset.CoreV1().Nodes().Delete(ctx, nodes.Items[i].Name, metav1.DeleteOptions{})
		}
	}
}

func (f *testFixture) reconcile() ctrl.Result {
	f.t.Helper()
	res, err := f.reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: f.ns, Name: "cells"},
	})
	if err != nil {
		f.t.Fatalf("Reconcile: %v", err)
	}
	return res
}

func (f *testFixture) reconcileExpectingError() error {
	f.t.Helper()
	_, err := f.reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: f.ns, Name: "cells"},
	})
	return err
}

func (f *testFixture) getPool() *cellsv1alpha1.GPUCellPool {
	f.t.Helper()
	var pool cellsv1alpha1.GPUCellPool
	if err := outerClient.Get(context.Background(), types.NamespacedName{Namespace: f.ns, Name: "cells"}, &pool); err != nil {
		f.t.Fatalf("get pool: %v", err)
	}
	return &pool
}

// patchPool mutates the pool spec the way an operator editing the object would.
func (f *testFixture) patchPool(mutate func(*cellsv1alpha1.GPUCellPool)) {
	f.t.Helper()
	pool := f.getPool()
	mutate(pool)
	if err := outerClient.Update(context.Background(), pool); err != nil {
		f.t.Fatalf("update pool: %v", err)
	}
}

func (f *testFixture) guests() []unstructured.Unstructured {
	f.t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(provisioner.SwiftGuestGVK.GroupVersion().WithKind("SwiftGuestList"))
	if err := outerClient.List(context.Background(), list, client.InNamespace(f.ns)); err != nil {
		f.t.Fatalf("list guests: %v", err)
	}
	return list.Items
}

// stampGuestRunning writes the status KubeSwift would write for a healthy cell.
func (f *testFixture) stampGuestRunning(name, ip, device, hostNode string) {
	f.t.Helper()
	ctx := context.Background()
	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(provisioner.SwiftGuestGVK)
	if err := outerClient.Get(ctx, types.NamespacedName{Namespace: f.ns, Name: name}, g); err != nil {
		f.t.Fatalf("get guest %s: %v", name, err)
	}
	g.Object["status"] = map[string]any{
		"phase":   "Running",
		"network": map[string]any{"primaryIP": ip},
		"gpu":     map[string]any{"devices": []any{device}, "nodeName": hostNode, "hypervisor": "cloud-hypervisor"},
	}
	if err := outerClient.Status().Update(ctx, g); err != nil {
		f.t.Fatalf("stamp guest status: %v", err)
	}
}

func (f *testFixture) guestUID(name string) string {
	f.t.Helper()
	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(provisioner.SwiftGuestGVK)
	if err := outerClient.Get(context.Background(), types.NamespacedName{Namespace: f.ns, Name: name}, g); err != nil {
		f.t.Fatalf("get guest: %v", err)
	}
	return string(g.GetUID())
}

// joinNode creates the workload Node a booted cell's kubelet would register,
// carrying a real HAMi registration annotation.
func (f *testFixture) joinNode(name string, instance string, withHAMi bool) {
	f.t.Helper()
	labels := map[string]string{"gpu": "on", cellsv1alpha1.LabelPool: "cells"}
	if instance != "" {
		labels[cellsv1alpha1.LabelInstance] = instance
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
	}
	if withHAMi {
		node.Annotations = map[string]string{
			"hami.io/node-nvidia-register": `[{"id":"GPU-` + name + `","count":10,"devmem":8192,` +
				`"devcore":100,"type":"NVIDIA GeForce GTX 1080","mode":"hami-core","health":true}]`,
		}
	}
	created, err := innerClientset.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{})
	if err != nil {
		f.t.Fatalf("create node %s: %v", name, err)
	}
	created.Status.Conditions = []corev1.NodeCondition{{
		Type: corev1.NodeReady, Status: corev1.ConditionTrue, LastHeartbeatTime: metav1.Now(),
	}}
	if _, err := innerClientset.CoreV1().Nodes().UpdateStatus(context.Background(), created, metav1.UpdateOptions{}); err != nil {
		f.t.Fatalf("set node ready: %v", err)
	}
}

// killKubelet makes a Node look like one nobody is heartbeating for: the object
// stays, Ready stays True, only the heartbeat goes cold. That is precisely the
// shape of a Node left behind by a deleted VM, and the only thing that
// distinguishes it from the same object after a live replacement adopted it.
//
// envtest runs no kubelet, so nothing renews a Lease here and liveness resolves
// from the Ready condition's heartbeat.
func (f *testFixture) killKubelet(name string) {
	f.t.Helper()
	node, err := innerClientset.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		f.t.Fatalf("get node %s: %v", name, err)
	}
	for i := range node.Status.Conditions {
		if node.Status.Conditions[i].Type == corev1.NodeReady {
			node.Status.Conditions[i].LastHeartbeatTime = metav1.NewTime(time.Now().Add(-30 * time.Minute))
		}
	}
	if _, err := innerClientset.CoreV1().Nodes().UpdateStatus(context.Background(), node, metav1.UpdateOptions{}); err != nil {
		f.t.Fatalf("stop node heartbeat: %v", err)
	}
}

func (f *testFixture) node(name string) *corev1.Node {
	f.t.Helper()
	n, err := innerClientset.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		f.t.Fatalf("get node %s: %v", name, err)
	}
	return n
}

// gpuPod puts a HAMi-annotated workload on a cell's node.
func (f *testFixture) gpuPod(name, node string) {
	f.t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: name,
			Annotations: map[string]string{
				"hami.io/vgpu-devices-allocated": "GPU-" + node + ",NVIDIA,3000,30:;",
			},
		},
		Spec: corev1.PodSpec{
			NodeName:   node,
			Containers: []corev1.Container{{Name: "c", Image: "busybox"}},
		},
	}
	created, err := innerClientset.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
	if err != nil {
		f.t.Fatalf("create pod: %v", err)
	}
	created.Status.Phase = corev1.PodRunning
	if _, err := innerClientset.CoreV1().Pods("default").UpdateStatus(context.Background(), created, metav1.UpdateOptions{}); err != nil {
		f.t.Fatalf("set pod running: %v", err)
	}
	f.t.Cleanup(func() {
		_ = innerClientset.CoreV1().Pods("default").Delete(context.Background(), name,
			*metav1.NewDeleteOptions(0))
	})
}

func randSuffix(seed string) string {
	h := uint32(2166136261)
	for i := 0; i < len(seed); i++ {
		h ^= uint32(seed[i])
		h *= 16777619
	}
	const alphabet = "abcdefghijklmnopqrstuvwxyz"
	out := make([]byte, 8)
	for i := range out {
		out[i] = alphabet[h%26]
		h /= 26
		if h == 0 {
			h = uint32(i+1) * 7919
		}
	}
	return string(out)
}

// Aliases and builders used by the reconcile tests.
type (
	unstructuredObj = unstructured.Unstructured
	corev1Secret    = corev1.Secret
)

var (
	swiftGuestGVK = provisioner.SwiftGuestGVK
	seedGVK       = provisioner.SwiftSeedProfileGVK
)

func newResourceSlice(node, driver, device string) *resourceapi.ResourceSlice {
	return &resourceapi.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: node + "-" + device},
		Spec: resourceapi.ResourceSliceSpec{
			Driver:   driver,
			Pool:     resourceapi.ResourcePool{Name: node, ResourceSliceCount: 1},
			NodeName: ptrTo(node),
			Devices:  []resourceapi.Device{{Name: device}},
		},
	}
}

// newUnreadySlice publishes a device on a node where vfio-pci is not loaded: it
// cannot back a VM, so it is published-but-not-free.
func newUnreadySlice(node, driver, device string) *resourceapi.ResourceSlice {
	s := newResourceSlice(node, driver, device)
	s.Spec.Devices[0].Attributes = map[resourceapi.QualifiedName]resourceapi.DeviceAttribute{
		"vfioReady": {BoolValue: ptrTo(false)},
	}
	return s
}

func ptrTo[T any](v T) *T { return &v }

// patchPoolStatus writes the pool's status directly, to set up a state the
// reconciler would otherwise take several passes to reach.
func (f *testFixture) patchPoolStatus(mutate func(*cellsv1alpha1.GPUCellPool)) {
	f.t.Helper()
	pool := f.getPool()
	mutate(pool)
	if err := outerClient.Status().Update(context.Background(), pool); err != nil {
		f.t.Fatalf("update pool status: %v", err)
	}
}

// deleteGuest removes a cell's guest out from under the pool, clearing the drain
// finalizer first so the delete completes.
func (f *testFixture) deleteGuest(name string) {
	f.t.Helper()
	ctx := context.Background()
	g := &unstructured.Unstructured{}
	g.SetGroupVersionKind(provisioner.SwiftGuestGVK)
	if err := outerClient.Get(ctx, types.NamespacedName{Namespace: f.ns, Name: name}, g); err != nil {
		f.t.Fatalf("get guest %s: %v", name, err)
	}
	g.SetFinalizers(nil)
	if err := outerClient.Update(ctx, g); err != nil {
		f.t.Fatalf("clear finalizers on %s: %v", name, err)
	}
	if err := outerClient.Delete(ctx, g); err != nil {
		f.t.Fatalf("delete guest %s: %v", name, err)
	}
}
