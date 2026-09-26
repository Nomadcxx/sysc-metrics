//go:build linux

package metrics

import (
	"math"
	"os"
	"syscall"
	"testing"
	"time"
)

func TestLinuxIntegration(t *testing.T) {
	cpuSampler := NewCPUSampler()
	firstCPU, err := cpuSampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	for i, core := range firstCPU.Cores {
		if i > 0 && firstCPU.Cores[i-1].ID >= core.ID {
			t.Fatalf("CPU cores are not sorted: %#v", firstCPU.Cores)
		}
		if core.Usage.Valid {
			assertFraction(t, "CPU core usage", core.Usage.Fraction)
		}
	}
	if firstCPU.LoadValid {
		for _, load := range []float64{firstCPU.Load1, firstCPU.Load5, firstCPU.Load15} {
			assertFiniteNonNegative(t, "CPU load", load)
		}
	}

	memory, err := ReadMemory()
	if err != nil {
		t.Fatal(err)
	}
	if memory.Memory.TotalBytes == 0 || memory.Memory.UsedBytes > memory.Memory.TotalBytes || memory.Memory.AvailableBytes > memory.Memory.TotalBytes || memory.Swap.UsedBytes > memory.Swap.TotalBytes || memory.Swap.AvailableBytes > memory.Swap.TotalBytes {
		t.Fatalf("invalid memory capacities: %#v", memory)
	}
	uptime, err := ReadUptime()
	if err != nil || uptime.Uptime < 0 {
		t.Fatalf("invalid uptime: %#v, %v", uptime, err)
	}

	filesystems, err := ReadFilesystems()
	if err != nil {
		t.Fatal(err)
	}
	for _, filesystem := range filesystems.Filesystems {
		if filesystem.MountPoint == "" || filesystem.Capacity.UsedBytes > filesystem.Capacity.TotalBytes || filesystem.Capacity.AvailableBytes > filesystem.Capacity.TotalBytes {
			t.Fatalf("invalid filesystem: %#v", filesystem)
		}
	}

	blockSampler := NewBlockSampler()
	firstBlock, err := blockSampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	for _, device := range firstBlock.Devices {
		if device.Name == "" {
			t.Fatalf("block device has no name: %#v", device)
		}
		if device.Rates.Valid {
			assertBlockRates(t, device.Rates)
		}
	}

	networkSampler := NewNetworkSampler()
	firstNetwork, err := networkSampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	for _, iface := range firstNetwork.Interfaces {
		if !networkIdentityValid(iface, firstNetwork.Issues) {
			t.Fatalf("network interface has invalid identity: %#v", iface)
		}
		if iface.Rates.Valid {
			assertNetworkRates(t, iface.Rates)
		}
	}

	thermal, err := ReadThermal()
	if err != nil {
		t.Fatal(err)
	}
	if thermal.Valid && (thermal.Celsius <= 0 || thermal.Celsius >= 150) {
		t.Fatalf("CPU temperature out of range: %#v", thermal)
	}

	gpus, err := ReadGPU()
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range gpus.GPUs {
		if g.Usage.Valid {
			assertFraction(t, "GPU usage", g.Usage.Fraction)
		}
		if g.TempValid && (g.Celsius <= 0 || g.Celsius >= 150) {
			t.Fatalf("GPU temperature out of range: %#v", g)
		}
		if g.VRAMValid && (g.VRAM.TotalBytes == 0 || g.VRAM.UsedBytes > g.VRAM.TotalBytes) {
			t.Fatalf("GPU VRAM out of range: %#v", g)
		}
		t.Logf("GPU %s %s VRAM valid=%v used=%d total=%d", g.PCIID, g.Name, g.VRAMValid, g.VRAM.UsedBytes>>20, g.VRAM.TotalBytes>>20)
	}

	gpuSampler := NewGPUSampler()
	firstGPUSample, err := gpuSampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range firstGPUSample.GPUs {
		if g.Usage.Valid {
			assertFraction(t, "GPU sampler usage", g.Usage.Fraction)
		}
		if g.TempValid && (g.Celsius <= 0 || g.Celsius >= 150) {
			t.Fatalf("GPU sampler temperature out of range: %#v", g)
		}
	}

	if testing.Short() {
		return
	}
	time.Sleep(100 * time.Millisecond)
	secondCPU, err := cpuSampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if secondCPU.Usage.Valid {
		assertFraction(t, "CPU aggregate usage", secondCPU.Usage.Fraction)
	}
	for _, core := range secondCPU.Cores {
		if core.Usage.Valid {
			assertFraction(t, "CPU core usage", core.Usage.Fraction)
		}
	}

	time.Sleep(100 * time.Millisecond)
	secondBlock, err := blockSampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	checkBlockContinuity(t, firstBlock, secondBlock)
	secondNetwork, err := networkSampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	checkNetworkContinuity(t, firstNetwork, secondNetwork)

	time.Sleep(100 * time.Millisecond)
	secondGPUSample, err := gpuSampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range secondGPUSample.GPUs {
		if g.Usage.Valid {
			assertFraction(t, "GPU sampler usage", g.Usage.Fraction)
		}
	}
	if err := gpuSampler.Close(); err != nil {
		t.Fatalf("GPUSampler.Close = %v", err)
	}
	if err := gpuSampler.Close(); err != nil {
		t.Fatalf("second GPUSampler.Close = %v", err)
	}
}

// TestIntelGPULive qualifies the i915 PMU sampler on real Intel hardware.
// It is opt-in because reading the counters may require elevated privileges:
// run with SYSC_METRICS_INTEL_GPU_LIVE=1 on a machine with an i915 GPU.
func TestIntelGPULive(t *testing.T) {
	if os.Getenv("SYSC_METRICS_INTEL_GPU_LIVE") != "1" {
		t.Skip("opt-in: set SYSC_METRICS_INTEL_GPU_LIVE=1 on a machine with an Intel i915 GPU")
	}
	var uts syscall.Utsname
	if err := syscall.Uname(&uts); err == nil {
		t.Logf("kernel release: %s", utsField(uts.Release[:]))
	}
	sampler := NewGPUSampler()
	first, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	firstIntel := i915GPUs(first)
	if len(firstIntel) == 0 {
		t.Skip("no Intel i915 GPU on this machine")
	}
	logDiscoveredPMU(t, sampler)
	for _, g := range firstIntel {
		t.Logf("first sample: PCIID=%s driver=%s temp=%v tempValid=%v usageValid=%v",
			g.PCIID, g.Driver, g.Celsius, g.TempValid, g.Usage.Valid)
		if g.Usage.Valid {
			t.Fatalf("first sample has valid usage: %#v", g.Usage)
		}
	}
	time.Sleep(500 * time.Millisecond)
	second, err := sampler.Sample()
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range i915GPUs(second) {
		t.Logf("second sample: PCIID=%s fraction=%v valid=%v",
			g.PCIID, g.Usage.Fraction, g.Usage.Valid)
		if !g.Usage.Valid {
			t.Fatalf("second sample usage is invalid (issues: %v); the i915 PMU needs CAP_PERFMON or CAP_SYS_ADMIN, and the unprivileged fdinfo fallback needs a DRM client of this user on the GPU in two consecutive samples", second.Issues)
		}
		assertFraction(t, "Intel GPU usage", g.Usage.Fraction)
		// Name the source, so a pass does not read as proof the PMU works.
		source := "DRM fdinfo (PMU unavailable)"
		for _, state := range sampler.engines {
			for _, engine := range state.engines {
				if engine.counter != nil {
					source = "i915 PMU"
				}
			}
		}
		t.Logf("usage source: %s", source)
	}
	if len(second.Issues) > 0 {
		t.Logf("issues: %v", second.Issues)
	}
	if err := sampler.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
}

func i915GPUs(snapshot GPUSnapshot) []GPU {
	var intel []GPU
	for _, g := range snapshot.GPUs {
		if g.Driver == "i915" {
			intel = append(intel, g)
		}
	}
	return intel
}

func logDiscoveredPMU(t *testing.T, sampler *GPUSampler) {
	t.Helper()
	for _, state := range sampler.engines {
		t.Logf("PMU %s root=%s type=%d BDF=%q", state.pmu.name, state.pmu.root, state.pmu.pmuType, state.pmu.bdf)
		for _, engine := range state.engines {
			t.Logf("  engine %s config=0x%x", engine.event.name, engine.event.config)
		}
	}
}

func utsField(field []int8) string {
	bytes := make([]byte, 0, len(field))
	for _, c := range field {
		if c == 0 {
			break
		}
		bytes = append(bytes, byte(c))
	}
	return string(bytes)
}

func networkIdentityValid(iface NetworkInterface, issues []Issue) bool {
	if iface.Name == "" {
		return false
	}
	if iface.Index != 0 {
		return true
	}
	source := "/sys/class/net/" + iface.Name + "/ifindex"
	for _, issue := range issues {
		if issue.Source == source && issue.Err != nil {
			return true
		}
	}
	return false
}

func assertFraction(t *testing.T, name string, value float64) {
	t.Helper()
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 1 {
		t.Fatalf("%s = %v", name, value)
	}
}

func assertFiniteNonNegative(t *testing.T, name string, value float64) {
	t.Helper()
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		t.Fatalf("%s = %v", name, value)
	}
}

func assertBlockRates(t *testing.T, rates BlockRates) {
	t.Helper()
	for _, value := range []float64{rates.ReadBytesPerSecond, rates.WriteBytesPerSecond, rates.ReadOperationsPerSecond, rates.WriteOperationsPerSecond, rates.BusyFraction} {
		assertFiniteNonNegative(t, "block rate", value)
	}
}

func assertNetworkRates(t *testing.T, rates NetworkRates) {
	t.Helper()
	for _, value := range []float64{rates.ReceiveBytesPerSecond, rates.TransmitBytesPerSecond, rates.ReceivePacketsPerSecond, rates.TransmitPacketsPerSecond} {
		assertFiniteNonNegative(t, "network rate", value)
	}
}

func checkBlockContinuity(t *testing.T, first, second BlockSnapshot) {
	previous := make(map[blockIdentity]BlockDevice, len(first.Devices))
	for _, device := range first.Devices {
		previous[blockIdentity{major: device.Major, minor: device.Minor}] = device
	}
	for _, device := range second.Devices {
		old, ok := previous[blockIdentity{major: device.Major, minor: device.Minor}]
		if !ok || device.ReadBytes < old.ReadBytes || device.WriteBytes < old.WriteBytes || device.ReadOperations < old.ReadOperations || device.WriteOperations < old.WriteOperations || device.Busy < old.Busy {
			if device.Rates.Valid {
				t.Fatalf("new/reset block device has valid rates: %#v", device)
			}
			continue
		}
		if !device.Rates.Valid {
			t.Fatalf("monotonic block device has invalid rates: %#v", device)
		}
		assertBlockRates(t, device.Rates)
	}
}

func checkNetworkContinuity(t *testing.T, first, second NetworkSnapshot) {
	previous := make(map[uint32]NetworkInterface, len(first.Interfaces))
	for _, iface := range first.Interfaces {
		previous[iface.Index] = iface
	}
	for _, iface := range second.Interfaces {
		old, ok := previous[iface.Index]
		if !ok || iface.ReceiveBytes < old.ReceiveBytes || iface.ReceivePackets < old.ReceivePackets || iface.TransmitBytes < old.TransmitBytes || iface.TransmitPackets < old.TransmitPackets {
			if iface.Rates.Valid {
				t.Fatalf("new/reset network interface has valid rates: %#v", iface)
			}
			continue
		}
		if !iface.Rates.Valid {
			t.Fatalf("monotonic network interface has invalid rates: %#v", iface)
		}
		assertNetworkRates(t, iface.Rates)
	}
}
