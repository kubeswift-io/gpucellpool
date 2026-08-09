package workload

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
)

const testKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: c
  cluster: {server: https://example.invalid:6443, insecure-skip-tls-verify: true}
contexts:
- name: c
  context: {cluster: c, user: u}
current-context: c
users:
- name: u
  user: {token: t}
`

func secret(rv string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "gpu-cells", Name: "kubeconfig", ResourceVersion: rv},
		Data:       data,
	}
}

func TestClientCacheReusesAndRotates(t *testing.T) {
	built := 0
	c := NewClientCache()
	c.newClient = func(*rest.Config) (kubernetes.Interface, error) {
		built++
		return fake.NewSimpleClientset(), nil
	}

	s := secret("1", map[string][]byte{"value": []byte(testKubeconfig)})
	if _, err := c.For("gpu-cells/kubeconfig", s, ""); err != nil {
		t.Fatalf("For: %v", err)
	}
	// N pools against one workload cluster must share one connection set.
	if _, err := c.For("gpu-cells/kubeconfig", s, ""); err != nil {
		t.Fatalf("For (second): %v", err)
	}
	if built != 1 {
		t.Errorf("built %d clients, want 1 (the second call must hit the cache)", built)
	}

	// A rotated credential must be picked up without a restart.
	rotated := secret("2", map[string][]byte{"value": []byte(testKubeconfig)})
	if _, err := c.For("gpu-cells/kubeconfig", rotated, ""); err != nil {
		t.Fatalf("For (rotated): %v", err)
	}
	if built != 2 {
		t.Errorf("built %d clients, want 2 after rotation", built)
	}
	if c.Len() != 1 {
		t.Errorf("cache holds %d entries, want 1", c.Len())
	}

	c.Forget("gpu-cells/kubeconfig")
	if c.Len() != 0 {
		t.Error("Forget did not drop the entry")
	}
}

func TestClientCacheRejectsUnusableSecrets(t *testing.T) {
	c := NewClientCache()
	c.newClient = func(*rest.Config) (kubernetes.Interface, error) { return fake.NewSimpleClientset(), nil }

	if _, err := c.For("k", secret("1", map[string][]byte{}), ""); err == nil {
		t.Error("accepted a Secret with no kubeconfig key")
	}
	if _, err := c.For("k", secret("1", map[string][]byte{"value": []byte("not a kubeconfig")}), ""); err == nil {
		t.Error("accepted an unparseable kubeconfig")
	}
	// A non-default key must be honoured.
	if _, err := c.For("k", secret("1", map[string][]byte{"custom": []byte(testKubeconfig)}), "custom"); err != nil {
		t.Errorf("custom key rejected: %v", err)
	}
}

func TestClassifyErrorDistinguishesTheThreeTickets(t *testing.T) {
	gr := schema.GroupResource{Resource: "nodes"}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"none", nil, cellsv1alpha1.ReasonConnected},
		{"expired credential", apierrors.NewUnauthorized("bad token"), cellsv1alpha1.ReasonCredentialInvalid},
		{"rbac reduced", apierrors.NewForbidden(gr, "cell-0", errors.New("nope")), cellsv1alpha1.ReasonForbidden},
		{"timeout", apierrors.NewTimeoutError("slow", 1), cellsv1alpha1.ReasonUnreachable},
		{"dns", &net.DNSError{Err: "no such host"}, cellsv1alpha1.ReasonUnreachable},
		{"unknown", errors.New("boom"), cellsv1alpha1.ReasonUnreachable},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyError(c.err); got != c.want {
				t.Errorf("ClassifyError(%v) = %s, want %s", c.err, got, c.want)
			}
		})
	}
}

// testNow is a fixed clock: liveness is a time comparison, and a test that read
// the wall clock would be the kind of test that passes at 3pm and fails at 3am.
var testNow = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

// nodeLease builds a kubelet heartbeat lease renewed at now-age.
func nodeLease(name string, age time.Duration) *coordinationv1.Lease {
	renew := metav1.NewMicroTime(testNow.Add(-age))
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: nodeLeaseNamespace},
		Spec:       coordinationv1.LeaseSpec{RenewTime: &renew},
	}
}

func readyNode(name string, labels map[string]string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func TestGetNodeState(t *testing.T) {
	ctx := context.Background()

	t.Run("missing node is not an error", func(t *testing.T) {
		st, err := GetNodeState(ctx, fake.NewSimpleClientset(), "cell-0", testNow)
		if err != nil || st.Exists {
			t.Errorf("got %+v/%v — a cell that has not joined yet is not a failure", st, err)
		}
	})

	t.Run("ready", func(t *testing.T) {
		st, err := GetNodeState(ctx, fake.NewSimpleClientset(readyNode("cell-0", map[string]string{"gpu": "on"})), "cell-0", testNow)
		if err != nil || !st.Exists || !st.Ready {
			t.Errorf("got %+v/%v", st, err)
		}
	})

	t.Run("no Ready condition is not ready", func(t *testing.T) {
		n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cell-0"}}
		st, _ := GetNodeState(ctx, fake.NewSimpleClientset(n), "cell-0", testNow)
		if st.Ready {
			t.Error("readiness assumed from silence")
		}
	})

	t.Run("cordoned", func(t *testing.T) {
		n := readyNode("cell-0", nil)
		n.Spec.Unschedulable = true
		st, _ := GetNodeState(ctx, fake.NewSimpleClientset(n), "cell-0", testNow)
		if !st.Unschedulable {
			t.Error("cordon not reported")
		}
	})
}

// TestKubeletLive pins the signal that decides whether a Node may be deleted.
// Getting it wrong in the "live" direction deletes a working cell's Node and the
// kubelet never re-registers (#14), so every unknown case must report live.
func TestKubeletLive(t *testing.T) {
	ctx := context.Background()
	node := readyNode("cell-0", nil)

	cases := []struct {
		name       string
		objects    []runtime.Object
		heartbeat  time.Duration // Ready-condition heartbeat age; 0 means absent
		wantLive   bool
		wantSource string
	}{
		{
			name:       "fresh lease is live",
			objects:    []runtime.Object{nodeLease("cell-0", 5*time.Second)},
			wantLive:   true,
			wantSource: "Lease",
		},
		{
			name:       "expired lease is not live",
			objects:    []runtime.Object{nodeLease("cell-0", 10*time.Minute)},
			wantLive:   false,
			wantSource: "Lease",
		},
		{
			name:       "lease that was never renewed is not live",
			objects:    []runtime.Object{&coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: "cell-0", Namespace: nodeLeaseNamespace}}},
			wantLive:   false,
			wantSource: "Lease",
		},
		{
			// No lease and no status heartbeat: nothing is known, and an unknown
			// must never authorise a delete.
			name:       "nothing readable reports live",
			wantLive:   true,
			wantSource: "",
		},
		{
			// A recent status heartbeat is the fallback signal. Its resolution is
			// coarse (5 min by default), hence the wide grace.
			name:       "recent node status is live",
			heartbeat:  2 * time.Minute,
			wantLive:   true,
			wantSource: "NodeStatus",
		},
		{
			name:       "stale node status is not live",
			heartbeat:  30 * time.Minute,
			wantLive:   false,
			wantSource: "NodeStatus",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := node.DeepCopy()
			if c.heartbeat != 0 {
				n.Status.Conditions[0].LastHeartbeatTime = metav1.NewTime(testNow.Add(-c.heartbeat))
			}
			cs := fake.NewSimpleClientset(append([]runtime.Object{n}, c.objects...)...)
			st, err := GetNodeState(ctx, cs, "cell-0", testNow)
			if err != nil {
				t.Fatalf("GetNodeState: %v", err)
			}
			if st.KubeletLive != c.wantLive || st.HeartbeatSource != c.wantSource {
				t.Errorf("KubeletLive=%v source=%q, want %v/%q",
					st.KubeletLive, st.HeartbeatSource, c.wantLive, c.wantSource)
			}
		})
	}

	t.Run("an unreadable lease falls back rather than reporting dead", func(t *testing.T) {
		cs := fake.NewSimpleClientset(node.DeepCopy())
		cs.PrependReactor("get", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}, "cell-0", errors.New("no rbac"))
		})
		st, err := GetNodeState(ctx, cs, "cell-0", testNow)
		if err != nil {
			t.Fatalf("a lease we may not read must not fail the observation: %v", err)
		}
		if !st.KubeletLive {
			t.Error("a Node whose liveness cannot be established was reported dead — that authorises deleting it")
		}
	})
}

func TestEnsureNodeMetadata(t *testing.T) {
	ctx := context.Background()

	t.Run("patches missing identity labels", func(t *testing.T) {
		cs := fake.NewSimpleClientset(readyNode("cell-0", map[string]string{"gpu": "on"}))
		changed, err := EnsureNodeMetadata(ctx, cs, "cell-0",
			map[string]string{"gpu": "on", cellsv1alpha1.LabelCell: "cell-0"}, nil, nil)
		if err != nil || !changed {
			t.Fatalf("changed=%v err=%v", changed, err)
		}
		node, _ := cs.CoreV1().Nodes().Get(ctx, "cell-0", metav1.GetOptions{})
		if node.Labels[cellsv1alpha1.LabelCell] != "cell-0" {
			t.Errorf("identity label not applied: %v", node.Labels)
		}
	})

	t.Run("steady state writes nothing", func(t *testing.T) {
		cs := fake.NewSimpleClientset(readyNode("cell-0", map[string]string{"gpu": "on"}))
		changed, err := EnsureNodeMetadata(ctx, cs, "cell-0", map[string]string{"gpu": "on"}, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if changed {
			t.Error("patched a Node that already matched")
		}
	})

	t.Run("foreign taints survive", func(t *testing.T) {
		n := readyNode("cell-0", nil)
		// A drain, or another controller, put this here. Stomping it would be a
		// scheduling incident.
		n.Spec.Taints = []corev1.Taint{{Key: "someone-else", Value: "v", Effect: corev1.TaintEffectNoSchedule}}
		cs := fake.NewSimpleClientset(n)

		if _, err := EnsureNodeMetadata(ctx, cs, "cell-0", nil, nil,
			[]corev1.Taint{{Key: "cells.kubeswift.io/dedicated", Value: "inference", Effect: corev1.TaintEffectNoSchedule}}); err != nil {
			t.Fatal(err)
		}
		node, _ := cs.CoreV1().Nodes().Get(ctx, "cell-0", metav1.GetOptions{})
		keys := map[string]bool{}
		for _, tt := range node.Spec.Taints {
			keys[tt.Key] = true
		}
		if !keys["someone-else"] || !keys["cells.kubeswift.io/dedicated"] {
			t.Errorf("taints = %v, want both preserved", node.Spec.Taints)
		}
	})
}

func TestCordonAndDelete(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(readyNode("cell-0", nil))

	if err := Cordon(ctx, cs, "cell-0"); err != nil {
		t.Fatalf("Cordon: %v", err)
	}
	node, _ := cs.CoreV1().Nodes().Get(ctx, "cell-0", metav1.GetOptions{})
	if !node.Spec.Unschedulable {
		t.Error("node not cordoned")
	}
	// Idempotent.
	if err := Cordon(ctx, cs, "cell-0"); err != nil {
		t.Errorf("second Cordon: %v", err)
	}
	// Cordoning a node that never registered is a no-op, not an error.
	if err := Cordon(ctx, cs, "ghost"); err != nil {
		t.Errorf("Cordon(missing): %v", err)
	}

	if err := DeleteNode(ctx, cs, "cell-0"); err != nil {
		t.Fatalf("DeleteNode: %v", err)
	}
	if _, err := cs.CoreV1().Nodes().Get(ctx, "cell-0", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Error("node not deleted")
	}
	if err := DeleteNode(ctx, cs, "cell-0"); err != nil {
		t.Errorf("DeleteNode is not idempotent: %v", err)
	}
}
