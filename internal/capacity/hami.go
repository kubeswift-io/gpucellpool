// Package capacity reads GPU capacity from the INNER cluster. It never writes,
// never allocates, and knows nothing about KubeSwift.
//
// HAMi is a dependency of shape, not of code: its annotations are parsed as data,
// so this operator carries no HAMi Go module and HAMi carries no knowledge of us.
// Everything here is deliberately tolerant — the annotations below are HAMi's
// internal scheduler↔device-plugin protocol, not a stable API, and a HAMi upgrade
// must not turn into a capacity outage.
package capacity

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// AnnotationNodeRegistration is the per-node GPU inventory HAMi publishes.
//
// Verbatim from a Phase-1 cell (HAMi 2.x, GTX 1080, 2026-08-07):
//
//	[{"id":"GPU-e71afe85-…","count":10,"devmem":8192,"devcore":100,
//	  "type":"NVIDIA GeForce GTX 1080","mode":"hami-core","health":true,
//	  "devicepairscore":{}}]
//
// Note what that capture did NOT contain: the "numa" field older documentation
// shows. And what it did: undocumented "mode" and "devicepairscore". Hence the
// parser below requires only id/devmem/devcore/health and ignores everything else.
const AnnotationNodeRegistration = "hami.io/node-nvidia-register"

// AnnotationDevicesAllocated / AnnotationDevicesToAllocate carry what HAMi bound
// to a pod, and what is still in flight. Measured format:
//
//	allocated:    "GPU-<uuid>,NVIDIA,3000,30:;"
//	to-allocate:  ";;"   (binding complete)
const (
	AnnotationDevicesAllocated  = "hami.io/vgpu-devices-allocated"
	AnnotationDevicesToAllocate = "hami.io/vgpu-devices-to-allocate"
	AnnotationBindTime          = "hami.io/bind-time"
)

// NodeLabelGate is the label HAMi's scheduler requires on a GPU node. This
// operator does not apply it of its own accord — the user declares it in
// spec.workloadCluster.node.labels, and we apply labels we do not interpret.
const NodeLabelGate = "gpu"

// Device is one physical GPU as HAMi advertises it on a node.
type Device struct {
	// UUID is the GPU UUID ("GPU-…"), HAMi's device identity.
	UUID string
	// LogicalSlots is HAMi's inflated count (deviceSplitCount, default 10).
	// It is NOT a device count, and never a capacity.
	LogicalSlots int
	// MemoryMiB is the device's total GPU memory in MiB.
	MemoryMiB int64
	// CorePercent is the device's total compute, as a percentage of itself.
	CorePercent int64
	// Model is the human-readable model, e.g. "NVIDIA GeForce GTX 1080" — note
	// the spaces; do not assume a hyphenated form.
	Model string
	// Mode is HAMi's sharing mode, e.g. "hami-core". Informational.
	Mode string
	// Healthy is HAMi's own health verdict. Unhealthy devices are advertised but
	// must not be counted as capacity.
	Healthy bool
}

// rawDevice mirrors only the fields we require or use. Unknown fields are
// ignored by encoding/json, which is exactly the forward compatibility we want.
// Pointers distinguish "absent" from "zero" so a missing field is an error rather
// than a silent 0 — reporting a device as having no memory is worse than failing.
type rawDevice struct {
	ID      *string `json:"id"`
	Count   *int    `json:"count"`
	DevMem  *int64  `json:"devmem"`
	DevCore *int64  `json:"devcore"`
	Type    string  `json:"type"`
	Mode    string  `json:"mode"`
	Health  *bool   `json:"health"`
}

// ParseNodeRegistration parses the node inventory annotation.
//
// An absent annotation is not an error: it means HAMi has not registered this
// node yet (found=false). A malformed one IS an error — the caller maps it to
// CapacityProviderReady=False/RegistrationUnparseable and reports capacity as
// Unknown. Never as zero: "0 GPUs, plenty free" from a failed parse is the worst
// possible silent failure in this system.
func ParseNodeRegistration(annotations map[string]string) (devices []Device, found bool, err error) {
	raw, ok := annotations[AnnotationNodeRegistration]
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, false, nil
	}

	var rawDevices []rawDevice
	if err := json.Unmarshal([]byte(raw), &rawDevices); err != nil {
		return nil, true, fmt.Errorf("parsing %s: %w", AnnotationNodeRegistration, err)
	}

	out := make([]Device, 0, len(rawDevices))
	for i, rd := range rawDevices {
		switch {
		case rd.ID == nil || *rd.ID == "":
			return nil, true, fmt.Errorf("parsing %s: device %d has no id", AnnotationNodeRegistration, i)
		case rd.DevMem == nil:
			return nil, true, fmt.Errorf("parsing %s: device %s has no devmem", AnnotationNodeRegistration, *rd.ID)
		case rd.DevCore == nil:
			return nil, true, fmt.Errorf("parsing %s: device %s has no devcore", AnnotationNodeRegistration, *rd.ID)
		case rd.Health == nil:
			return nil, true, fmt.Errorf("parsing %s: device %s has no health", AnnotationNodeRegistration, *rd.ID)
		}
		d := Device{
			UUID:        *rd.ID,
			MemoryMiB:   *rd.DevMem,
			CorePercent: *rd.DevCore,
			Model:       rd.Type,
			Mode:        rd.Mode,
			Healthy:     *rd.Health,
		}
		if rd.Count != nil {
			d.LogicalSlots = *rd.Count
		}
		out = append(out, d)
	}
	return out, true, nil
}

// HealthyDevices returns the devices that count as capacity.
func HealthyDevices(devices []Device) []Device {
	out := make([]Device, 0, len(devices))
	for _, d := range devices {
		if d.Healthy {
			out = append(out, d)
		}
	}
	return out
}

// Allocation is one device share held by one workload.
type Allocation struct {
	UUID        string
	Vendor      string
	MemoryMiB   int64
	CorePercent int64
}

// ParseAllocations parses an allocated / to-allocate annotation value.
//
// Measured format: records of "uuid,vendor,memMiB,cores" terminated by ":", with
// ";" separating containers — e.g. "GPU-…,NVIDIA,3000,30:;". A completed
// binding leaves to-allocate as ";;", which parses to no allocations.
//
// Records with MORE than four fields are accepted and the extras ignored: HAMi
// has added fields before and will again, and refusing to parse would strand the
// pool at CapacityProviderReady=False on an upgrade.
func ParseAllocations(value string) ([]Allocation, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}

	var out []Allocation
	for _, group := range strings.Split(value, ";") {
		if strings.TrimSpace(group) == "" {
			continue
		}
		for _, record := range strings.Split(group, ":") {
			record = strings.TrimSpace(record)
			if record == "" {
				continue
			}
			fields := strings.Split(record, ",")
			if len(fields) < 4 {
				return nil, fmt.Errorf("parsing %s: record %q has %d fields, want at least 4 (uuid,vendor,memory,cores)",
					AnnotationDevicesAllocated, record, len(fields))
			}
			mem, err := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parsing %s: record %q memory: %w", AnnotationDevicesAllocated, record, err)
			}
			cores, err := strconv.ParseInt(strings.TrimSpace(fields[3]), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("parsing %s: record %q cores: %w", AnnotationDevicesAllocated, record, err)
			}
			out = append(out, Allocation{
				UUID:        strings.TrimSpace(fields[0]),
				Vendor:      strings.TrimSpace(fields[1]),
				MemoryMiB:   mem,
				CorePercent: cores,
			})
		}
	}
	return out, nil
}

// SumAllocations totals held memory and compute.
func SumAllocations(allocs []Allocation) (memoryMiB, corePercent int64) {
	for _, a := range allocs {
		memoryMiB += a.MemoryMiB
		corePercent += a.CorePercent
	}
	return memoryMiB, corePercent
}
