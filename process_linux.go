//go:build linux

package metrics

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrProcessIdentityChanged means a PID now identifies a different process.
var ErrProcessIdentityChanged = errors.New("process identity changed")

type parsedProcessStat struct {
	pid            int
	name           string
	parentPID      int
	cpuTicks       uint64
	startTimeTicks uint64
}

type parsedProcessStatus struct {
	uid           uint32
	uidValid      bool
	residentBytes uint64
	residentValid bool
}

func parseProcessStat(line string) (parsedProcessStat, error) {
	line = strings.TrimSpace(line)
	open := strings.Index(line, " (")
	close := strings.LastIndex(line, ") ")
	if open < 1 || close <= open+2 {
		return parsedProcessStat{}, fmt.Errorf("malformed process stat")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line[:open]))
	if err != nil || pid <= 0 {
		return parsedProcessStat{}, fmt.Errorf("invalid PID")
	}
	fields := strings.Fields(line[close+2:])
	if len(fields) < 20 {
		return parsedProcessStat{}, fmt.Errorf("expected at least 20 fields after name")
	}
	parentPID, err := strconv.Atoi(fields[1])
	if err != nil || parentPID < 0 {
		return parsedProcessStat{}, fmt.Errorf("invalid parent PID")
	}
	userTicks, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return parsedProcessStat{}, fmt.Errorf("invalid user CPU ticks: %w", err)
	}
	systemTicks, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return parsedProcessStat{}, fmt.Errorf("invalid system CPU ticks: %w", err)
	}
	if math.MaxUint64-userTicks < systemTicks {
		return parsedProcessStat{}, fmt.Errorf("CPU ticks overflow")
	}
	startTimeTicks, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || startTimeTicks == 0 {
		return parsedProcessStat{}, fmt.Errorf("invalid start time ticks")
	}
	return parsedProcessStat{
		pid:            pid,
		name:           line[open+2 : close],
		parentPID:      parentPID,
		cpuTicks:       userTicks + systemTicks,
		startTimeTicks: startTimeTicks,
	}, nil
}

func parseProcessStatus(r io.Reader) (parsedProcessStatus, error) {
	var status parsedProcessStatus
	seenUID := false
	seenResident := false
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxScannerToken)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "Uid:":
			if seenUID {
				return parsedProcessStatus{}, fmt.Errorf("duplicate Uid field")
			}
			seenUID = true
			if len(fields) < 2 {
				return parsedProcessStatus{}, fmt.Errorf("Uid: missing real UID")
			}
			uid, err := strconv.ParseUint(fields[1], 10, 32)
			if err != nil {
				return parsedProcessStatus{}, fmt.Errorf("Uid: invalid real UID: %w", err)
			}
			status.uid = uint32(uid)
			status.uidValid = true
		case "VmRSS:":
			if seenResident {
				return parsedProcessStatus{}, fmt.Errorf("duplicate VmRSS field")
			}
			seenResident = true
			if len(fields) != 3 || fields[2] != "kB" {
				return parsedProcessStatus{}, fmt.Errorf("VmRSS: expected kB value")
			}
			residentKB, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil || residentKB > math.MaxUint64/1024 {
				return parsedProcessStatus{}, fmt.Errorf("VmRSS: invalid kB value")
			}
			status.residentBytes = residentKB * 1024
			status.residentValid = true
		}
	}
	if err := scanner.Err(); err != nil {
		return parsedProcessStatus{}, fmt.Errorf("scan: %w", err)
	}
	if !seenUID {
		return parsedProcessStatus{}, fmt.Errorf("missing Uid field")
	}
	return status, nil
}

func newProcessSampler(root string) *ProcessSampler {
	return &ProcessSampler{root: root}
}

func readProcessArgs(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	parts := strings.Split(string(data), "\x00")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts, nil
}

func (s *ProcessSampler) readTotalCPUTicks() (uint64, error) {
	path := filepath.Join(s.root, "stat")
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	defer file.Close()
	parsed, err := parseCPUStat(file)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	total, ok := parsed.aggregate.total()
	if !ok {
		return 0, fmt.Errorf("%s: aggregate CPU ticks overflow", path)
	}
	return total, nil
}

// Sample returns one process snapshot. The sampler is not safe for concurrent use.
func (s *ProcessSampler) Sample() (ProcessSnapshot, error) {
	at := time.Now()
	total, err := s.readTotalCPUTicks()
	if err != nil {
		return ProcessSnapshot{}, err
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return ProcessSnapshot{}, fmt.Errorf("%s: %w", s.root, err)
	}

	snapshot := ProcessSnapshot{CollectedAt: at}
	current := make(map[ProcessIdentity]uint64)
	totalDeltaValid := s.hasPrevious && total > s.previousTotal
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		dir := filepath.Join(s.root, entry.Name())
		statPath := filepath.Join(dir, "stat")
		data, err := os.ReadFile(statPath)
		if err != nil {
			if !os.IsNotExist(err) {
				snapshot.Issues = append(snapshot.Issues, Issue{Source: statPath, Err: err})
			}
			continue
		}
		stat, err := parseProcessStat(string(data))
		if err != nil {
			snapshot.Issues = append(snapshot.Issues, Issue{Source: statPath, Err: err})
			continue
		}
		identity := ProcessIdentity{PID: stat.pid, StartTimeTicks: stat.startTimeTicks}
		process := Process{Identity: identity, Name: stat.name, ParentPID: stat.parentPID}
		current[identity] = stat.cpuTicks
		if previous, ok := s.previous[identity]; totalDeltaValid && ok && stat.cpuTicks >= previous {
			process.CPU = CPUUsage{
				Fraction: float64(stat.cpuTicks-previous) / float64(total-s.previousTotal),
				Valid:    true,
			}
		}

		statusPath := filepath.Join(dir, "status")
		statusFile, err := os.Open(statusPath)
		if err != nil {
			snapshot.Issues = append(snapshot.Issues, Issue{Source: statusPath, Err: err})
		} else {
			status, parseErr := parseProcessStatus(statusFile)
			_ = statusFile.Close()
			if parseErr != nil {
				snapshot.Issues = append(snapshot.Issues, Issue{Source: statusPath, Err: parseErr})
			} else {
				process.UID = status.uid
				process.UIDValid = status.uidValid
				process.ResidentBytes = status.residentBytes
				process.ResidentValid = status.residentValid
			}
		}

		cmdlinePath := filepath.Join(dir, "cmdline")
		process.Args, err = readProcessArgs(cmdlinePath)
		if err != nil {
			snapshot.Issues = append(snapshot.Issues, Issue{Source: cmdlinePath, Err: err})
		}
		snapshot.Processes = append(snapshot.Processes, process)
	}
	sort.Slice(snapshot.Processes, func(i, j int) bool {
		return snapshot.Processes[i].Identity.PID < snapshot.Processes[j].Identity.PID
	})
	s.previousTotal = total
	s.previous = current
	s.hasPrevious = true
	return snapshot, nil
}

// ValidateProcessIdentity confirms that identity still names the same process.
func ValidateProcessIdentity(identity ProcessIdentity) error {
	return validateProcessIdentity("/proc", identity)
}

func validateProcessIdentity(root string, identity ProcessIdentity) error {
	if identity.PID <= 0 || identity.StartTimeTicks == 0 {
		return fmt.Errorf("invalid process identity")
	}
	path := filepath.Join(root, strconv.Itoa(identity.PID), "stat")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	stat, err := parseProcessStat(string(data))
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if stat.pid != identity.PID || stat.startTimeTicks != identity.StartTimeTicks {
		return fmt.Errorf("%w: PID %d", ErrProcessIdentityChanged, identity.PID)
	}
	return nil
}
