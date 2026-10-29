//go:build linux

package metrics

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
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
	swapBytes     uint64
	swapValid     bool
}

type processIOCounters struct {
	readBytes  uint64
	writeBytes uint64
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
	seenSwap := false
	scanner := newKernelScanner(r)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if !bytes.HasPrefix(line, []byte("Uid:")) && !bytes.HasPrefix(line, []byte("VmRSS:")) && !bytes.HasPrefix(line, []byte("VmSwap:")) {
			continue
		}
		fields := bytes.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch string(fields[0]) {
		case "Uid:":
			if seenUID {
				return parsedProcessStatus{}, fmt.Errorf("duplicate Uid field")
			}
			seenUID = true
			if len(fields) < 2 {
				return parsedProcessStatus{}, fmt.Errorf("Uid: missing real UID")
			}
			uid, err := strconv.ParseUint(string(fields[1]), 10, 32)
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
			if len(fields) != 3 || string(fields[2]) != "kB" {
				return parsedProcessStatus{}, fmt.Errorf("VmRSS: expected kB value")
			}
			residentKB, err := strconv.ParseUint(string(fields[1]), 10, 64)
			if err != nil || residentKB > math.MaxUint64/1024 {
				return parsedProcessStatus{}, fmt.Errorf("VmRSS: invalid kB value")
			}
			status.residentBytes = residentKB * 1024
			status.residentValid = true
		case "VmSwap:":
			if seenSwap {
				return parsedProcessStatus{}, fmt.Errorf("duplicate VmSwap field")
			}
			seenSwap = true
			if len(fields) != 3 || string(fields[2]) != "kB" {
				return parsedProcessStatus{}, fmt.Errorf("VmSwap: expected kB value")
			}
			swapKB, err := strconv.ParseUint(string(fields[1]), 10, 64)
			if err != nil || swapKB > math.MaxUint64/1024 {
				return parsedProcessStatus{}, fmt.Errorf("VmSwap: invalid kB value")
			}
			status.swapBytes = swapKB * 1024
			status.swapValid = true
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

func parseProcessIO(r io.Reader) (processIOCounters, error) {
	var counters processIOCounters
	seenRead := false
	seenWrite := false
	scanner := newKernelScanner(r)
	for scanner.Scan() {
		fields := bytes.Fields(bytes.TrimSpace(scanner.Bytes()))
		if len(fields) == 0 {
			continue
		}
		var dst *uint64
		var seen *bool
		switch string(fields[0]) {
		case "read_bytes:":
			dst, seen = &counters.readBytes, &seenRead
		case "write_bytes:":
			dst, seen = &counters.writeBytes, &seenWrite
		default:
			continue
		}
		if *seen {
			return processIOCounters{}, fmt.Errorf("duplicate %s field", string(fields[0]))
		}
		*seen = true
		if len(fields) != 2 {
			return processIOCounters{}, fmt.Errorf("%s: expected one counter", string(fields[0]))
		}
		value, err := strconv.ParseUint(string(fields[1]), 10, 64)
		if err != nil {
			return processIOCounters{}, fmt.Errorf("%s: invalid counter: %w", string(fields[0]), err)
		}
		*dst = value
	}
	if err := scanner.Err(); err != nil {
		return processIOCounters{}, fmt.Errorf("scan: %w", err)
	}
	if !seenRead || !seenWrite {
		return processIOCounters{}, fmt.Errorf("missing read_bytes or write_bytes field")
	}
	return counters, nil
}

func parseProcessMemory(r io.Reader) (ProcessMemory, error) {
	var memory ProcessMemory
	seenPrivate := false
	seenShared := false
	scanner := newKernelScanner(r)
	for scanner.Scan() {
		fields := bytes.Fields(bytes.TrimSpace(scanner.Bytes()))
		if len(fields) == 0 {
			continue
		}
		var total *uint64
		var seen *bool
		switch {
		case bytes.HasPrefix(fields[0], []byte("Private_")):
			total, seen = &memory.PrivateBytes, &seenPrivate
		case bytes.HasPrefix(fields[0], []byte("Shared_")):
			total, seen = &memory.SharedBytes, &seenShared
		default:
			continue
		}
		if len(fields) != 3 || string(fields[2]) != "kB" {
			return ProcessMemory{}, fmt.Errorf("%s: expected kB value", string(fields[0]))
		}
		kB, err := strconv.ParseUint(string(fields[1]), 10, 64)
		if err != nil || kB > math.MaxUint64/1024 {
			return ProcessMemory{}, fmt.Errorf("%s: invalid kB value", string(fields[0]))
		}
		value := kB * 1024
		if math.MaxUint64-*total < value {
			return ProcessMemory{}, fmt.Errorf("%s: byte total overflow", string(fields[0]))
		}
		*total += value
		*seen = true
	}
	if err := scanner.Err(); err != nil {
		return ProcessMemory{}, fmt.Errorf("scan: %w", err)
	}
	if !seenPrivate || !seenShared {
		return ProcessMemory{}, fmt.Errorf("missing private or shared memory fields")
	}
	return memory, nil
}

func newProcessSampler(root string) *ProcessSampler {
	return &ProcessSampler{root: root}
}

func parseProcessArgs(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	parts := strings.Split(string(data), "\x00")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// readProcessFile reuses buffer across proc files. data aliases nextBuffer, so
// callers must copy any returned values that outlive the next read.
func readProcessFile(path string, buffer []byte) (data, nextBuffer []byte, err error) {
	fd, err := openProcessFile(path, syscall.Open)
	if err != nil {
		return nil, buffer, &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer syscall.Close(fd)

	if cap(buffer) == 0 {
		buffer = make([]byte, 1024)
	} else {
		buffer = buffer[:cap(buffer)]
	}
	read := 0
	for {
		if read == len(buffer) {
			next := make([]byte, len(buffer)*2)
			copy(next, buffer[:read])
			buffer = next
		}
		n, readErr := syscall.Read(fd, buffer[read:])
		read += n
		if errors.Is(readErr, syscall.EINTR) {
			continue
		}
		if readErr != nil {
			return nil, buffer, &os.PathError{Op: "read", Path: path, Err: readErr}
		}
		if n == 0 {
			return buffer[:read], buffer, nil
		}
	}
}

func openProcessFile(path string, open func(string, int, uint32) (int, error)) (int, error) {
	for {
		fd, err := open(path, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
		if !errors.Is(err, syscall.EINTR) {
			return fd, err
		}
	}
}

func expectedProcessReadError(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission)
}

func (s *ProcessSampler) readTotalCPUTicks() (uint64, error) {
	if s.readTotal != nil {
		return s.readTotal()
	}
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
//
// Aggregate ticks are read before the walk and again after every process
// stat. The window for this sample runs from the previous pre-walk read to
// this post-walk read, which covers both process observations. A saturated
// process read later than the opening /proc/stat therefore stays inside the
// fraction instead of being dropped (issue #21). A delta that still exceeds
// that covering window is a counter regression and stays invalid (issue #7).
func (s *ProcessSampler) Sample() (ProcessSnapshot, error) {
	at := time.Now()
	totalBefore, err := s.readTotalCPUTicks()
	if err != nil {
		return ProcessSnapshot{}, err
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return ProcessSnapshot{}, fmt.Errorf("%s: %w", s.root, err)
	}

	snapshot := ProcessSnapshot{CollectedAt: at, Processes: make([]Process, 0, len(entries))}
	current := make(map[ProcessIdentity]uint64, len(entries))
	currentIO := make(map[ProcessIdentity]processIOCounters, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		dir := filepath.Join(s.root, entry.Name())
		// ponytail: this Linux-only walk uses fixed proc filenames, so append their slash directly.
		statPath := dir + "/stat"
		data, buffer, err := readProcessFile(statPath, s.readBuffer)
		s.readBuffer = buffer
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

		exePath := dir + "/exe"
		if executable, err := os.Readlink(exePath); err != nil {
			if !expectedProcessReadError(err) {
				snapshot.Issues = append(snapshot.Issues, Issue{Source: exePath, Err: err})
			}
		} else {
			process.Executable = executable
			process.ExecutableValid = true
		}

		statusPath := dir + "/status"
		statusData, buffer, err := readProcessFile(statusPath, s.readBuffer)
		s.readBuffer = buffer
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				snapshot.Issues = append(snapshot.Issues, Issue{Source: statusPath, Err: err})
			}
		} else {
			status, parseErr := parseProcessStatus(bytes.NewReader(statusData))
			if parseErr != nil {
				snapshot.Issues = append(snapshot.Issues, Issue{Source: statusPath, Err: parseErr})
			} else {
				process.UID = status.uid
				process.UIDValid = status.uidValid
				process.ResidentBytes = status.residentBytes
				process.ResidentValid = status.residentValid
				process.SwapBytes = status.swapBytes
				process.SwapValid = status.swapValid
			}
		}

		ioPath := dir + "/io"
		ioData, buffer, err := readProcessFile(ioPath, s.readBuffer)
		s.readBuffer = buffer
		if err != nil {
			if !expectedProcessReadError(err) {
				snapshot.Issues = append(snapshot.Issues, Issue{Source: ioPath, Err: err})
			}
		} else if counters, parseErr := parseProcessIO(bytes.NewReader(ioData)); parseErr != nil {
			snapshot.Issues = append(snapshot.Issues, Issue{Source: ioPath, Err: parseErr})
		} else {
			currentIO[identity] = counters
		}

		cmdlinePath := dir + "/cmdline"
		cmdlineData, buffer, err := readProcessFile(cmdlinePath, s.readBuffer)
		s.readBuffer = buffer
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				snapshot.Issues = append(snapshot.Issues, Issue{Source: cmdlinePath, Err: err})
			}
		} else {
			process.Args = parseProcessArgs(cmdlineData)
		}
		snapshot.Processes = append(snapshot.Processes, process)
	}

	totalAfter, err := s.readTotalCPUTicks()
	if err != nil {
		return ProcessSnapshot{}, err
	}
	if s.hasPrevious && totalAfter > s.previousTotal {
		totalDelta := totalAfter - s.previousTotal
		for i := range snapshot.Processes {
			ticks, ok := current[snapshot.Processes[i].Identity]
			if !ok {
				continue
			}
			previous, ok := s.previous[snapshot.Processes[i].Identity]
			if !ok || ticks < previous {
				continue
			}
			delta := ticks - previous
			if delta <= totalDelta {
				snapshot.Processes[i].CPU = CPUUsage{
					Fraction: float64(delta) / float64(totalDelta),
					Valid:    true,
				}
			}
		}
	}
	if !s.previousIOAt.IsZero() {
		elapsed := at.Sub(s.previousIOAt).Seconds()
		if elapsed > 0 {
			for i := range snapshot.Processes {
				identity := snapshot.Processes[i].Identity
				currentCounters, ok := currentIO[identity]
				if !ok {
					continue
				}
				previousCounters, ok := s.previousIO[identity]
				if !ok || currentCounters.readBytes < previousCounters.readBytes || currentCounters.writeBytes < previousCounters.writeBytes {
					continue
				}
				snapshot.Processes[i].IO = ProcessIO{
					ReadBytesPerSecond:  float64(currentCounters.readBytes-previousCounters.readBytes) / elapsed,
					WriteBytesPerSecond: float64(currentCounters.writeBytes-previousCounters.writeBytes) / elapsed,
					Valid:               true,
				}
			}
		}
	}

	sort.Slice(snapshot.Processes, func(i, j int) bool {
		return snapshot.Processes[i].Identity.PID < snapshot.Processes[j].Identity.PID
	})
	s.previousTotal = totalBefore
	s.previous = current
	s.hasPrevious = true
	s.previousIO = currentIO
	s.previousIOAt = at
	return snapshot, nil
}

// ValidateProcessIdentity confirms that identity still names the same process.
func ValidateProcessIdentity(identity ProcessIdentity) error {
	return validateProcessIdentity("/proc", identity)
}

// ReadProcessMemory reads private and shared memory for one selected process.
// It rejects the result if the PID no longer identifies the requested process.
func ReadProcessMemory(identity ProcessIdentity) (ProcessMemory, error) {
	return readProcessMemory("/proc", identity)
}

func readProcessMemory(root string, identity ProcessIdentity) (ProcessMemory, error) {
	if err := validateProcessIdentity(root, identity); err != nil {
		return ProcessMemory{}, err
	}
	path := filepath.Join(root, strconv.Itoa(identity.PID), "smaps_rollup")
	data, err := os.ReadFile(path)
	if err != nil {
		return ProcessMemory{}, fmt.Errorf("%s: %w", path, err)
	}
	memory, err := parseProcessMemory(bytes.NewReader(data))
	if err != nil {
		return ProcessMemory{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := validateProcessIdentity(root, identity); err != nil {
		return ProcessMemory{}, err
	}
	return memory, nil
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
