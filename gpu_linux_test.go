//go:build linux

package metrics

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeDRMCard(t *testing.T, root, card, driver, vendor, device string, files map[string]string) string {
	t.Helper()
	dev := filepath.Join(root, card, "device")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		t.Fatal(err)
	}
	if driver != "" {
		drv := filepath.Join(root, "drivers", driver)
		if err := os.MkdirAll(drv, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(drv, filepath.Join(dev, "driver")); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(filepath.Join(dev, "vendor"), vendor)
	mustWrite(filepath.Join(dev, "device"), device)
	for name, body := range files {
		mustWrite(filepath.Join(dev, name), body)
	}
	return dev
}

func TestGPUReadsAmdBusyAndTempAndSkipsSimpleDRM(t *testing.T) {
	root := t.TempDir()
	writeDRMCard(t, root, "card0", "amdgpu", "0x1002", "0x67df", map[string]string{
		"gpu_busy_percent":         "14",
		"hwmon/hwmon0/temp1_input": "45000",
	})
	if err := os.MkdirAll(filepath.Join(root, "card0-DP-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDRMCard(t, root, "card1", "simpledrm", "0x0000", "0x0000", nil)

	snap, err := readGPU(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.GPUs) != 1 {
		t.Fatalf("GPUs = %#v", snap.GPUs)
	}
	g := snap.GPUs[0]
	if g.PCIID != "1002:67df" || g.Driver != "amdgpu" {
		t.Fatalf("identity = %#v", g)
	}
	if !g.Usage.Valid || g.Usage.Fraction != 0.14 {
		t.Fatalf("usage = %#v", g.Usage)
	}
	if !g.TempValid || g.Celsius != 45 {
		t.Fatalf("temp = %#v", g)
	}
}

func TestGPUReadsAmdVRAM(t *testing.T) {
	tests := []struct {
		name      string
		files     map[string]string
		wantValid bool
		want      Capacity
	}{
		{"discrete", map[string]string{
			"mem_info_vram_used":  "1073741824",
			"mem_info_vram_total": "8589934592",
		}, true, Capacity{TotalBytes: 8589934592, UsedBytes: 1073741824, AvailableBytes: 7516192768}},
		{"apu carve-out reported as is", map[string]string{
			"mem_info_vram_used":  "268435456",
			"mem_info_vram_total": "536870912",
		}, true, Capacity{TotalBytes: 536870912, UsedBytes: 268435456, AvailableBytes: 268435456}},
		{"total missing", map[string]string{"mem_info_vram_used": "1"}, false, Capacity{}},
		{"total zero", map[string]string{"mem_info_vram_used": "1", "mem_info_vram_total": "0"}, false, Capacity{}},
		{"used above total clamps available", map[string]string{
			"mem_info_vram_used":  "9",
			"mem_info_vram_total": "8",
		}, true, Capacity{TotalBytes: 8, UsedBytes: 9, AvailableBytes: 0}},
		{"no files", nil, false, Capacity{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeDRMCard(t, root, "card0", "amdgpu", "0x1002", "0x744c", tt.files)
			snap, err := readGPU(root, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			g := snap.GPUs[0]
			if g.VRAMValid != tt.wantValid || g.VRAM != tt.want {
				t.Fatalf("VRAM = %#v valid=%v, want %#v valid=%v", g.VRAM, g.VRAMValid, tt.want, tt.wantValid)
			}
		})
	}
}

func TestGPUFillsNvidiaFromSMI(t *testing.T) {
	root := t.TempDir()
	writeNvidiaCard(t, root, "card0", "0000:01:00.0", "0x10de", "0x2684")
	called := false
	smi := func() ([]byte, error) {
		called = true
		return []byte("0000:01:00.0, 37, 61, 463, 8188, GeForce RTX 4090\n"), nil
	}
	snap, err := readGPU(root, nil, smi)
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("nvidia-smi was not invoked")
	}
	if len(snap.GPUs) != 1 {
		t.Fatalf("GPUs = %#v", snap.GPUs)
	}
	g := snap.GPUs[0]
	if !g.Usage.Valid || g.Usage.Fraction != 0.37 {
		t.Fatalf("usage = %#v", g.Usage)
	}
	if !g.TempValid || g.Celsius != 61 {
		t.Fatalf("temp = %#v", g)
	}
	if g.Name != "GeForce RTX 4090" {
		t.Fatalf("name = %q", g.Name)
	}
	wantVRAM := Capacity{TotalBytes: 8188 << 20, UsedBytes: 463 << 20, AvailableBytes: (8188 - 463) << 20}
	if !g.VRAMValid || g.VRAM != wantVRAM {
		t.Fatalf("VRAM = %#v valid=%v", g.VRAM, g.VRAMValid)
	}
}

func TestParseNvidiaCSVColumns(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		wantVRAM bool
		used     float64
		total    float64
		wantName string
		wantUtil bool
	}{
		{"full row", "0000:08:00.0, 0, 56, 463, 8188, NVIDIA GeForce RTX 4060", true, 463, 8188, "NVIDIA GeForce RTX 4060", true},
		{"memory not available", "0000:08:00.0, 12, 50, [N/A], [N/A], GRID A100-4C", false, 0, 0, "GRID A100-4C", true},
		{"comma in name", "0000:08:00.0, 1, 40, 10, 20, Quadro, Special Edition", true, 10, 20, "Quadro, Special Edition", true},
		{"old four-column row", "0000:08:00.0, 37, 61, GeForce RTX 4090", false, 0, 0, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := parseNvidiaCSV([]byte(tt.line + "\n"))
			if len(rows) != 1 {
				t.Fatalf("rows = %#v", rows)
			}
			r := rows[0]
			if r.hasVRAM != tt.wantVRAM || r.vramUsedMiB != tt.used || r.vramTotalMiB != tt.total {
				t.Fatalf("vram = %v %v/%v, want %v %v/%v", r.hasVRAM, r.vramUsedMiB, r.vramTotalMiB, tt.wantVRAM, tt.used, tt.total)
			}
			if r.name != tt.wantName || r.hasUtil != tt.wantUtil {
				t.Fatalf("name=%q util=%v, want %q %v", r.name, r.hasUtil, tt.wantName, tt.wantUtil)
			}
		})
	}
}

func TestGPUDoesNotInvokeSMIWithoutNvidia(t *testing.T) {
	root := t.TempDir()
	writeDRMCard(t, root, "card0", "i915", "0x8086", "0x5917", nil)
	smi := func() ([]byte, error) {
		t.Fatal("nvidia-smi invoked without an NVIDIA device")
		return nil, nil
	}
	if _, err := readGPU(root, nil, smi); err != nil {
		t.Fatal(err)
	}
}

func TestGPUMissingSMILeavesNvidiaInvalid(t *testing.T) {
	root := t.TempDir()
	writeNvidiaCard(t, root, "card0", "0000:01:00.0", "0x10de", "0x2684")
	snap, err := readGPU(root, nil, func() ([]byte, error) {
		return nil, os.ErrNotExist
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.GPUs) != 1 {
		t.Fatalf("GPUs = %#v", snap.GPUs)
	}
	g := snap.GPUs[0]
	if g.Usage.Valid || g.TempValid || g.VRAMValid {
		t.Fatalf("missing smi still set readings: %#v", g)
	}
}

func TestGPUPCINameFromIDsFile(t *testing.T) {
	root := t.TempDir()
	writeDRMCard(t, root, "card0", "i915", "0x8086", "0x5917", nil)
	ids := filepath.Join(t.TempDir(), "pci.ids")
	if err := os.WriteFile(ids, []byte("8086  Intel Corporation\n\t5917  UHD Graphics 620\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap, err := readGPU(root, []string{ids}, func() ([]byte, error) {
		t.Fatal("smi")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.GPUs) != 1 || snap.GPUs[0].Name != "UHD Graphics 620" {
		t.Fatalf("GPUs = %#v", snap.GPUs)
	}
}

func writeNvidiaCard(t *testing.T, root, card, bdf, vendor, device string) {
	t.Helper()
	pci := filepath.Join(root, "pci", bdf)
	if err := os.MkdirAll(pci, 0o755); err != nil {
		t.Fatal(err)
	}
	drv := filepath.Join(root, "drivers", "nvidia")
	if err := os.MkdirAll(drv, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(drv, filepath.Join(pci, "driver")); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"vendor": vendor, "device": device} {
		if err := os.WriteFile(filepath.Join(pci, name), []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cardDir := filepath.Join(root, card)
	if err := os.MkdirAll(cardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(pci, filepath.Join(cardDir, "device")); err != nil {
		t.Fatal(err)
	}
}

func writeIntelCard(t *testing.T, root, card, bdf, device string, files map[string]string) {
	t.Helper()
	pci := filepath.Join(root, "pci", bdf)
	if err := os.MkdirAll(pci, 0o755); err != nil {
		t.Fatal(err)
	}
	drv := filepath.Join(root, "drivers", "i915")
	if err := os.MkdirAll(drv, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(drv, filepath.Join(pci, "driver")); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"vendor": "0x8086", "device": device} {
		if err := os.WriteFile(filepath.Join(pci, name), []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range files {
		path := filepath.Join(pci, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cardDir := filepath.Join(root, card)
	if err := os.MkdirAll(cardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(pci, filepath.Join(cardDir, "device")); err != nil {
		t.Fatal(err)
	}
}

type fakeCounter struct {
	values []uint64
	errs   map[int]error
	calls  int
	closed bool
}

func (c *fakeCounter) read() (uint64, error) {
	index := c.calls
	if index >= len(c.values) {
		index = len(c.values) - 1
	}
	c.calls++
	if err := c.errs[c.calls-1]; err != nil {
		return 0, err
	}
	return c.values[index], nil
}

func (c *fakeCounter) close() error {
	c.closed = true
	return nil
}

type fakeOpener struct {
	counters map[uint64]*fakeCounter
	failed   map[uint64]error
	opened   int
}

func (o *fakeOpener) open(pmuType, config uint64) (gpuCounter, error) {
	o.opened++
	if err := o.failed[config]; err != nil {
		return nil, err
	}
	counter := o.counters[config]
	if counter == nil {
		return nil, fmt.Errorf("no fake counter for config 0x%x", config)
	}
	return counter, nil
}

func scriptedClock(times []time.Time) func() time.Time {
	i := 0
	return func() time.Time {
		if i >= len(times) {
			i = len(times) - 1
		}
		i++
		return times[i-1]
	}
}

func testTimes(count int) []time.Time {
	base := time.Unix(1_700_000_000, 0)
	times := make([]time.Time, count)
	for i := range times {
		times[i] = base.Add(time.Duration(i) * time.Second)
	}
	return times
}

func failSMI(t *testing.T) func() ([]byte, error) {
	t.Helper()
	return func() ([]byte, error) {
		t.Fatal("nvidia-smi invoked for an Intel-only fixture")
		return nil, nil
	}
}

func newTestGPUSampler(drmRoot, pmuRoot string, pciIDs []string, smi func() ([]byte, error), opener *fakeOpener, times []time.Time) *GPUSampler {
	sampler := newGPUSampler(drmRoot, pmuRoot, pciIDs, smi)
	sampler.open = opener.open
	sampler.now = scriptedClock(times)
	return sampler
}

func writeIntelPMUFixture(t *testing.T, pmuRoot string, events map[string]string) {
	t.Helper()
	units := make(map[string]string, len(events))
	for event := range events {
		units[event] = "ns"
	}
	writePMU(t, pmuRoot, "i915", "", events, units)
}

func TestGPUSamplerFillsIntelUsageAfterTwoSamples(t *testing.T) {
	root := t.TempDir()
	writeIntelCard(t, root, "card0", "0000:00:02.0", "0x3ea0", map[string]string{
		"hwmon/hwmon0/temp1_input": "45000",
	})
	pmuRoot := t.TempDir()
	writeIntelPMUFixture(t, pmuRoot, map[string]string{"rcs0-busy": "config=0x0", "bcs0-busy": "config=0x1000"})
	ids := filepath.Join(t.TempDir(), "pci.ids")
	if err := os.WriteFile(ids, []byte("8086  Intel Corporation\n\t3ea0  UHD Graphics 620\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opener := &fakeOpener{counters: map[uint64]*fakeCounter{
		0x0:    {values: []uint64{0, 250_000_000}},
		0x1000: {values: []uint64{0, 500_000_000}},
	}}
	sampler := newTestGPUSampler(root, pmuRoot, []string{ids}, failSMI(t), opener, testTimes(2))

	first, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if len(first.GPUs) != 1 {
		t.Fatalf("first GPUs = %#v", first.GPUs)
	}
	g := first.GPUs[0]
	if g.PCIID != "8086:3ea0" || g.Driver != "i915" || g.Name != "UHD Graphics 620" {
		t.Fatalf("first identity = %#v", g)
	}
	if !g.TempValid || g.Celsius != 45 {
		t.Fatalf("first temp = %#v", g)
	}
	if g.Usage.Valid {
		t.Fatalf("first sample has valid usage: %#v", g.Usage)
	}

	second, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if len(second.GPUs) != 1 {
		t.Fatalf("second GPUs = %#v", second.GPUs)
	}
	g = second.GPUs[0]
	if g.PCIID != "8086:3ea0" || g.Name != "UHD Graphics 620" || !g.TempValid || g.Celsius != 45 {
		t.Fatalf("second identity = %#v", g)
	}
	if !g.Usage.Valid || g.Usage.Fraction != 0.5 {
		t.Fatalf("second usage = %#v", g.Usage)
	}
}

func TestGPUSamplerValidZeroStaysValid(t *testing.T) {
	root := t.TempDir()
	writeIntelCard(t, root, "card0", "0000:00:02.0", "0x3ea0", nil)
	pmuRoot := t.TempDir()
	writeIntelPMUFixture(t, pmuRoot, map[string]string{"rcs0-busy": "config=0x0"})
	opener := &fakeOpener{counters: map[uint64]*fakeCounter{
		0x0: {values: []uint64{0, 0}},
	}}
	sampler := newTestGPUSampler(root, pmuRoot, nil, nil, opener, testTimes(2))
	if _, err := sampler.Sample(); err != nil {
		t.Fatal(err)
	}
	second, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if len(second.GPUs) != 1 || !second.GPUs[0].Usage.Valid || second.GPUs[0].Usage.Fraction != 0 {
		t.Fatalf("zero delta = %#v", second.GPUs)
	}
}

func TestGPUSamplerReadErrorRebaselinesAndRecovers(t *testing.T) {
	root := t.TempDir()
	writeIntelCard(t, root, "card0", "0000:00:02.0", "0x3ea0", nil)
	pmuRoot := t.TempDir()
	writeIntelPMUFixture(t, pmuRoot, map[string]string{"rcs0-busy": "config=0x0"})
	readErr := fmt.Errorf("counter read failed")
	opener := &fakeOpener{counters: map[uint64]*fakeCounter{
		0x0: {values: []uint64{0, 100_000, 200_000, 300_000}, errs: map[int]error{1: readErr}},
	}}
	sampler := newTestGPUSampler(root, pmuRoot, nil, nil, opener, testTimes(4))
	if _, err := sampler.Sample(); err != nil {
		t.Fatal(err)
	}
	second, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if second.GPUs[0].Usage.Valid {
		t.Fatalf("failed read produced valid usage: %#v", second.GPUs[0].Usage)
	}
	if len(second.Issues) != 1 || !errors.Is(second.Issues[0], readErr) {
		t.Fatalf("read error issues = %#v", second.Issues)
	}
	third, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if third.GPUs[0].Usage.Valid {
		t.Fatalf("rebaseline sample produced valid usage: %#v", third.GPUs[0].Usage)
	}
	fourth, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if !fourth.GPUs[0].Usage.Valid || fourth.GPUs[0].Usage.Fraction != 0.0001 {
		t.Fatalf("recovered usage = %#v", fourth.GPUs[0].Usage)
	}
}

func TestGPUSamplerCounterDecreaseRebaselinesAndRecovers(t *testing.T) {
	root := t.TempDir()
	writeIntelCard(t, root, "card0", "0000:00:02.0", "0x3ea0", nil)
	pmuRoot := t.TempDir()
	writeIntelPMUFixture(t, pmuRoot, map[string]string{"rcs0-busy": "config=0x0"})
	opener := &fakeOpener{counters: map[uint64]*fakeCounter{
		0x0: {values: []uint64{1_000_000, 500_000, 600_000}},
	}}
	sampler := newTestGPUSampler(root, pmuRoot, nil, nil, opener, testTimes(3))
	if _, err := sampler.Sample(); err != nil {
		t.Fatal(err)
	}
	second, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if second.GPUs[0].Usage.Valid {
		t.Fatalf("decrease produced valid usage: %#v", second.GPUs[0].Usage)
	}
	third, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if !third.GPUs[0].Usage.Valid || third.GPUs[0].Usage.Fraction != 0.0001 {
		t.Fatalf("post-reset usage = %#v", third.GPUs[0].Usage)
	}
}

func TestGPUSamplerImpossibleDeltaRebaselinesAndRecovers(t *testing.T) {
	root := t.TempDir()
	writeIntelCard(t, root, "card0", "0000:00:02.0", "0x3ea0", nil)
	pmuRoot := t.TempDir()
	writeIntelPMUFixture(t, pmuRoot, map[string]string{"rcs0-busy": "config=0x0"})
	opener := &fakeOpener{counters: map[uint64]*fakeCounter{
		0x0: {values: []uint64{0, 2_000_000_000, 2_100_000_000}},
	}}
	sampler := newTestGPUSampler(root, pmuRoot, nil, nil, opener, testTimes(3))
	if _, err := sampler.Sample(); err != nil {
		t.Fatal(err)
	}
	second, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if second.GPUs[0].Usage.Valid {
		t.Fatalf("impossible delta produced valid usage: %#v", second.GPUs[0].Usage)
	}
	third, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if !third.GPUs[0].Usage.Valid || third.GPUs[0].Usage.Fraction != 0.1 {
		t.Fatalf("post-rebaseline usage = %#v", third.GPUs[0].Usage)
	}
}

func TestGPUSamplerOpenFailureKeepsIdentityAndIssues(t *testing.T) {
	root := t.TempDir()
	writeIntelCard(t, root, "card0", "0000:00:02.0", "0x3ea0", map[string]string{
		"hwmon/hwmon0/temp1_input": "45000",
	})
	pmuRoot := t.TempDir()
	writeIntelPMUFixture(t, pmuRoot, map[string]string{"rcs0-busy": "config=0x0"})
	openErr := os.ErrPermission
	opener := &fakeOpener{failed: map[uint64]error{0x0: openErr}}
	sampler := newTestGPUSampler(root, pmuRoot, nil, nil, opener, testTimes(2))
	first, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	second, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	for i, snapshot := range []GPUSnapshot{first, second} {
		g := snapshot.GPUs[0]
		if g.PCIID != "8086:3ea0" || !g.TempValid {
			t.Fatalf("sample %d identity = %#v", i, g)
		}
		if g.Usage.Valid {
			t.Fatalf("sample %d usage = %#v", i, g.Usage)
		}
		found := false
		for _, issue := range snapshot.Issues {
			if errors.Is(issue, openErr) {
				found = true
			}
		}
		if !found {
			t.Fatalf("sample %d issues = %#v", i, snapshot.Issues)
		}
	}
}

func TestGPUSamplerMissingPMUIssuesAndKeepsIdentity(t *testing.T) {
	root := t.TempDir()
	writeIntelCard(t, root, "card0", "0000:00:02.0", "0x3ea0", nil)
	pmuRoot := t.TempDir()
	sampler := newTestGPUSampler(root, pmuRoot, nil, nil, &fakeOpener{}, testTimes(1))
	snapshot, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.GPUs) != 1 {
		t.Fatalf("GPUs = %#v", snapshot.GPUs)
	}
	g := snapshot.GPUs[0]
	if g.PCIID != "8086:3ea0" || g.Driver != "i915" {
		t.Fatalf("identity = %#v", g)
	}
	if g.Usage.Valid {
		t.Fatalf("usage = %#v", g.Usage)
	}
	if len(snapshot.Issues) == 0 {
		t.Fatalf("missing PMU produced no issues: %#v", snapshot.Issues)
	}
}

func TestGPUSamplerPassesAMDThroughWithoutPMU(t *testing.T) {
	root := t.TempDir()
	writeDRMCard(t, root, "card0", "amdgpu", "0x1002", "0x67df", map[string]string{
		"gpu_busy_percent": "14",
	})
	pmuRoot := t.TempDir()
	opener := &fakeOpener{}
	sampler := newTestGPUSampler(root, pmuRoot, nil, nil, opener, testTimes(1))
	snapshot, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.GPUs) != 1 || !snapshot.GPUs[0].Usage.Valid || snapshot.GPUs[0].Usage.Fraction != 0.14 {
		t.Fatalf("AMD passthrough = %#v", snapshot.GPUs)
	}
	if opener.opened != 0 {
		t.Fatalf("PMU counters opened = %d", opener.opened)
	}
}

func TestGPUSamplerDeviceDisappearanceDropsState(t *testing.T) {
	root := t.TempDir()
	writeIntelCard(t, root, "card0", "0000:00:02.0", "0x3ea0", nil)
	pmuRoot := t.TempDir()
	writeIntelPMUFixture(t, pmuRoot, map[string]string{"rcs0-busy": "config=0x0"})
	counter := &fakeCounter{values: []uint64{0, 100_000, 200_000}}
	opener := &fakeOpener{counters: map[uint64]*fakeCounter{0x0: counter}}
	sampler := newTestGPUSampler(root, pmuRoot, nil, nil, opener, testTimes(5))
	if _, err := sampler.Sample(); err != nil {
		t.Fatal(err)
	}
	second, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if !second.GPUs[0].Usage.Valid {
		t.Fatalf("second usage = %#v", second.GPUs[0].Usage)
	}
	if err := os.RemoveAll(filepath.Join(root, "card0")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "pci", "0000:00:02.0")); err != nil {
		t.Fatal(err)
	}
	third, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if len(third.GPUs) != 0 {
		t.Fatalf("GPUs after removal = %#v", third.GPUs)
	}
	if !counter.closed {
		t.Fatal("removed GPU counter was not closed")
	}
	writeIntelCard(t, root, "card0", "0000:00:02.0", "0x3ea0", nil)
	fourth, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if fourth.GPUs[0].Usage.Valid {
		t.Fatalf("reappeared GPU resumed history: %#v", fourth.GPUs[0].Usage)
	}
	fifth, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if !fifth.GPUs[0].Usage.Valid || fifth.GPUs[0].Usage.Fraction != 0 {
		t.Fatalf("reappeared GPU usage = %#v", fifth.GPUs[0].Usage)
	}
}

func TestGPUSamplerCloseIsIdempotentAndClosesCounters(t *testing.T) {
	root := t.TempDir()
	writeIntelCard(t, root, "card0", "0000:00:02.0", "0x3ea0", nil)
	pmuRoot := t.TempDir()
	writeIntelPMUFixture(t, pmuRoot, map[string]string{"rcs0-busy": "config=0x0", "bcs0-busy": "config=0x1000"})
	rcs0 := &fakeCounter{values: []uint64{0, 100_000}}
	bcs0 := &fakeCounter{values: []uint64{0, 100_000}}
	opener := &fakeOpener{counters: map[uint64]*fakeCounter{0x0: rcs0, 0x1000: bcs0}}
	sampler := newTestGPUSampler(root, pmuRoot, nil, nil, opener, testTimes(4))
	if _, err := sampler.Sample(); err != nil {
		t.Fatal(err)
	}
	if _, err := sampler.Sample(); err != nil {
		t.Fatal(err)
	}
	if err := sampler.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if err := sampler.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if !rcs0.closed || !bcs0.closed {
		t.Fatalf("counters closed = %v %v", rcs0.closed, bcs0.closed)
	}
	after, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if after.GPUs[0].Usage.Valid {
		t.Fatalf("sample after Close resumed history: %#v", after.GPUs[0].Usage)
	}
}

func TestReadGPUKeepsIntelUsageInvalid(t *testing.T) {
	root := t.TempDir()
	writeIntelCard(t, root, "card0", "0000:00:02.0", "0x3ea0", nil)
	snapshot, err := readGPU(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.GPUs) != 1 || snapshot.GPUs[0].Usage.Valid {
		t.Fatalf("ReadGPU Intel usage = %#v", snapshot.GPUs)
	}
}

// nvidia-smi prints an eight-digit PCI domain; sysfs prints four. The two
// name the same device, and a mismatch silently drops every NVIDIA reading.
func TestMatchBDFAcrossDomainWidths(t *testing.T) {
	for _, tc := range []struct {
		sysfs, smi string
		want       bool
	}{
		{"0000:08:00.0", "00000000:08:00.0", true},
		{"0000:08:00.0", "0000:08:00.0", true},
		{"0000:08:00.0", "08:00.0", true},
		{"0000:0A:00.0", "00000000:0a:00.0", true},
		{"0001:08:00.0", "00000000:08:00.0", false},
		{"0000:08:00.0", "00000000:09:00.0", false},
		{"0000:08:00.0", "00000000:08:00.1", false},
	} {
		if got := matchBDF(tc.sysfs, tc.smi); got != tc.want {
			t.Errorf("matchBDF(%q, %q) = %v, want %v", tc.sysfs, tc.smi, got, tc.want)
		}
	}
}

func TestGPUFillsNvidiaWithEightDigitDomain(t *testing.T) {
	root := t.TempDir()
	writeNvidiaCard(t, root, "card1", "0000:08:00.0", "0x10de", "0x2808")
	snap, err := readGPU(root, nil, func() ([]byte, error) {
		return []byte("00000000:08:00.0, 12, 56, 444, 8188, NVIDIA GeForce RTX 4060\n"), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	g := snap.GPUs[0]
	if !g.Usage.Valid || !g.TempValid || !g.VRAMValid {
		t.Fatalf("eight-digit domain dropped the nvidia-smi row: %#v", g)
	}
}
