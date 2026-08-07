package capacity

import (
	"context"
	"errors"
)

// ErrUnsupported is returned by a provider method that this mode does not
// implement. Callers surface it as a condition with a reason — never as zero
// capacity.
var ErrUnsupported = errors.New("not supported by this capacity provider mode")

// Health is whether the provider is present and readable at all.
type Health struct {
	Ready bool
	// Reason is a cellsv1alpha1.Reason* value.
	Reason string
	// Message is operator-actionable detail.
	Message string
}

// ModelCapacity is capacity for one GPU model. It is always populated, and it is
// the only compute truth when a pool is not homogeneous: percentages of different
// GPU models are not commensurable.
type ModelCapacity struct {
	Model              string
	Devices            int
	MemoryTotalMiB     int64
	MemoryAllocatedMiB int64
	ComputeTotal       int64
	ComputeAllocated   int64
}

// Capacity is the pool-wide view across a set of cell nodes.
type Capacity struct {
	Devices     int
	Homogeneous bool
	ByModel     []ModelCapacity

	MemoryTotalMiB     int64
	MemoryAllocatedMiB int64

	// ComputeTotal/ComputeAllocated are percent-of-device sums and are only
	// meaningful while Homogeneous is true.
	ComputeTotal     int64
	ComputeAllocated int64
}

// Allocations is what still holds a cell's GPU — the scale-down and drain gate.
type Allocations struct {
	// Consumers is the number of pods holding a share.
	Consumers   int
	MemoryMiB   int64
	CorePercent int64
	// InFlight is true when a binding is still in progress. A drain that ignored
	// in-flight allocations would race the scheduler.
	InFlight bool
}

// Empty reports whether a cell can be removed without destroying work.
func (a Allocations) Empty() bool { return a.Consumers == 0 && !a.InFlight }

// Demand is unsatisfiable GPU demand in the workload cluster.
type Demand struct {
	// PendingRequests is the number of GPU requests that cannot be placed.
	PendingRequests int
	// SatisfiableByOneCell is how many of them a fresh cell of this pool's shape
	// would actually satisfy. Scaling on anything else creates cells that do not
	// help, so this is what a scaling decision must use.
	SatisfiableByOneCell int
}

// Provider observes GPU capacity in the workload cluster. It never writes, never
// allocates, and never learns about KubeSwift. HAMi is one implementation.
type Provider interface {
	Name() string

	// Health reports whether the provider is present and parseable across the
	// given cell nodes.
	Health(ctx context.Context, nodes []string) (Health, error)

	// Devices reports how many healthy GPU devices the provider advertises on
	// one node — the third leg of cell readiness.
	Devices(ctx context.Context, node string) (int, error)

	// Capacity aggregates total and allocated capacity across cell nodes.
	Capacity(ctx context.Context, nodes []string) (Capacity, error)

	// Allocations reports what still holds one node's GPU.
	Allocations(ctx context.Context, node string) (Allocations, error)

	// PendingDemand reports unsatisfiable GPU demand.
	//
	// ref is the reference device a fresh cell of this pool would bring, used to
	// judge satisfiability. A nil ref means we do not know the shape (an empty
	// pool has no advertised device to learn it from), in which case nothing is
	// reported as satisfiable: an automatic action must not run on a guess.
	PendingDemand(ctx context.Context, ref *Device) (Demand, error)
}
