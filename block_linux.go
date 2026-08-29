//go:build linux

package metrics

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type blockIdentity struct {
	major uint32
	minor uint32
}

type blockState struct {
	at     time.Time
	device BlockDevice
}

func parseDiskstats(r io.Reader) ([]BlockDevice, []Issue, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxScannerToken)
	devices := []BlockDevice{}
	issues := []Issue{}
	seen := make(map[blockIdentity]bool)
	rows := false
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		rows = true
		device, err := parseDiskstatsRow(fields)
		if err != nil {
			name := "diskstats"
			if len(fields) > 2 {
				name = fields[2]
			}
			issues = append(issues, Issue{Source: "/proc/diskstats:" + name, Err: err})
			continue
		}
		identity := blockIdentity{major: device.Major, minor: device.Minor}
		if seen[identity] {
			return nil, nil, fmt.Errorf("/proc/diskstats: duplicate %d:%d identity", device.Major, device.Minor)
		}
		seen[identity] = true
		devices = append(devices, device)
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("/proc/diskstats: scan: %w", err)
	}
	if rows && len(devices) == 0 {
		return nil, issues, fmt.Errorf("/proc/diskstats: no valid device rows")
	}
	sort.Slice(devices, func(i, j int) bool {
		if devices[i].Name != devices[j].Name {
			return devices[i].Name < devices[j].Name
		}
		if devices[i].Major != devices[j].Major {
			return devices[i].Major < devices[j].Major
		}
		return devices[i].Minor < devices[j].Minor
	})
	return devices, issues, nil
}

func parseDiskstatsRow(fields []string) (BlockDevice, error) {
	if len(fields) < 13 || fields[2] == "" {
		return BlockDevice{}, fmt.Errorf("short or nameless row")
	}
	major, err := strconv.ParseUint(fields[0], 10, 32)
	if err != nil {
		return BlockDevice{}, fmt.Errorf("major: %w", err)
	}
	minor, err := strconv.ParseUint(fields[1], 10, 32)
	if err != nil {
		return BlockDevice{}, fmt.Errorf("minor: %w", err)
	}
	values := make([]uint64, 10)
	for i := range values {
		value, err := strconv.ParseUint(fields[i+3], 10, 64)
		if err != nil {
			return BlockDevice{}, fmt.Errorf("field %d: %w", i, err)
		}
		values[i] = value
	}
	readBytes, ok := checkedMultiply(values[2], 512)
	if !ok {
		return BlockDevice{}, fmt.Errorf("read sectors overflow bytes")
	}
	writeBytes, ok := checkedMultiply(values[6], 512)
	if !ok {
		return BlockDevice{}, fmt.Errorf("write sectors overflow bytes")
	}
	busy, err := millisDuration(values[9])
	if err != nil {
		return BlockDevice{}, err
	}
	return BlockDevice{Name: fields[2], Major: uint32(major), Minor: uint32(minor), ReadOperations: values[0], ReadBytes: readBytes, WriteOperations: values[4], WriteBytes: writeBytes, Busy: busy}, nil
}

func millisDuration(milliseconds uint64) (time.Duration, error) {
	if milliseconds > math.MaxInt64/uint64(time.Millisecond) {
		return 0, fmt.Errorf("milliseconds overflow duration")
	}
	return time.Duration(milliseconds) * time.Millisecond, nil
}

func blockRates(previous, current BlockDevice, elapsed time.Duration) BlockRates {
	if elapsed <= 0 || current.ReadBytes < previous.ReadBytes || current.WriteBytes < previous.WriteBytes || current.ReadOperations < previous.ReadOperations || current.WriteOperations < previous.WriteOperations || current.Busy < previous.Busy {
		return BlockRates{}
	}
	seconds := elapsed.Seconds()
	rates := BlockRates{
		ReadBytesPerSecond:       float64(current.ReadBytes-previous.ReadBytes) / seconds,
		WriteBytesPerSecond:      float64(current.WriteBytes-previous.WriteBytes) / seconds,
		ReadOperationsPerSecond:  float64(current.ReadOperations-previous.ReadOperations) / seconds,
		WriteOperationsPerSecond: float64(current.WriteOperations-previous.WriteOperations) / seconds,
		BusyFraction:             float64(current.Busy-previous.Busy) / float64(elapsed),
		Valid:                    true,
	}
	if math.IsNaN(rates.ReadBytesPerSecond) || math.IsInf(rates.ReadBytesPerSecond, 0) || math.IsNaN(rates.WriteBytesPerSecond) || math.IsInf(rates.WriteBytesPerSecond, 0) || math.IsNaN(rates.ReadOperationsPerSecond) || math.IsInf(rates.ReadOperationsPerSecond, 0) || math.IsNaN(rates.WriteOperationsPerSecond) || math.IsInf(rates.WriteOperationsPerSecond, 0) || math.IsNaN(rates.BusyFraction) || math.IsInf(rates.BusyFraction, 0) {
		return BlockRates{}
	}
	return rates
}

func (s *BlockSampler) sampleBlock(devices []BlockDevice, issues []Issue, at time.Time) BlockSnapshot {
	snapshot := BlockSnapshot{CollectedAt: at, Devices: make([]BlockDevice, 0, len(devices)), Issues: append([]Issue(nil), issues...)}
	elapsed := time.Duration(0)
	if !s.previousAt.IsZero() {
		elapsed = at.Sub(s.previousAt)
	}
	next := make(map[blockIdentity]blockState, len(devices))
	for _, device := range devices {
		identity := blockIdentity{major: device.Major, minor: device.Minor}
		if previous, ok := s.previous[identity]; ok && elapsed > 0 {
			device.Rates = blockRates(previous.device, device, elapsed)
		}
		if s.previousAt.IsZero() || elapsed > 0 {
			next[identity] = blockState{at: at, device: device}
		} else {
			if previous, ok := s.previous[identity]; ok {
				next[identity] = previous
			}
		}
		snapshot.Devices = append(snapshot.Devices, device)
	}
	if s.previousAt.IsZero() || elapsed > 0 {
		s.previousAt = at
	}
	s.previous = next
	sort.Slice(snapshot.Devices, func(i, j int) bool {
		if snapshot.Devices[i].Name != snapshot.Devices[j].Name {
			return snapshot.Devices[i].Name < snapshot.Devices[j].Name
		}
		if snapshot.Devices[i].Major != snapshot.Devices[j].Major {
			return snapshot.Devices[i].Major < snapshot.Devices[j].Major
		}
		return snapshot.Devices[i].Minor < snapshot.Devices[j].Minor
	})
	return snapshot
}

// Sample returns one block-device snapshot. The sampler is not safe for concurrent use.
func (s *BlockSampler) Sample() (BlockSnapshot, error) {
	at := time.Now()
	file, err := os.Open("/proc/diskstats")
	if err != nil {
		return BlockSnapshot{}, fmt.Errorf("/proc/diskstats: %w", err)
	}
	defer file.Close()
	devices, issues, err := parseDiskstats(file)
	if err != nil {
		return BlockSnapshot{}, err
	}
	return s.sampleBlock(devices, issues, at), nil
}
