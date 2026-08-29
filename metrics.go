//go:build linux

// Package metrics provides read-only Linux system telemetry snapshots.
package metrics

import (
	"errors"
	"fmt"
	"time"
)

// Issue describes a recoverable source or entity failure in a partial snapshot.
type Issue struct {
	Source string
	Err    error
}

// Error returns the source and underlying error.
func (i Issue) Error() string {
	if i.Err == nil {
		return i.Source
	}
	if i.Source == "" {
		return i.Err.Error()
	}
	return fmt.Sprintf("%s: %v", i.Source, i.Err)
}

// Unwrap returns the underlying source error.
func (i Issue) Unwrap() error { return i.Err }

// Capacity contains byte totals. Byte fields are uint64 values.
type Capacity struct {
	TotalBytes     uint64
	UsedBytes      uint64
	AvailableBytes uint64
}

// CPUUsage contains a utilization fraction. Valid distinguishes an observed zero from no sample.
type CPUUsage struct {
	Fraction float64
	Valid    bool
}

// CPUCore contains one core's usage and best-effort frequency.
type CPUCore struct {
	ID             int
	Usage          CPUUsage
	FrequencyHz    uint64
	FrequencyValid bool
}

// CPUSnapshot contains aggregate and per-core CPU data collected at one time.
// Cores and Issues are caller-owned snapshot slices.
type CPUSnapshot struct {
	CollectedAt time.Time
	Usage       CPUUsage
	Cores       []CPUCore
	Load1       float64
	Load5       float64
	Load15      float64
	LoadValid   bool
	Issues      []Issue
}

// MemorySnapshot contains memory and swap byte capacities collected at one time.
type MemorySnapshot struct {
	CollectedAt time.Time
	Memory      Capacity
	Swap        Capacity
}

// UptimeSnapshot contains uptime as a duration collected at one time.
type UptimeSnapshot struct {
	CollectedAt time.Time
	Uptime      time.Duration
}

// Filesystem contains one mounted filesystem and its byte capacity.
type Filesystem struct {
	MountID    uint64
	MountPoint string
	Source     string
	Type       string
	ReadOnly   bool
	Capacity   Capacity
}

// FilesystemSnapshot contains mounted filesystem data collected at one time.
// Filesystems and Issues are caller-owned snapshot slices.
type FilesystemSnapshot struct {
	CollectedAt time.Time
	Filesystems []Filesystem
	Issues      []Issue
}

// BlockRates contains rates derived from two block-device observations.
// Valid distinguishes observed zero rates from unavailable derived values.
type BlockRates struct {
	ReadBytesPerSecond       float64
	WriteBytesPerSecond      float64
	ReadOperationsPerSecond  float64
	WriteOperationsPerSecond float64
	BusyFraction             float64
	Valid                    bool
}

// BlockDevice contains cumulative counters and derived rates for one block device.
type BlockDevice struct {
	Name            string
	Major           uint32
	Minor           uint32
	ReadBytes       uint64
	WriteBytes      uint64
	ReadOperations  uint64
	WriteOperations uint64
	Busy            time.Duration
	Rates           BlockRates
}

// BlockSnapshot contains block-device data collected at one time.
// Devices and Issues are caller-owned snapshot slices.
type BlockSnapshot struct {
	CollectedAt time.Time
	Devices     []BlockDevice
	Issues      []Issue
}

// NetworkRates contains rates derived from two network observations.
// Valid distinguishes observed zero rates from unavailable derived values.
type NetworkRates struct {
	ReceiveBytesPerSecond    float64
	TransmitBytesPerSecond   float64
	ReceivePacketsPerSecond  float64
	TransmitPacketsPerSecond float64
	Valid                    bool
}

// NetworkInterface contains cumulative counters and derived rates for one interface.
type NetworkInterface struct {
	Name            string
	Index           uint32
	ReceiveBytes    uint64
	ReceivePackets  uint64
	ReceiveErrors   uint64
	ReceiveDropped  uint64
	TransmitBytes   uint64
	TransmitPackets uint64
	TransmitErrors  uint64
	TransmitDropped uint64
	Rates           NetworkRates
}

// NetworkSnapshot contains network-interface data collected at one time.
// Interfaces and Issues are caller-owned snapshot slices.
type NetworkSnapshot struct {
	CollectedAt time.Time
	Interfaces  []NetworkInterface
	Issues      []Issue
}

// CPUSampler retains the previous CPU counters for sequential rate sampling.
type CPUSampler struct {
	previous *cpuState
}

// NewCPUSampler returns a CPU sampler owned by one sequential polling caller.
func NewCPUSampler() *CPUSampler { return &CPUSampler{} }

// NewBlockSampler returns a block sampler owned by one sequential polling caller.
func NewBlockSampler() *BlockSampler { return &BlockSampler{} }

// BlockSampler retains previous block counters for sequential rate sampling.
type BlockSampler struct{}

// Sample returns one block-device snapshot. The sampler is not safe for concurrent use.
func (s *BlockSampler) Sample() (BlockSnapshot, error) {
	return BlockSnapshot{}, errors.New("not implemented")
}

// NewNetworkSampler returns a network sampler owned by one sequential polling caller.
func NewNetworkSampler() *NetworkSampler { return &NetworkSampler{} }

// NetworkSampler retains previous interface counters for sequential rate sampling.
type NetworkSampler struct{}

// Sample returns one network snapshot. The sampler is not safe for concurrent use.
func (s *NetworkSampler) Sample() (NetworkSnapshot, error) {
	return NetworkSnapshot{}, errors.New("not implemented")
}

// ReadFilesystems returns one mounted-filesystem snapshot from Linux.
func ReadFilesystems() (FilesystemSnapshot, error) {
	return FilesystemSnapshot{}, errors.New("not implemented")
}
