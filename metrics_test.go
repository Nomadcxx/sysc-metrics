package metrics

import (
	"errors"
	"strings"
	"testing"
)

var (
	_ func() *CPUSampler                             = NewCPUSampler
	_ func(*CPUSampler) (CPUSnapshot, error)         = (*CPUSampler).Sample
	_ func() (MemorySnapshot, error)                 = ReadMemory
	_ func() (UptimeSnapshot, error)                 = ReadUptime
	_ func() (FilesystemSnapshot, error)             = ReadFilesystems
	_ func() *BlockSampler                           = NewBlockSampler
	_ func(*BlockSampler) (BlockSnapshot, error)     = (*BlockSampler).Sample
	_ func() *NetworkSampler                         = NewNetworkSampler
	_ func(*NetworkSampler) (NetworkSnapshot, error) = (*NetworkSampler).Sample
)

func TestIssueWrapsSourceAndCause(t *testing.T) {
	cause := errors.New("bad value")
	issue := Issue{Source: "/proc/example", Err: cause}

	if !strings.Contains(issue.Error(), "/proc/example") || !strings.Contains(issue.Error(), "bad value") {
		t.Fatalf("Issue.Error() = %q", issue.Error())
	}
	if !errors.Is(issue, cause) {
		t.Fatal("Issue does not unwrap its cause")
	}
}

func TestPublicValueTypesHaveInvalidDerivedZeroValues(t *testing.T) {
	var cpu CPUUsage
	var core CPUCore
	var snapshot CPUSnapshot
	var block BlockRates
	var network NetworkRates

	if cpu.Valid || core.FrequencyValid || snapshot.LoadValid || block.Valid || network.Valid {
		t.Fatal("derived validity flags must default to false")
	}

	_ = Capacity{}
	_ = MemorySnapshot{}
	_ = UptimeSnapshot{}
	_ = Filesystem{}
	_ = FilesystemSnapshot{}
	_ = BlockDevice{}
	_ = BlockSnapshot{}
	_ = NetworkInterface{}
	_ = NetworkSnapshot{}
}
