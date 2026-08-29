//go:build linux

package metrics

import (
	"strings"
	"testing"
	"time"
)

func TestParseMeminfo(t *testing.T) {
	at := time.Unix(10, 0)
	tests := []struct {
		name  string
		input string
		want  MemorySnapshot
		bad   bool
	}{
		{
			name:  "values and ordering",
			input: "SwapFree: 0 kB\nUnknown: 3 MB\nMemAvailable: 768 kB\nSwapTotal: 0 kB\nMemTotal: 1024 kB\n",
			want:  MemorySnapshot{CollectedAt: at, Memory: Capacity{TotalBytes: 1024 * 1024, UsedBytes: 256 * 1024, AvailableBytes: 768 * 1024}},
		},
		{name: "missing", input: "MemTotal: 1 kB\nMemAvailable: 1 kB\nSwapTotal: 1 kB\n", bad: true},
		{name: "duplicate", input: "MemTotal: 1 kB\nMemTotal: 1 kB\nMemAvailable: 1 kB\nSwapTotal: 1 kB\nSwapFree: 1 kB\n", bad: true},
		{name: "unit", input: "MemTotal: 1 MB\nMemAvailable: 1 kB\nSwapTotal: 1 kB\nSwapFree: 1 kB\n", bad: true},
		{name: "negative", input: "MemTotal: -1 kB\nMemAvailable: 1 kB\nSwapTotal: 1 kB\nSwapFree: 1 kB\n", bad: true},
		{name: "malformed", input: "MemTotal: nope kB\nMemAvailable: 1 kB\nSwapTotal: 1 kB\nSwapFree: 1 kB\n", bad: true},
		{name: "available exceeds total", input: "MemTotal: 1 kB\nMemAvailable: 2 kB\nSwapTotal: 1 kB\nSwapFree: 1 kB\n", bad: true},
		{name: "swap free exceeds total", input: "MemTotal: 1 kB\nMemAvailable: 1 kB\nSwapTotal: 1 kB\nSwapFree: 2 kB\n", bad: true},
		{name: "multiplication overflow", input: "MemTotal: 18446744073709551615 kB\nMemAvailable: 1 kB\nSwapTotal: 1 kB\nSwapFree: 1 kB\n", bad: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseMeminfo(strings.NewReader(tt.input), at)
			if tt.bad {
				if err == nil {
					t.Fatal("parseMeminfo unexpectedly succeeded")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("parseMeminfo() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
