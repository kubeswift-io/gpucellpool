package v1alpha1

import (
	"testing"

	cellsv1alpha1 "github.com/kubeswift-io/gpucellpool/api/v1alpha1"
)

func TestAutoscalingRules(t *testing.T) {
	i32 := func(i int32) *int32 { return &i }

	t.Run("disabled needs nothing", func(t *testing.T) {
		p := validPool()
		p.Spec.Autoscaling = &cellsv1alpha1.AutoscalingSpec{Enabled: false}
		assertValid(t, p)
	})

	t.Run("enabled requires a ceiling", func(t *testing.T) {
		// An unbounded pool that misreads demand can consume every GPU in the
		// cluster, so maxReplicas is not optional.
		p := validPool()
		p.Spec.Autoscaling = &cellsv1alpha1.AutoscalingSpec{Enabled: true, MinReplicas: i32(1)}
		assertRejected(t, p, "maxReplicas")
	})

	t.Run("floor above ceiling", func(t *testing.T) {
		p := validPool()
		p.Spec.Autoscaling = &cellsv1alpha1.AutoscalingSpec{
			Enabled: true, MinReplicas: i32(5), MaxReplicas: i32(2)}
		assertRejected(t, p, "must not exceed maxReplicas")
	})

	t.Run("automatic scale-down is a separate phase", func(t *testing.T) {
		// Destroying someone's running work on a heuristic is the one
		// unrecoverable mistake here, so the flag is rejected rather than
		// silently doing nothing.
		p := validPool()
		p.Spec.Autoscaling = &cellsv1alpha1.AutoscalingSpec{
			Enabled: true, MaxReplicas: i32(4), ScaleDown: cellsv1alpha1.ScaleDownAuto}
		assertRejected(t, p, "scaleDown")
	})

	t.Run("valid autoscaling", func(t *testing.T) {
		p := validPool()
		p.Spec.Autoscaling = &cellsv1alpha1.AutoscalingSpec{
			Enabled: true, MinReplicas: i32(1), MaxReplicas: i32(8),
			ScaleDown: cellsv1alpha1.ScaleDownManual}
		assertValid(t, p)
	})
}
