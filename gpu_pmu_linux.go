//go:build linux

package metrics

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// pmuEvent is one discovered engine-busy PMU event.
type pmuEvent struct {
	name   string // engine name without the -busy suffix
	config uint64 // perf_event_attr config value
}

// pmuCandidate is one sysfs PMU that may belong to a GPU driver.
type pmuCandidate struct {
	name    string // sysfs directory name
	root    string // sysfs directory path
	driver  string // GPU driver the PMU was discovered for
	pmuType uint64 // perf_event_attr type value
	bdf     string // PCI BDF from the device link, empty when absent
}

// gpuRef identifies one GPU for PMU mapping purposes.
type gpuRef struct {
	bdf    string
	driver string
}

func parsePMUType(body string) (uint64, error) {
	value, err := strconv.ParseUint(strings.TrimSpace(body), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid PMU type %q", strings.TrimSpace(body))
	}
	return value, nil
}

func parseEventConfig(body string) (uint64, error) {
	for _, field := range strings.Fields(body) {
		if strings.HasPrefix(field, "config=") {
			value, err := strconv.ParseUint(strings.TrimPrefix(field, "config="), 0, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid config in %q", strings.TrimSpace(body))
			}
			return value, nil
		}
	}
	return 0, fmt.Errorf("no config= in %q", strings.TrimSpace(body))
}

// formatConfigMask parses the PMU format files and returns the mask of bits
// the config field accepts, as in "config:0-20".
func formatConfigMask(formatRoot string) (uint64, error) {
	entries, err := os.ReadDir(formatRoot)
	if err != nil {
		return 0, err
	}
	for _, entry := range entries {
		body, err := os.ReadFile(filepath.Join(formatRoot, entry.Name()))
		if err != nil {
			continue
		}
		fields := strings.SplitN(strings.TrimSpace(string(body)), ":", 2)
		if len(fields) != 2 || fields[0] != "config" {
			continue
		}
		bits := strings.Split(fields[1], "-")
		if len(bits) != 2 {
			continue
		}
		low, errLow := strconv.Atoi(strings.TrimSpace(bits[0]))
		high, errHigh := strconv.Atoi(strings.TrimSpace(bits[1]))
		if errLow != nil || errHigh != nil || low < 0 || high < low || high > 63 {
			continue
		}
		var mask uint64
		for bit := low; bit <= high; bit++ {
			mask |= 1 << uint(bit)
		}
		return mask, nil
	}
	return 0, fmt.Errorf("no config format in %s", formatRoot)
}

// discoverGPUPMUs lists the PMU directories under devicesRoot named after
// driver and reads their type and device identity. Discovery problems become
// issues; candidates without a readable type are skipped.
func discoverGPUPMUs(devicesRoot, driver string) ([]pmuCandidate, []Issue) {
	entries, err := os.ReadDir(devicesRoot)
	if err != nil {
		return nil, []Issue{{Source: devicesRoot, Err: err}}
	}
	var candidates []pmuCandidate
	var issues []Issue
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || name != driver && !strings.HasPrefix(name, driver+"_") {
			continue
		}
		root := filepath.Join(devicesRoot, name)
		body, err := os.ReadFile(filepath.Join(root, "type"))
		if err != nil {
			issues = append(issues, Issue{Source: filepath.Join(root, "type"), Err: err})
			continue
		}
		pmuType, err := parsePMUType(string(body))
		if err != nil {
			issues = append(issues, Issue{Source: filepath.Join(root, "type"), Err: err})
			continue
		}
		candidate := pmuCandidate{name: name, root: root, driver: driver, pmuType: pmuType}
		if link, err := os.Readlink(filepath.Join(root, "device")); err == nil {
			candidate.bdf = filepath.Base(filepath.Clean(link))
		}
		candidates = append(candidates, candidate)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].name < candidates[j].name })
	return candidates, issues
}

// discoverBusyEvents parses every *-busy event definition under the PMU's
// events directory. Engines whose definition, format-mask fit, or ns unit
// cannot be verified are skipped with an issue.
func discoverBusyEvents(pmuRoot string) ([]pmuEvent, []Issue) {
	eventsRoot := filepath.Join(pmuRoot, "events")
	entries, err := os.ReadDir(eventsRoot)
	if err != nil {
		return nil, []Issue{{Source: eventsRoot, Err: err}}
	}
	mask, maskErr := formatConfigMask(filepath.Join(pmuRoot, "format"))
	var events []pmuEvent
	var issues []Issue
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, "-busy") {
			continue
		}
		path := filepath.Join(eventsRoot, name)
		body, err := os.ReadFile(path)
		if err != nil {
			issues = append(issues, Issue{Source: path, Err: err})
			continue
		}
		config, err := parseEventConfig(string(body))
		if err != nil {
			issues = append(issues, Issue{Source: path, Err: err})
			continue
		}
		if maskErr == nil && config&^mask != 0 {
			issues = append(issues, Issue{Source: path, Err: fmt.Errorf("config 0x%x outside format mask 0x%x", config, mask)})
			continue
		}
		unitPath := path + ".unit"
		unitBody, err := os.ReadFile(unitPath)
		if err != nil {
			issues = append(issues, Issue{Source: unitPath, Err: err})
			continue
		}
		if unit := strings.TrimSpace(string(unitBody)); unit != "ns" {
			issues = append(issues, Issue{Source: unitPath, Err: fmt.Errorf("unit %q is not ns", unit)})
			continue
		}
		events = append(events, pmuEvent{name: strings.TrimSuffix(name, "-busy"), config: config})
	}
	return events, issues
}

// mapGPUPMUs assigns each GPU the index of its PMU candidate, or -1. A PMU
// with a device link maps by PCI BDF and driver. When no candidate carries a
// link, a driver-named PMU may serve exactly one GPU carrying that driver;
// ambiguity stays unmapped.
func mapGPUPMUs(gpus []gpuRef, candidates []pmuCandidate) []int {
	mapped := make([]int, len(gpus))
	for i := range mapped {
		mapped[i] = -1
	}
	if len(candidates) == 0 {
		return mapped
	}
	hasLinks := false
	for _, candidate := range candidates {
		if candidate.bdf != "" {
			hasLinks = true
			break
		}
	}
	claimed := make([]bool, len(candidates))
	if hasLinks {
		for i, gpu := range gpus {
			for j, candidate := range candidates {
				if claimed[j] || candidate.bdf == "" || candidate.driver != gpu.driver {
					continue
				}
				if candidate.bdf == gpu.bdf {
					mapped[i] = j
					claimed[j] = true
					break
				}
			}
		}
		return mapped
	}
	driver := candidates[0].driver
	matching := make([]int, 0, len(gpus))
	for i, gpu := range gpus {
		if gpu.driver == driver {
			matching = append(matching, i)
		}
	}
	if len(matching) == 1 && len(candidates) == 1 {
		mapped[matching[0]] = 0
	}
	return mapped
}

// engineBusy converts two engine-busy counter reads into a 0..1 fraction of
// the elapsed interval. A missing previous value, a counter decrease or
// reset, an impossible delta, or a non-positive interval returns invalid;
// the caller must then rebaseline from the current reading.
func engineBusy(previous, current uint64, hasPrevious bool, elapsed time.Duration) (float64, bool) {
	if !hasPrevious || elapsed <= 0 || current < previous {
		return 0, false
	}
	delta := current - previous
	if delta > uint64(elapsed) {
		return 0, false
	}
	return float64(delta) / float64(elapsed), true
}

// maxBusyFraction aggregates engine fractions by taking the maximum, which
// stays within 0..1 and does not double-count parallel engines.
func maxBusyFraction(fractions []float64) float64 {
	var maximum float64
	for _, fraction := range fractions {
		if fraction > maximum {
			maximum = fraction
		}
	}
	return maximum
}

// gpuCounter is one open PMU event counter delivering monotonic readings.
type gpuCounter interface {
	read() (uint64, error)
	close() error
}

const perfFlagFDCloexec = 1 << 3

// perfEventAttr mirrors the ABI-0 perf_event_attr layout (64 bytes).
type perfEventAttr struct {
	Type         uint32
	Size         uint32
	Config       uint64
	SamplePeriod uint64
	SampleType   uint64
	ReadFormat   uint64
	Flags        uint64
	WakeupEvents uint32
	BpType       uint32
	BpAddr       uint64
}

var perfEventOpen = func(attr *perfEventAttr, pid, cpu, groupFd int, flags uint64) (int, error) {
	fd, _, errno := syscall.Syscall6(syscall.SYS_PERF_EVENT_OPEN,
		uintptr(unsafe.Pointer(attr)), uintptr(pid), uintptr(cpu),
		uintptr(groupFd), uintptr(flags), 0)
	if errno != 0 {
		return -1, errno
	}
	return int(fd), nil
}

// openGPUCounter opens one PMU event counter, enabled and accumulating, for
// the owning GPUSampler. The descriptor is close-on-exec so exec'd helpers
// such as nvidia-smi never inherit it.
func openGPUCounter(pmuType, config uint64) (gpuCounter, error) {
	attr := perfEventAttr{
		Type:   uint32(pmuType),
		Size:   uint32(unsafe.Sizeof(perfEventAttr{})),
		Config: config,
		Flags:  perfFlagFDCloexec,
	}
	fd, err := perfEventOpen(&attr, -1, 0, -1, 0)
	if err != nil {
		return nil, fmt.Errorf("perf_event_open: %w", err)
	}
	return &perfCounter{file: os.NewFile(uintptr(fd), "perf_event")}, nil
}

type perfCounter struct {
	file *os.File
}

func (c *perfCounter) read() (uint64, error) {
	var buf [8]byte
	n, err := c.file.Read(buf[:])
	if err != nil {
		return 0, err
	}
	if n != len(buf) {
		return 0, io.ErrUnexpectedEOF
	}
	return binary.LittleEndian.Uint64(buf[:]), nil
}

func (c *perfCounter) close() error { return c.file.Close() }
