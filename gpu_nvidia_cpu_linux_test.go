//go:build linux && !race

package metrics

// A CPU measurement under the race detector costs several times the CPU the
// measurement is about, so the check lives outside the race build.

import (
	"syscall"
	"testing"
	"time"
)

// processCPUTime is the CPU time this process and the processes it waited
// for have spent.
func processCPUTime() time.Duration {
	var self, children syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &self); err != nil {
		return 0
	}
	if err := syscall.Getrusage(syscall.RUSAGE_CHILDREN, &children); err != nil {
		return 0
	}
	return time.Duration(self.Utime.Nano() + self.Stime.Nano() +
		children.Utime.Nano() + children.Stime.Nano())
}

// TestGPUSamplerSampleCostsLessThanTheProcessBudget measures the CPU a
// sample costs and keeps it under the budget the issue sets (issue #53). The
// measurement closes the sampler first so RUSAGE_CHILDREN includes the CPU
// used by its long-lived nvidia-smi child. A fork and exec of nvidia-smi per
// sample costs far more than the budget.
func TestGPUSamplerSampleCostsLessThanTheProcessBudget(t *testing.T) {
	if _, err := runNvidiaSMI(); err != nil {
		t.Skipf("nvidia-smi is not available: %v", err)
	}
	sampler := NewGPUSampler()
	defer sampler.Close()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := sampler.Sample(); err != nil {
			t.Fatal(err)
		}
		if sampler.stream != nil && len(sampler.stream.tick) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the child never framed a report")
		}
		time.Sleep(50 * time.Millisecond)
	}
	before := processCPUTime()
	for i := 0; i < 20; i++ {
		if _, err := sampler.Sample(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := sampler.Close(); err != nil {
		t.Fatal(err)
	}
	if per := (processCPUTime() - before) / 20; per > 5*time.Millisecond {
		t.Fatalf("a sample cost %s of CPU, want at most 5ms", per)
	}
}
