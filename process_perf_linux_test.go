//go:build linux

package metrics

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestParseProcessStatusAllocationBudget(t *testing.T) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		t.Skipf("read proc status fixture: %v", err)
	}
	fixture := string(data)
	allocs := testing.AllocsPerRun(10, func() {
		if _, err := parseProcessStatus(strings.NewReader(fixture)); err != nil {
			panic(err)
		}
	})
	if allocs > 5 {
		t.Fatalf("parseProcessStatus allocated %.0f objects per run; want <= 5", allocs)
	}
}

func TestParseProcessStatusAcceptsLongUnrelatedLine(t *testing.T) {
	input := "Name:\tworker\nLongField:\t" + strings.Repeat("x", (64<<10)+1) + "\nUid:\t1000\t1000\t1000\t1000\n"
	got, err := parseProcessStatus(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if !got.uidValid || got.uid != 1000 {
		t.Fatalf("status = %#v", got)
	}
}

func TestReadProcessFileGrowsAndReusesBuffer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "status")
	want := bytes.Repeat([]byte("x"), 8*1024)
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	got, buffer, err := readProcessFile(path, make([]byte, 1024))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("first read = %d bytes, %v; want %d bytes", len(got), err, len(want))
	}
	if cap(buffer) < len(want) {
		t.Fatalf("buffer capacity = %d; want at least %d", cap(buffer), len(want))
	}
	bufferStart := &buffer[0]

	want = []byte("Uid:\t1000\n")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	got, buffer, err = readProcessFile(path, buffer)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("reused read = %q, %v; want %q", got, err, want)
	}
	if cap(buffer) < len(want) {
		t.Fatalf("reused buffer capacity = %d; want at least %d", cap(buffer), len(want))
	}
	if &buffer[0] != bufferStart {
		t.Fatal("readProcessFile replaced a reusable buffer that had enough capacity")
	}
}

func TestOpenProcessFileRetriesEINTR(t *testing.T) {
	path := filepath.Join(t.TempDir(), "status")
	if err := os.WriteFile(path, []byte("status"), 0o600); err != nil {
		t.Fatal(err)
	}

	attempts := 0
	fd, err := openProcessFile(path, func(path string, mode int, perm uint32) (int, error) {
		attempts++
		if attempts == 1 {
			return -1, syscall.EINTR
		}
		return syscall.Open(path, mode, perm)
	})
	if err != nil {
		t.Fatalf("openProcessFile() error = %v", err)
	}
	defer syscall.Close(fd)
	if attempts != 2 {
		t.Fatalf("open attempts = %d; want 2", attempts)
	}
}

func BenchmarkParseProcessStatus(b *testing.B) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		b.Fatal(err)
	}
	fixture := string(data)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := parseProcessStatus(strings.NewReader(fixture)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkProcessSamplerSample(b *testing.B) {
	sampler := NewProcessSampler()
	if _, err := sampler.Sample(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	var snapshot ProcessSnapshot
	for i := 0; i < b.N; i++ {
		var err error
		snapshot, err = sampler.Sample()
		if err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(len(snapshot.Processes)), "procs")
}

func BenchmarkCPUSamplerSample(b *testing.B) {
	sampler := NewCPUSampler()
	if _, err := sampler.Sample(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sampler.Sample(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNetworkSamplerSample(b *testing.B) {
	sampler := NewNetworkSampler()
	if _, err := sampler.Sample(); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := sampler.Sample(); err != nil {
			b.Fatal(err)
		}
	}
}
