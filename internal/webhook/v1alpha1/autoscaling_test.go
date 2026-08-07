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

	t.Run("automatic scale-down needs a floor", func(t *testing.T) {
		// Without one the pool can shrink to zero and every later request pays a
		// full cell boot — which defeats the point of holding a pool.
		p := validPool()
		p.Spec.Autoscaling = &cellsv1alpha1.AutoscalingSpec{
			Enabled: true, MaxReplicas: i32(4), ScaleDown: cellsv1alpha1.ScaleDownAuto}
		assertRejected(t, p, "minReplicas")
	})

	t.Run("automatic scale-down with a floor is accepted", func(t *testing.T) {
		p := validPool()
		p.Spec.Autoscaling = &cellsv1alpha1.AutoscalingSpec{
			Enabled: true, MinReplicas: i32(1), MaxReplicas: i32(4),
			ScaleDown: cellsv1alpha1.ScaleDownAuto}
		assertValid(t, p)
	})

	t.Run("valid autoscaling", func(t *testing.T) {
		p := validPool()
		p.Spec.Autoscaling = &cellsv1alpha1.AutoscalingSpec{
			Enabled: true, MinReplicas: i32(1), MaxReplicas: i32(8),
			ScaleDown: cellsv1alpha1.ScaleDownManual}
		assertValid(t, p)
	})
}
