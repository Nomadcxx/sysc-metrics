//go:build linux

package metrics

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseProcessStatKeepsParenthesizedNameAndIdentity(t *testing.T) {
	got, err := parseProcessStat("42 (worker (copy) done) S 7 0 0 0 0 0 0 0 0 0 11 3 0 0 0 0 0 0 991 0 4")
	if err != nil {
		t.Fatal(err)
	}
	if got.pid != 42 || got.name != "worker (copy) done" || got.parentPID != 7 ||
		got.cpuTicks != 14 || got.startTimeTicks != 991 {
		t.Fatalf("stat = %#v", got)
	}
}

func TestParseProcessStatRejectsMalformedInput(t *testing.T) {
	for _, input := range []string{
		"", "42 worker S 1", "x (worker) S 1", "42 (worker) S 1", "42 (worker) S x 0 0 0 0 0 0 0 0 1 2 0 0 0 0 0 0 9",
	} {
		if _, err := parseProcessStat(input); err == nil {
			t.Errorf("parseProcessStat(%q) unexpectedly succeeded", input)
		}
	}
}

func TestParseProcessStatusReadsRealUIDAndResidentBytes(t *testing.T) {
	got, err := parseProcessStatus(strings.NewReader("Name:\tworker\nUid:\t1000\t1001\t1002\t1003\nVmRSS:\t42 kB\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.uidValid || got.uid != 1000 || !got.residentValid || got.residentBytes != 42*1024 {
		t.Fatalf("status = %#v", got)
	}

	got, err = parseProcessStatus(strings.NewReader("Uid:\t0\t0\t0\t0\n"))
	if err != nil || !got.uidValid || got.uid != 0 || got.residentValid {
		t.Fatalf("status without VmRSS = %#v, %v", got, err)
	}
	for _, input := range []string{"", "Uid:\tnope\n", "Uid:\t1\nUid:\t2\n", "Uid:\t1\nVmRSS:\t-1 kB\n"} {
		if _, err := parseProcessStatus(strings.NewReader(input)); err == nil {
			t.Errorf("parseProcessStatus(%q) unexpectedly succeeded", input)
		}
	}
}

func TestProcessSamplerUsesSequentialCPUAndForgetsVanishedProcesses(t *testing.T) {
	root := t.TempDir()
	writeProcStat(t, root, 100)
	writeProcess(t, root, 42, "worker (copy)", 1, 10, 991, 1000, 40, []string{"worker", "--copy"})

	sampler := newProcessSampler(root)
	first, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Processes) != 1 || first.Processes[0].CPU.Valid {
		t.Fatalf("first snapshot = %#v", first)
	}
	p := first.Processes[0]
	if p.Identity != (ProcessIdentity{PID: 42, StartTimeTicks: 991}) || p.Name != "worker (copy)" ||
		p.ParentPID != 1 || !p.UIDValid || p.UID != 1000 || !p.ResidentValid || p.ResidentBytes != 40*1024 ||
		len(p.Args) != 2 || p.Args[1] != "--copy" {
		t.Fatalf("process = %#v", p)
	}

	writeProcStat(t, root, 200)
	writeProcess(t, root, 42, "worker (copy)", 1, 30, 991, 1000, 44, []string{"worker", "--copy"})
	second, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Processes) != 1 || !second.Processes[0].CPU.Valid || second.Processes[0].CPU.Fraction != 0.2 {
		t.Fatalf("second snapshot = %#v", second)
	}

	if err := os.RemoveAll(filepath.Join(root, "42")); err != nil {
		t.Fatal(err)
	}
	writeProcStat(t, root, 300)
	third, err := sampler.Sample()
	if err != nil || len(third.Processes) != 0 {
		t.Fatalf("vanished snapshot = %#v, %v", third, err)
	}

	writeProcStat(t, root, 400)
	writeProcess(t, root, 42, "worker", 1, 50, 991, 1000, 44, nil)
	fourth, err := sampler.Sample()
	if err != nil || len(fourth.Processes) != 1 || fourth.Processes[0].CPU.Valid {
		t.Fatalf("reappeared snapshot = %#v, %v", fourth, err)
	}
}

func TestProcessSamplerDoesNotBridgePIDReuse(t *testing.T) {
	root := t.TempDir()
	writeProcStat(t, root, 100)
	writeProcess(t, root, 7, "old", 1, 10, 100, 1000, 1, nil)
	sampler := newProcessSampler(root)
	if _, err := sampler.Sample(); err != nil {
		t.Fatal(err)
	}

	writeProcStat(t, root, 200)
	writeProcess(t, root, 7, "new", 1, 80, 200, 1000, 2, nil)
	got, err := sampler.Sample()
	if err != nil || len(got.Processes) != 1 || got.Processes[0].CPU.Valid || got.Processes[0].Identity.StartTimeTicks != 200 {
		t.Fatalf("reused PID snapshot = %#v, %v", got, err)
	}
}

func TestProcessSamplerKeepsGoodProcessesAndReportsBadEntries(t *testing.T) {
	root := t.TempDir()
	writeProcStat(t, root, 100)
	writeProcess(t, root, 2, "good", 1, 5, 20, 1000, 1, nil)
	if err := os.Mkdir(filepath.Join(root, "3"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "3", "stat"), []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := newProcessSampler(root).Sample()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Processes) != 1 || got.Processes[0].Identity.PID != 2 || len(got.Issues) != 1 ||
		got.Issues[0].Source != filepath.Join(root, "3", "stat") {
		t.Fatalf("partial snapshot = %#v", got)
	}
}

func TestProcessSamplerKeepsProcessWhenStatusIsPartial(t *testing.T) {
	root := t.TempDir()
	writeProcStat(t, root, 100)
	writeProcess(t, root, 2, "partial", 1, 5, 20, 1000, 1, nil)
	status := filepath.Join(root, "2", "status")
	if err := os.WriteFile(status, []byte("Uid:\tinvalid\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := newProcessSampler(root).Sample()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Processes) != 1 || got.Processes[0].Identity.PID != 2 || got.Processes[0].UIDValid ||
		got.Processes[0].ResidentValid || len(got.Issues) != 1 || got.Issues[0].Source != status {
		t.Fatalf("partial status snapshot = %#v", got)
	}
}

func TestValidateProcessIdentityRefusesReplacementAndVanishedProcess(t *testing.T) {
	root := t.TempDir()
	writeProcess(t, root, 42, "worker", 1, 1, 991, 1000, 1, nil)
	id := ProcessIdentity{PID: 42, StartTimeTicks: 991}
	if err := validateProcessIdentity(root, id); err != nil {
		t.Fatal(err)
	}

	writeProcess(t, root, 42, "replacement", 1, 1, 992, 1000, 1, nil)
	if err := validateProcessIdentity(root, id); !errors.Is(err, ErrProcessIdentityChanged) {
		t.Fatalf("replacement validation error = %v", err)
	}
	if err := os.RemoveAll(filepath.Join(root, "42")); err != nil {
		t.Fatal(err)
	}
	if err := validateProcessIdentity(root, id); err == nil {
		t.Fatal("vanished process validated")
	}
	if err := validateProcessIdentity(root, ProcessIdentity{}); err == nil {
		t.Fatal("zero identity validated")
	}
}

func writeProcStat(t *testing.T, root string, total uint64) {
	t.Helper()
	body := fmt.Sprintf("cpu %d 0 0 0 0 0 0 0\n", total)
	if err := os.WriteFile(filepath.Join(root, "stat"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeProcess(t *testing.T, root string, pid int, name string, ppid int, cpu, start uint64, uid uint32, rssKB uint64, args []string) {
	t.Helper()
	dir := filepath.Join(root, fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fields := make([]string, 22) // fields 3 through 24
	for i := range fields {
		fields[i] = "0"
	}
	fields[0] = "S"
	fields[1] = fmt.Sprint(ppid)
	fields[11] = fmt.Sprint(cpu)
	fields[12] = "0"
	fields[19] = fmt.Sprint(start)
	fields[21] = fmt.Sprint(rssKB)
	stat := fmt.Sprintf("%d (%s) %s\n", pid, name, strings.Join(fields, " "))
	status := fmt.Sprintf("Name:\t%s\nUid:\t%d\t%d\t%d\t%d\nVmRSS:\t%d kB\n", name, uid, uid, uid, uid, rssKB)
	cmdline := strings.Join(args, "\x00")
	if len(args) > 0 {
		cmdline += "\x00"
	}
	for name, body := range map[string]string{"stat": stat, "status": status, "cmdline": cmdline} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProcessSamplerInvalidatesImpossibleCPUDelta(t *testing.T) {
	root := t.TempDir()
	writeProcStat(t, root, 100)
	writeProcess(t, root, 42, "worker", 1, 10, 991, 1000, 40, []string{"worker"})

	sampler := newProcessSampler(root)
	if _, err := sampler.Sample(); err != nil {
		t.Fatal(err)
	}
	// Both aggregate reads see 110. The process advanced 20 against that
	// 10-tick window, which is still above 1 once the reads cover it, so
	// the sample stays invalid.
	writeProcStat(t, root, 110)
	writeProcess(t, root, 42, "worker", 1, 30, 991, 1000, 40, []string{"worker"})
	snap, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Processes) != 1 {
		t.Fatalf("processes = %#v", snap.Processes)
	}
	if snap.Processes[0].CPU.Valid {
		t.Fatalf("impossible fraction accepted: %#v", snap.Processes[0].CPU)
	}
}

// Aggregate /proc/stat used to be read once, before the walk. A saturated
// process reached a few milliseconds later then led that total — 801 process
// ticks against an 800-tick aggregate — and CPU.Valid stayed false (issue #21).
//
// Each sample's first total is the pre-walk read and its second is the
// post-walk read:
//
//	sample 1: before 1000, process 100, after 1050
//	sample 2: before 1800 (delta 800 from the previous pre-walk read),
//	          process 901 (delta 801), after 1801
//
// Using the opening read as the window end drops the process. The previous
// pre-walk total through this post-walk total is 801 and covers both process
// reads, so the fraction is 1. Closing the window with the post-walk reads
// alone (1801-1050) is still short of 801 when the process was reached later
// on the previous walk.
func TestProcessSamplerKeepsSaturatedProcessInsideAggregateWindow(t *testing.T) {
	root := t.TempDir()
	writeProcStat(t, root, 0)
	writeProcess(t, root, 42, "worker", 1, 100, 991, 1000, 40, []string{"worker"})
	totals := &scriptedCPUTotals{}
	totals.set(1000, 1050)
	sampler := newProcessSampler(root)
	sampler.readTotal = totals.read

	first, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Processes) != 1 || first.Processes[0].CPU.Valid {
		t.Fatalf("first snapshot = %#v", first)
	}
	if totals.reads != 2 {
		t.Fatalf("first sample aggregate reads = %d, want 2 (before and after the walk)", totals.reads)
	}

	totals.set(1800, 1801)
	writeProcess(t, root, 42, "worker", 1, 901, 991, 1000, 40, []string{"worker"})
	second, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if totals.reads != 2 {
		t.Fatalf("second sample aggregate reads = %d, want 2 (before and after the walk)", totals.reads)
	}
	if len(second.Processes) != 1 {
		t.Fatalf("processes = %#v", second.Processes)
	}
	cpu := second.Processes[0].CPU
	if !cpu.Valid || cpu.Fraction != 1 || cpu.Fraction > 1 {
		t.Fatalf("CPU = %#v, want valid fraction 1 (801 process ticks inside the covering aggregate window, not the 800-tick pre-walk delta)", cpu)
	}
}

// scriptedCPUTotals returns the pre-walk aggregate on the first read of a
// sample and the post-walk aggregate on every read after that.
type scriptedCPUTotals struct {
	before uint64
	after  uint64
	reads  int
}

func (s *scriptedCPUTotals) set(before, after uint64) {
	s.before = before
	s.after = after
	s.reads = 0
}

func (s *scriptedCPUTotals) read() (uint64, error) {
	s.reads++
	if s.reads == 1 {
		return s.before, nil
	}
	return s.after, nil
}
