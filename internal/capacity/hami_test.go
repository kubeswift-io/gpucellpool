package capacity

import (
	"strings"
	"testing"
)

// measuredRegistration is a VERBATIM capture from the Phase-1 cell
// (dev/boba, GTX 1080, HAMi 2.x, 2026-08-07). Keep it byte-for-byte: it is the
// only evidence we have of the real shape, including the fields upstream docs do
// not mention (mode, devicepairscore) and the one they do but HAMi omits (numa).
const measuredRegistration = `[{"id":"GPU-e71afe85-9309-864a-477e-91caa89f3932","count":10,` +
	`"devmem":8192,"devcore":100,"type":"NVIDIA GeForce GTX 1080","mode":"hami-core",` +
	`"health":true,"devicepairscore":{}}]`

// measuredAllocated is likewise verbatim: one pod holding 3000MiB / 30% of the
// cell's only GPU.
const measuredAllocated = `GPU-e71afe85-9309-864a-477e-91caa89f3932,NVIDIA,3000,30:;`

func TestParseNodeRegistrationOnMeasuredFixture(t *testing.T) {
	devices, found, err := ParseNodeRegistration(map[string]string{
		AnnotationNodeRegistration: measuredRegistration,
	})
	if err != nil {
		t.Fatalf("parsing the real annotation failed: %v", err)
	}
	if !found {
		t.Fatal("found = false for a present annotation")
	}
	if len(devices) != 1 {
		t.Fatalf("got %d devices, want 1", len(devices))
	}

	d := devices[0]
	if d.UUID != "GPU-e71afe85-9309-864a-477e-91caa89f3932" {
		t.Errorf("UUID = %q", d.UUID)
	}
	if d.MemoryMiB != 8192 || d.CorePercent != 100 {
		t.Errorf("memory/cores = %d/%d, want 8192/100", d.MemoryMiB, d.CorePercent)
	}
	// The model string contains spaces. Anything that assumes a hyphenated form
	// (as some docs show) will mis-key the per-model capacity buckets.
	if d.Model != "NVIDIA GeForce GTX 1080" {
		t.Errorf("Model = %q, want the spaced form", d.Model)
	}
	if d.Mode != "hami-core" || !d.Healthy {
		t.Errorf("mode/health = %q/%v", d.Mode, d.Healthy)
	}
	// THE trap: allocatable nvidia.com/gpu is 10 on this node. One physical GPU.
	if d.LogicalSlots != 10 {
		t.Errorf("LogicalSlots = %d, want 10", d.LogicalSlots)
	}
	if len(devices) != 1 {
		t.Errorf("device count must come from the annotation, not the slot count")
	}
}

func TestParseNodeRegistrationTolerance(t *testing.T) {
	t.Run("unknown fields are ignored", func(t *testing.T) {
		// A future HAMi adds fields; that must not break capacity reporting.
		_, _, err := ParseNodeRegistration(map[string]string{
			AnnotationNodeRegistration: `[{"id":"GPU-1","count":10,"devmem":8192,"devcore":100,` +
				`"health":true,"somethingNew":{"nested":[1,2]},"anotherOne":"x"}]`,
		})
		if err != nil {
			t.Errorf("unknown fields rejected: %v", err)
		}
	})

	t.Run("absent numa is fine", func(t *testing.T) {
		_, _, err := ParseNodeRegistration(map[string]string{
			AnnotationNodeRegistration: `[{"id":"GPU-1","devmem":8192,"devcore":100,"health":true}]`,
		})
		if err != nil {
			t.Errorf("device without numa rejected: %v", err)
		}
	})

	t.Run("annotation absent is not an error", func(t *testing.T) {
		devices, found, err := ParseNodeRegistration(map[string]string{})
		if err != nil || found || devices != nil {
			t.Errorf("got %v/%v/%v, want nil/false/nil — HAMi may simply not have registered yet", devices, found, err)
		}
	})
}

func TestParseNodeRegistrationFailsLoudlyRatherThanReportingZero(t *testing.T) {
	// Each of these must be an ERROR, never an empty device list: a caller that
	// saw "no devices" would report a healthy cell as having no GPU, or an
	// unsaturated pool as full.
	cases := map[string]string{
		"malformed json":  `[{"id":"GPU-1",`,
		"not an array":    `{"id":"GPU-1"}`,
		"missing id":      `[{"devmem":8192,"devcore":100,"health":true}]`,
		"missing devmem":  `[{"id":"GPU-1","devcore":100,"health":true}]`,
		"missing devcore": `[{"id":"GPU-1","devmem":8192,"health":true}]`,
		"missing health":  `[{"id":"GPU-1","devmem":8192,"devcore":100}]`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			devices, found, err := ParseNodeRegistration(map[string]string{AnnotationNodeRegistration: raw})
			if err == nil {
				t.Fatalf("accepted %q and returned %d devices; a parse failure must not read as zero capacity", raw, len(devices))
			}
			if !found {
				t.Error("found = false for a present-but-broken annotation")
			}
		})
	}
}

func TestHealthyDevices(t *testing.T) {
	in := []Device{{UUID: "a", Healthy: true}, {UUID: "b", Healthy: false}, {UUID: "c", Healthy: true}}
	got := HealthyDevices(in)
	if len(got) != 2 || got[0].UUID != "a" || got[1].UUID != "c" {
		t.Errorf("HealthyDevices = %v", got)
	}
}

func TestParseAllocationsOnMeasuredFixture(t *testing.T) {
	allocs, err := ParseAllocations(measuredAllocated)
	if err != nil {
		t.Fatalf("parsing the real annotation failed: %v", err)
	}
	if len(allocs) != 1 {
		t.Fatalf("got %d allocations, want 1", len(allocs))
	}
	a := allocs[0]
	if a.UUID != "GPU-e71afe85-9309-864a-477e-91caa89f3932" || a.Vendor != "NVIDIA" {
		t.Errorf("got %+v", a)
	}
	if a.MemoryMiB != 3000 || a.CorePercent != 30 {
		t.Errorf("memory/cores = %d/%d, want 3000/30", a.MemoryMiB, a.CorePercent)
	}
}

func TestParseAllocationsEdgeCases(t *testing.T) {
	t.Run("completed binding sentinel", func(t *testing.T) {
		// to-allocate is ";;" once binding is done — no allocations, no error.
		allocs, err := ParseAllocations(";;")
		if err != nil || len(allocs) != 0 {
			t.Errorf("got %v/%v, want none/nil", allocs, err)
		}
	})

	t.Run("empty", func(t *testing.T) {
		if allocs, err := ParseAllocations(""); err != nil || allocs != nil {
			t.Errorf("got %v/%v", allocs, err)
		}
	})

	t.Run("two devices in one pod", func(t *testing.T) {
		allocs, err := ParseAllocations("GPU-a,NVIDIA,3000,30:GPU-b,NVIDIA,4000,50:;")
		if err != nil {
			t.Fatalf("err: %v", err)
		}
		if len(allocs) != 2 || allocs[1].UUID != "GPU-b" || allocs[1].MemoryMiB != 4000 {
			t.Errorf("got %+v", allocs)
		}
	})

	t.Run("extra fields tolerated", func(t *testing.T) {
		// Forward compatibility: a fifth field must not strand the pool at
		// CapacityProviderReady=False after a HAMi upgrade.
		allocs, err := ParseAllocations("GPU-a,NVIDIA,3000,30,somethingNew:;")
		if err != nil {
			t.Fatalf("extra field rejected: %v", err)
		}
		if len(allocs) != 1 || allocs[0].MemoryMiB != 3000 {
			t.Errorf("got %+v", allocs)
		}
	})

	t.Run("malformed is an error", func(t *testing.T) {
		for _, bad := range []string{
			"GPU-a,NVIDIA,3000:;",            // too few fields
			"GPU-a,NVIDIA,notanumber,30:;",   // memory unparseable
			"GPU-a,NVIDIA,3000,notanumber:;", // cores unparseable
		} {
			if _, err := ParseAllocations(bad); err == nil {
				t.Errorf("accepted malformed record %q", bad)
			}
		}
	})
}

func TestSumAllocations(t *testing.T) {
	// The Phase-1 case: two pods, 3000MiB/30% each, on one 8192MiB device.
	mem, cores := SumAllocations([]Allocation{
		{MemoryMiB: 3000, CorePercent: 30},
		{MemoryMiB: 3000, CorePercent: 30},
	})
	if mem != 6000 || cores != 60 {
		t.Errorf("sum = %d/%d, want 6000/60", mem, cores)
	}
	if mem, cores := SumAllocations(nil); mem != 0 || cores != 0 {
		t.Errorf("empty sum = %d/%d", mem, cores)
	}
}

func TestAnnotationNamesMatchUpstream(t *testing.T) {
	// Cheap guard: these strings are the entire contract with HAMi. A typo here
	// reads as "HAMi not detected" forever.
	for _, want := range []string{
		"hami.io/node-nvidia-register",
		"hami.io/vgpu-devices-allocated",
		"hami.io/vgpu-devices-to-allocate",
		"hami.io/bind-time",
	} {
		got := map[string]string{
			"hami.io/node-nvidia-register":     AnnotationNodeRegistration,
			"hami.io/vgpu-devices-allocated":   AnnotationDevicesAllocated,
			"hami.io/vgpu-devices-to-allocate": AnnotationDevicesToAllocate,
			"hami.io/bind-time":                AnnotationBindTime,
		}[want]
		if got != want || !strings.HasPrefix(got, "hami.io/") {
			t.Errorf("annotation constant drifted: %q", got)
		}
	}
}
