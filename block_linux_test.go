//go:build linux

package metrics

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestParseDiskstats(t *testing.T) {
	input := "8 0 sda 10 2 100 20 30 4 200 40 1 50 60 extra\n" +
		"253 0 dm-0 1 0 2 3 4 0 5 6 0 7 8\n" +
		"8 1 sda1 1 0 3 4 5 0 6 7 0 8 9\n"
	devices, issues, err := parseDiskstats(strings.NewReader(input))
	if err != nil || len(issues) != 0 || len(devices) != 3 {
		t.Fatalf("parseDiskstats() = %#v, %#v, %v", devices, issues, err)
	}
	var sda BlockDevice
	for _, device := range devices {
		if device.Name == "sda" {
			sda = device
		}
	}
	if sda.Major != 8 || sda.Minor != 0 || sda.ReadBytes != 51200 || sda.WriteBytes != 102400 || sda.Busy != 50*time.Millisecond {
		t.Fatalf("parsed disk = %#v", sda)
	}
	if _, _, err := parseDiskstats(strings.NewReader("8 0 sda 1 0 1 1 1 0 1 1 0 1 1\n8 0 sdb 1 0 1 1 1 0 1 1 0 1 1\n")); err == nil {
		t.Fatal("duplicate device identity unexpectedly succeeded")
	}
}

func TestParseDiskstatsRejectsInvalidRowsAndOverflow(t *testing.T) {
	valid := "8 0 sda 1 0 1 1 1 0 1 1 0 1 1\n"
	devices, issues, err := parseDiskstats(strings.NewReader(valid + "8 1 bad 1 nope 1 1 1 0 1 1 0 1 1\n"))
	if err != nil || len(devices) != 1 || len(issues) != 1 || !strings.Contains(issues[0].Source, "bad") {
		t.Fatalf("partial parse = %#v, %#v, %v", devices, issues, err)
	}
	for _, input := range []string{
		"8 0 sda 1 0 1\n",
		"8 0 sda 1 0 18446744073709551615 1 1 0 1 1 0 1 1\n",
		"8 0 sda 1 0 1 1 1 0 1 1 0 18446744073709551615 1\n",
	} {
		devices, issues, err := parseDiskstats(strings.NewReader(input))
		if err == nil || len(devices) != 0 || len(issues) != 1 {
			t.Errorf("parseDiskstats(%q) = %#v, %#v, %v", input, devices, issues, err)
		}
	}
	if _, err := millisDuration(math.MaxUint64); err == nil {
		t.Fatal("millisecond overflow unexpectedly succeeded")
	}
}

func TestBlockSamplerRatesAndLifecycle(t *testing.T) {
	sampler := NewBlockSampler()
	device := func(name string, read, write, readOps, writeOps uint64, busy time.Duration) BlockDevice {
		return BlockDevice{Name: name, Major: 8, Minor: 0, ReadBytes: read, WriteBytes: write, ReadOperations: readOps, WriteOperations: writeOps, Busy: busy}
	}
	at := time.Unix(1, 0)
	first := sampler.sampleBlock([]BlockDevice{device("sda", 100, 200, 10, 20, time.Second)}, nil, at)
	if first.Devices[0].Rates.Valid {
		t.Fatal("first block sample has valid rates")
	}
	second := sampler.sampleBlock([]BlockDevice{device("sda", 300, 600, 14, 26, 3*time.Second)}, nil, at.Add(2*time.Second))
	rates := second.Devices[0].Rates
	if !rates.Valid || rates.ReadBytesPerSecond != 100 || rates.WriteBytesPerSecond != 200 || rates.ReadOperationsPerSecond != 2 || rates.WriteOperationsPerSecond != 3 || rates.BusyFraction != 1 {
		t.Fatalf("block rates = %#v", rates)
	}
	zero := sampler.sampleBlock([]BlockDevice{device("sda", 300, 600, 14, 26, 3*time.Second)}, nil, at.Add(3*time.Second))
	if !zero.Devices[0].Rates.Valid || zero.Devices[0].Rates.ReadBytesPerSecond != 0 {
		t.Fatalf("zero block rates = %#v", zero.Devices[0].Rates)
	}
	added := sampler.sampleBlock([]BlockDevice{device("sda", 400, 700, 15, 27, 4*time.Second), {Name: "vda", Major: 9, Minor: 0}}, nil, at.Add(4*time.Second))
	if len(added.Devices) != 2 || added.Devices[1].Rates.Valid {
		t.Fatalf("added block device = %#v", added.Devices)
	}
	removed := sampler.sampleBlock([]BlockDevice{device("sda", 500, 800, 16, 28, 5*time.Second)}, nil, at.Add(5*time.Second))
	if len(removed.Devices) != 1 || removed.Devices[0].Name != "sda" {
		t.Fatalf("removed block device = %#v", removed.Devices)
	}
	returned := sampler.sampleBlock([]BlockDevice{device("sda", 600, 900, 17, 29, 6*time.Second), {Name: "vda", Major: 9, Minor: 0}}, nil, at.Add(6*time.Second))
	if returned.Devices[1].Rates.Valid {
		t.Fatal("reappearing block device retained an old baseline")
	}
}

func TestBlockSamplerResetAndElapsedRules(t *testing.T) {
	sampler := NewBlockSampler()
	at := time.Unix(1, 0)
	base := BlockDevice{Name: "sda", Major: 8, Minor: 0, ReadBytes: 10, WriteBytes: 10, ReadOperations: 1, WriteOperations: 1, Busy: time.Second}
	sampler.sampleBlock([]BlockDevice{base}, nil, at)
	reset := base
	reset.ReadBytes = 1
	if got := sampler.sampleBlock([]BlockDevice{reset}, nil, at.Add(time.Second)); got.Devices[0].Rates.Valid {
		t.Fatal("counter reset produced valid rate")
	}
	next := reset
	next.ReadBytes = 2
	if got := sampler.sampleBlock([]BlockDevice{next}, nil, at.Add(2*time.Second)); !got.Devices[0].Rates.Valid {
		t.Fatal("post-reset rate remained invalid")
	}
	before := next
	before.ReadBytes = 100
	sampler.sampleBlock([]BlockDevice{before}, nil, at.Add(2*time.Second))
	after := before
	after.ReadBytes = 101
	if got := sampler.sampleBlock([]BlockDevice{after}, nil, at.Add(3*time.Second)); !got.Devices[0].Rates.Valid || got.Devices[0].Rates.ReadBytesPerSecond != 99 {
		t.Fatalf("non-positive elapsed replaced baseline: %#v", got.Devices[0].Rates)
	}
}
