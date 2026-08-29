//go:build linux

package metrics

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestParseCPUStat(t *testing.T) {
	parsed, err := parseCPUStat(strings.NewReader("cpu 10 20 30 40 5 6 7 8 100 200\ncpu2 1 2 3 4 5 6 7 8\ncpu0 8 7 6 5 4 3 2 1\nintr 1 2 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.aggregate != (cpuTimes{user: 10, nice: 20, system: 30, idle: 40, iowait: 5, irq: 6, softirq: 7, steal: 8}) {
		t.Fatalf("aggregate = %#v", parsed.aggregate)
	}
	if len(parsed.cores) != 2 || parsed.cores[0].user != 8 || parsed.cores[2].idle != 4 {
		t.Fatalf("cores = %#v", parsed.cores)
	}
	total, ok := parsed.aggregate.total()
	if !ok || total != 126 {
		t.Fatalf("aggregate total = %d, %v", total, ok)
	}
}

func TestParseCPUStatRejectsInvalidStructure(t *testing.T) {
	tests := []string{
		"cpu 1 2 3 4 5 6 7\n",
		"cpu 1 2 nope 4 5 6 7 8\n",
		"cpu 1 2 3 4 5 6 7 8\ncpu 1 2 3 4 5 6 7 8\n",
		"cpu 1 2 3 4 5 6 7 8\ncpu0 1 2 3 4 5 6 7 8\ncpu0 1 2 3 4 5 6 7 8\n",
		"cpu0 1 2 3 4 5 6 7 8\n",
		strings.Repeat("x", maxScannerToken+1),
	}
	for _, input := range tests {
		if _, err := parseCPUStat(strings.NewReader(input)); err == nil {
			t.Errorf("parseCPUStat(%q) unexpectedly succeeded", input[:min(len(input), 24)])
		}
	}
}

func TestCPUUsage(t *testing.T) {
	tests := []struct {
		name  string
		prev  cpuTimes
		cur   cpuTimes
		want  float64
		valid bool
	}{
		{
			name:  "busy and idle",
			prev:  cpuTimes{user: 10, idle: 10},
			cur:   cpuTimes{user: 20, idle: 20},
			want:  0.5,
			valid: true,
		},
		{
			name:  "legitimate zero",
			prev:  cpuTimes{user: 10, idle: 10},
			cur:   cpuTimes{user: 10, idle: 20},
			want:  0,
			valid: true,
		},
		{name: "identical", prev: cpuTimes{user: 10}, cur: cpuTimes{user: 10}},
		{name: "counter regression", prev: cpuTimes{user: 10}, cur: cpuTimes{user: 9}},
		{name: "idle overflow", prev: cpuTimes{}, cur: cpuTimes{idle: math.MaxUint64, iowait: 1}},
		{name: "total overflow", prev: cpuTimes{}, cur: cpuTimes{user: math.MaxUint64, nice: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cpuUsage(tt.prev, tt.cur)
			if got.Valid != tt.valid || (got.Valid && got.Fraction != tt.want) {
				t.Fatalf("cpuUsage() = %#v, want %.3f valid=%v", got, tt.want, tt.valid)
			}
		})
	}
}

func TestCPUUsageRejectsEveryCounterRegression(t *testing.T) {
	mutations := []struct {
		name   string
		change func(*cpuTimes)
	}{
		{"user", func(value *cpuTimes) { value.user = 9 }},
		{"nice", func(value *cpuTimes) { value.nice = 9 }},
		{"system", func(value *cpuTimes) { value.system = 9 }},
		{"idle", func(value *cpuTimes) { value.idle = 9 }},
		{"iowait", func(value *cpuTimes) { value.iowait = 9 }},
		{"irq", func(value *cpuTimes) { value.irq = 9 }},
		{"softirq", func(value *cpuTimes) { value.softirq = 9 }},
		{"steal", func(value *cpuTimes) { value.steal = 9 }},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			previous := cpuTimes{user: 10, nice: 10, system: 10, idle: 10, iowait: 10, irq: 10, softirq: 10, steal: 10}
			current := previous
			mutation.change(&current)
			if got := cpuUsage(previous, current); got.Valid {
				t.Fatalf("cpuUsage() = %#v after %s regression", got, mutation.name)
			}
		})
	}
}

func TestParseLoadavg(t *testing.T) {
	load, err := parseLoadavg(strings.NewReader("1.25 2 3.5 1/100 123\n"))
	if err != nil || load != (cpuLoad{Load1: 1.25, Load5: 2, Load15: 3.5, Valid: true}) {
		t.Fatalf("parseLoadavg() = %#v, %v", load, err)
	}
	for _, input := range []string{"", "1 2\n", "1 -2 3\n", "NaN 2 3\n", "Inf 2 3\n", "nope 2 3\n"} {
		if _, err := parseLoadavg(strings.NewReader(input)); err == nil {
			t.Errorf("parseLoadavg(%q) unexpectedly succeeded", input)
		}
	}
}

func TestParseFrequency(t *testing.T) {
	got, err := parseFrequency(strings.NewReader("2400000\n"))
	if err != nil || got != 2400000000 {
		t.Fatalf("parseFrequency() = %d, %v", got, err)
	}
	for _, input := range []string{"", "-1\n", "nope\n", "18446744073709551615\n"} {
		if _, err := parseFrequency(strings.NewReader(input)); err == nil {
			t.Errorf("parseFrequency(%q) unexpectedly succeeded", input)
		}
	}
}

func TestCPUSamplerLifecycle(t *testing.T) {
	sampler := NewCPUSampler()
	first := parsedCPU{aggregate: cpuTimes{user: 10, idle: 10}, cores: map[int]cpuTimes{0: {user: 10, idle: 10}}}
	second := parsedCPU{aggregate: cpuTimes{user: 20, idle: 20}, cores: map[int]cpuTimes{
		0: {user: 20, idle: 20},
		2: {user: 1, idle: 1},
	}}
	load := cpuLoad{Load1: 1, Load5: 2, Load15: 3, Valid: true}

	snapshot := sampler.sampleCPU(first, load, map[int]uint64{0: 1000}, nil, time.Unix(1, 0))
	if snapshot.Usage.Valid || snapshot.Cores[0].Usage.Valid || snapshot.Cores[0].FrequencyHz != 1000 || !snapshot.Cores[0].FrequencyValid {
		t.Fatalf("first snapshot = %#v", snapshot)
	}
	snapshot = sampler.sampleCPU(second, load, map[int]uint64{0: 2000}, nil, time.Unix(2, 0))
	if !snapshot.Usage.Valid || snapshot.Usage.Fraction != 0.5 || len(snapshot.Cores) != 2 || !snapshot.Cores[0].Usage.Valid || snapshot.Cores[1].Usage.Valid || snapshot.Cores[1].ID != 2 {
		t.Fatalf("second snapshot = %#v", snapshot)
	}
	if snapshot.Cores[1].FrequencyValid {
		t.Fatal("missing frequency should remain invalid")
	}
	removed := parsedCPU{aggregate: cpuTimes{user: 30, idle: 30}, cores: map[int]cpuTimes{2: {user: 2, idle: 2}}}
	snapshot = sampler.sampleCPU(removed, load, nil, nil, time.Unix(3, 0))
	if len(snapshot.Cores) != 1 || snapshot.Cores[0].ID != 2 {
		t.Fatalf("removed core retained: %#v", snapshot.Cores)
	}
}

func TestCPUSamplerResetInvalidatesOneObservation(t *testing.T) {
	sampler := NewCPUSampler()
	at := time.Unix(1, 0)
	base := parsedCPU{aggregate: cpuTimes{user: 10, idle: 10}, cores: map[int]cpuTimes{0: {user: 10, idle: 10}}}
	sampler.sampleCPU(base, cpuLoad{}, nil, nil, at)
	reset := parsedCPU{aggregate: cpuTimes{user: 5, idle: 11}, cores: map[int]cpuTimes{0: {user: 5, idle: 11}}}
	if got := sampler.sampleCPU(reset, cpuLoad{}, nil, nil, at.Add(time.Second)); got.Usage.Valid || got.Cores[0].Usage.Valid {
		t.Fatalf("reset snapshot = %#v", got)
	}
	next := parsedCPU{aggregate: cpuTimes{user: 6, idle: 12}, cores: map[int]cpuTimes{0: {user: 6, idle: 12}}}
	if got := sampler.sampleCPU(next, cpuLoad{}, nil, nil, at.Add(2*time.Second)); !got.Usage.Valid || !got.Cores[0].Usage.Valid {
		t.Fatalf("post-reset snapshot = %#v", got)
	}
}
