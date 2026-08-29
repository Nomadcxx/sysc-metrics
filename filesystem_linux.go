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
		if len(before) < 6 || len(after) < 2 {
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
		var character byte
		switch escape {
		case `\040`:
			character = ' '
		case `\011`:
			character = '\t'
		case `\012`:
			character = '\n'
		case `\134`:
			character = '\\'
		default:
			return "", fmt.Errorf("invalid escape %q", escape)
		}
		decoded.WriteByte(character)
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
	if stat.Bsize <= 0 {
		return Capacity{}, fmt.Errorf("statfs: invalid block size")
	}
	blockSize := uint64(stat.Bsize)
	if stat.Bfree > stat.Blocks {
		return Capacity{}, fmt.Errorf("statfs: free blocks exceed total")
	}
	if stat.Bavail > stat.Blocks {
		return Capacity{}, fmt.Errorf("statfs: available blocks exceed total")
	}
	total, ok := checkedMultiply(stat.Blocks, blockSize)
	if !ok {
		return Capacity{}, fmt.Errorf("statfs: total bytes overflow")
	}
	used, ok := checkedMultiply(stat.Blocks-stat.Bfree, blockSize)
	if !ok {
		return Capacity{}, fmt.Errorf("statfs: used bytes overflow")
	}
	available, ok := checkedMultiply(stat.Bavail, blockSize)
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
func ReadFilesystems() (FilesystemSnapshot, error) {
	at := time.Now()
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return FilesystemSnapshot{}, fmt.Errorf("/proc/self/mountinfo: %w", err)
	}
	defer file.Close()
	return readFilesystems(file, func(path string, stat *syscall.Statfs_t) error {
		return syscall.Statfs(path, stat)
	}, at)
}
