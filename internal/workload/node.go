package workload

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
)

// NodeState is what the reconciler needs to know about a cell's workload Node.
type NodeState struct {
	// Exists is false until the kubelet has registered.
	Exists bool
	// Ready mirrors the Node's Ready condition.
	Ready bool
	// Unschedulable is true when the Node is cordoned (by us during a drain, or
	// by someone else).
	Unschedulable bool
	// Labels and Annotations as observed, used for identity checks and for the
	// in-guest preflight result.
	Labels      map[string]string
	Annotations map[string]string

	// KubeletLive is true when a kubelet is heartbeating for this Node right now.
	//
	// It exists to separate two things a Node's labels cannot tell apart: a Node
	// left behind by a dead incarnation of a cell, and the SAME Node object after
	// the replacement's kubelet adopted it — a kubelet keeps labels it did not set,
	// so both carry the old identity label. Deleting the second one is
	// unrecoverable from this side: the kubelet does not re-register, it just logs
	// "Error updating node status, will retry" forever and the cell never joins.
	//
	// Unknown reads as LIVE. Failing to delete a phantom Node costs a stale
	// capacity reading that the next reconcile corrects; deleting a live one costs
	// the cell.
	KubeletLive bool

	// HeartbeatSource names where KubeletLive came from — "Lease", "NodeStatus", or
	// "" when neither could be read. Recorded because the two have very different
	// resolutions and an operator debugging a reap needs to know which was used.
	HeartbeatSource string
}

// Heartbeat grace periods, per source.
//
// The kubelet renews its Lease every ~10s (lease duration 40s), so 90s is already
// generous. Node STATUS is only pushed every nodeStatusReportFrequency — 5 minutes
// by default once leases are enabled — so judging liveness by it needs a much wider
// window, and is a fallback rather than the signal we want.
const (
	leaseGrace      = 90 * time.Second
	nodeStatusGrace = 6 * time.Minute
)

// nodeLeaseNamespace is where kubelets renew their heartbeat leases.
const nodeLeaseNamespace = "kube-node-lease"

// GetNodeState reads a cell's Node. A missing Node is not an error: the cell is
// simply still joining.
//
// now is passed in rather than read from the clock so liveness is testable.
func GetNodeState(ctx context.Context, cs kubernetes.Interface, name string, now time.Time) (NodeState, error) {
	node, err := cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return NodeState{}, nil
	}
	if err != nil {
		return NodeState{}, fmt.Errorf("reading node %s: %w", name, err)
	}
	live, src := kubeletLive(ctx, cs, node, now)
	return NodeState{
		Exists:          true,
		Ready:           IsReady(node),
		Unschedulable:   node.Spec.Unschedulable,
		Labels:          node.Labels,
		Annotations:     node.Annotations,
		KubeletLive:     live,
		HeartbeatSource: src,
	}, nil
}

// ListPoolNodes returns the state of every Node in the workload cluster carrying
// this pool's label, keyed by node name.
//
// The pool label — not the operator's own status — is the authority on which Nodes
// belong to a pool. Status can be lost, and a row is dropped the moment a cell is
// retired, so anything that cleans up after cells has to be able to find them
// without it.
func ListPoolNodes(ctx context.Context, cs kubernetes.Interface, pool string, now time.Time) (map[string]NodeState, error) {
	list, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{
		LabelSelector: cellsv1alpha1.LabelPool + "=" + pool,
	})
	if err != nil {
		return nil, fmt.Errorf("listing nodes of pool %s: %w", pool, err)
	}
	out := make(map[string]NodeState, len(list.Items))
	for i := range list.Items {
		node := &list.Items[i]
		live, src := kubeletLive(ctx, cs, node, now)
		out[node.Name] = NodeState{
			Exists:          true,
			Ready:           IsReady(node),
			Unschedulable:   node.Spec.Unschedulable,
			Labels:          node.Labels,
			Annotations:     node.Annotations,
			KubeletLive:     live,
			HeartbeatSource: src,
		}
	}
	return out, nil
}

// kubeletLive reports whether a kubelet is currently heartbeating for a Node.
//
// The Lease is the real signal. When it cannot be read — RBAC, an old cluster, a
// transient error — this falls back to the Ready condition's heartbeat, and if
// that is missing too it reports live: see NodeState.KubeletLive for why unknown
// must not authorise a delete.
func kubeletLive(ctx context.Context, cs kubernetes.Interface, node *corev1.Node, now time.Time) (bool, string) {
	lease, err := cs.CoordinationV1().Leases(nodeLeaseNamespace).Get(ctx, node.Name, metav1.GetOptions{})
	switch {
	case err == nil && lease.Spec.RenewTime != nil:
		return now.Sub(lease.Spec.RenewTime.Time) < leaseGrace, "Lease"
	case err == nil:
		// A Lease with no renewTime has never been held.
		return false, "Lease"
	case apierrors.IsNotFound(err):
		// No Lease at all. On a lease-enabled cluster a live kubelet always has
		// one, so this is evidence of absence — but only once the node status
		// agrees, which the fallback below checks.
	}
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady && !c.LastHeartbeatTime.IsZero() {
			return now.Sub(c.LastHeartbeatTime.Time) < nodeStatusGrace, "NodeStatus"
		}
	}
	return true, ""
}

// IsReady reports the Node's Ready condition. An absent condition is not ready —
// never assume readiness from silence.
func IsReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// EnsureNodeMetadata makes a cell's Node carry the labels, annotations and taints
// the pool declares. It patches only what is missing or wrong, and returns
// changed=false when the Node already matches, so a steady-state reconcile writes
// nothing.
//
// This is also the fallback for identity labels the kubelet did not apply itself:
// the controller patching the Node is the reliable path (the same conclusion
// capi-kubeswift reached for providerID).
func EnsureNodeMetadata(
	ctx context.Context, cs kubernetes.Interface, name string,
	labels, annotations map[string]string, taints []corev1.Taint,
) (bool, error) {
	node, err := cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("reading node %s: %w", name, err)
	}

	patchMeta := map[string]any{}
	if want := missingOrWrong(node.Labels, labels); len(want) > 0 {
		patchMeta["labels"] = want
	}
	if want := missingOrWrong(node.Annotations, annotations); len(want) > 0 {
		patchMeta["annotations"] = want
	}

	patch := map[string]any{}
	if len(patchMeta) > 0 {
		patch["metadata"] = patchMeta
	}
	// Taints are spec, and are replaced wholesale only when ours are absent: a
	// cell's Node may legitimately carry taints from elsewhere (a drain, another
	// controller), and stomping them would be a scheduling incident.
	if merged, changed := mergeTaints(node.Spec.Taints, taints); changed {
		raw, err := json.Marshal(merged)
		if err != nil {
			return false, fmt.Errorf("encoding taints for node %s: %w", name, err)
		}
		var asAny []any
		if err := json.Unmarshal(raw, &asAny); err != nil {
			return false, fmt.Errorf("encoding taints for node %s: %w", name, err)
		}
		patch["spec"] = map[string]any{"taints": asAny}
	}

	if len(patch) == 0 {
		return false, nil
	}
	body, err := json.Marshal(patch)
	if err != nil {
		return false, fmt.Errorf("encoding node patch: %w", err)
	}
	if _, err := cs.CoreV1().Nodes().Patch(ctx, name, types.StrategicMergePatchType, body, metav1.PatchOptions{}); err != nil {
		return false, fmt.Errorf("patching node %s: %w", name, err)
	}
	return true, nil
}

// Cordon marks a cell's Node unschedulable — the first step of a drain, done
// BEFORE counting allocations so the count cannot race the scheduler placing one
// more pod.
func Cordon(ctx context.Context, cs kubernetes.Interface, name string) error {
	node, err := cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading node %s: %w", name, err)
	}
	if node.Spec.Unschedulable {
		return nil
	}
	patch := []byte(`{"spec":{"unschedulable":true}}`)
	if _, err := cs.CoreV1().Nodes().Patch(ctx, name, types.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("cordoning node %s: %w", name, err)
	}
	return nil
}

// DeleteNode removes a cell's Node object. Called for a cell being torn down, and
// for a phantom Node left by a previous incarnation.
//
// "Before the replacement joins" cannot be assumed — the replacement's kubelet may
// already have adopted the object — so the caller must establish that no kubelet is
// heartbeating for it first (NodeState.KubeletLive).
func DeleteNode(ctx context.Context, cs kubernetes.Interface, name string) error {
	err := cs.CoreV1().Nodes().Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting node %s: %w", name, err)
	}
	return nil
}

// missingOrWrong returns the subset of want that the node does not already have.
func missingOrWrong(have, want map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range want {
		if have[k] != v {
			out[k] = v
		}
	}
	return out
}

// mergeTaints adds our taints to whatever the Node already carries, without
// removing foreign ones.
func mergeTaints(have, want []corev1.Taint) ([]corev1.Taint, bool) {
	if len(want) == 0 {
		return have, false
	}
	out := append([]corev1.Taint(nil), have...)
	changed := false
	for _, w := range want {
		found := false
		for i, h := range out {
			if h.Key == w.Key && h.Effect == w.Effect {
				found = true
				if h.Value != w.Value {
					out[i].Value = w.Value
					changed = true
				}
				break
			}
		}
		if !found {
			out = append(out, w)
			changed = true
		}
	}
	return out, changed
}
