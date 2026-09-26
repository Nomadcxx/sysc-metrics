//go:build linux

// Package metrics provides read-only Linux system telemetry snapshots.
package metrics

import (
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
type BlockSampler struct {
	previous   map[blockIdentity]blockState
	previousAt time.Time
}

// NewNetworkSampler returns a network sampler owned by one sequential polling caller.
func NewNetworkSampler() *NetworkSampler { return &NetworkSampler{} }

// NetworkSampler retains previous interface counters for sequential rate sampling.
type NetworkSampler struct {
	previous   map[uint32]networkState
	previousAt time.Time
}

// ProcessIdentity distinguishes a process from a later process that reuses its PID.
type ProcessIdentity struct {
	PID            int
	StartTimeTicks uint64
}

// Process contains one process observation and its best-effort derived CPU usage.
type Process struct {
	Identity      ProcessIdentity
	Name          string
	ParentPID     int
	UID           uint32
	UIDValid      bool
	ResidentBytes uint64
	ResidentValid bool
	Args          []string
	CPU           CPUUsage
}

// ProcessSnapshot contains processes collected at one time. Processes, Args,
// and Issues are caller-owned snapshot slices.
type ProcessSnapshot struct {
	CollectedAt time.Time
	Processes   []Process
	Issues      []Issue
}

// ProcessSampler retains previous process and aggregate CPU counters for
// sequential rate sampling.
type ProcessSampler struct {
	root          string
	previousTotal uint64
	previous      map[ProcessIdentity]uint64
	hasPrevious   bool
}

// NewProcessSampler returns a process sampler owned by one sequential polling caller.
func NewProcessSampler() *ProcessSampler { return newProcessSampler("/proc") }

// BatteryState is the charging state of the aggregate battery.
type BatteryState uint8

const (
	BatteryUnknown BatteryState = iota
	BatteryCharging
	BatteryDischarging
	BatteryFull
)

// BatterySnapshot is the aggregate battery observation. Present is false on a
// machine with no battery; that is not an error.
type BatterySnapshot struct {
	CollectedAt   time.Time
	Present       bool
	Charge        float64 // 0..1
	ChargeValid   bool
	State         BatteryState
	EnergyJoules  float64
	RateWatts     float64
	RateValid     bool
	TimeRemaining time.Duration
	TimeValid     bool
	Issues        []Issue
}

// ReadBattery collects the current aggregate battery. It prefers sysfs
// power-supply devices of type Battery or UPS. A machine with none reports
// Present false.
func ReadBattery() (BatterySnapshot, error) {
	return readBattery(powerSupplyRoot)
}

// ThermalSnapshot is one scored CPU package temperature in Celsius.
// Valid is false when no known sensor exists; that is not an error.
type ThermalSnapshot struct {
	CollectedAt time.Time
	Celsius     float64
	Valid       bool
	Source      string
	Issues      []Issue
}

// ReadThermal collects the current CPU package temperature.
func ReadThermal() (ThermalSnapshot, error) {
	return readThermal(hwmonRoot, thermalZoneRoot)
}

// GPUUsage is a 0..1 busy fraction. Valid distinguishes a real zero from no sample.
type GPUUsage struct {
	Fraction float64
	Valid    bool
}

// GPU is one display device.
type GPU struct {
	PCIID     string
	Driver    string
	Name      string
	Usage     GPUUsage
	Celsius   float64
	TempValid bool
	// VRAM is dedicated device memory. VRAMValid is false when the driver
	// does not report both used and total; an integrated part's shared
	// system memory is never counted here.
	VRAM      Capacity
	VRAMValid bool
}

// GPUSnapshot lists GPUs collected at one time. GPUs and Issues are caller-owned.
type GPUSnapshot struct {
	CollectedAt time.Time
	GPUs        []GPU
	Issues      []Issue
}

// ReadGPU collects usage and temperature for each discrete or integrated GPU.
func ReadGPU() (GPUSnapshot, error) {
	return readGPU(drmRoot, pciIDsPaths, nvidiaSMI)
}

// GPUSampler retains Intel i915 PMU engine counters for sequential rate
// sampling. It is owned by one sequential polling caller, starts no
// goroutines, and owns open PMU counters until Close. ReadGPU cannot report
// Intel usage; a consumer that needs it must sample here and close the
// sampler when its polling stops.
type GPUSampler struct {
	drmRoot     string
	pmuRoot     string
	pciIDs      []string
	smi         func() ([]byte, error)
	now         func() time.Time
	open        func(pmuType, config uint64) (gpuCounter, error)
	engines     map[string]*gpuPMUState
	hasPrevious bool
	previousAt  time.Time
	// DRM client fdinfo: the unprivileged usage path for GPUs whose
	// driver and PMU give no busy figure.
	procRoot  string
	fdPrev    drmClients
	fdPrevAt  time.Time
	fdHasPrev bool
}

// NewGPUSampler returns a GPU sampler owned by one sequential polling caller.
// It reports the same snapshot as ReadGPU and additionally fills Intel i915
// usage from the second sample on.
func NewGPUSampler() *GPUSampler {
	return newGPUSampler(drmRoot, pmuDevicesRoot, pciIDsPaths, nvidiaSMI)
}
