//go:build linux

package metrics

import (
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParseMountinfo(t *testing.T) {
	input := "20 1 0:1 / /same\\040mount rw,nosuid - ext4 /dev/a rw\n" +
		"10 1 0:2 / /same\\040mount ro - tmpfs tmpfs rw\n" +
		"30 1 0:3 / /tab\\011line\\012slash\\134 rw - xfs /dev/x rw\n"
	mounts, err := parseMountinfo(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 3 || mounts[0].MountID != 10 || mounts[1].MountID != 20 {
		t.Fatalf("mount order = %#v", mounts)
	}
	if mounts[0].MountPoint != "/same mount" || !mounts[0].ReadOnly || mounts[0].Type != "tmpfs" || mounts[0].Source != "tmpfs" {
		t.Fatalf("decoded mount = %#v", mounts[0])
	}
	if mounts[2].MountPoint != "/tab\tline\nslash\\" {
		t.Fatalf("escaped mount point = %q", mounts[2].MountPoint)
	}
}

func TestParseMountinfoRejectsMalformedRows(t *testing.T) {
	inputs := []string{
		"1 1 0:1 / / rw ext4 /dev/a rw\n",
		"1 1 0:1 / / rw - ext4\n",
		"1 1 0:1 / / rw - ext4 /dev/a\n",
		"nope 1 0:1 / / rw - ext4 /dev/a rw\n",
		"1 1 0:1 / /bad\\999 rw - ext4 /dev/a rw\n",
		"",
	}
	for _, input := range inputs {
		if _, err := parseMountinfo(strings.NewReader(input)); err == nil {
			t.Errorf("parseMountinfo(%q) unexpectedly succeeded", input)
		}
	}
}

func TestStatfsCapacity(t *testing.T) {
	stat := syscall.Statfs_t{Blocks: 100, Bfree: 20, Bavail: 15, Bsize: 4096}
	got, err := statfsCapacity(stat)
	if err != nil || got != (Capacity{TotalBytes: 409600, UsedBytes: 327680, AvailableBytes: 61440}) {
		t.Fatalf("statfsCapacity() = %#v, %v", got, err)
	}
	bad := []syscall.Statfs_t{
		{Blocks: 1, Bfree: 2, Bavail: 0, Bsize: 1},
		{Blocks: 1, Bfree: 0, Bavail: 2, Bsize: 1},
		{Blocks: 1, Bfree: 0, Bavail: 0, Bsize: -1},
		{Blocks: ^uint64(0), Bfree: 0, Bavail: 0, Bsize: 2},
	}
	for _, stat := range bad {
		if _, err := statfsCapacity(stat); err == nil {
			t.Errorf("statfsCapacity(%#v) unexpectedly succeeded", stat)
		}
	}
}

func TestReadFilesystemsPreservesHealthyMounts(t *testing.T) {
	input := "1 1 0:1 / /good rw - ext4 /dev/a rw\n2 1 0:2 / /bad rw - ext4 /dev/b rw\n"
	statfsFunc := func(path string, stat *syscall.Statfs_t) error {
		if path == "/bad" {
			return errors.New("permission denied")
		}
		*stat = syscall.Statfs_t{Blocks: 10, Bfree: 2, Bavail: 3, Bsize: 1024}
		return nil
	}
	snapshot, err := readFilesystems(strings.NewReader(input), statfsFunc, time.Unix(1, 0))
	if err != nil || len(snapshot.Filesystems) != 1 || snapshot.Filesystems[0].MountPoint != "/good" || len(snapshot.Issues) != 1 || !strings.Contains(snapshot.Issues[0].Source, "/bad") {
		t.Fatalf("readFilesystems() = %#v, %v", snapshot, err)
	}
}
