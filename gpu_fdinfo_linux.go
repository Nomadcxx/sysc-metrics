package metrics

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// procRoot is where DRM client fdinfo is read from.
const procRoot = "/proc"

// drmClients maps a PCI address to its DRM clients, and each client to its
// cumulative busy nanoseconds per engine.
type drmClients map[string]map[string]map[string]uint64

// readDRMClients reads the DRM fdinfo of every process this user can see.
// Each client is counted once however many descriptors hold it. Processes of
// other users are unreadable and skipped: that is the fallback's documented
// limit, not an error worth reporting on every sample.
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
		dir := filepath.Join(root, pid.Name(), "fdinfo")
		fds, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			pdev, id, engines := parseDRMFDInfo(filepath.Join(dir, fd.Name()))
			if pdev == "" || id == "" || len(engines) == 0 {
				continue
			}
			if out[pdev] == nil {
				out[pdev] = map[string]map[string]uint64{}
			}
			out[pdev][id] = engines
		}
	}
	return out
}

func parseDRMFDInfo(path string) (pdev, id string, engines map[string]uint64) {
	file, err := os.Open(path)
	if err != nil {
		return "", "", nil
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
		case strings.HasPrefix(key, "drm-engine-") && !strings.HasPrefix(key, "drm-engine-capacity-"):
			ns, err := strconv.ParseUint(strings.TrimSuffix(value, " ns"), 10, 64)
			if err != nil {
				continue
			}
			if engines == nil {
				engines = map[string]uint64{}
			}
			engines[strings.TrimPrefix(key, "drm-engine-")] = ns
		}
	}
	return pdev, id, engines
}

// fdinfoBusy is the busiest engine's share of elapsed, summed over clients
// present in both samples. A client that appeared, exited, or whose counter
// went backwards (a reused id) has no delta for this interval and is left
// out; with no client seen twice there is no reading at all.
func fdinfoBusy(prev, cur map[string]map[string]uint64, elapsed time.Duration) (float64, bool) {
	if elapsed <= 0 {
		return 0, false
	}
	sums := map[string]uint64{}
	seen := false
	for id, engines := range cur {
		before, ok := prev[id]
		if !ok {
			continue
		}
		backwards := false
		for engine, ns := range engines {
			if ns < before[engine] {
				backwards = true
			}
		}
		if backwards {
			continue
		}
		seen = true
		for engine, ns := range engines {
			sums[engine] += ns - before[engine]
		}
	}
	if !seen {
		return 0, false
	}
	busiest := uint64(0)
	for _, ns := range sums {
		busiest = max(busiest, ns)
	}
	return min(float64(busiest)/float64(elapsed.Nanoseconds()), 1), true
}

// applyFDInfo fills usage for GPUs the drm sysfs, nvidia-smi and PMU paths
// left invalid, from DRM client fdinfo. It is the unprivileged path: the i915
// PMU needs CAP_PERFMON, which a desktop shell does not hold. /proc is walked
// only while some GPU still lacks usage.
func (s *GPUSampler) applyFDInfo(now time.Time, found []gpuFound, snapshot *GPUSnapshot) {
	var missing []int
	for i := range snapshot.GPUs {
		if !snapshot.GPUs[i].Usage.Valid && found[i].bdf != "" {
			missing = append(missing, i)
		}
	}
	if len(missing) == 0 {
		s.fdPrev, s.fdHasPrev = nil, false
		return
	}
	clients := readDRMClients(s.procRoot)
	if s.fdHasPrev {
		elapsed := now.Sub(s.fdPrevAt)
		for _, i := range missing {
			bdf := strings.ToLower(found[i].bdf)
			if busy, ok := fdinfoBusy(s.fdPrev[bdf], clients[bdf], elapsed); ok {
				snapshot.GPUs[i].Usage = GPUUsage{Fraction: busy, Valid: true}
			}
		}
	}
	s.fdPrev, s.fdPrevAt, s.fdHasPrev = clients, now, true
}
