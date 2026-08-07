package capacity

import "context"

// FakeProvider is a scripted Provider for tests. The lab has one GPU, so every
// behaviour that needs two or more cells — aggregation, heterogeneity,
// degradation, drain gating — is proven here rather than on hardware.
//
// It lives in the non-test build so the controller's own tests (a different
// package) can use it.
type FakeProvider struct {
	ProviderName string

	// HealthResult and HealthErr script Health.
	HealthResult Health
	HealthErr    error

	// DevicesByNode scripts Devices; missing nodes report 0 (still joining).
	DevicesByNode map[string]int
	DevicesErr    error

	CapacityResult Capacity
	CapacityErr    error

	AllocationsByNode map[string]Allocations
	AllocationsErr    error

	DemandResult Demand
	DemandErr    error

	// Calls records method names in order, so a test can assert that a
	// destructive path never even asked.
	Calls []string
}

// NewFakeProvider returns a healthy provider with no cells.
func NewFakeProvider() *FakeProvider {
	return &FakeProvider{
		ProviderName:      "Fake",
		HealthResult:      Health{Ready: true, Reason: "FakeDetected"},
		DevicesByNode:     map[string]int{},
		AllocationsByNode: map[string]Allocations{},
		DemandErr:         ErrUnsupported,
	}
}

func (f *FakeProvider) record(name string) { f.Calls = append(f.Calls, name) }

// Name implements Provider.
func (f *FakeProvider) Name() string {
	if f.ProviderName == "" {
		return "Fake"
	}
	return f.ProviderName
}

// Health implements Provider.
func (f *FakeProvider) Health(_ context.Context, _ []string) (Health, error) {
	f.record("Health")
	return f.HealthResult, f.HealthErr
}

// Devices implements Provider.
func (f *FakeProvider) Devices(_ context.Context, node string) (int, error) {
	f.record("Devices")
	if f.DevicesErr != nil {
		return 0, f.DevicesErr
	}
	return f.DevicesByNode[node], nil
}

// Capacity implements Provider.
func (f *FakeProvider) Capacity(_ context.Context, _ []string) (Capacity, error) {
	f.record("Capacity")
	return f.CapacityResult, f.CapacityErr
}

// Allocations implements Provider.
func (f *FakeProvider) Allocations(_ context.Context, node string) (Allocations, error) {
	f.record("Allocations")
	if f.AllocationsErr != nil {
		return Allocations{}, f.AllocationsErr
	}
	return f.AllocationsByNode[node], nil
}

// PendingDemand implements Provider.
func (f *FakeProvider) PendingDemand(_ context.Context) (Demand, error) {
	f.record("PendingDemand")
	return f.DemandResult, f.DemandErr
}

var _ Provider = (*FakeProvider)(nil)
