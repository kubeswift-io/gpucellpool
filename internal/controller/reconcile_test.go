package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
	"github.com/kubeswift-io/gpucellpool/internal/inventory"
)

func condition(t *testing.T, pool *cellsv1alpha1.GPUCellPool, typ string) *metav1.Condition {
	t.Helper()
	return meta.FindStatusCondition(pool.Status.Conditions, typ)
}

func TestReconcileCreatesACell(t *testing.T) {
	f := newFixture(t, nil)
	f.reconcile()

	guests := f.guests()
	if len(guests) != 1 {
		t.Fatalf("got %d guests, want 1", len(guests))
	}
	g := guests[0]
	if g.GetName() != "cells-0" {
		t.Errorf("guest name = %q, want cells-0", g.GetName())
	}
	if g.GetLabels()[cellsv1alpha1.LabelPool] != "cells" {
		t.Errorf("pool label missing: %v", g.GetLabels())
	}
	if len(g.GetOwnerReferences()) != 1 || g.GetOwnerReferences()[0].Kind != "GPUCellPool" {
		t.Errorf("owner reference = %v", g.GetOwnerReferences())
	}
	// Without this, a cell can be deleted out from under running HAMi workloads.
	if len(g.GetFinalizers()) == 0 || g.GetFinalizers()[0] != cellsv1alpha1.FinalizerCellDrain {
		t.Errorf("drain finalizer missing: %v", g.GetFinalizers())
	}

	spec := g.Object["spec"].(map[string]any)
	if spec["runPolicy"] != "Always" {
		t.Errorf("runPolicy = %v", spec["runPolicy"])
	}
	if enabled := spec["migration"].(map[string]any)["enabled"]; enabled != false {
		t.Errorf("migration.enabled = %v, want false: cells are replaced, not migrated", enabled)
	}
	if spec["gpuResourceClaim"].(map[string]any)["resourceClaimTemplateName"] != "single-vfio-gpu" {
		t.Errorf("gpuResourceClaim = %v", spec["gpuResourceClaim"])
	}

	// The per-cell seed carries the hostname; that is what makes the workload
	// Node name equal the cell name.
	seed := &unstructuredObj{}
	seed.SetGroupVersionKind(seedGVK)
	if err := outerClient.Get(context.Background(),
		types.NamespacedName{Namespace: f.ns, Name: "cells-0-seed"}, seed); err != nil {
		t.Fatalf("seed profile not created: %v", err)
	}
	seedSpec := seed.Object["spec"].(map[string]any)
	if seedSpec["userData"] != "" {
		t.Errorf("userData = %v, want empty: the token lives in the Secret", seedSpec["userData"])
	}

	pool := f.getPool()
	if pool.Status.Replicas != 1 || pool.Status.ReadyCells != 0 {
		t.Errorf("status replicas/ready = %d/%d", pool.Status.Replicas, pool.Status.ReadyCells)
	}
	if len(pool.Status.Cells) != 1 || pool.Status.Cells[0].Phase != cellsv1alpha1.CellPhaseAllocatingGPU {
		t.Errorf("cells = %+v, want one in AllocatingGPU", pool.Status.Cells)
	}
}

func TestReconcileRendersAPerCellBootstrapSecret(t *testing.T) {
	f := newFixture(t, func(p *cellsv1alpha1.GPUCellPool) {
		p.Spec.Cell.NodeIPFrom = "node"
	})
	// A template that needs per-cell values.
	src := &corev1Secret{}
	if err := outerClient.Get(context.Background(),
		types.NamespacedName{Namespace: f.ns, Name: "join"}, src); err != nil {
		t.Fatalf("get join secret: %v", err)
	}
	src.Data["user-data"] = []byte(`#cloud-config
hostname: {{ cellName }}
labels: {{ nodeLabels }}
iface: {{ nodeIPInterface }}
gpus: {{ expectedGPUs }}
`)
	if err := outerClient.Update(context.Background(), src); err != nil {
		t.Fatalf("update join secret: %v", err)
	}

	f.reconcile()

	rendered := &corev1Secret{}
	if err := outerClient.Get(context.Background(),
		types.NamespacedName{Namespace: f.ns, Name: "cells-0-bootstrap"}, rendered); err != nil {
		t.Fatalf("per-cell bootstrap secret not created: %v", err)
	}
	got := string(rendered.Data["user-data"])
	for _, want := range []string{
		"hostname: cells-0",
		"cells.kubeswift.io/cell=cells-0",
		"gpu=on",
		"iface: node",
		"gpus: 1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered cloud-init missing %q:\n%s", want, got)
		}
	}
	// The user's template must not be modified.
	if err := outerClient.Get(context.Background(),
		types.NamespacedName{Namespace: f.ns, Name: "join"}, src); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src.Data["user-data"]), "{{ cellName }}") {
		t.Error("the operator rewrote the user's join Secret")
	}

	// The seed profile must reference the RENDERED secret, not the template.
	seed := &unstructuredObj{}
	seed.SetGroupVersionKind(seedGVK)
	if err := outerClient.Get(context.Background(),
		types.NamespacedName{Namespace: f.ns, Name: "cells-0-seed"}, seed); err != nil {
		t.Fatalf("get seed: %v", err)
	}
	ref := seed.Object["spec"].(map[string]any)["userDataFrom"].(map[string]any)["secretKeyRef"].(map[string]any)
	if ref["name"] != "cells-0-bootstrap" {
		t.Errorf("seed references %v, want the per-cell secret", ref["name"])
	}

	// A second pass must not churn the Secret.
	before := rendered.ResourceVersion
	f.reconcile()
	if err := outerClient.Get(context.Background(),
		types.NamespacedName{Namespace: f.ns, Name: "cells-0-bootstrap"}, rendered); err != nil {
		t.Fatal(err)
	}
	if rendered.ResourceVersion != before {
		t.Error("bootstrap secret rewritten on a no-op reconcile")
	}
}

func TestReconcileRejectsATemplateWithAnUnknownToken(t *testing.T) {
	f := newFixture(t, nil)
	src := &corev1Secret{}
	if err := outerClient.Get(context.Background(),
		types.NamespacedName{Namespace: f.ns, Name: "join"}, src); err != nil {
		t.Fatal(err)
	}
	src.Data["user-data"] = []byte("hostname: {{ cellname }}\n") // typo
	if err := outerClient.Update(context.Background(), src); err != nil {
		t.Fatal(err)
	}

	// Loud, not silent: a cell must not boot with a half-substituted boot script.
	err := f.reconcileExpectingError()
	if err == nil {
		t.Fatal("reconcile accepted a template with an unknown token")
	}
	if !strings.Contains(err.Error(), "cellname") {
		t.Errorf("error does not name the token: %v", err)
	}
	if len(f.guests()) != 0 {
		t.Error("a cell was created despite the unrenderable template")
	}
}

func TestReconcileReachesReadyThroughBothLayers(t *testing.T) {
	f := newFixture(t, nil)
	f.reconcile() // create

	// Outer: KubeSwift allocates a GPU and runs the VM.
	f.stampGuestRunning("cells-0", "10.77.0.5", "0000:01:00.0", "boba")
	f.reconcile()
	if p := f.getPool(); p.Status.Cells[0].Phase != cellsv1alpha1.CellPhaseJoining {
		t.Fatalf("phase = %s, want Joining once the VM is up", p.Status.Cells[0].Phase)
	}

	// Inner: the kubelet registers, but HAMi has not advertised the GPU yet.
	f.joinNode("cells-0", f.guestUID("cells-0"), false)
	f.reconcile()
	pool := f.getPool()
	if pool.Status.Cells[0].Phase != cellsv1alpha1.CellPhaseAwaitingGPUCapacity {
		t.Fatalf("phase = %s, want AwaitingGPUCapacity", pool.Status.Cells[0].Phase)
	}
	if pool.Status.ReadyCells != 0 {
		t.Error("a joined node with no GPU counted as Ready")
	}
	if c := condition(t, pool, cellsv1alpha1.ConditionReady); c.Status != metav1.ConditionFalse {
		t.Error("pool Ready=True with no GPU advertised")
	}

	// HAMi registers the device.
	node := f.node("cells-0")
	node.Annotations = map[string]string{
		"hami.io/node-nvidia-register": `[{"id":"GPU-0","count":10,"devmem":8192,"devcore":100,` +
			`"type":"NVIDIA GeForce GTX 1080","mode":"hami-core","health":true}]`,
	}
	if _, err := innerClientset.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("annotate node: %v", err)
	}
	f.reconcile()

	pool = f.getPool()
	if pool.Status.Cells[0].Phase != cellsv1alpha1.CellPhaseReady {
		t.Fatalf("phase = %s, want Ready; message: %s", pool.Status.Cells[0].Phase, pool.Status.Cells[0].Message)
	}
	if pool.Status.ReadyCells != 1 {
		t.Errorf("readyCells = %d, want 1", pool.Status.ReadyCells)
	}
	if c := condition(t, pool, cellsv1alpha1.ConditionReady); c.Status != metav1.ConditionTrue {
		t.Errorf("Ready = %+v", c)
	}

	// Both capacities are reported, and they are not the same number.
	if pool.Status.PhysicalCapacity == nil || pool.Status.PhysicalCapacity.GPUs != 1 {
		t.Errorf("physicalCapacity = %+v, want 1 whole GPU", pool.Status.PhysicalCapacity)
	}
	wc := pool.Status.WorkloadCapacity
	if wc == nil || wc.GPUDevices != 1 {
		t.Fatalf("workloadCapacity = %+v", wc)
	}
	if got := wc.GPUMemory.Total.Value(); got != 8192*1024*1024 {
		t.Errorf("workload memory total = %d bytes, want 8Gi", got)
	}
	if !wc.Homogeneous || wc.GPUCompute == nil || wc.GPUCompute.Total != 100 {
		t.Errorf("compute = %+v", wc.GPUCompute)
	}
	if cell := pool.Status.Cells[0]; cell.NodeName != "cells-0" || cell.HostNode != "boba" ||
		len(cell.Devices) != 1 || cell.CapacityDevices != 1 {
		t.Errorf("cell status lost a layer: %+v", cell)
	}
}

func TestReconcilePatchesIdentityLabelsOntoTheNode(t *testing.T) {
	f := newFixture(t, nil)
	f.reconcile()
	f.stampGuestRunning("cells-0", "10.77.0.5", "0000:01:00.0", "boba")
	f.reconcile()

	// A kubelet that did not self-apply the identity labels: the controller must
	// patch them, which is the reliable path.
	f.joinNode("cells-0", "", true)
	f.reconcile()

	node := f.node("cells-0")
	uid := f.guestUID("cells-0")
	if node.Labels[cellsv1alpha1.LabelInstance] != uid {
		t.Errorf("instance label = %q, want the guest UID %q", node.Labels[cellsv1alpha1.LabelInstance], uid)
	}
	if node.Labels[cellsv1alpha1.LabelCell] != "cells-0" || node.Labels[cellsv1alpha1.LabelCellIndex] != "0" {
		t.Errorf("identity labels = %v", node.Labels)
	}
	if node.Labels["gpu"] != "on" {
		t.Error("user-declared label dropped")
	}
}

func TestReconcileReapsAPhantomNodeWhoseKubeletIsGone(t *testing.T) {
	f := newFixture(t, nil)
	f.reconcile()
	f.stampGuestRunning("cells-0", "10.77.0.5", "0000:01:00.0", "boba")
	f.reconcile()

	// A Node with this cell's NAME, a previous incarnation's UID, and no kubelet
	// behind it. Adopting it would report a phantom Ready cell, with HAMi
	// advertising capacity for a GPU that no longer exists.
	f.joinNode("cells-0", "uid-from-a-previous-life", true)
	f.killKubelet("cells-0")
	f.reconcile()

	if _, err := innerClientset.CoreV1().Nodes().Get(context.Background(), "cells-0", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("phantom node survived: %v", err)
	}
	pool := f.getPool()
	if pool.Status.Cells[0].Phase == cellsv1alpha1.CellPhaseReady {
		t.Error("cell went Ready off a phantom node")
	}
}

// TestReconcileAdoptsANodeItsOwnKubeletTookOver is the other half of #14, and the
// case that cost a cell: a replacement's kubelet registers under the reused node
// name, adopts the EXISTING Node object, and keeps the identity label it finds
// there. By label alone that is indistinguishable from a phantom — but deleting it
// is unrecoverable, because a kubelet whose Node is removed under it does not
// re-register. It logs "Error updating node status, will retry" and the cell waits
// forever.
func TestReconcileAdoptsANodeItsOwnKubeletTookOver(t *testing.T) {
	f := newFixture(t, nil)
	f.reconcile()
	f.stampGuestRunning("cells-0", "10.77.0.5", "0000:01:00.0", "boba")
	f.reconcile()

	// Same foreign label as above — the difference is only that a kubelet is
	// heartbeating for it (joinNode stamps a fresh heartbeat).
	f.joinNode("cells-0", "uid-from-a-previous-life", true)
	f.reconcile()

	node, err := innerClientset.CoreV1().Nodes().Get(context.Background(), "cells-0", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the live cell's Node was deleted under its kubelet: %v", err)
	}
	if got := node.Labels[cellsv1alpha1.LabelInstance]; got != f.guestUID("cells-0") {
		t.Errorf("instance label = %q, want the current guest UID %q — an adopted Node must be re-labelled, "+
			"or it looks stale again on the next pass", got, f.guestUID("cells-0"))
	}
	if p := f.getPool(); p.Status.Cells[0].Phase != cellsv1alpha1.CellPhaseReady {
		t.Errorf("cell phase = %s, want Ready: an adopted Node is this cell's Node", p.Status.Cells[0].Phase)
	}
}

// TestReconcileReapsTheNodeOfARetiredCell covers what SET UP the collision: a
// failed cell's row is dropped when the pool no longer wants its index, and
// nothing else remembers the cell afterwards. Leaving its Node behind is what a
// later replacement adopts.
func TestReconcileReapsTheNodeOfARetiredCell(t *testing.T) {
	f := newFixture(t, nil)
	f.reconcile()
	f.stampGuestRunning("cells-0", "10.77.0.5", "0000:01:00.0", "boba")
	f.joinNode("cells-0", f.guestUID("cells-0"), true)
	f.reconcile()
	if p := f.getPool(); p.Status.Cells[0].Phase != cellsv1alpha1.CellPhaseReady {
		t.Fatalf("setup: phase = %s", p.Status.Cells[0].Phase)
	}

	// The guest goes away and the pool stops wanting the slot, which is the moment
	// the status row is dropped. The kubelet has only just died, so the Node still
	// LOOKS live — and that is the whole difficulty: a cleanup that only gets this
	// one chance finds a live-looking Node, correctly declines to delete it, and
	// never looks again. Measured on hardware before this was a sweep: the Node sat
	// there NotReady for four minutes and nothing ever came back for it.
	f.deleteGuest("cells-0")
	f.patchPool(func(p *cellsv1alpha1.GPUCellPool) { p.Spec.Replicas = 0 })
	f.reconcile()

	if _, err := innerClientset.CoreV1().Nodes().Get(context.Background(), "cells-0", metav1.GetOptions{}); err != nil {
		t.Fatalf("a Node whose kubelet had not yet gone cold was deleted: %v", err)
	}

	// Now the heartbeat goes cold. A later pass must pick it up, with no status row
	// left to remind it the cell ever existed.
	f.killKubelet("cells-0")
	f.reconcile()

	if _, err := innerClientset.CoreV1().Nodes().Get(context.Background(), "cells-0", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the retired cell's Node was left behind: %v", err)
	}
}

// TestReconcileLeavesForeignNodesAlone keeps the orphan sweep from becoming a
// licence to delete nodes: it is keyed on this pool's label, and a Node without it
// is none of the pool's business however dead its kubelet looks.
func TestReconcileLeavesForeignNodesAlone(t *testing.T) {
	f := newFixture(t, nil)
	f.reconcile()
	f.stampGuestRunning("cells-0", "10.77.0.5", "0000:01:00.0", "boba")
	f.joinNode("cells-0", f.guestUID("cells-0"), true)

	// A node belonging to somebody else, cold and unlabelled.
	other := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "someone-elses-worker"}}
	if _, err := innerClientset.CoreV1().Nodes().Create(context.Background(), other, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create foreign node: %v", err)
	}
	t.Cleanup(func() {
		_ = innerClientset.CoreV1().Nodes().Delete(context.Background(), "someone-elses-worker", metav1.DeleteOptions{})
	})
	f.reconcile()

	if _, err := innerClientset.CoreV1().Nodes().Get(context.Background(), "someone-elses-worker", metav1.GetOptions{}); err != nil {
		t.Fatalf("the pool deleted a Node it does not own: %v", err)
	}
}

func TestUnreachableWorkloadClusterFreezesEverything(t *testing.T) {
	f := newFixture(t, nil)
	f.reconcile()
	f.stampGuestRunning("cells-0", "10.77.0.5", "0000:01:00.0", "boba")
	f.joinNode("cells-0", f.guestUID("cells-0"), true)
	f.reconcile()
	if p := f.getPool(); p.Status.Cells[0].Phase != cellsv1alpha1.CellPhaseReady {
		t.Fatalf("setup: phase = %s", p.Status.Cells[0].Phase)
	}

	// Break the credential: the kubeconfig now points somewhere that does not answer.
	secret := &corev1Secret{}
	if err := outerClient.Get(context.Background(),
		types.NamespacedName{Namespace: f.ns, Name: "workload-kubeconfig"}, secret); err != nil {
		t.Fatalf("get secret: %v", err)
	}
	secret.Data["value"] = []byte(`apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://127.0.0.1:1"}}]
contexts: [{name: c, context: {cluster: c, user: u}}]
current-context: c
users: [{name: u, user: {}}]
`)
	if err := outerClient.Update(context.Background(), secret); err != nil {
		t.Fatalf("update secret: %v", err)
	}

	f.reconcile()
	pool := f.getPool()

	if c := condition(t, pool, cellsv1alpha1.ConditionWorkloadClusterReachable); c.Status != metav1.ConditionFalse {
		t.Errorf("WorkloadClusterReachable = %+v, want False", c)
	}
	// The cell is frozen, not condemned: we know nothing new, so we conclude
	// nothing new.
	if got := pool.Status.Cells[0].Phase; got != cellsv1alpha1.CellPhaseReady {
		t.Errorf("phase moved to %s while the workload cluster was unreachable", got)
	}
	// And nothing was destroyed.
	if len(f.guests()) != 1 {
		t.Error("a cell was deleted while the workload cluster was unreachable")
	}
	// Capacity is retained with its timestamp rather than zeroed.
	if pool.Status.WorkloadCapacity == nil || pool.Status.WorkloadCapacity.GPUDevices != 1 {
		t.Errorf("capacity was zeroed on an unreadable provider: %+v", pool.Status.WorkloadCapacity)
	}
}

func TestScaleDownDrainsAndWaitsForWorkloads(t *testing.T) {
	f := newFixture(t, func(p *cellsv1alpha1.GPUCellPool) { p.Spec.Replicas = 1 })
	f.reconcile()
	f.stampGuestRunning("cells-0", "10.77.0.5", "0000:01:00.0", "boba")
	f.joinNode("cells-0", f.guestUID("cells-0"), true)
	f.reconcile()

	// A workload takes a share of the cell's GPU.
	f.gpuPod("holder", "cells-0")

	// Scale to zero.
	pool := f.getPool()
	pool.Spec.Replicas = 0
	if err := outerClient.Update(context.Background(), pool); err != nil {
		t.Fatalf("scale down: %v", err)
	}

	f.reconcile()
	if len(f.guests()) != 1 {
		t.Fatal("cell deleted while a workload still held its GPU")
	}
	if !f.node("cells-0").Spec.Unschedulable {
		t.Error("node not cordoned before counting allocations")
	}
	f.reconcile()
	if len(f.guests()) != 1 {
		t.Fatal("cell deleted on a later pass despite the held allocation")
	}

	// Release it; now the cell may go.
	if err := innerClientset.CoreV1().Pods("default").Delete(context.Background(), "holder",
		*metav1.NewDeleteOptions(0)); err != nil {
		t.Fatalf("delete pod: %v", err)
	}
	f.reconcile()

	guests := f.guests()
	if len(guests) == 1 && guests[0].GetDeletionTimestamp() == nil {
		t.Error("cell not deleted after its GPU was released")
	}
}

func TestPoolDeletionTearsDownCells(t *testing.T) {
	f := newFixture(t, nil)
	f.reconcile()
	f.stampGuestRunning("cells-0", "10.77.0.5", "0000:01:00.0", "boba")
	f.joinNode("cells-0", f.guestUID("cells-0"), true)
	f.reconcile()

	pool := f.getPool()
	if err := outerClient.Delete(context.Background(), pool); err != nil {
		t.Fatalf("delete pool: %v", err)
	}

	// First pass: drains and removes the cell (no workloads hold it).
	f.reconcile()
	if _, err := innerClientset.CoreV1().Nodes().Get(context.Background(), "cells-0", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Error("workload Node not removed before the VM")
	}

	// Second pass: no cells left, so the pool finalizer goes.
	f.reconcile()
	err := outerClient.Get(context.Background(), types.NamespacedName{Namespace: f.ns, Name: "cells"}, pool)
	if err == nil && len(pool.Finalizers) > 0 {
		t.Errorf("pool finalizer still held: %v", pool.Finalizers)
	}
}

// The drain window on pool deletion runs from the DELETE, not from whenever the
// cell last changed phase.
//
// Before the fix the clock was cell.LastTransitionTime, which for a healthy cell
// is when it went Ready and never moves again. Any cell that had been Ready for
// longer than drainTimeout was therefore already expired on the first reconcile
// after the delete, so deletion.policy: Drain took the GPU out from under a live
// workload immediately — silently, since the deletion path also published no
// status. Reproduced on hardware: a cell Ready for 209s with drainTimeout 1m lost
// its GPU 6s after the delete while a HAMi pod still held it.
func TestPoolDeletionDrainWindowRunsFromTheDeleteNotTheLastPhaseChange(t *testing.T) {
	f := newFixture(t, func(p *cellsv1alpha1.GPUCellPool) {
		p.Spec.Replicas = 1
		p.Spec.Deletion = &cellsv1alpha1.DeletionSpec{
			Policy:       cellsv1alpha1.DeletionPolicyDrain,
			DrainTimeout: &metav1.Duration{Duration: 10 * time.Minute},
		}
	})
	f.reconcile()
	f.stampGuestRunning("cells-0", "10.77.0.5", "0000:01:00.0", "boba")
	f.joinNode("cells-0", f.guestUID("cells-0"), true)
	f.reconcile()

	// A workload holds the cell's GPU.
	f.gpuPod("holder", "cells-0")

	// The cell has been Ready far longer than drainTimeout — the steady state for
	// any pool that has been up for a while.
	pool := f.getPool()
	old := metav1.NewTime(f.now.Add(-1 * time.Hour))
	pool.Status.Cells[0].LastTransitionTime = &old
	if err := outerClient.Status().Update(context.Background(), pool); err != nil {
		t.Fatalf("age the cell: %v", err)
	}

	if err := outerClient.Delete(context.Background(), f.getPool()); err != nil {
		t.Fatalf("delete pool: %v", err)
	}

	f.reconcile()
	if len(f.guests()) != 1 || f.guests()[0].GetDeletionTimestamp() != nil {
		t.Fatal("cell torn down immediately on pool deletion while a workload held its GPU")
	}

	// The wait is also observable, which it was not before: the row kept saying
	// Ready with no message for the whole teardown.
	pool = f.getPool()
	if len(pool.Status.Cells) != 1 ||
		pool.Status.Cells[0].Phase != cellsv1alpha1.CellPhaseDraining ||
		pool.Status.Cells[0].Reason != cellsv1alpha1.ReasonWaitingForAllocations {
		t.Errorf("drain state not published: %+v", pool.Status.Cells)
	}

	// Past the window, the pool deletion still wins — a pool stuck draining
	// forever is worse than a rough teardown.
	f.now = f.now.Add(11 * time.Minute)
	f.reconcile()
	if guests := f.guests(); len(guests) == 1 && guests[0].GetDeletionTimestamp() == nil {
		t.Error("drain did not time out after drainTimeout elapsed from the delete")
	}
}

// A cell that fails keeps the reason it failed for, and says so once.
//
// The reason used to survive a single reconcile: cellStatus wrote it, that write
// woke the reconciler, and AdvanceCell's terminal-Failed branch returns a bare
// phase whose empty Reason/Message overwrote it. Measured on hardware — three
// consecutive cells failing on an expired join credential, every Failed row
// reading `reason: "" message: ""` at 3-second polling. It also left
// suspectCredentialFailures, which keys on ReasonNodeNeverRegistered, with
// nothing to count, and the exhaustion message ending in a bare colon.
func TestAFailedCellKeepsItsDiagnosisAndEmitsAnEvent(t *testing.T) {
	f := newFixture(t, func(p *cellsv1alpha1.GPUCellPool) {
		p.Spec.Bootstrap.ReadyTimeout = &metav1.Duration{Duration: 5 * time.Minute}
	})
	f.reconcile()
	// The VM runs and holds a GPU, but no workload Node ever registers — what a
	// credential the apiserver refuses looks like from here.
	f.stampGuestRunning("cells-0", "10.77.0.5", "0000:01:00.0", "boba")
	f.reconcile()
	if p := f.getPool(); p.Status.Cells[0].Reason != cellsv1alpha1.ReasonNodeNeverRegistered {
		t.Fatalf("reason = %q, want NodeNeverRegistered while joining", p.Status.Cells[0].Reason)
	}

	// Past the bootstrap budget it fails.
	f.now = f.now.Add(6 * time.Minute)
	_ = f.events() // drop everything recorded before the failure
	f.reconcile()

	pool := f.getPool()
	cell := pool.Status.Cells[0]
	if cell.Phase != cellsv1alpha1.CellPhaseFailed || cell.FailureCount != 1 {
		t.Fatalf("cell = %s/%d, want Failed after one failure", cell.Phase, cell.FailureCount)
	}
	if cell.Reason != cellsv1alpha1.ReasonNodeNeverRegistered || cell.Message == "" {
		t.Fatalf("failure not attributed: reason=%q message=%q", cell.Reason, cell.Message)
	}

	var failure string
	for _, e := range f.events() {
		if strings.Contains(e, cellsv1alpha1.ReasonNodeNeverRegistered) {
			failure = e
		}
	}
	if failure == "" {
		t.Error("no Event names the failure; a discarded GPU boot left no trace")
	} else if !strings.Contains(failure, "cells-0") {
		t.Errorf("failure Event does not name the cell: %q", failure)
	}

	// The reconcile the status write itself triggers is the one that used to erase
	// it, so a second pass is the whole point of this test.
	f.reconcile()
	f.reconcile()
	cell = f.getPool().Status.Cells[0]
	if cell.Reason != cellsv1alpha1.ReasonNodeNeverRegistered || cell.Message == "" {
		t.Errorf("diagnosis erased after the failure: reason=%q message=%q", cell.Reason, cell.Message)
	}
	// And it is not re-announced every pass.
	for _, e := range f.events() {
		if strings.Contains(e, cellsv1alpha1.ReasonNodeNeverRegistered) {
			t.Errorf("failure Event repeated on a later pass: %q", e)
		}
	}
}

// The same guard, for a pool whose GPUs come from the NATIVE backend.
//
// The two backends keep separate books for the same physical devices, and the
// inventory used to read the DRA one regardless. On a single-GPU cluster whose
// device was held by a native allocation, the pool announced "1 free GPU(s) in
// the infrastructure cluster" and created a cell per bootstrap timeout — a
// root-disk clone and a boot each, none of which could ever get a device.
// The DRA twin of this test could not catch it: a native allocation never
// appears in a ResourceClaim.
func TestNoFreeGPUStallsAPoolOnTheNativeBackend(t *testing.T) {
	f := newFixture(t, func(p *cellsv1alpha1.GPUCellPool) {
		p.Spec.Cell.GPU = cellsv1alpha1.CellGPUSpec{
			Count:   1,
			Backend: cellsv1alpha1.GPUBackendNative,
			Native: &cellsv1alpha1.CellGPUNativeSpec{
				GPUProfileRef: corev1.LocalObjectReference{Name: "cell-gpu"},
			},
		}
	})

	// The one GPU in the cluster is held — by anything: another pool, or a
	// sandbox, which is how this was found. Only the native ledger records it.
	node := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{
			"vfioReady": true,
			"gpus": []any{map[string]any{
				"index":       int64(0),
				"pciAddress":  "0000:01:00.0",
				"allocated":   true,
				"allocatedTo": "sandbox:other/holder",
			}},
		},
	}}
	node.SetGroupVersionKind(inventory.SwiftGPUNodeGVK)
	node.SetName("gpu-node-" + f.ns)
	if err := outerClient.Create(context.Background(), node); err != nil {
		t.Fatalf("create SwiftGPUNode: %v", err)
	}
	t.Cleanup(func() { _ = outerClient.Delete(context.Background(), node) })
	// Status is a subresource on the stub CRD, so it needs its own write.
	node.Object["status"] = map[string]any{
		"vfioReady": true,
		"gpus": []any{map[string]any{
			"index": int64(0), "pciAddress": "0000:01:00.0",
			"allocated": true, "allocatedTo": "sandbox:other/holder",
		}},
	}
	if err := outerClient.Status().Update(context.Background(), node); err != nil {
		t.Fatalf("write SwiftGPUNode status: %v", err)
	}

	f.reconcile()

	if n := len(f.guests()); n != 0 {
		t.Errorf("created %d guests while the only GPU was natively allocated", n)
	}
	pool := f.getPool()
	c := condition(t, pool, cellsv1alpha1.ConditionPhysicalGPUsAvailable)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != cellsv1alpha1.ReasonInsufficientGPUs {
		t.Errorf("PhysicalGPUsAvailable = %+v, want False/InsufficientGPUs", c)
	}
	if pool.Status.PhysicalCapacity == nil || pool.Status.PhysicalCapacity.FreeGPUsInCluster == nil ||
		*pool.Status.PhysicalCapacity.FreeGPUsInCluster != 0 {
		t.Errorf("physicalCapacity = %+v, want 0 free", pool.Status.PhysicalCapacity)
	}
}

func TestNoFreeGPUStallsInsteadOfQueueingGuests(t *testing.T) {
	// Two cells wanted, one GPU published and already claimed: the pool must stop
	// rather than create a guest that could never be scheduled.
	f := newFixture(t, func(p *cellsv1alpha1.GPUCellPool) { p.Spec.Replicas = 2 })

	// The device is published but NOT usable (vfio-pci is not loaded on its
	// node), so the driver is known to publish and known to have nothing free.
	//
	// This deliberately does not go through an allocated ResourceClaim: envtest's
	// apiserver silently prunes ResourceClaim.status.allocation, so a claim-based
	// fixture would prove nothing here. The allocation accounting itself is
	// covered by the unit tests in internal/inventory.
	slice := newUnreadySlice("boba", "gpu.kubeswift.io", "gpu-0000-01-00-0")
	if err := outerClient.Create(context.Background(), slice); err != nil {
		t.Fatalf("create slice: %v", err)
	}
	t.Cleanup(func() { _ = outerClient.Delete(context.Background(), slice) })

	f.reconcile()

	pool := f.getPool()
	if len(f.guests()) != 0 {
		t.Errorf("created %d guests with no free GPU", len(f.guests()))
	}
	c := condition(t, pool, cellsv1alpha1.ConditionPhysicalGPUsAvailable)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != cellsv1alpha1.ReasonInsufficientGPUs {
		t.Errorf("PhysicalGPUsAvailable = %+v, want False/InsufficientPhysicalGPU", c)
	}
}

func TestFailedCellIsNotReplacedBeforeItsBackoff(t *testing.T) {
	f := newFixture(t, nil)
	f.reconcile()

	// KubeSwift reports a terminal failure.
	g := &unstructuredObj{}
	g.SetGroupVersionKind(swiftGuestGVK)
	if err := outerClient.Get(context.Background(),
		types.NamespacedName{Namespace: f.ns, Name: "cells-0"}, g); err != nil {
		t.Fatalf("get guest: %v", err)
	}
	g.Object["status"] = map[string]any{
		"phase": "Failed",
		"conditions": []any{map[string]any{
			"type": "Resolved", "status": "False",
			"reason": "ResolutionFailed", "message": "SwiftImage not Ready",
		}},
	}
	if err := outerClient.Status().Update(context.Background(), g); err != nil {
		t.Fatalf("stamp failure: %v", err)
	}

	f.reconcile()
	pool := f.getPool()
	if pool.Status.Cells[0].Phase != cellsv1alpha1.CellPhaseFailed {
		t.Fatalf("phase = %s, want Failed", pool.Status.Cells[0].Phase)
	}
	if pool.Status.Cells[0].FailureCount != 1 {
		t.Errorf("failureCount = %d, want 1", pool.Status.Cells[0].FailureCount)
	}
	if pool.Status.FailedCells != 1 {
		t.Errorf("failedCells = %d", pool.Status.FailedCells)
	}

	// Immediately after failing, the cell must NOT be replaced.
	f.reconcile()
	if len(f.guests()) != 1 || f.guests()[0].GetDeletionTimestamp() != nil {
		t.Error("failed cell replaced before its 30s backoff expired")
	}

	// After the backoff, it is.
	f.now = f.now.Add(time.Minute)
	f.reconcile()
	guests := f.guests()
	if len(guests) == 1 && guests[0].GetDeletionTimestamp() == nil {
		t.Error("failed cell not replaced after its backoff expired")
	}
}
