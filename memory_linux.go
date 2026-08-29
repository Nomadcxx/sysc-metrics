//go:build linux

package metrics

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

const maxScannerToken = 1 << 20

func parseMeminfo(r io.Reader, at time.Time) (MemorySnapshot, error) {
	const unit = uint64(1024)
	required := map[string]uint64{}
	seen := make(map[string]bool, 4)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxScannerToken)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		name := strings.TrimSuffix(fields[0], ":")
		if name != "MemTotal" && name != "MemAvailable" && name != "SwapTotal" && name != "SwapFree" {
			continue
		}
		if seen[name] {
			return MemorySnapshot{}, fmt.Errorf("%s: duplicate field", name)
		}
		seen[name] = true
		if len(fields) != 3 || fields[0] != name+":" || fields[2] != "kB" {
			return MemorySnapshot{}, fmt.Errorf("%s: expected unsigned kB value", name)
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return MemorySnapshot{}, fmt.Errorf("%s: invalid value: %w", name, err)
		}
		if value > math.MaxUint64/unit {
			return MemorySnapshot{}, fmt.Errorf("%s: value overflows bytes", name)
		}
		required[name] = value * unit
	}
	if err := scanner.Err(); err != nil {
		return MemorySnapshot{}, fmt.Errorf("scan: %w", err)
	}
	for _, name := range []string{"MemTotal", "MemAvailable", "SwapTotal", "SwapFree"} {
		if !seen[name] {
			return MemorySnapshot{}, fmt.Errorf("%s: missing field", name)
		}
	}
	if required["MemAvailable"] > required["MemTotal"] {
		return MemorySnapshot{}, fmt.Errorf("meminfo: MemAvailable exceeds MemTotal")
	}
	if required["SwapFree"] > required["SwapTotal"] {
		return MemorySnapshot{}, fmt.Errorf("meminfo: SwapFree exceeds SwapTotal")
	}
	return MemorySnapshot{
		CollectedAt: at,
		Memory: Capacity{
			TotalBytes:     required["MemTotal"],
			UsedBytes:      required["MemTotal"] - required["MemAvailable"],
			AvailableBytes: required["MemAvailable"],
		},
		Swap: Capacity{
			TotalBytes:     required["SwapTotal"],
			UsedBytes:      required["SwapTotal"] - required["SwapFree"],
			AvailableBytes: required["SwapFree"],
		},
	}, nil
}

// ReadMemory returns one memory and swap snapshot from Linux.
func ReadMemory() (MemorySnapshot, error) {
	at := time.Now()
	file, err := os.Open("/proc/meminfo")
	if err != nil {
		return MemorySnapshot{}, fmt.Errorf("/proc/meminfo: %w", err)
	}
	defer file.Close()
	snapshot, err := parseMeminfo(file, at)
	if err != nil {
		return MemorySnapshot{}, fmt.Errorf("/proc/meminfo: %w", err)
	}
	return snapshot, nil
}
