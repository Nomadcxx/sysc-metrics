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

type cpuTimes struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
}

type parsedCPU struct {
	aggregate cpuTimes
	cores     map[int]cpuTimes
}

type cpuLoad struct {
	Load1, Load5, Load15 float64
	Valid                bool
}

type cpuState struct {
	at        time.Time
	aggregate cpuTimes
	cores     map[int]cpuTimes
}

func parseCPUStat(r io.Reader) (parsedCPU, error) {
	var parsed parsedCPU
	parsed.cores = make(map[int]cpuTimes)
	hasAggregate := false
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxScannerToken)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		label := fields[0]
		switch {
		case label == "cpu":
			if parsed.aggregate != (cpuTimes{}) || hasAggregate {
				return parsedCPU{}, fmt.Errorf("cpu: duplicate aggregate row")
			}
			times, err := parseCPUTimes(fields)
			if err != nil {
				return parsedCPU{}, fmt.Errorf("cpu: %w", err)
			}
			parsed.aggregate = times
			hasAggregate = true
		case strings.HasPrefix(label, "cpu"):
			id, err := strconv.Atoi(strings.TrimPrefix(label, "cpu"))
			if err != nil || id < 0 {
				return parsedCPU{}, fmt.Errorf("%s: invalid core ID", label)
			}
			if _, exists := parsed.cores[id]; exists {
				return parsedCPU{}, fmt.Errorf("%s: duplicate core row", label)
			}
			times, err := parseCPUTimes(fields)
			if err != nil {
				return parsedCPU{}, fmt.Errorf("%s: %w", label, err)
			}
			parsed.cores[id] = times
		}
	}
	if err := scanner.Err(); err != nil {
		return parsedCPU{}, fmt.Errorf("scan: %w", err)
	}
	if !hasAggregate {
		return parsedCPU{}, fmt.Errorf("cpu: missing aggregate row")
	}
	return parsed, nil
}

func parseCPUTimes(fields []string) (cpuTimes, error) {
	if len(fields) < 9 {
		return cpuTimes{}, fmt.Errorf("expected at least eight counters")
	}
	values := make([]uint64, 8)
	for i := range values {
		value, err := strconv.ParseUint(fields[i+1], 10, 64)
		if err != nil {
			return cpuTimes{}, fmt.Errorf("counter %d: %w", i, err)
		}
		values[i] = value
	}
	return cpuTimes{user: values[0], nice: values[1], system: values[2], idle: values[3], iowait: values[4], irq: values[5], softirq: values[6], steal: values[7]}, nil
}

func (t cpuTimes) total() (uint64, bool) {
	values := [...]uint64{t.user, t.nice, t.system, t.idle, t.iowait, t.irq, t.softirq, t.steal}
	var total uint64
	for _, value := range values {
		if math.MaxUint64-total < value {
			return 0, false
		}
		total += value
	}
	return total, true
}

func (t cpuTimes) idleTotal() (uint64, bool) {
	if math.MaxUint64-t.idle < t.iowait {
		return 0, false
	}
	return t.idle + t.iowait, true
}

func cpuUsage(previous, current cpuTimes) CPUUsage {
	if current.user < previous.user || current.nice < previous.nice || current.system < previous.system || current.idle < previous.idle || current.iowait < previous.iowait || current.irq < previous.irq || current.softirq < previous.softirq || current.steal < previous.steal {
		return CPUUsage{}
	}
	previousTotal, ok := previous.total()
	if !ok {
		return CPUUsage{}
	}
	currentTotal, ok := current.total()
	if !ok || currentTotal < previousTotal {
		return CPUUsage{}
	}
	previousIdle, ok := previous.idleTotal()
	if !ok {
		return CPUUsage{}
	}
	currentIdle, ok := current.idleTotal()
	if !ok || currentIdle < previousIdle {
		return CPUUsage{}
	}
	totalDelta := currentTotal - previousTotal
	idleDelta := currentIdle - previousIdle
	if totalDelta == 0 || idleDelta > totalDelta {
		return CPUUsage{}
	}
	busyDelta := totalDelta - idleDelta
	fraction := float64(busyDelta) / float64(totalDelta)
	if math.IsNaN(fraction) || math.IsInf(fraction, 0) {
		return CPUUsage{}
	}
	return CPUUsage{Fraction: fraction, Valid: true}
}

func parseLoadavg(r io.Reader) (cpuLoad, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxScannerToken)
	var fields []string
	for scanner.Scan() {
		fields = strings.Fields(scanner.Text())
		if len(fields) > 0 {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return cpuLoad{}, fmt.Errorf("scan: %w", err)
	}
	if len(fields) < 3 {
		return cpuLoad{}, fmt.Errorf("loadavg: missing load fields")
	}
	values := [3]float64{}
	for i := range values {
		value, err := strconv.ParseFloat(fields[i], 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return cpuLoad{}, fmt.Errorf("loadavg field %d: invalid value", i)
		}
		values[i] = value
	}
	return cpuLoad{Load1: values[0], Load5: values[1], Load15: values[2], Valid: true}, nil
}

func parseFrequency(r io.Reader) (uint64, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxScannerToken)
	var token string
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) > 0 {
			token = fields[0]
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("scan: %w", err)
	}
	if token == "" {
		return 0, fmt.Errorf("frequency: missing kHz value")
	}
	kHz, err := strconv.ParseUint(token, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("frequency: invalid kHz value: %w", err)
	}
	if kHz > math.MaxUint64/1000 {
		return 0, fmt.Errorf("frequency: Hz value overflows")
	}
	return kHz * 1000, nil
}

func (s *CPUSampler) sampleCPU(parsed parsedCPU, load cpuLoad, frequencies map[int]uint64, issues []Issue, at time.Time) CPUSnapshot {
	snapshot := CPUSnapshot{CollectedAt: at, Load1: load.Load1, Load5: load.Load5, Load15: load.Load15, LoadValid: load.Valid, Issues: append([]Issue(nil), issues...)}
	positiveElapsed := s.previous != nil && at.Sub(s.previous.at) > 0
	if positiveElapsed {
		snapshot.Usage = cpuUsage(s.previous.aggregate, parsed.aggregate)
	}
	ids := make([]int, 0, len(parsed.cores))
	for id := range parsed.cores {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	snapshot.Cores = make([]CPUCore, 0, len(ids))
	for _, id := range ids {
		core := CPUCore{ID: id}
		if previous, ok := s.previousCore(id); ok && positiveElapsed {
			core.Usage = cpuUsage(previous, parsed.cores[id])
		}
		if frequency, ok := frequencies[id]; ok {
			core.FrequencyHz = frequency
			core.FrequencyValid = true
		}
		snapshot.Cores = append(snapshot.Cores, core)
	}
	if s.previous == nil || positiveElapsed {
		cores := make(map[int]cpuTimes, len(parsed.cores))
		for id, times := range parsed.cores {
			cores[id] = times
		}
		s.previous = &cpuState{at: at, aggregate: parsed.aggregate, cores: cores}
	} else {
		cores := make(map[int]cpuTimes, len(parsed.cores))
		for id := range parsed.cores {
			if previous, ok := s.previous.cores[id]; ok {
				cores[id] = previous
			}
		}
		s.previous.cores = cores
	}
	return snapshot
}

func (s *CPUSampler) previousCore(id int) (cpuTimes, bool) {
	if s.previous == nil {
		return cpuTimes{}, false
	}
	times, ok := s.previous.cores[id]
	return times, ok
}

// Sample returns one CPU snapshot. The sampler is not safe for concurrent use.
func (s *CPUSampler) Sample() (CPUSnapshot, error) {
	at := time.Now()
	file, err := os.Open("/proc/stat")
	if err != nil {
		return CPUSnapshot{}, fmt.Errorf("/proc/stat: %w", err)
	}
	defer file.Close()
	parsed, err := parseCPUStat(file)
	if err != nil {
		return CPUSnapshot{}, fmt.Errorf("/proc/stat: %w", err)
	}
	load := cpuLoad{}
	issues := []Issue{}
	loadFile, err := os.Open("/proc/loadavg")
	if err != nil {
		issues = append(issues, Issue{Source: "/proc/loadavg", Err: err})
	} else {
		load, err = parseLoadavg(loadFile)
		_ = loadFile.Close()
		if err != nil {
			issues = append(issues, Issue{Source: "/proc/loadavg", Err: err})
		}
	}
	frequencies := make(map[int]uint64)
	for id := range parsed.cores {
		path := fmt.Sprintf("/sys/devices/system/cpu/cpu%d/cpufreq/scaling_cur_freq", id)
		frequencyFile, openErr := os.Open(path)
		if openErr != nil {
			if !os.IsNotExist(openErr) {
				issues = append(issues, Issue{Source: path, Err: openErr})
			}
			continue
		}
		frequency, parseErr := parseFrequency(frequencyFile)
		_ = frequencyFile.Close()
		if parseErr != nil {
			issues = append(issues, Issue{Source: path, Err: parseErr})
			continue
		}
		frequencies[id] = frequency
	}
	return s.sampleCPU(parsed, load, frequencies, issues, at), nil
}
