//go:build linux

package metrics

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writePMU(t *testing.T, devicesRoot, name, linkedBDF string, events, units map[string]string) string {
	t.Helper()
	root := filepath.Join(devicesRoot, name)
	if err := os.MkdirAll(filepath.Join(root, "events"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "format"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "type"), []byte("13\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "format", "i915_eventid"), []byte("config:0-20\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for event, config := range events {
		if err := os.WriteFile(filepath.Join(root, "events", event), []byte(config+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for event, unit := range units {
		if err := os.WriteFile(filepath.Join(root, "events", event+".unit"), []byte(unit+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if linkedBDF != "" {
		target := filepath.Join(devicesRoot, "..", "pci", linkedBDF)
		if err := os.Symlink(target, filepath.Join(root, "device")); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestPMUDiscoversBusyEventsAndIgnoresOthers(t *testing.T) {
	root := t.TempDir()
	writePMU(t, root, "i915", "", map[string]string{
		"rcs0-busy":        "config=0x0",
		"bcs0-busy":        "config=0x1000",
		"engine7-busy":     "config=0x4000",
		"rcs0-sema":        "config=0x2",
		"actual-frequency": "config=0x100000",
	}, map[string]string{
		"rcs0-busy":    "ns",
		"bcs0-busy":    "ns",
		"engine7-busy": "ns",
	})

	candidates, issues := discoverGPUPMUs(root, "i915")
	if len(issues) != 0 {
		t.Fatalf("issues = %#v", issues)
	}
	if len(candidates) != 1 {
		t.Fatalf("candidates = %#v", candidates)
	}
	events, eventIssues := discoverBusyEvents(candidates[0].root)
	if len(eventIssues) != 0 {
		t.Fatalf("event issues = %#v", eventIssues)
	}
	if len(events) != 3 {
		t.Fatalf("events = %#v", events)
	}
	// Sorted by engine name; non-busy and unrelated events are ignored, and
	// engine names unknown to this build are discovered from sysfs.
	if events[0].name != "bcs0" || events[0].config != 0x1000 {
		t.Fatalf("events[0] = %#v", events[0])
	}
	if events[1].name != "engine7" || events[1].config != 0x4000 {
		t.Fatalf("events[1] = %#v", events[1])
	}
	if events[2].name != "rcs0" || events[2].config != 0x0 {
		t.Fatalf("events[2] = %#v", events[2])
	}
}

func TestPMUBusyEventProblemsBecomeIssues(t *testing.T) {
	root := t.TempDir()
	writePMU(t, root, "i915", "", map[string]string{
		"rcs0-busy":  "config=0x0",
		"bcs0-busy":  "config=not-hex",
		"vcs0-busy":  "events=0x1",
		"vecs0-busy": "config=0x3000",
	}, map[string]string{
		"rcs0-busy":  "ns",
		"vecs0-busy": "M",
		// bcs0-busy and vcs0-busy have no unit file.
	})

	candidates, issues := discoverGPUPMUs(root, "i915")
	if len(issues) != 0 || len(candidates) != 1 {
		t.Fatalf("candidates = %#v, issues = %#v", candidates, issues)
	}
	events, eventIssues := discoverBusyEvents(candidates[0].root)
	if len(events) != 1 || events[0].name != "rcs0" {
		t.Fatalf("events = %#v", events)
	}
	if len(eventIssues) != 3 {
		t.Fatalf("event issues = %#v", eventIssues)
	}
	sources := make(map[string]bool, len(eventIssues))
	for _, issue := range eventIssues {
		sources[filepath.Base(issue.Source)] = true
		if issue.Err == nil {
			t.Fatalf("issue without cause: %#v", issue)
		}
	}
	for _, expected := range []string{"bcs0-busy", "vcs0-busy", "vecs0-busy.unit"} {
		if !sources[expected] {
			t.Fatalf("missing issue for %s: %#v", expected, eventIssues)
		}
	}
}

func TestPMUDiscoversCandidatesByDriverName(t *testing.T) {
	root := t.TempDir()
	writePMU(t, root, "i915", "", map[string]string{"rcs0-busy": "config=0x0"}, map[string]string{"rcs0-busy": "ns"})
	writePMU(t, root, "i915_0000_00_02_0", "0000:00:02.0", map[string]string{"rcs0-busy": "config=0x0"}, map[string]string{"rcs0-busy": "ns"})
	for _, other := range []string{"cpu", "msr", "software"} {
		if err := os.MkdirAll(filepath.Join(root, other), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	candidates, issues := discoverGPUPMUs(root, "i915")
	if len(issues) != 0 {
		t.Fatalf("issues = %#v", issues)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidates = %#v", candidates)
	}
	byName := make(map[string]pmuCandidate, len(candidates))
	for _, candidate := range candidates {
		byName[candidate.name] = candidate
		if candidate.pmuType != 13 {
			t.Fatalf("candidate type = %d", candidate.pmuType)
		}
	}
	if _, ok := byName["i915"]; !ok {
		t.Fatalf("missing exact i915 candidate: %#v", candidates)
	}
	if got := byName["i915_0000_00_02_0"].bdf; got != "0000:00:02.0" {
		t.Fatalf("linked candidate bdf = %q", got)
	}
	if got := byName["i915"].bdf; got != "" {
		t.Fatalf("unlinked candidate bdf = %q", got)
	}
}

func TestPMUMissingTypeOrEventsIsAnIssue(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "i915"), 0o755); err != nil {
		t.Fatal(err)
	}
	candidates, issues := discoverGPUPMUs(root, "i915")
	if len(candidates) != 0 || len(issues) != 1 {
		t.Fatalf("candidates = %#v, issues = %#v", candidates, issues)
	}

	candidates, issues = discoverGPUPMUs(filepath.Join(root, "absent"), "i915")
	if len(candidates) != 0 || len(issues) != 1 {
		t.Fatalf("candidates = %#v, issues = %#v", candidates, issues)
	}
}

func TestPMUMapsByDeviceLink(t *testing.T) {
	gpus := []gpuRef{{bdf: "0000:00:02.0", driver: "i915"}, {bdf: "0000:03:00.0", driver: "i915"}}
	pmus := []pmuCandidate{
		{name: "i915_0000_00_02_0", driver: "i915", bdf: "0000:00:02.0"},
		{name: "i915_0000_03_00_0", driver: "i915", bdf: "0000:03:00.0"},
	}
	mapped := mapGPUPMUs(gpus, pmus)
	if mapped[0] != 0 || mapped[1] != 1 {
		t.Fatalf("mapped = %v", mapped)
	}
}

func TestPMUMapsSingleUnlinkedGPU(t *testing.T) {
	gpus := []gpuRef{{bdf: "0000:00:02.0", driver: "i915"}}
	pmus := []pmuCandidate{{name: "i915", driver: "i915", bdf: ""}}
	mapped := mapGPUPMUs(gpus, pmus)
	if mapped[0] != 0 {
		t.Fatalf("mapped = %v", mapped)
	}
}

func TestPMUDoesNotMapAmbiguousGPUs(t *testing.T) {
	gpus := []gpuRef{{bdf: "0000:00:02.0", driver: "i915"}, {bdf: "0000:03:00.0", driver: "i915"}}
	pmus := []pmuCandidate{{name: "i915", driver: "i915", bdf: ""}}
	mapped := mapGPUPMUs(gpus, pmus)
	if mapped[0] != -1 || mapped[1] != -1 {
		t.Fatalf("mapped = %v", mapped)
	}
}

func TestPMUDoesNotMapAcrossDriversOrUnknownLinks(t *testing.T) {
	gpus := []gpuRef{
		{bdf: "0000:03:00.0", driver: "amdgpu"},
		{bdf: "0000:00:02.0", driver: "i915"},
	}
	pmus := []pmuCandidate{
		{name: "i915", driver: "i915", bdf: ""},
		{name: "i915_0000_99_99_9", driver: "i915", bdf: "0000:99:99.9"},
	}
	mapped := mapGPUPMUs(gpus, pmus)
	if mapped[0] != -1 || mapped[1] != -1 {
		t.Fatalf("mapped = %v", mapped)
	}
}

func TestPMUFormatMaskParsing(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "format"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "format", "i915_eventid"), []byte("config:0-20\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mask, err := formatConfigMask(filepath.Join(root, "format"))
	if err != nil || mask != 0x1fffff {
		t.Fatalf("mask = 0x%x err = %v", mask, err)
	}
	if err := os.WriteFile(filepath.Join(root, "format", "broken"), []byte("not-a-format\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := formatConfigMask(filepath.Join(root, "absent")); err == nil {
		t.Fatal("missing format root returned no error")
	}
}

func TestPMUBusyEventOutsideFormatMaskIsAnIssue(t *testing.T) {
	root := t.TempDir()
	writePMU(t, root, "i915", "", map[string]string{
		"rcs0-busy":    "config=0x1000",
		"engine9-busy": "config=0x800000",
	}, map[string]string{
		"rcs0-busy":    "ns",
		"engine9-busy": "ns",
	})

	candidates, issues := discoverGPUPMUs(root, "i915")
	if len(issues) != 0 || len(candidates) != 1 {
		t.Fatalf("candidates = %#v, issues = %#v", candidates, issues)
	}
	events, eventIssues := discoverBusyEvents(candidates[0].root)
	if len(events) != 1 || events[0].name != "rcs0" {
		t.Fatalf("events = %#v", events)
	}
	if len(eventIssues) != 1 || filepath.Base(eventIssues[0].Source) != "engine9-busy" {
		t.Fatalf("event issues = %#v", eventIssues)
	}
}

func TestPMUEngineBusyFirstSampleHasNoDelta(t *testing.T) {
	fraction, valid := engineBusy(0, 500_000_000, false, time.Second)
	if valid {
		t.Fatalf("first sample reported valid: %v", fraction)
	}
}

func TestPMUEngineBusySecondSampleIsValidFraction(t *testing.T) {
	fraction, valid := engineBusy(0, 250_000_000, true, time.Second)
	if !valid || fraction != 0.25 {
		t.Fatalf("fraction = %v valid = %v", fraction, valid)
	}
}

func TestPMUEngineBusyZeroDeltaIsValidZero(t *testing.T) {
	fraction, valid := engineBusy(1_000_000, 1_000_000, true, time.Second)
	if !valid || fraction != 0 {
		t.Fatalf("fraction = %v valid = %v", fraction, valid)
	}
}

func TestPMUEngineBusyCounterDecreaseRebaselines(t *testing.T) {
	fraction, valid := engineBusy(2_000_000_000, 500_000_000, true, time.Second)
	if valid {
		t.Fatalf("decrease reported valid: %v", fraction)
	}
}

func TestPMUEngineBusyImpossibleDeltaRebaselines(t *testing.T) {
	fraction, valid := engineBusy(0, 2_000_000_000, true, time.Second)
	if valid {
		t.Fatalf("impossible delta reported valid: %v", fraction)
	}
}

func TestPMUEngineBusyNonPositiveIntervalInvalid(t *testing.T) {
	for _, elapsed := range []time.Duration{0, -time.Second} {
		if _, valid := engineBusy(0, 500_000_000, true, elapsed); valid {
			t.Fatalf("elapsed %v reported valid", elapsed)
		}
	}
}

func TestPMUEngineBusyFractionStaysBounded(t *testing.T) {
	for _, span := range []time.Duration{time.Nanosecond, time.Millisecond, time.Second, time.Hour} {
		fraction, valid := engineBusy(0, uint64(span.Nanoseconds()), true, span)
		if !valid || fraction <= 0 || fraction > 1 {
			t.Fatalf("span %v: fraction = %v valid = %v", span, fraction, valid)
		}
	}
}

func TestPMUAggregationIsOrderStableMaximum(t *testing.T) {
	fractions := []float64{0.125, 0.875, 0.5}
	if got := maxBusyFraction(fractions); got != 0.875 {
		t.Fatalf("max = %v", got)
	}
	if got := maxBusyFraction([]float64{0.875, 0.125, 0.5}); got != 0.875 {
		t.Fatalf("max = %v", got)
	}
	if got := maxBusyFraction(nil); got != 0 {
		t.Fatalf("max = %v", got)
	}
}
