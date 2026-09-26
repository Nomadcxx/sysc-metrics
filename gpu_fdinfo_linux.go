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
	// A driver that publishes no drm-engine-* time (xe's cycle counters,
	// nouveau, virtual GPUs) would otherwise cost a /proc walk every sample
	// for a reading that can never come.
	fdinfoMaxEmptyWalks = 3
	// fdinfoRetryEvery is how many samples pass between retries once the
	// walk has given up, so a GPU client that starts later is still found.
	fdinfoRetryEvery = 30
)

// drmClient is one DRM client's cumulative busy nanoseconds per engine
// class, and the instance count of each class that has more than one.
type drmClient struct {
	busy     map[string]uint64
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
			if pdev == "" || id == "" || len(client.busy) == 0 {
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

// fdinfoBusy is the busiest engine class's share of its capacity over
// elapsed, summed over clients present in both samples. A class with several
// instances accumulates up to capacity × elapsed (drm-usage-stats), so its
// time is divided by that. A client that appeared, exited, or whose counter
// went backwards (a reused id) has no delta for this interval and is left
// out; with no client seen twice there is no reading at all.
func fdinfoBusy(prev, cur map[string]drmClient, elapsed time.Duration) (float64, bool) {
	if elapsed <= 0 {
		return 0, false
	}
	sums := map[string]uint64{}
	capacity := map[string]uint64{}
	seen := false
	for id, client := range cur {
		before, ok := prev[id]
		if !ok {
			continue
		}
		backwards := false
		for engine, ns := range client.busy {
			if ns < before.busy[engine] {
				backwards = true
			}
		}
		if backwards {
			continue
		}
		seen = true
		for engine, ns := range client.busy {
			sums[engine] += ns - before.busy[engine]
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
func (s *GPUSampler) applyFDInfo(now time.Time, found []gpuFound, snapshot *GPUSnapshot) {
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
		elapsed := now.Sub(s.fdPrevAt)
		for _, i := range missing {
			bdf := strings.ToLower(found[i].bdf)
			if busy, ok := fdinfoBusy(s.fdPrev[bdf], clients[bdf], elapsed); ok {
				snapshot.GPUs[i].Usage = GPUUsage{Fraction: busy, Valid: true}
				filled = true
			}
		}
	}
	s.fdPrev, s.fdPrevAt, s.fdHasPrev = clients, now, true
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
