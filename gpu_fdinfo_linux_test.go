package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// writeFDInfo writes one /proc/<pid>/fdinfo/<fd> entry under procRoot.
func writeFDInfo(t *testing.T, procRoot string, pid, fd int, body string) {
	t.Helper()
	dir := filepath.Join(procRoot, fmt.Sprint(pid), "fdinfo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprint(fd)), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func drmClient(pdev, id string, render, video uint64) string {
	return fmt.Sprintf("pos:\t0\nflags:\t02100002\ndrm-driver:\ti915\ndrm-client-id:\t%s\ndrm-pdev:\t%s\n"+
		"drm-engine-render:\t%d ns\ndrm-engine-copy:\t0 ns\ndrm-engine-video:\t%d ns\n", id, pdev, render, video)
}

func TestReadDRMClientsGroupsByDeviceAndClient(t *testing.T) {
	proc := t.TempDir()
	// One client holding two descriptors counts once.
	writeFDInfo(t, proc, 100, 7, drmClient("0000:00:02.0", "126", 500, 20))
	writeFDInfo(t, proc, 100, 10, drmClient("0000:00:02.0", "126", 500, 20))
	writeFDInfo(t, proc, 200, 3, drmClient("0000:00:02.0", "9", 40, 0))
	writeFDInfo(t, proc, 300, 4, drmClient("0000:08:00.0", "1", 999, 0))
	writeFDInfo(t, proc, 400, 1, "pos:\t0\nflags:\t0100002\nmnt_id:\t26\n")
	if err := os.MkdirAll(filepath.Join(proc, "self"), 0o755); err != nil {
		t.Fatal(err)
	}

	clients := readDRMClients(proc)
	igpu := clients["0000:00:02.0"]
	if len(igpu) != 2 {
		t.Fatalf("clients on 0000:00:02.0 = %#v, want two", igpu)
	}
	if igpu["126"]["render"] != 500 || igpu["126"]["video"] != 20 || igpu["9"]["render"] != 40 {
		t.Fatalf("engine times = %#v", igpu)
	}
	if clients["0000:08:00.0"]["1"]["render"] != 999 {
		t.Fatalf("second device = %#v", clients["0000:08:00.0"])
	}
	if got := readDRMClients(filepath.Join(proc, "missing")); len(got) != 0 {
		t.Fatalf("missing proc root = %#v, want none", got)
	}
}

func TestGPUSamplerFallsBackToFDInfoWhenPMUIsDenied(t *testing.T) {
	root := t.TempDir()
	writeIntelCard(t, root, "card0", "0000:00:02.0", "0x3ea0", nil)
	pmuRoot := t.TempDir()
	writeIntelPMUFixture(t, pmuRoot, map[string]string{"rcs0-busy": "config=0x0"})
	opener := &fakeOpener{failed: map[uint64]error{0x0: os.ErrPermission}}
	sampler := newTestGPUSampler(root, pmuRoot, nil, nil, opener, testTimes(2))
	proc := t.TempDir()
	sampler.procRoot = proc

	writeFDInfo(t, proc, 100, 7, drmClient("0000:00:02.0", "126", 1_000_000_000, 0))
	writeFDInfo(t, proc, 200, 3, drmClient("0000:00:02.0", "9", 0, 0))
	first, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if first.GPUs[0].Usage.Valid {
		t.Fatalf("first sample has usage %#v; it has no baseline", first.GPUs[0].Usage)
	}

	// Over one second: render busy 250ms + 250ms, video 100ms. The busiest
	// engine is the device's usage.
	writeFDInfo(t, proc, 100, 7, drmClient("0000:00:02.0", "126", 1_250_000_000, 100_000_000))
	writeFDInfo(t, proc, 200, 3, drmClient("0000:00:02.0", "9", 250_000_000, 0))
	second, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	g := second.GPUs[0]
	if !g.Usage.Valid || g.Usage.Fraction != 0.5 {
		t.Fatalf("usage = %#v, want 0.5 from DRM fdinfo", g.Usage)
	}
}

func TestFDInfoUsageCountsOnlyClientsSeenTwice(t *testing.T) {
	prev := map[string]map[string]map[string]uint64{
		"0000:00:02.0": {
			"1": {"render": 100},             // exits before the next sample
			"2": {"render": 500},             // steady
			"3": {"render": 900_000_000_000}, // id reused: counter went backwards
		},
	}
	cur := map[string]map[string]uint64{
		"2": {"render": 100_000_500},
		"3": {"render": 5},
		"4": {"render": 800_000_000}, // new client: no baseline yet
	}
	busy, ok := fdinfoBusy(prev["0000:00:02.0"], cur, 1_000_000_000)
	if !ok || busy != 0.1 {
		t.Fatalf("busy = %v, %v; want 0.1 from the steady client alone", busy, ok)
	}

	if _, ok := fdinfoBusy(nil, cur, 1_000_000_000); ok {
		t.Fatal("no previous clients produced a usage; there is no baseline")
	}
	if _, ok := fdinfoBusy(prev["0000:00:02.0"], cur, 0); ok {
		t.Fatal("zero elapsed produced a usage")
	}
	over, ok := fdinfoBusy(map[string]map[string]uint64{"2": {"render": 0}},
		map[string]map[string]uint64{"2": {"render": 3_000_000_000}}, 1_000_000_000)
	if !ok || over != 1 {
		t.Fatalf("busy above elapsed = %v, %v; want clamped to 1", over, ok)
	}
}

func TestGPUSamplerKeepsPMUUsageOverFDInfo(t *testing.T) {
	root := t.TempDir()
	writeIntelCard(t, root, "card0", "0000:00:02.0", "0x3ea0", nil)
	pmuRoot := t.TempDir()
	writeIntelPMUFixture(t, pmuRoot, map[string]string{"rcs0-busy": "config=0x0"})
	opener := &fakeOpener{counters: map[uint64]*fakeCounter{0x0: {values: []uint64{0, 250_000_000}}}}
	sampler := newTestGPUSampler(root, pmuRoot, nil, nil, opener, testTimes(2))
	proc := t.TempDir()
	sampler.procRoot = proc
	writeFDInfo(t, proc, 100, 7, drmClient("0000:00:02.0", "126", 0, 0))
	if _, err := sampler.Sample(); err != nil {
		t.Fatal(err)
	}
	writeFDInfo(t, proc, 100, 7, drmClient("0000:00:02.0", "126", 900_000_000, 0))
	second, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if g := second.GPUs[0]; !g.Usage.Valid || g.Usage.Fraction != 0.25 {
		t.Fatalf("usage = %#v, want the PMU's 0.25", g.Usage)
	}
}
