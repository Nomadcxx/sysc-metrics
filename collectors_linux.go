//go:build linux

package metrics

import "time"

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

// ProcessSampler retains previous process and aggregate CPU counters for
// sequential rate sampling. previousTotal is the aggregate tick count read
// before the previous process walk. The next sample closes that window with
// a second read taken after its own walk, so both process stats fall inside
// it.
type ProcessSampler struct {
	root          string
	readBuffer    []byte
	previousTotal uint64
	previous      map[ProcessIdentity]uint64
	hasPrevious   bool
	// readTotal, when set, replaces the aggregate tick read from <root>/stat.
	// Production leaves it nil. Tests use it to move the total between the
	// pre-walk and post-walk reads.
	readTotal func() (uint64, error)
}

// NewProcessSampler returns a process sampler owned by one sequential polling caller.
func NewProcessSampler() *ProcessSampler { return newProcessSampler("/proc") }

// ReadBattery collects the current aggregate battery. It prefers sysfs
// power-supply devices of type Battery or UPS. A machine with none reports
// Present false.
func ReadBattery() (BatterySnapshot, error) {
	return readBattery(powerSupplyRoot)
}

// ReadThermal collects the current CPU package temperature.
func ReadThermal() (ThermalSnapshot, error) {
	return readThermal(hwmonRoot, thermalZoneRoot)
}

// ReadGPU collects usage and temperature for each discrete or integrated GPU.
func ReadGPU() (GPUSnapshot, error) {
	return readGPU(drmRoot, pciIDsPaths, nvidiaSMI)
}

// GPUSampler retains Intel i915 PMU engine counters and DRM client fdinfo
// baselines for sequential rate sampling. It is owned by one sequential
// polling caller, starts no goroutines, and owns open PMU counters until
// Close. ReadGPU cannot report Intel usage; a consumer that needs it must
// sample here and close the sampler when its polling stops.
//
// On a machine with an NVIDIA GPU the sampler owns one long-lived
// nvidia-smi child that reports the rows on a loop, so a sample reads the
// child's output instead of forking nvidia-smi (issue #53). The child is
// started with the first sample that needs it and ends with Close.
//
// Without the privilege the PMU needs, usage for a GPU still lacking it comes
// from walking /proc for this user's /dev/dri descriptors: one readlink per
// descriptor each sample, and only while some GPU lacks usage. Other users'
// clients are invisible to that walk, so usage can stay invalid.
type GPUSampler struct {
	drmRoot  string
	pmuRoot  string
	pciNames *pciNameCache
	smi      func() ([]byte, error)
	now      func() time.Time
	open     func(pmuRoot string, pmuType, config uint64) (gpuCounter, error)
	engines  map[string]*gpuPMUState
	// start begins the long-lived nvidia-smi child. It is nil when the
	// sampler must not run one, and the child is never started then.
	start func(period time.Duration) (nvidiaProcess, error)
	// stream is the live child, nvidiaErr the failure of the last start
	// and nvidiaRetryAt the moment a failed child may be started again.
	stream         *nvidiaStream
	nvidiaErr      error
	nvidiaRetryAt  time.Time
	nvidiaFailures int
	// lastSample is the instant of the last sample, so the child's loop
	// period follows the cadence of the consumer.
	lastSample time.Time
	// sampleGen increments once per Sample. A scripted now can return one
	// instant for every clock read in that sample; production time.Now
	// ignores it.
	sampleGen int
	// DRM client fdinfo: the unprivileged usage path for GPUs whose
	// driver and PMU give no busy figure.
	procRoot  string
	fdPrev    drmClients
	fdPrevAt  time.Time
	fdHasPrev bool
	fdEmpty   int // consecutive walks with no engine data for a GPU missing usage
	fdIdle    int // samples skipped since the walk gave up
	fdWalks   int // walks made, for tests and diagnostics
}

// NewGPUSampler returns a GPU sampler owned by one sequential polling caller.
// It reports the same snapshot as ReadGPU and additionally fills Intel i915
// usage, and usage for any GPU the other paths leave empty, from the second
// sample on (PMU counters, else DRM client fdinfo).
func NewGPUSampler() *GPUSampler {
	sampler := newGPUSampler(drmRoot, pmuDevicesRoot, pciIDsPaths, nvidiaSMI)
	// The sampler owns one long-lived nvidia-smi child instead of forking
	// nvidia-smi for every sample (issue #53).
	sampler.start = startNvidiaChild
	return sampler
}
