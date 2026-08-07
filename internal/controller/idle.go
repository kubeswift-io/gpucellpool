package controller

import (
	"context"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
	"github.com/kubeswift-io/gpucellpool/internal/capacity"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// idleCells returns the Ready cells the capacity provider reports as holding
// nothing, emptiest first.
//
// A cell whose allocations cannot be READ is never listed. That is the difference
// between "this cell is empty" and "I do not know what this cell holds", and only
// the first may lead to a deletion.
func (r *GPUCellPoolReconciler) idleCells(
	ctx context.Context, provider capacity.Provider, cells []cellsv1alpha1.CellStatus,
) []string {
	log := logf.FromContext(ctx)
	var out []string
	for _, c := range cells {
		if c.Phase != cellsv1alpha1.CellPhaseReady || c.NodeName == "" {
			continue
		}
		a, err := provider.Allocations(ctx, c.NodeName)
		if err != nil {
			log.V(1).Info("allocations unreadable; not treating the cell as idle",
				"cell", c.Name, "err", err.Error())
			continue
		}
		if a.Empty() {
			out = append(out, c.Name)
		}
	}
	return out
}
