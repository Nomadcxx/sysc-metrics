package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFDInfo writes one /proc/<pid>/fdinfo/<fd> entry under procRoot.
func writeFDInfo(t *testing.T, procRoot string, pid, fd int, body string) {
	t.Helper()
	writeFDInfoTo(t, procRoot, pid, fd, "/dev/dri/renderD128", body)
}

// writeFDInfoTo writes the fdinfo entry and the fd link the walk filters on.
func writeFDInfoTo(t *testing.T, procRoot string, pid, fd int, target, body string) {
	t.Helper()
	dir := filepath.Join(procRoot, fmt.Sprint(pid), "fdinfo")
	links := filepath.Join(procRoot, fmt.Sprint(pid), "fd")
	for _, d := range []string{dir, links} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprint(fd)), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(links, fmt.Sprint(fd))
	_ = os.Remove(link)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func fdinfoBody(pdev, id string, render, video uint64) string {
	return fmt.Sprintf("pos:\t0\nflags:\t02100002\ndrm-driver:\ti915\ndrm-client-id:\t%s\ndrm-pdev:\t%s\n"+
		"drm-engine-render:\t%d ns\ndrm-engine-copy:\t0 ns\ndrm-engine-video:\t%d ns\n", id, pdev, render, video)
}

func TestReadDRMClientsGroupsByDeviceAndClient(t *testing.T) {
	proc := t.TempDir()
	// One client holding two descriptors counts once.
	writeFDInfo(t, proc, 100, 7, fdinfoBody("0000:00:02.0", "126", 500, 20))
	writeFDInfo(t, proc, 100, 10, fdinfoBody("0000:00:02.0", "126", 500, 20))
	writeFDInfo(t, proc, 200, 3, fdinfoBody("0000:00:02.0", "9", 40, 0))
	writeFDInfo(t, proc, 300, 4, fdinfoBody("0000:08:00.0", "1", 999, 0))
	writeFDInfoTo(t, proc, 400, 1, "/dev/null", "pos:\t0\nflags:\t0100002\nmnt_id:\t26\n")
	// DRM keys behind a non-DRM descriptor are never opened: the walk only
	// parses descriptors that point into /dev/dri.
	writeFDInfoTo(t, proc, 500, 2, "/tmp/stale", fdinfoBody("0000:00:02.0", "77", 1, 0))
	if err := os.MkdirAll(filepath.Join(proc, "self"), 0o755); err != nil {
		t.Fatal(err)
	}

	clients := readDRMClients(proc)
	igpu := clients["0000:00:02.0"]
	if len(igpu) != 2 {
		t.Fatalf("clients on 0000:00:02.0 = %#v, want two", igpu)
	}
	if igpu["126"].busy["render"] != 500 || igpu["126"].busy["video"] != 20 || igpu["9"].busy["render"] != 40 {
		t.Fatalf("engine times = %#v", igpu)
	}
	if clients["0000:08:00.0"]["1"].busy["render"] != 999 {
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

	writeFDInfo(t, proc, 100, 7, fdinfoBody("0000:00:02.0", "126", 1_000_000_000, 0))
	writeFDInfo(t, proc, 200, 3, fdinfoBody("0000:00:02.0", "9", 0, 0))
	first, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if first.GPUs[0].Usage.Valid {
		t.Fatalf("first sample has usage %#v; it has no baseline", first.GPUs[0].Usage)
	}

	// Over one second: render busy 250ms + 250ms, video 100ms. The busiest
	// engine is the device's usage.
	writeFDInfo(t, proc, 100, 7, fdinfoBody("0000:00:02.0", "126", 1_250_000_000, 100_000_000))
	writeFDInfo(t, proc, 200, 3, fdinfoBody("0000:00:02.0", "9", 250_000_000, 0))
	second, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	g := second.GPUs[0]
	if !g.Usage.Valid || g.Usage.Fraction != 0.5 {
		t.Fatalf("usage = %#v, want 0.5 from DRM fdinfo", g.Usage)
	}
	// The denied PMU is not an error once fdinfo supplied the reading.
	for _, issue := range second.Issues {
		if strings.HasPrefix(issue.Source, pmuRoot) {
			t.Fatalf("PMU issue %v reported although fdinfo filled usage", issue)
		}
	}
}

// A GPU whose driver publishes no engine fdinfo can never get a reading from
// the walk. After a few empty walks the sampler stops paying for it, says so
// once, and retries only occasionally.
func TestGPUSamplerBacksOffAnFDInfoWalkThatFindsNothing(t *testing.T) {
	root := t.TempDir()
	writeIntelCard(t, root, "card0", "0000:00:02.0", "0x3ea0", nil)
	pmuRoot := t.TempDir()
	writeIntelPMUFixture(t, pmuRoot, map[string]string{"rcs0-busy": "config=0x0"})
	opener := &fakeOpener{failed: map[uint64]error{0x0: os.ErrPermission}}
	sampler := newTestGPUSampler(root, pmuRoot, nil, nil, opener, testTimes(fdinfoRetryEvery+fdinfoMaxEmptyWalks+2))
	sampler.procRoot = t.TempDir() // no DRM clients at all

	gaveUp := 0
	for i := 0; i < fdinfoRetryEvery+fdinfoMaxEmptyWalks+2; i++ {
		snap, err := sampler.Sample()
		if err != nil {
			t.Fatal(err)
		}
		for _, issue := range snap.Issues {
			if issue.Source == sampler.procRoot {
				gaveUp++
			}
		}
	}
	if sampler.fdWalks != fdinfoMaxEmptyWalks+1 {
		t.Fatalf("walks = %d, want %d empty walks then one retry", sampler.fdWalks, fdinfoMaxEmptyWalks+1)
	}
	if gaveUp != 1 {
		t.Fatalf("give-up issue reported %d times, want once", gaveUp)
	}
}

func busyOnly(engines map[string]uint64) drmClient {
	return drmClient{busy: engines}
}

func TestFDInfoUsageCountsOnlyClientsSeenTwice(t *testing.T) {
	prev := map[string]drmClient{
		"1": busyOnly(map[string]uint64{"render": 100}),             // exits before the next sample
		"2": busyOnly(map[string]uint64{"render": 500}),             // steady
		"3": busyOnly(map[string]uint64{"render": 900_000_000_000}), // id reused: counter went backwards
	}
	cur := map[string]drmClient{
		"2": busyOnly(map[string]uint64{"render": 100_000_500}),
		"3": busyOnly(map[string]uint64{"render": 5}),
		"4": busyOnly(map[string]uint64{"render": 800_000_000}), // new client: no baseline yet
	}
	busy, ok := fdinfoBusy(prev, cur, 1_000_000_000)
	if !ok || busy != 0.1 {
		t.Fatalf("busy = %v, %v; want 0.1 from the steady client alone", busy, ok)
	}

	if _, ok := fdinfoBusy(nil, cur, 1_000_000_000); ok {
		t.Fatal("no previous clients produced a usage; there is no baseline")
	}
	if _, ok := fdinfoBusy(prev, cur, 0); ok {
		t.Fatal("zero elapsed produced a usage")
	}
	over, ok := fdinfoBusy(map[string]drmClient{"2": busyOnly(map[string]uint64{"render": 0})},
		map[string]drmClient{"2": busyOnly(map[string]uint64{"render": 3_000_000_000})}, 1_000_000_000)
	if !ok || over != 1 {
		t.Fatalf("busy above elapsed = %v, %v; want clamped to 1", over, ok)
	}
}

// A class with several engine instances accumulates up to capacity × elapsed
// (drm-usage-stats), so two video engines each half busy are 50%, not 100%.
func TestFDInfoUsageDividesByEngineCapacity(t *testing.T) {
	prev := map[string]drmClient{"1": {busy: map[string]uint64{"render": 0, "video": 0}, capacity: map[string]uint64{"video": 2}}}
	cur := map[string]drmClient{"1": {busy: map[string]uint64{"render": 300_000_000, "video": 1_000_000_000}, capacity: map[string]uint64{"video": 2}}}
	busy, ok := fdinfoBusy(prev, cur, 1_000_000_000)
	if !ok || busy != 0.5 {
		t.Fatalf("busy = %v, %v; want 0.5 from two half-busy video engines", busy, ok)
	}

	proc := t.TempDir()
	writeFDInfo(t, proc, 100, 7, fdinfoBody("0000:00:02.0", "1", 0, 0)+"drm-engine-capacity-video:\t2\n")
	if got := readDRMClients(proc)["0000:00:02.0"]["1"].capacity["video"]; got != 2 {
		t.Fatalf("parsed video capacity = %d, want 2", got)
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
	writeFDInfo(t, proc, 100, 7, fdinfoBody("0000:00:02.0", "126", 0, 0))
	if _, err := sampler.Sample(); err != nil {
		t.Fatal(err)
	}
	writeFDInfo(t, proc, 100, 7, fdinfoBody("0000:00:02.0", "126", 900_000_000, 0))
	second, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if g := second.GPUs[0]; !g.Usage.Valid || g.Usage.Fraction != 0.25 {
		t.Fatalf("usage = %#v, want the PMU's 0.25", g.Usage)
	}
}

// BenchmarkReadDRMClients walks the real /proc: the per-sample cost of the
// unprivileged fallback on the machine it runs on.
func BenchmarkReadDRMClients(b *testing.B) {
	for b.Loop() {
		readDRMClients(procRoot)
	}
}
