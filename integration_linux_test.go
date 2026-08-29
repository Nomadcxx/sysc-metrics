//go:build linux

package metrics

import (
	"math"
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
