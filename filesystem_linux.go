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
	"syscall"
	"time"
)

type mountInfo struct {
	MountID    uint64
	MountPoint string
	Source     string
	Type       string
	ReadOnly   bool
}

func parseMountinfo(r io.Reader) ([]mountInfo, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxScannerToken)
	mounts := []mountInfo{}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		separator := strings.Index(line, " - ")
		if separator < 0 {
			return nil, fmt.Errorf("mountinfo: missing separator")
		}
		before := strings.Fields(line[:separator])
		after := strings.Fields(line[separator+3:])
		if len(before) < 6 || len(after) < 3 {
			return nil, fmt.Errorf("mountinfo: short row")
		}
		mountID, err := strconv.ParseUint(before[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("mountinfo: invalid mount ID: %w", err)
		}
		mountPoint, err := decodeMountField(before[4])
		if err != nil {
			return nil, fmt.Errorf("mountinfo: mount point: %w", err)
		}
		source, err := decodeMountField(after[1])
		if err != nil {
			return nil, fmt.Errorf("mountinfo: source: %w", err)
		}
		readOnly := false
		for _, option := range strings.Split(before[5], ",") {
			if option == "ro" {
				readOnly = true
				break
			}
		}
		mounts = append(mounts, mountInfo{MountID: mountID, MountPoint: mountPoint, Source: source, Type: after[0], ReadOnly: readOnly})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("mountinfo: scan: %w", err)
	}
	if len(mounts) == 0 {
		return nil, fmt.Errorf("mountinfo: no mounts")
	}
	sort.Slice(mounts, func(i, j int) bool {
		if mounts[i].MountPoint != mounts[j].MountPoint {
			return mounts[i].MountPoint < mounts[j].MountPoint
		}
		return mounts[i].MountID < mounts[j].MountID
	})
	return mounts, nil
}

func decodeMountField(value string) (string, error) {
	var decoded strings.Builder
	decoded.Grow(len(value))
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' {
			decoded.WriteByte(value[i])
			continue
		}
		if i+3 >= len(value) {
			return "", fmt.Errorf("truncated escape")
		}
		escape := value[i : i+4]
		// The kernel mangles mount fields with seq_escape over octal
		// bytes: mount points use " \t\n\\" but sources additionally
		// escape '#' as \043 (fs/proc_namespace.c mangle()). Accept any
		// \NNN; the named cases are \040 space, \011 tab, \012 newline,
		// \134 backslash (issue #16).
		character, err := strconv.ParseUint(escape[1:], 8, 8)
		if err != nil {
			return "", fmt.Errorf("invalid escape %q", escape)
		}
		decoded.WriteByte(byte(character))
		i += 3
	}
	return decoded.String(), nil
}

func checkedMultiply(left, right uint64) (uint64, bool) {
	if right != 0 && left > math.MaxUint64/right {
		return 0, false
	}
	return left * right, true
}

func statfsCapacity(stat syscall.Statfs_t) (Capacity, error) {
	// Frsize is the fragment size the block counts are expressed in; Bsize is
	// only the "optimal transfer" hint. Trust Frsize when the filesystem
	// provides it, fall back to Bsize (issue #33).
	blockSize := stat.Bsize
	if stat.Frsize > 0 {
		blockSize = stat.Frsize
	}
	if blockSize <= 0 {
		return Capacity{}, fmt.Errorf("statfs: invalid block size")
	}
	size := uint64(blockSize)
	if stat.Bfree > stat.Blocks {
		return Capacity{}, fmt.Errorf("statfs: free blocks exceed total")
	}
	if stat.Bavail > stat.Blocks {
		return Capacity{}, fmt.Errorf("statfs: available blocks exceed total")
	}
	total, ok := checkedMultiply(stat.Blocks, size)
	if !ok {
		return Capacity{}, fmt.Errorf("statfs: total bytes overflow")
	}
	used, ok := checkedMultiply(stat.Blocks-stat.Bfree, size)
	if !ok {
		return Capacity{}, fmt.Errorf("statfs: used bytes overflow")
	}
	available, ok := checkedMultiply(stat.Bavail, size)
	if !ok {
		return Capacity{}, fmt.Errorf("statfs: available bytes overflow")
	}
	return Capacity{TotalBytes: total, UsedBytes: used, AvailableBytes: available}, nil
}

func readFilesystems(r io.Reader, statfsFunc func(string, *syscall.Statfs_t) error, at time.Time) (FilesystemSnapshot, error) {
	mounts, err := parseMountinfo(r)
	if err != nil {
		return FilesystemSnapshot{}, err
	}
	snapshot := FilesystemSnapshot{CollectedAt: at, Filesystems: make([]Filesystem, 0, len(mounts))}
	for _, mount := range mounts {
		// An autofs trigger has no capacity of its own, and statfs on it
		// fires the automount (fs/statfs.c resolves with LOOKUP_AUTOMOUNT).
		// A filesystem mounted on top of it has its own mountinfo row.
		if mount.Type == "autofs" {
			continue
		}
		var stat syscall.Statfs_t
		if err := statfsFunc(mount.MountPoint, &stat); err != nil {
			snapshot.Issues = append(snapshot.Issues, Issue{Source: mount.MountPoint, Err: err})
			continue
		}
		capacity, err := statfsCapacity(stat)
		if err != nil {
			snapshot.Issues = append(snapshot.Issues, Issue{Source: mount.MountPoint, Err: err})
			continue
		}
		snapshot.Filesystems = append(snapshot.Filesystems, Filesystem{MountID: mount.MountID, MountPoint: mount.MountPoint, Source: mount.Source, Type: mount.Type, ReadOnly: mount.ReadOnly, Capacity: capacity})
	}
	return snapshot, nil
}

// ReadFilesystems returns one mounted-filesystem snapshot from Linux.
// Autofs trigger points are not reported: statfs on a trigger fires the
// automount, and anything mounted on top has its own mountinfo row.
func ReadFilesystems() (FilesystemSnapshot, error) {
	at := time.Now()
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return FilesystemSnapshot{}, fmt.Errorf("/proc/self/mountinfo: %w", err)
	}
	defer file.Close()
	snapshot, err := readFilesystems(file, func(path string, stat *syscall.Statfs_t) error {
		return syscall.Statfs(path, stat)
	}, at)
	if err != nil {
		return FilesystemSnapshot{}, fmt.Errorf("/proc/self/mountinfo: %w", err)
	}
	return snapshot, nil
}
