//go:build freebsd

package metrics

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestFreeBSDCPUWarmupAndReset(t *testing.T) {
	s := NewCPUSampler()
	body := []byte("10 0 5 5 80\n10 0 5 5 80\n{ 0.25 0.5 0.75 }\n")
	first, err := s.sampleCPU(body, time.Now())
	if err != nil || first.Usage.Valid || len(first.Cores) != 1 || !first.LoadValid {
		t.Fatalf("warmup: %+v, %v", first, err)
	}
	second, err := s.sampleCPU([]byte("20 0 10 10 160\n20 0 10 10 160\n{ 0.25 0.5 0.75 }\n"), time.Now())
	if err != nil || !second.Usage.Valid || math.Abs(second.Usage.Fraction-0.2) > 1e-12 || !second.Cores[0].Usage.Valid {
		t.Fatalf("second: %+v, %v", second, err)
	}
	idle, err := s.sampleCPU([]byte("20 0 10 10 260\n20 0 10 10 260\n{ 0 0 0 }\n"), time.Now())
	if err != nil || !idle.Usage.Valid || idle.Usage.Fraction != 0 {
		t.Fatalf("observed idle: %+v, %v", idle, err)
	}

	reset, err := s.sampleCPU([]byte("0 0 0 0 0\n0 0 0 0 0\n{ 0 0 0 }\n"), time.Now())
	if err != nil || reset.Usage.Valid {
		t.Fatalf("reset: %+v, %v", reset, err)
	}
	for _, invalid := range []string{"1 2 3 4\n1 2 3 4 5\n{ 0 0 0 }", "1 2 3 4 5\n1 2 3\n{ 0 0 0 }", "-1 2 3 4 5\n1 2 3 4 5\n{ 0 0 0 }", "18446744073709551615 1 0 0 0\n1 2 3 4 5\n{ 0 0 0 }"} {
		if _, err := s.sampleCPU([]byte(invalid), time.Now()); err == nil {
			t.Fatalf("accepted invalid counters: %q", invalid)
		}
	}
}

func TestFreeBSDMemoryAndSwapBounds(t *testing.T) {
	swap := []byte("Device 1K-blocks Used Avail Capacity\n/dev/gpt/swap file 100 25 75 25%\n/dev/gpt/second 200 50 150 25%\nTotal 300 75 225 25%\n")
	m, err := parseFreeBSDMemory([]byte("1000\n4096\n100\n200\n"), swap, time.Now())
	if err != nil || m.Memory.TotalBytes != 4096000 || m.Memory.AvailableBytes != 1228800 || m.Swap.TotalBytes != 307200 || m.Swap.UsedBytes != 76800 {
		t.Fatalf("memory: %+v, %v", m, err)
	}
	for _, ram := range []string{"100\n4096\n70\n40\n", "18446744073709551615\n4096\n0\n0\n", "100\n0\n0\n0\n", "100\n4096\n-1\n0\n"} {
		if _, err := parseFreeBSDMemory([]byte(ram), swap, time.Now()); err == nil {
			t.Fatalf("accepted invalid RAM: %q", ram)
		}
	}
	for _, badSwap := range []string{
		"Device 1K-blocks Used Avail Capacity\n/dev/swap 100 101 0 101%\n",
		"Device 1K-blocks Used Avail Capacity\n/dev/swap 100 25 75 invalid\n",
		"Device 1K-blocks Used Avail Capacity\n/dev/swap 18446744073709551615 0 18446744073709551615 0%\n",
	} {
		if _, err := parseFreeBSDMemory([]byte("100\n4096\n1\n1\n"), []byte(badSwap), time.Now()); err == nil {
			t.Fatalf("accepted invalid swap: %q", badSwap)
		}
	}

}

func TestFreeBSDLiveSnapshots(t *testing.T) {
	s := NewCPUSampler()
	if _, err := s.Sample(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	cpu, err := s.Sample()
	if err != nil || !cpu.Usage.Valid || cpu.Usage.Fraction < 0 || cpu.Usage.Fraction > 1 || len(cpu.Cores) == 0 {
		t.Fatalf("live CPU: %+v, %v", cpu, err)
	}
	m, err := ReadMemory()
	if err != nil || m.Memory.TotalBytes == 0 || m.Memory.UsedBytes+m.Memory.AvailableBytes != m.Memory.TotalBytes || m.Swap.UsedBytes+m.Swap.AvailableBytes != m.Swap.TotalBytes {
		t.Fatalf("live memory: %+v, %v", m, err)
	}
	gpu := NewGPUSampler()
	if _, err := gpu.Sample(); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("GPU unavailable: %v", err)
	}
	if err := gpu.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("native CPU %.2f%%, memory %d/%d bytes, swap %d bytes", cpu.Usage.Fraction*100, m.Memory.UsedBytes, m.Memory.TotalBytes, m.Swap.TotalBytes)
}
