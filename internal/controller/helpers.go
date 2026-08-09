package controller

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
	"github.com/kubeswift-io/gpucellpool/internal/provisioner"
)

func clientKey(pool *cellsv1alpha1.GPUCellPool) string {
	return pool.Namespace + "/" + pool.Spec.WorkloadCluster.KubeconfigSecretRef.Name
}

func phaseOr(p cellsv1alpha1.CellPhase, fallback cellsv1alpha1.CellPhase) cellsv1alpha1.CellPhase {
	if p == "" {
		return fallback
	}
	return p
}

func transitionOr(t *metav1.Time, fallback time.Time) time.Time {
	if t == nil {
		return fallback
	}
	return t.Time
}

func durationOr(d *metav1.Duration, fallback time.Duration) time.Duration {
	if d == nil || d.Duration == 0 {
		return fallback
	}
	return d.Duration
}

// expectedDevices is how many GPUs a cell must advertise before it is Ready.
// Defaults to the cell's GPU count, which is the only sensible default: a cell
// that holds one device and advertises none is not usable.
func expectedDevices(pool *cellsv1alpha1.GPUCellPool) int {
	if h := pool.Spec.Capacity.HAMi; h != nil && h.ExpectedDevicesPerCell != nil {
		return int(*h.ExpectedDevicesPerCell)
	}
	if c := pool.Spec.Cell.GPU.Count; c > 0 {
		return int(c)
	}
	return 1
}

func hamiMode(pool *cellsv1alpha1.GPUCellPool) string {
	if h := pool.Spec.Capacity.HAMi; h != nil && h.Mode != "" {
		return h.Mode
	}
	return cellsv1alpha1.HAMiModeDevicePlugin
}

func providerName(pool *cellsv1alpha1.GPUCellPool) string {
	if pool.Spec.Capacity.Provider != "" {
		return pool.Spec.Capacity.Provider
	}
	return cellsv1alpha1.CapacityProviderHAMi
}

func deletionPolicy(pool *cellsv1alpha1.GPUCellPool) string {
	if pool.Spec.Deletion != nil && pool.Spec.Deletion.Policy != "" {
		return pool.Spec.Deletion.Policy
	}
	return cellsv1alpha1.DeletionPolicyDrain
}

func drainTimeout(pool *cellsv1alpha1.GPUCellPool) time.Duration {
	if pool.Spec.Deletion != nil {
		return durationOr(pool.Spec.Deletion.DrainTimeout, 10*time.Minute)
	}
	return 10 * time.Minute
}

func nodeLabels(pool *cellsv1alpha1.GPUCellPool) map[string]string {
	if pool.Spec.WorkloadCluster.Node == nil {
		return nil
	}
	return pool.Spec.WorkloadCluster.Node.Labels
}

func nodeAnnotations(pool *cellsv1alpha1.GPUCellPool) map[string]string {
	if pool.Spec.WorkloadCluster.Node == nil {
		return nil
	}
	return pool.Spec.WorkloadCluster.Node.Annotations
}

func nodeTaints(pool *cellsv1alpha1.GPUCellPool) []corev1.Taint {
	if pool.Spec.WorkloadCluster.Node == nil {
		return nil
	}
	return pool.Spec.WorkloadCluster.Node.Taints
}

// everReady reports whether any cell has ever reached Ready — the signal that
// distinguishes a burst of failures from a pool that has never worked. It is
// derived from live state plus the pool's own Ready condition history, so it
// survives an operator restart.
func everReady(pool *cellsv1alpha1.GPUCellPool, cells []cellsv1alpha1.CellStatus) bool {
	for _, c := range cells {
		if c.Phase == cellsv1alpha1.CellPhaseReady {
			return true
		}
	}
	for _, c := range pool.Status.Conditions {
		if c.Type == cellsv1alpha1.ConditionReady && c.Reason == cellsv1alpha1.ReasonAllCellsReady {
			return true
		}
	}
	return pool.Status.ReadyCells > 0
}

// autoscalingEnabled reports whether demand-driven scale-up is on.
func autoscalingEnabled(pool *cellsv1alpha1.GPUCellPool) bool {
	return pool.Spec.Autoscaling != nil && pool.Spec.Autoscaling.Enabled
}

// autoScaleDown reports whether the pool may shrink itself.
func autoScaleDown(pool *cellsv1alpha1.GPUCellPool) bool {
	return autoscalingEnabled(pool) && pool.Spec.Autoscaling.ScaleDown == cellsv1alpha1.ScaleDownAuto
}

// rollingUpdate reports whether the pool replaces stale cells itself.
func rollingUpdate(pool *cellsv1alpha1.GPUCellPool) bool {
	return pool.Spec.UpdatePolicy != nil &&
		pool.Spec.UpdatePolicy.Type == cellsv1alpha1.UpdateRolling
}

// needsIdleness reports whether anything this pass may want to REMOVE a cell, and
// therefore needs to know which cells hold nothing. Both automatic scale-down and a
// rolling update destroy a cell, so both require it — reading it for only one of them
// left the other seeing no idle cells at all and silently never acting.
func needsIdleness(pool *cellsv1alpha1.GPUCellPool) bool {
	return autoScaleDown(pool) || rollingUpdate(pool)
}

// liveCells counts cells that exist and are not on their way out — the baseline a
// scaling decision grows from.
func liveCells(cells []cellsv1alpha1.CellStatus) int32 {
	var n int32
	for _, c := range cells {
		switch c.Phase {
		case cellsv1alpha1.CellPhaseDraining, cellsv1alpha1.CellPhaseDeleting:
		case cellsv1alpha1.CellPhaseFailed:
			if c.GuestUID != "" {
				n++ // still occupies its index and possibly its GPU
			}
		default:
			n++
		}
	}
	return n
}

// readyNodeNames are the cells whose Node has registered — the only ones a
// capacity provider can say anything about.
func readyNodeNames(cells []cellsv1alpha1.CellStatus) []string {
	var out []string
	for _, c := range cells {
		if c.NodeName != "" {
			out = append(out, c.NodeName)
		}
	}
	return out
}

// capiOwnsBootstrap reports whether the workload cluster's own Cluster API bootstrap
// provider produces the cells' join data. When it does, spec.bootstrap is unused:
// nothing is rendered and no join Secret is required.
func capiOwnsBootstrap(pool *cellsv1alpha1.GPUCellPool) bool {
	return pool.Spec.Cell.Provisioner == provisioner.ProvisionerClusterAPI &&
		pool.Spec.Cell.ClusterAPI != nil &&
		pool.Spec.Cell.ClusterAPI.BootstrapConfigTemplateRef != nil
}
