package metrics

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// procRoot is where DRM client fdinfo is read from.
const procRoot = "/proc"

const (
	// fdinfoMaxEmptyWalks is how many consecutive walks may find no engine
	// data for any GPU still missing usage before the sampler stops walking.
	// A driver that publishes neither engine time nor cycle counters
	// (nouveau, virtual GPUs) would otherwise cost a /proc walk every sample
	// for a reading that can never come.
	fdinfoMaxEmptyWalks = 3
	// fdinfoRetryEvery is how many samples pass between retries once the
	// walk has given up, so a GPU client that starts later is still found.
	fdinfoRetryEvery = 30
)

// drmClient is one DRM client's cumulative busy nanoseconds per engine
// class, its busy-cycle counters where the driver publishes them (xe),
// and the instance count of each class that has more than one.
type drmClient struct {
	busy     map[string]uint64
	cycles   map[string]uint64
	total    map[string]uint64
	capacity map[string]uint64
}

// drmClients maps a PCI address to its DRM clients by client id.
type drmClients map[string]map[string]drmClient

// readDRMClients reads the DRM fdinfo of every process this user can see.
// Only descriptors that point into /dev/dri are parsed, so the walk costs one
// readlink per descriptor rather than one file read. Each client is counted
// once however many descriptors hold it. Processes of other users are
// unreadable and skipped: that is the fallback's documented limit.
func readDRMClients(root string) drmClients {
	out := drmClients{}
	pids, err := os.ReadDir(root)
	if err != nil {
		return out
	}
	for _, pid := range pids {
		if _, err := strconv.Atoi(pid.Name()); err != nil {
			continue
		}
		fdDir := filepath.Join(root, pid.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil || !strings.HasPrefix(target, "/dev/dri/") {
				continue
			}
			pdev, id, client := parseDRMFDInfo(filepath.Join(root, pid.Name(), "fdinfo", fd.Name()))
			if pdev == "" || id == "" || len(client.busy) == 0 && len(client.cycles) == 0 {
				continue
			}
			if out[pdev] == nil {
				out[pdev] = map[string]drmClient{}
			}
			out[pdev][id] = client
		}
	}
	return out
}

func parseDRMFDInfo(path string) (pdev, id string, client drmClient) {
	file, err := os.Open(path)
	if err != nil {
		return "", "", drmClient{}
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch {
		case key == "drm-pdev":
			pdev = strings.ToLower(value)
		case key == "drm-client-id":
			id = value
		case strings.HasPrefix(key, "drm-engine-capacity-"):
			n, err := strconv.ParseUint(value, 10, 64)
			if err != nil || n == 0 {
				continue
			}
			if client.capacity == nil {
				client.capacity = map[string]uint64{}
			}
			client.capacity[strings.TrimPrefix(key, "drm-engine-capacity-")] = n
		case strings.HasPrefix(key, "drm-cycles-"):
			// Busy cycles for the engine. xe publishes these instead of
			// drm-engine-* time (issue #574).
			n, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				continue
			}
			if client.cycles == nil {
				client.cycles = map[string]uint64{}
			}
			client.cycles[strings.TrimPrefix(key, "drm-cycles-")] = n
		case strings.HasPrefix(key, "drm-total-cycles-"):
			// A GPU-clock timestamp matching drm-cycles' update rate, so
			// utilization needs no CPU-side interval (drm-usage-stats).
			n, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				continue
			}
			if client.total == nil {
				client.total = map[string]uint64{}
			}
			client.total[strings.TrimPrefix(key, "drm-total-cycles-")] = n
		case strings.HasPrefix(key, "drm-engine-"):
			// Engine time is specified in nanoseconds; any other unit is
			// not engine time and is left out.
			ns, err := strconv.ParseUint(strings.TrimSuffix(value, " ns"), 10, 64)
			if err != nil {
				continue
			}
			if client.busy == nil {
				client.busy = map[string]uint64{}
			}
			client.busy[strings.TrimPrefix(key, "drm-engine-")] = ns
		}
	}
	return pdev, id, client
}

// fdinfoCycleBusy is the busiest engine class's share of its capacity from
// the drm-cycles-*/drm-total-cycles-* counters xe publishes in place of
// drm-engine-* time (issue #574). Both counters advance on the GPU clock,
// so the ratio needs no CPU-side interval and is unaffected by the shell
// sleeping between samples. drm-total-cycles is a device-wide timestamp
// (every client of a device reports the same value), so the cycle counts
// are summed over clients while the total is taken once.
//
// A class whose delta total did not advance cannot be divided; it is left
// out rather than read as idle. The same rebase rule as engine time
// applies: a class first seen on a steady client carries its cumulative
// count from before we sampled it and contributes nothing (issue #9).
func fdinfoCycleBusy(prev, cur map[string]drmClient) (float64, bool) {
	sums := map[string]uint64{}
	totals := map[string]uint64{}
	capacity := map[string]uint64{}
	seen := false
	for id, client := range cur {
		before, ok := prev[id]
		if !ok {
			continue
		}
		if countersRegressed(before, client) {
			continue
		}
		for class, cycles := range client.cycles {
			base, had := before.cycles[class]
			total, hasTotal := client.total[class]
			prevTotal, hadTotal := before.total[class]
			// Both counters must have advanced: a class first seen
			// mid-stream carries a cumulative count, and a GPU clock that
			// did not move leaves no ratio to read at all.
			if !had || !hasTotal || !hadTotal || total <= prevTotal {
				continue
			}
			seen = true
			sums[class] += cycles - base
			totals[class] = max(totals[class], total-prevTotal)
			capacity[class] = max(capacity[class], client.capacity[class], 1)
		}
	}
	if !seen {
		return 0, false
	}
	busiest := 0.0
	for class, cycles := range sums {
		if totals[class] == 0 {
			continue
		}
		busiest = max(busiest, float64(cycles)/(float64(totals[class])*float64(capacity[class])))
	}
	return min(busiest, 1), true
}

// countersRegressed reports whether any cumulative counter went backwards,
// which the usage-stats spec allows until a monotonic update catches up; a
// reused client id reappears with small values and must not be read as a
// huge delta.
func countersRegressed(before, cur drmClient) bool {
	for class, ns := range cur.busy {
		if prevNS, had := before.busy[class]; had && ns < prevNS {
			return true
		}
	}
	for class, n := range cur.cycles {
		if prevN, had := before.cycles[class]; had && n < prevN {
			return true
		}
	}
	for class, n := range cur.total {
		if prevN, had := before.total[class]; had && n < prevN {
			return true
		}
	}
	return false
}

// fdinfoBusy is the busiest engine class's share of its capacity, summed
// over clients present in both samples. Engine time (i915, amdgpu) is
// divided by capacity × elapsed: a class with several instances accumulates
// up to capacity × elapsed (drm-usage-stats). Cycle counters (xe) are
// divided by their own GPU-clock total instead, so a driver publishing both
// reads on the cycles. A client that appeared, exited, or whose counter went
// backwards (a reused id) has no delta for this interval and is left out;
// with no client seen twice there is no reading at all.
func fdinfoBusy(prev, cur map[string]drmClient, elapsed time.Duration) (float64, bool) {
	busy, ok := fdinfoEngineBusy(prev, cur, elapsed)
	if cycles, cycleOK := fdinfoCycleBusy(prev, cur); cycleOK && (!ok || cycles > busy) {
		// Cycles are the more accurate reading where a driver has both.
		busy, ok = cycles, true
	}
	return busy, ok
}

func fdinfoEngineBusy(prev, cur map[string]drmClient, elapsed time.Duration) (float64, bool) {
	if elapsed <= 0 {
		return 0, false
	}
	sums := map[string]uint64{}
	capacity := map[string]uint64{}
	seen := false
	for id, client := range cur {
		before, ok := prev[id]
		if !ok || countersRegressed(before, client) {
			continue
		}
		for engine, ns := range client.busy {
			base, had := before.busy[engine]
			if !had {
				// A new engine class on an existing client carries its
				// cumulative busy time from before we ever sampled it;
				// counting that as this interval's delta reports a
				// pinned 1.0. Rebase it and contribute nothing yet
				// (issue #9, same spirit as a brand-new client).
				continue
			}
			seen = true
			sums[engine] += ns - base
			capacity[engine] = max(capacity[engine], client.capacity[engine], 1)
		}
	}
	if !seen {
		return 0, false
	}
	busiest := 0.0
	for engine, ns := range sums {
		busiest = max(busiest, float64(ns)/(float64(capacity[engine])*float64(elapsed.Nanoseconds())))
	}
	return min(busiest, 1), true
}

// applyFDInfo fills usage for GPUs the drm sysfs, nvidia-smi and PMU paths
// left invalid, from DRM client fdinfo. It is the unprivileged path: the i915
// PMU needs CAP_PERFMON, which a desktop shell does not hold. /proc is walked
// only while some GPU still lacks usage, and backs off when the walk keeps
// finding no engine data for any of them.
//
// Elapsed is the gap between fdinfo reads. The clock is read after
// readDRMClients returns, so time spent in readGPUs (pci.ids, nvidia-smi)
// or the PMU reads is part of the window. Dividing by the earlier
// collection stamp clamps a late read to 1 (issue #25).
func (s *GPUSampler) applyFDInfo(found []gpuFound, snapshot *GPUSnapshot) {
	var missing []int
	for i := range snapshot.GPUs {
		if !snapshot.GPUs[i].Usage.Valid && found[i].bdf != "" {
			missing = append(missing, i)
		}
	}
	if len(missing) == 0 {
		s.fdPrev, s.fdHasPrev, s.fdEmpty, s.fdIdle = nil, false, 0, 0
		return
	}
	if s.fdEmpty >= fdinfoMaxEmptyWalks {
		s.fdIdle++
		if s.fdIdle < fdinfoRetryEvery {
			return
		}
		s.fdIdle = 0
	}

	clients := readDRMClients(s.procRoot)
	readAt := s.now()
	s.fdWalks++
	anyData := false
	for _, i := range missing {
		if len(clients[strings.ToLower(found[i].bdf)]) > 0 {
			anyData = true
		}
	}
	if anyData {
		s.fdEmpty = 0
	} else {
		s.fdEmpty++
		if s.fdEmpty == fdinfoMaxEmptyWalks {
			snapshot.Issues = append(snapshot.Issues, Issue{
				Source: s.procRoot,
				Err:    errors.New("no DRM fdinfo engine time for a GPU without usage; retrying occasionally"),
			})
		}
	}

	filled := false
	if s.fdHasPrev {
		elapsed := readAt.Sub(s.fdPrevAt)
		for _, i := range missing {
			bdf := strings.ToLower(found[i].bdf)
			if busy, ok := fdinfoBusy(s.fdPrev[bdf], clients[bdf], elapsed); ok {
				snapshot.GPUs[i].Usage = GPUUsage{Fraction: busy, Valid: true}
				filled = true
			}
		}
	}
	s.fdPrev, s.fdPrevAt, s.fdHasPrev = clients, readAt, true
	if filled {
		s.dropPMUIssuesIfCovered(snapshot)
	}
}

// dropPMUIssuesIfCovered removes the PMU's open failures once every i915 GPU
// has usage from fdinfo: a denied counter is then the expected unprivileged
// state, not an error a consumer should surface on every sample.
func (s *GPUSampler) dropPMUIssuesIfCovered(snapshot *GPUSnapshot) {
	for _, g := range snapshot.GPUs {
		if g.Driver == "i915" && !g.Usage.Valid {
			return
		}
	}
	kept := snapshot.Issues[:0]
	for _, issue := range snapshot.Issues {
		if !strings.HasPrefix(issue.Source, s.pmuRoot) {
			kept = append(kept, issue)
		}
	}
	snapshot.Issues = kept
}
