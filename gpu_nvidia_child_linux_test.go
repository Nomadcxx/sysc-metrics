//go:build linux && !race

package metrics

// The sampler owns a real nvidia-smi child here, so the check lives outside
// the race build to keep that child out of the race-detector run. What it
// asserts is structural, not a CPU budget: one child serves every sample.

import (
	"testing"
	"time"
)

// TestGPUSamplerKeepsOneChildAcrossSamples is the regression test for issue
// #53. A fork and exec of nvidia-smi per sample was the defect; measuring the
// process's CPU time against a fixed budget also caught it but failed on any
// host that was busy, slower, or built with coverage, so it could not tell a
// regression from the machine it ran on. Counting the starts is exact.
func TestGPUSamplerKeepsOneChildAcrossSamples(t *testing.T) {
	if _, err := runNvidiaSMI(); err != nil {
		t.Skipf("nvidia-smi is not available: %v", err)
	}
	sampler := NewGPUSampler()
	defer sampler.Close()

	starts := 0
	start := sampler.start
	sampler.start = func(period time.Duration) (nvidiaProcess, error) {
		starts++
		return start(period)
	}

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
	for i := 0; i < 20; i++ {
		if _, err := sampler.Sample(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := sampler.Close(); err != nil {
		t.Fatal(err)
	}
	if starts != 1 {
		t.Fatalf("the sampler started %d children across the samples, want one long-lived child", starts)
	}
}
