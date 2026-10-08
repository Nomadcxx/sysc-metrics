//go:build linux

package metrics

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	drmRoot        = "/sys/class/drm"
	pmuDevicesRoot = "/sys/bus/event_source/devices"
)

var pciIDsPaths = []string{
	"/usr/share/hwdata/pci.ids",
	"/usr/share/misc/pci.ids",
}

var nvidiaSMI = runNvidiaSMI

type gpuFound struct {
	card string
	bdf  string
	gpu  GPU
}

func readGPU(drmRoot string, pciIDs []string, smi func() ([]byte, error)) (GPUSnapshot, error) {
	snapshot, _, err := readGPUs(drmRoot, pciIDs, smi, time.Now())
	return snapshot, err
}

func readGPUs(drmRoot string, pciIDs []string, smi func() ([]byte, error), now time.Time) (GPUSnapshot, []gpuFound, error) {
	entries, err := os.ReadDir(drmRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return GPUSnapshot{CollectedAt: now}, nil, nil
		}
		return GPUSnapshot{}, nil, err
	}

	var (
		found     []gpuFound
		issues    []Issue
		hasNvidia bool
	)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		if !strings.HasPrefix(name, "card") || strings.Contains(name, "-") {
			continue
		}
		dev := filepath.Join(drmRoot, name, "device")
		driver := gpuDriver(dev)
		if driver == "" {
			driver = gpuUeventDriver(dev)
		}
		if driver == "simpledrm" {
			continue
		}
		vendor, errV := readSysfsString(filepath.Join(dev, "vendor"))
		device, errD := readSysfsString(filepath.Join(dev, "device"))
		if errV != nil || errD != nil {
			if errV != nil {
				issues = append(issues, Issue{Source: filepath.Join(dev, "vendor"), Err: errV})
			}
			if errD != nil {
				issues = append(issues, Issue{Source: filepath.Join(dev, "device"), Err: errD})
			}
			continue
		}
		pci := formatPCIID(vendor, device)
		g := GPU{PCIID: pci, Driver: driver, Name: lookupPCIName(pciIDs, vendor, device)}
		if strings.HasPrefix(pci, "10de:") {
			hasNvidia = true
		}
		busyPath := filepath.Join(dev, "gpu_busy_percent")
		if busy, err := readSysfsInt(busyPath); err == nil {
			if fraction, ok := busyFraction(float64(busy)); ok {
				g.Usage = GPUUsage{Fraction: fraction, Valid: true}
			} else {
				issues = append(issues, Issue{Source: busyPath, Err: fmt.Errorf("gpu_busy_percent %d outside 0..100", busy)})
			}
		} else if !os.IsNotExist(err) {
			// Present but unreadable (permissions, EISDIR, bad content) is
			// reported, not silently treated as "no sensor" (issue #3).
			issues = append(issues, Issue{Source: busyPath, Err: err})
		}
		usedPath := filepath.Join(dev, "mem_info_vram_used")
		totalPath := filepath.Join(dev, "mem_info_vram_total")
		used, usedErr := readSysfsUint(usedPath)
		total, totalErr := readSysfsUint(totalPath)
		// Absent on both sides (or one side) is "no VRAM reporting driver";
		// present-but-unreadable is a fault and must be reported (issue #35,
		// same rule as gpu_busy_percent under issue #3).
		for _, pair := range []struct {
			path string
			err  error
		}{{usedPath, usedErr}, {totalPath, totalErr}} {
			if pair.err != nil && !os.IsNotExist(pair.err) {
				issues = append(issues, Issue{Source: pair.path, Err: pair.err})
			}
		}
		if usedErr == nil && totalErr == nil {
			g.VRAM, g.VRAMValid = vramCapacity(used, total)
		}
		if c, ok := readGPUHwmonTemp(dev, &issues); ok {
			g.Celsius, g.TempValid = c, true
		}
		found = append(found, gpuFound{card: name, bdf: gpuBDF(dev), gpu: g})
	}

	if hasNvidia && smi != nil {
		out, err := smi()
		if err != nil {
			issues = append(issues, Issue{Source: "nvidia-smi", Err: err})
		} else {
			applyNvidiaCSV(found, out)
		}
	}

	sort.Slice(found, func(i, j int) bool {
		if found[i].gpu.PCIID != found[j].gpu.PCIID {
			return found[i].gpu.PCIID < found[j].gpu.PCIID
		}
		return found[i].card < found[j].card
	})
	out := make([]GPU, len(found))
	for i, g := range found {
		out[i] = g.gpu
	}
	return GPUSnapshot{CollectedAt: now, GPUs: out, Issues: issues}, found, nil
}

// vramCapacity builds a Capacity from used and total bytes. A zero total is
// not a device with no memory; it is a driver that did not report one. Used
// above total is the same kind of nonsense (issue #35): the pair is
// rejected, not clamped, so no consumer trusts a broken figure.
func vramCapacity(used, total uint64) (Capacity, bool) {
	if total == 0 || used > total {
		return Capacity{}, false
	}
	c := Capacity{TotalBytes: total, UsedBytes: used}
	if used < total {
		c.AvailableBytes = total - used
	}
	return c, true
}

func gpuDriver(dev string) string {
	link, err := os.Readlink(filepath.Join(dev, "driver"))
	if err != nil {
		return ""
	}
	return filepath.Base(link)
}

func gpuUeventDriver(dev string) string {
	body, err := os.ReadFile(filepath.Join(dev, "uevent"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "DRIVER=") {
			return strings.TrimSpace(strings.TrimPrefix(line, "DRIVER="))
		}
	}
	return ""
}

func gpuBDF(dev string) string {
	link, err := os.Readlink(dev)
	if err != nil {
		return filepath.Base(dev)
	}
	if !filepath.IsAbs(link) {
		link = filepath.Join(filepath.Dir(dev), link)
	}
	return filepath.Base(filepath.Clean(link))
}

func formatPCIID(vendor, device string) string {
	return strings.ToLower(strings.TrimPrefix(vendor, "0x")) + ":" + strings.ToLower(strings.TrimPrefix(device, "0x"))
}

// busyFraction converts a 0..100 percent sensor value to a Valid fraction.
// Non-finite or out-of-band values are never trustworthy (issue #3).
func busyFraction(percent float64) (float64, bool) {
	if math.IsNaN(percent) || math.IsInf(percent, 0) || percent < 0 || percent > 100 {
		return 0, false
	}
	return percent / 100, true
}

func readGPUHwmonTemp(dev string, issues *[]Issue) (float64, bool) {
	matches, err := filepath.Glob(filepath.Join(dev, "hwmon", "hwmon*", "temp1_input"))
	if err != nil || len(matches) == 0 {
		return 0, false
	}
	sort.Strings(matches)
	milli, err := readSysfsInt(matches[0])
	if err != nil {
		if !os.IsNotExist(err) {
			// Present but unreadable sensor: report, don't hide (issue #3).
			*issues = append(*issues, Issue{Source: matches[0], Err: err})
		}
		return 0, false
	}
	celsius := float64(milli) / 1000
	if !validCelsius(celsius) {
		// Out-of-band values (0, negatives, NaN sentinels, >=150) leave
		// TempValid false instead of advertising garbage (issue #12).
		return 0, false
	}
	return celsius, true
}

func lookupPCIName(paths []string, vendor, device string) string {
	vendor = strings.ToLower(strings.TrimPrefix(vendor, "0x"))
	device = strings.ToLower(strings.TrimPrefix(device, "0x"))
	for _, path := range paths {
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if name := parsePCIName(string(body), vendor, device); name != "" {
			return name
		}
	}
	return ""
}

func parsePCIName(ids, vendor, device string) string {
	inVendor := false
	for _, line := range strings.Split(ids, "\n") {
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		if !strings.HasPrefix(line, "\t") {
			inVendor = strings.HasPrefix(strings.ToLower(line), vendor)
			continue
		}
		if !inVendor || strings.HasPrefix(line, "\t\t") {
			continue
		}
		fields := strings.SplitN(strings.TrimPrefix(line, "\t"), " ", 2)
		if len(fields) >= 2 && strings.ToLower(fields[0]) == device {
			return strings.TrimSpace(fields[1])
		}
	}
	return ""
}

func applyNvidiaCSV(found []gpuFound, out []byte) {
	rows := parseNvidiaCSV(out)
	for i := range found {
		for _, row := range rows {
			if !matchBDF(found[i].bdf, row.bdf) {
				continue
			}
			if !found[i].gpu.Usage.Valid && row.hasUtil {
				found[i].gpu.Usage = GPUUsage{Fraction: row.util, Valid: true}
			}
			if !found[i].gpu.TempValid && row.hasTemp {
				found[i].gpu.Celsius, found[i].gpu.TempValid = row.temp, true
			}
			if !found[i].gpu.VRAMValid && row.hasVRAM {
				found[i].gpu.VRAM, found[i].gpu.VRAMValid = vramCapacity(
					row.vramUsedMiB<<20, row.vramTotalMiB<<20)
			}
			if found[i].gpu.Name == "" && row.name != "" {
				found[i].gpu.Name = row.name
			}
		}
	}
}

func parseNvidiaCSV(out []byte) []nvidiaRow {
	var rows []nvidiaRow
	for _, line := range bytes.Split(out, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		raw := strings.Split(string(line), ",")
		if len(raw) < 3 {
			continue
		}
		parts := append([]string(nil), raw...)
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		row := nvidiaRow{bdf: parts[0]}
		// nvidia-smi prints "nan"/"inf" for dead fields and ParseFloat
		// happily accepts both; only in-band values fill the row (issue #3).
		if u, err := strconv.ParseFloat(parts[1], 64); err == nil {
			if f, ok := busyFraction(u); ok {
				row.util, row.hasUtil = f, true
			}
		}
		if c, err := strconv.ParseFloat(parts[2], 64); err == nil && validCelsius(c) {
			row.temp, row.hasTemp = c, true
		}
		// Memory sits before the name because the name is re-joined on
		// commas. A row too short to carry memory fills no name either, so
		// a number is never read as a name or a name as a number.
		if len(parts) >= 6 {
			used, errU := strconv.ParseUint(parts[3], 10, 44)
			total, errT := strconv.ParseUint(parts[4], 10, 44)
			if errU == nil && errT == nil {
				row.vramUsedMiB, row.vramTotalMiB, row.hasVRAM = used, total, true
			}
			row.name = strings.TrimSpace(strings.Join(raw[5:], ","))
		}
		rows = append(rows, row)
	}
	return rows
}

type nvidiaRow struct {
	bdf     string
	util    float64
	hasUtil bool
	temp    float64
	hasTemp bool
	// VRAM arrives in MiB from --format=csv,nounits.
	vramUsedMiB  uint64
	vramTotalMiB uint64
	hasVRAM      bool
	name         string
}

// matchBDF compares PCI addresses written with different domain widths:
// sysfs prints four hex digits (0000:08:00.0), nvidia-smi prints eight
// (00000000:08:00.0), and a bare bus:device.function means domain zero.
func matchBDF(sysfsBDF, smiBDF string) bool {
	d1, rest1, ok1 := splitBDF(sysfsBDF)
	d2, rest2, ok2 := splitBDF(smiBDF)
	return ok1 && ok2 && d1 == d2 && rest1 == rest2
}

func splitBDF(s string) (domain uint64, rest string, ok bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if strings.Count(s, ":") == 1 {
		return 0, s, s != ""
	}
	head, tail, found := strings.Cut(s, ":")
	if !found {
		return 0, "", false
	}
	d, err := strconv.ParseUint(head, 16, 32)
	if err != nil {
		return 0, "", false
	}
	return d, tail, true
}

func runNvidiaSMI() ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=pci.bus_id,utilization.gpu,temperature.gpu,memory.used,memory.total,name",
		"--format=csv,noheader,nounits")
	return cmd.Output()
}

type gpuEngineState struct {
	event    pmuEvent
	counter  gpuCounter
	value    uint64
	hasValue bool
	readAt   time.Time // sampler clock when this counter was last read
}

type gpuPMUState struct {
	pmu     pmuCandidate
	engines []gpuEngineState
}

func newGPUSampler(drmRoot, pmuRoot string, pciIDs []string, smi func() ([]byte, error)) *GPUSampler {
	return &GPUSampler{
		drmRoot:  drmRoot,
		pmuRoot:  pmuRoot,
		pciIDs:   pciIDs,
		smi:      smi,
		now:      time.Now,
		open:     openGPUCounter,
		engines:  make(map[string]*gpuPMUState),
		procRoot: procRoot,
	}
}

// Sample returns one GPU snapshot. The sampler is not safe for concurrent use.
func (s *GPUSampler) Sample() (GPUSnapshot, error) {
	s.sampleGen++
	// CollectedAt stays this instant (issue #4). PMU elapsed is not taken
	// here: the counter read happens after readGPUs, and a stamp this early
	// drops a full engine as an impossible delta (issue #18). fdinfo elapsed
	// is the gap between its own reads, stamped when readDRMClients returns;
	// using this earlier stamp clamps a late read to 1 (issue #25).
	collectedAt := s.now()
	snapshot, found, err := readGPUs(s.drmRoot, s.pciIDs, s.smi, collectedAt)
	if err != nil {
		return GPUSnapshot{}, err
	}
	s.applyIntelPMU(found, &snapshot)
	s.applyFDInfo(found, &snapshot)
	return snapshot, nil
}

// Close closes every open PMU counter and drops sampling state. It is
// idempotent; sampling after Close starts from a fresh baseline.
func (s *GPUSampler) Close() error {
	var closeErr error
	for _, state := range s.engines {
		if err := state.closeCounters(); err != nil && closeErr == nil {
			closeErr = err
		}
	}
	s.engines = make(map[string]*gpuPMUState)
	s.fdPrev, s.fdHasPrev, s.fdEmpty, s.fdIdle = nil, false, 0, 0
	return closeErr
}

// applyIntelPMU layers i915 PMU engine usage onto the DRM snapshot. State is
// keyed by PCI BDF; engines of GPUs that disappeared are closed and dropped,
// so a reappearance starts from a fresh baseline.
func (s *GPUSampler) applyIntelPMU(found []gpuFound, snapshot *GPUSnapshot) {
	var refs []gpuRef
	var intel []int
	for i, f := range found {
		if f.gpu.Driver == "i915" {
			intel = append(intel, i)
			refs = append(refs, gpuRef{bdf: f.bdf, driver: f.gpu.Driver})
		}
	}
	present := make(map[string]bool, len(intel))
	if len(intel) > 0 {
		candidates, issues := discoverGPUPMUs(s.pmuRoot, "i915")
		snapshot.Issues = append(snapshot.Issues, issues...)
		mapped := mapGPUPMUs(refs, candidates)
		for k, i := range intel {
			f := &found[i]
			if mapped[k] < 0 {
				snapshot.Issues = append(snapshot.Issues, Issue{
					Source: filepath.Join(s.pmuRoot, f.gpu.Driver),
					Err:    fmt.Errorf("no %s PMU mapped for GPU %s", f.gpu.Driver, f.bdf),
				})
				continue
			}
			present[f.bdf] = true
			state, ok := s.engines[f.bdf]
			if !ok {
				var issues []Issue
				state, issues = newGPUPMUState(candidates[mapped[k]])
				s.engines[f.bdf] = state
				snapshot.Issues = append(snapshot.Issues, issues...)
			}
			s.sampleGPU(state, &snapshot.GPUs[i], snapshot)
		}
	}
	for key, state := range s.engines {
		if !present[key] {
			_ = state.closeCounters()
			delete(s.engines, key)
		}
	}
}

func newGPUPMUState(pmu pmuCandidate) (*gpuPMUState, []Issue) {
	events, issues := discoverBusyEvents(pmu.root)
	state := &gpuPMUState{pmu: pmu, engines: make([]gpuEngineState, len(events))}
	for i, event := range events {
		state.engines[i] = gpuEngineState{event: event}
	}
	return state, issues
}

// sampleGPU reads every engine counter of one mapped GPU. Any open failure,
// read failure, or rebaseline leaves that sample's usage invalid; partial
// engine data never fabricates a device-wide percentage.
//
// Elapsed is the gap between this engine's counter reads. The clock is read
// after read returns, so time spent in readGPUs or inside the read itself
// is part of the window instead of an impossible delta (issue #18).
func (s *GPUSampler) sampleGPU(state *gpuPMUState, gpu *GPU, snapshot *GPUSnapshot) {
	if len(state.engines) == 0 {
		snapshot.Issues = append(snapshot.Issues, Issue{
			Source: filepath.Join(state.pmu.root, "events"),
			Err:    errors.New("no *-busy events"),
		})
		return
	}
	valid := true
	var fractions []float64
	for i := range state.engines {
		engine := &state.engines[i]
		source := filepath.Join(state.pmu.root, "events", engine.event.name+"-busy")
		if engine.counter == nil {
			counter, err := s.open(state.pmu.pmuType, engine.event.config)
			if err != nil {
				snapshot.Issues = append(snapshot.Issues, Issue{Source: source, Err: err})
				valid = false
				continue
			}
			engine.counter = counter
			engine.hasValue = false
		}
		value, err := engine.counter.read()
		if err != nil {
			snapshot.Issues = append(snapshot.Issues, Issue{Source: source, Err: err})
			engine.hasValue = false
			valid = false
			continue
		}
		readAt := s.now()
		elapsed := time.Duration(0)
		if engine.hasValue {
			elapsed = readAt.Sub(engine.readAt)
		}
		if fraction, ok := engineBusy(engine.value, value, engine.hasValue, elapsed); ok {
			fractions = append(fractions, fraction)
		} else {
			valid = false
		}
		engine.value, engine.hasValue, engine.readAt = value, true, readAt
	}
	if valid {
		gpu.Usage = GPUUsage{Fraction: maxBusyFraction(fractions), Valid: true}
	}
}

func (st *gpuPMUState) closeCounters() error {
	var closeErr error
	for i := range st.engines {
		if st.engines[i].counter == nil {
			continue
		}
		if err := st.engines[i].counter.close(); err != nil && closeErr == nil {
			closeErr = err
		}
		st.engines[i].counter = nil
	}
	return closeErr
}
