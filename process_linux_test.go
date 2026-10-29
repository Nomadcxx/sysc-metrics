//go:build linux

package metrics

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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
	got, err := parseProcessStatus(strings.NewReader("Name:\tworker\n\u00a0Uid:\t1000\t1001\t1002\t1003\n\u2003VmRSS:\t42 kB\u00a0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.uidValid || got.uid != 1000 || !got.residentValid || got.residentBytes != 42*1024 || got.swapValid {
		t.Fatalf("status = %#v", got)
	}

	got, err = parseProcessStatus(strings.NewReader("Uid:\t0\t0\t0\t0\n"))
	if err != nil || !got.uidValid || got.uid != 0 || got.residentValid {
		t.Fatalf("status without VmRSS = %#v, %v", got, err)
	}
	for _, input := range []string{
		"", "Uid:\tnope\n", "Uid:\t1\nUid:\t2\n", "Uid:\t1\nVmRSS:\t-1 kB\n",
		"Uid:\t1\nVmSwap:\t-1 kB\n", "Uid:\t1\nVmSwap:\t1 kB\nVmSwap:\t2 kB\n",
		"Uid:\t1\nVmSwap:\t18014398509481984 kB\n",
	} {
		if _, err := parseProcessStatus(strings.NewReader(input)); err == nil {
			t.Errorf("parseProcessStatus(%q) unexpectedly succeeded", input)
		}
	}
}

func TestParseProcessStatusReadsVmSwap(t *testing.T) {
	got, err := parseProcessStatus(strings.NewReader("Uid:\t1000\t1000\t1000\t1000\nVmSwap:\t7 kB\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.swapValid || got.swapBytes != 7*1024 {
		t.Fatalf("swap = %d, valid=%v", got.swapBytes, got.swapValid)
	}
}

func TestParseProcessIOReadsDiskCounters(t *testing.T) {
	got, err := parseProcessIO(strings.NewReader("rchar: 90\nread_bytes: 300\nwrite_bytes: 700\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.readBytes != 300 || got.writeBytes != 700 {
		t.Fatalf("I/O counters = %#v", got)
	}
	for _, input := range []string{
		"write_bytes: 1\n",
		"read_bytes: nope\nwrite_bytes: 1\n",
		"read_bytes: 1\nread_bytes: 2\nwrite_bytes: 1\n",
	} {
		if _, err := parseProcessIO(strings.NewReader(input)); err == nil {
			t.Errorf("parseProcessIO(%q) unexpectedly succeeded", input)
		}
	}
}

func TestProcessSamplerAddsExecutableSwapAndIORates(t *testing.T) {
	root := t.TempDir()
	writeProcStat(t, root, 100)
	writeProcess(t, root, 42, "worker", 1, 10, 991, 1000, 40, []string{"worker"})
	writeProcessIO(t, root, 42, 100, 200)
	sampler := newProcessSampler(root)

	first, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Processes) != 1 {
		t.Fatalf("first snapshot = %#v", first)
	}
	p := first.Processes[0]
	if !p.ExecutableValid || p.Executable != "/usr/bin/worker" || !p.SwapValid || p.SwapBytes != 7*1024 {
		t.Fatalf("first process details = %#v", p)
	}
	if p.IO.Valid {
		t.Fatalf("first I/O sample must be invalid: %#v", p.IO)
	}

	time.Sleep(time.Millisecond)
	writeProcStat(t, root, 200)
	writeProcess(t, root, 42, "worker", 1, 30, 991, 1000, 40, []string{"worker"})
	writeProcessIO(t, root, 42, 160, 280)
	second, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Processes) != 1 || !second.Processes[0].IO.Valid {
		t.Fatalf("second process I/O = %#v", second)
	}
	elapsed := second.CollectedAt.Sub(first.CollectedAt).Seconds()
	if elapsed <= 0 {
		t.Fatalf("sample interval = %v", elapsed)
	}
	got := second.Processes[0].IO
	if got.ReadBytesPerSecond != 60/elapsed || got.WriteBytesPerSecond != 80/elapsed {
		t.Fatalf("I/O rates = %#v, interval=%v", got, elapsed)
	}

	time.Sleep(time.Millisecond)
	writeProcStat(t, root, 300)
	writeProcess(t, root, 42, "worker", 1, 50, 991, 1000, 40, []string{"worker"})
	writeProcessIO(t, root, 42, 150, 270)
	third, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Processes) != 1 || third.Processes[0].IO.Valid {
		t.Fatalf("regressed I/O counters stayed valid: %#v", third)
	}

	time.Sleep(time.Millisecond)
	writeProcStat(t, root, 400)
	writeProcess(t, root, 42, "worker", 1, 70, 991, 1000, 40, []string{"worker"})
	writeProcessIO(t, root, 42, 150, 270)
	fourth, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if len(fourth.Processes) != 1 || !fourth.Processes[0].IO.Valid ||
		fourth.Processes[0].IO.ReadBytesPerSecond != 0 || fourth.Processes[0].IO.WriteBytesPerSecond != 0 {
		t.Fatalf("valid zero-rate sample = %#v", fourth)
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
	writeProcess(t, root, 42, "replacement", 1, 30, 991, 1000, 44, []string{"replacement", "--second"})
	second, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Processes) != 1 || !second.Processes[0].CPU.Valid || second.Processes[0].CPU.Fraction != 0.2 {
		t.Fatalf("second snapshot = %#v", second)
	}
	if first.Processes[0].Name != "worker (copy)" || len(first.Processes[0].Args) != 2 || first.Processes[0].Args[1] != "--copy" {
		t.Fatalf("first snapshot changed after buffer reuse: %#v", first.Processes[0])
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
	writeProcessIO(t, root, 7, 100, 200)
	sampler := newProcessSampler(root)
	if _, err := sampler.Sample(); err != nil {
		t.Fatal(err)
	}

	writeProcStat(t, root, 200)
	writeProcess(t, root, 7, "new", 1, 80, 200, 1000, 2, nil)
	writeProcessIO(t, root, 7, 200, 400)
	got, err := sampler.Sample()
	if err != nil || len(got.Processes) != 1 || got.Processes[0].CPU.Valid || got.Processes[0].IO.Valid || got.Processes[0].Identity.StartTimeTicks != 200 {
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

func TestProcessSamplerKeepsProcessWhenExecutableAndIOAreUnreadable(t *testing.T) {
	root := t.TempDir()
	writeProcStat(t, root, 100)
	writeProcess(t, root, 2, "partial", 1, 5, 20, 1000, 1, nil)
	for _, name := range []string{"exe", "io", "status", "cmdline"} {
		if err := os.Remove(filepath.Join(root, "2", name)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := newProcessSampler(root).Sample()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Processes) != 1 || got.Processes[0].ExecutableValid || got.Processes[0].UIDValid ||
		got.Processes[0].IO.Valid || len(got.Processes[0].Args) != 0 || len(got.Issues) != 0 {
		t.Fatalf("partial process snapshot = %#v", got)
	}
}

func TestProcessSamplerSkipsPermissionDeniedIO(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read mode-zero proc files")
	}
	root := t.TempDir()
	writeProcStat(t, root, 100)
	writeProcess(t, root, 2, "partial", 1, 5, 20, 1000, 1, nil)
	ioPath := filepath.Join(root, "2", "io")
	if err := os.Chmod(ioPath, 0); err != nil {
		t.Fatal(err)
	}

	got, err := newProcessSampler(root).Sample()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Processes) != 1 || got.Processes[0].IO.Valid || len(got.Issues) != 0 {
		t.Fatalf("permission-denied process snapshot = %#v", got)
	}
}

func TestProcessSamplerReportsMalformedIO(t *testing.T) {
	root := t.TempDir()
	writeProcStat(t, root, 100)
	writeProcess(t, root, 2, "partial", 1, 5, 20, 1000, 1, nil)
	ioPath := filepath.Join(root, "2", "io")
	if err := os.WriteFile(ioPath, []byte("read_bytes: invalid\nwrite_bytes: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := newProcessSampler(root).Sample()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Processes) != 1 || got.Processes[0].IO.Valid || len(got.Issues) != 1 || got.Issues[0].Source != ioPath {
		t.Fatalf("malformed process I/O snapshot = %#v", got)
	}
}

func TestExpectedProcessReadError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "missing", err: &os.PathError{Err: syscall.ENOENT}, want: true},
		{name: "permission denied", err: &os.PathError{Err: syscall.EACCES}, want: true},
		{name: "operation not permitted", err: &os.PathError{Err: syscall.EPERM}, want: true},
		{name: "I/O failure", err: &os.PathError{Err: syscall.EIO}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := expectedProcessReadError(tt.err); got != tt.want {
				t.Fatalf("expectedProcessReadError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
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

func TestParseProcessMemorySumsPrivateAndSharedFields(t *testing.T) {
	input := "Rss: 64 kB\nPrivate_Clean: 10 kB\nPrivate_Dirty: 2 kB\nPrivate_Hugetlb: 3 kB\n" +
		"Shared_Clean: 4 kB\nShared_Dirty: 5 kB\nShared_Hugetlb: 6 kB\n"
	got, err := parseProcessMemory(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if got.PrivateBytes != 15*1024 || got.SharedBytes != 15*1024 {
		t.Fatalf("process memory = %#v", got)
	}
}

func TestParseProcessMemoryRejectsMalformedOrIncompleteFields(t *testing.T) {
	for _, input := range []string{
		"Private_Clean: invalid kB\nShared_Clean: 1 kB\n",
		"Private_Clean: 1 KB\nShared_Clean: 1 kB\n",
		"Private_Clean: 1 kB\n",
		"Private_Clean: 18014398509481984 kB\nShared_Clean: 1 kB\n",
		"Private_Clean: 18014398509481983 kB\nPrivate_Dirty: 1 kB\nShared_Clean: 1 kB\n",
	} {
		if _, err := parseProcessMemory(strings.NewReader(input)); err == nil {
			t.Errorf("parseProcessMemory(%q) unexpectedly succeeded", input)
		}
	}
}

func TestReadProcessMemoryUsesIdentityAndRollup(t *testing.T) {
	root := t.TempDir()
	writeProcess(t, root, 42, "worker", 1, 10, 991, 1000, 40, nil)
	path := filepath.Join(root, "42", "smaps_rollup")
	rollup := "Private_Clean: 11 kB\nPrivate_Dirty: 2 kB\nShared_Clean: 7 kB\nShared_Dirty: 3 kB\n"
	if err := os.WriteFile(path, []byte(rollup), 0o644); err != nil {
		t.Fatal(err)
	}
	id := ProcessIdentity{PID: 42, StartTimeTicks: 991}
	got, err := readProcessMemory(root, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.PrivateBytes != 13*1024 || got.SharedBytes != 10*1024 {
		t.Fatalf("process memory = %#v", got)
	}

	writeProcess(t, root, 42, "replacement", 1, 1, 992, 1000, 40, nil)
	if _, err := readProcessMemory(root, id); !errors.Is(err, ErrProcessIdentityChanged) {
		t.Fatalf("replacement error = %v", err)
	}
}

func TestReadProcessMemoryReadsCurrentProcess(t *testing.T) {
	pid := os.Getpid()
	data, err := os.ReadFile(filepath.Join("/proc", fmt.Sprint(pid), "stat"))
	if err != nil {
		t.Fatal(err)
	}
	stat, err := parseProcessStat(string(data))
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadProcessMemory(ProcessIdentity{PID: pid, StartTimeTicks: stat.startTimeTicks})
	if err != nil {
		t.Fatal(err)
	}
	if got.PrivateBytes+got.SharedBytes == 0 {
		t.Fatalf("empty process memory: %#v", got)
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
	status := fmt.Sprintf("Name:\t%s\nUid:\t%d\t%d\t%d\t%d\nVmRSS:\t%d kB\nVmSwap:\t7 kB\n", name, uid, uid, uid, uid, rssKB)
	cmdline := strings.Join(args, "\x00")
	if len(args) > 0 {
		cmdline += "\x00"
	}
	for name, body := range map[string]string{"stat": stat, "status": status, "cmdline": cmdline, "io": "read_bytes: 0\nwrite_bytes: 0\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	exePath := filepath.Join(dir, "exe")
	if err := os.Remove(exePath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/bin/"+name, exePath); err != nil {
		t.Fatal(err)
	}
}

func writeProcessIO(t *testing.T, root string, pid int, readBytes, writeBytes uint64) {
	t.Helper()
	body := fmt.Sprintf("rchar: 10\nwchar: 20\nread_bytes: %d\nwrite_bytes: %d\n", readBytes, writeBytes)
	path := filepath.Join(root, fmt.Sprint(pid), "io")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
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
