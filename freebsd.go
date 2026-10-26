//go:build freebsd

package metrics

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func freebsdCommand(path string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	body, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("%s: %w", path, ctx.Err())
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return body, nil
}

type freebsdTicks [5]uint64

type freebsdCPUState struct {
	aggregate freebsdTicks
	cores     []freebsdTicks
}

// CPUSampler retains FreeBSD CPU counters for one sequential polling caller.
type CPUSampler struct{ previous *freebsdCPUState }

func NewCPUSampler() *CPUSampler { return &CPUSampler{} }

func parseFreeBSDTicks(fields []string) (freebsdTicks, error) {
	var ticks freebsdTicks
	if len(fields) != len(ticks) {
		return ticks, fmt.Errorf("expected five CPU counters")
	}
	var total uint64
	for i, field := range fields {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return ticks, fmt.Errorf("CPU counter: %w", err)
		}
		if value > math.MaxUint64-total {
			return ticks, fmt.Errorf("CPU counter total overflow")
		}
		total += value
		ticks[i] = value
	}
	return ticks, nil
}

func freebsdUsage(previous, current freebsdTicks) CPUUsage {
	var total, idle uint64
	for i, value := range current {
		if value < previous[i] {
			return CPUUsage{}
		}
		delta := value - previous[i]
		total += delta
		if i == 4 {
			idle = delta
		}
	}
	if total == 0 {
		return CPUUsage{}
	}
	return CPUUsage{Fraction: float64(total-idle) / float64(total), Valid: true}
}

func (s *CPUSampler) sampleCPU(body []byte, at time.Time) (CPUSnapshot, error) {
	rows := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(rows) != 3 {
		return CPUSnapshot{}, fmt.Errorf("sysctl CPU: expected aggregate, cores and load rows")
	}
	aggregate, err := parseFreeBSDTicks(strings.Fields(rows[0]))
	if err != nil {
		return CPUSnapshot{}, err
	}
	cores := strings.Fields(rows[1])
	if len(cores) == 0 || len(cores)%5 != 0 {
		return CPUSnapshot{}, fmt.Errorf("sysctl CPU: invalid per-core counters")
	}
	current := freebsdCPUState{aggregate: aggregate, cores: make([]freebsdTicks, len(cores)/5)}
	snapshot := CPUSnapshot{CollectedAt: at, Cores: make([]CPUCore, len(current.cores))}
	for i := range current.cores {
		current.cores[i], err = parseFreeBSDTicks(cores[i*5 : (i+1)*5])
		if err != nil {
			return CPUSnapshot{}, err
		}
		snapshot.Cores[i].ID = i
	}
	load := strings.Fields(strings.Trim(rows[2], " {}\t"))
	if len(load) != 3 {
		return CPUSnapshot{}, fmt.Errorf("sysctl CPU: invalid load average")
	}
	var values [3]float64
	for i, field := range load {
		values[i], err = strconv.ParseFloat(field, 64)
		if err != nil || math.IsNaN(values[i]) || math.IsInf(values[i], 0) || values[i] < 0 {
			return CPUSnapshot{}, fmt.Errorf("sysctl CPU: invalid load value %q", field)
		}
	}
	snapshot.Load1, snapshot.Load5, snapshot.Load15, snapshot.LoadValid = values[0], values[1], values[2], true
	if s.previous != nil {
		snapshot.Usage = freebsdUsage(s.previous.aggregate, current.aggregate)
		for i := range current.cores {
			if i < len(s.previous.cores) {
				snapshot.Cores[i].Usage = freebsdUsage(s.previous.cores[i], current.cores[i])
			}
		}
	}
	s.previous = &current
	return snapshot, nil
}

// Sample returns native CPU utilization and load. Frequencies remain unavailable.
func (s *CPUSampler) Sample() (CPUSnapshot, error) {
	at := time.Now()
	body, err := freebsdCommand("/sbin/sysctl", "-n", "kern.cp_time", "kern.cp_times", "vm.loadavg")
	if err != nil {
		return CPUSnapshot{}, err
	}
	return s.sampleCPU(body, at)
}

func parseFreeBSDMemory(ram, swap []byte, at time.Time) (MemorySnapshot, error) {
	fields := strings.Fields(string(ram))
	if len(fields) != 4 {
		return MemorySnapshot{}, fmt.Errorf("sysctl memory: expected four counters")
	}
	var counts [4]uint64
	for i, field := range fields {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return MemorySnapshot{}, fmt.Errorf("sysctl memory counter: %w", err)
		}
		counts[i] = value
	}
	pages, size, free, inactive := counts[0], counts[1], counts[2], counts[3]
	if pages == 0 || size == 0 || pages > math.MaxUint64/size || free > pages || inactive > pages-free {
		return MemorySnapshot{}, fmt.Errorf("sysctl memory: invalid page capacities")
	}
	total, available := pages*size, (free+inactive)*size
	snapshot := MemorySnapshot{CollectedAt: at, Memory: Capacity{TotalBytes: total, UsedBytes: total - available, AvailableBytes: available}}
	var swapTotal, swapUsed uint64
	lines := strings.Split(strings.TrimSpace(string(swap)), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "Device") {
		return MemorySnapshot{}, fmt.Errorf("swapinfo: missing header")
	}
	for _, line := range lines[1:] {
		row := strings.Fields(line)
		if len(row) == 0 {
			continue
		}
		if len(row) < 5 {
			return MemorySnapshot{}, fmt.Errorf("swapinfo: invalid row")
		}
		if row[0] == "Total" {
			continue
		}
		// Device paths can contain spaces; numeric columns come from the right.
		row = row[len(row)-4:]
		percent, err := strconv.ParseUint(strings.TrimSuffix(row[3], "%"), 10, 64)
		if err != nil || !strings.HasSuffix(row[3], "%") || percent > 100 {
			return MemorySnapshot{}, fmt.Errorf("swapinfo: invalid percentage")
		}
		var values [3]uint64
		for i := range values {
			value, err := strconv.ParseUint(row[i], 10, 64)
			if err != nil {
				return MemorySnapshot{}, fmt.Errorf("swapinfo counter: %w", err)
			}
			values[i] = value
		}
		blocks, used, avail := values[0], values[1], values[2]
		if used > blocks || avail != blocks-used || blocks > math.MaxUint64/1024 || swapTotal > math.MaxUint64-blocks*1024 || swapUsed > math.MaxUint64-used*1024 {
			return MemorySnapshot{}, fmt.Errorf("swapinfo: invalid capacities")
		}
		swapTotal += blocks * 1024
		swapUsed += used * 1024
	}
	snapshot.Swap = Capacity{TotalBytes: swapTotal, UsedBytes: swapUsed, AvailableBytes: swapTotal - swapUsed}
	return snapshot, nil
}

// ReadMemory reports managed RAM; free and inactive pages count as available.
func ReadMemory() (MemorySnapshot, error) {
	at := time.Now()
	ram, err := freebsdCommand("/sbin/sysctl", "-n", "vm.stats.vm.v_page_count", "hw.pagesize", "vm.stats.vm.v_free_count", "vm.stats.vm.v_inactive_count")
	if err != nil {
		return MemorySnapshot{}, err
	}
	swap, err := freebsdCommand("/usr/sbin/swapinfo", "-k")
	if err != nil {
		return MemorySnapshot{}, err
	}
	return parseFreeBSDMemory(ram, swap, at)
}

// GPUSampler preserves the consumer API while reporting unsupported telemetry.
type GPUSampler struct{}

func NewGPUSampler() *GPUSampler                   { return &GPUSampler{} }
func (s *GPUSampler) Sample() (GPUSnapshot, error) { return ReadGPU() }
func (s *GPUSampler) Close() error                 { return nil }
func ReadGPU() (GPUSnapshot, error) {
	return GPUSnapshot{CollectedAt: time.Now()}, fmt.Errorf("FreeBSD GPU telemetry: %w", errors.ErrUnsupported)
}
