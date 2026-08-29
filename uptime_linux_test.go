//go:build linux

package metrics

import (
	"strings"
	"testing"
	"time"
)

func TestParseUptime(t *testing.T) {
	at := time.Unix(20, 0)
	tests := []struct {
		name  string
		input string
		want  time.Duration
		bad   bool
	}{
		{name: "whole seconds", input: "123.0 0.0\n", want: 123 * time.Second},
		{name: "fractional seconds", input: "1.234567 2.0\n", want: 1234567 * time.Microsecond},
		{name: "missing", input: "", bad: true},
		{name: "negative", input: "-1.0 0.0\n", bad: true},
		{name: "malformed", input: "nope 0.0\n", bad: true},
		{name: "duration overflow", input: "2562047h48m6.854776s 0\n", bad: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseUptime(strings.NewReader(tt.input), at)
			if tt.bad {
				if err == nil {
					t.Fatal("parseUptime unexpectedly succeeded")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Uptime != tt.want || !got.CollectedAt.Equal(at) {
				t.Fatalf("parseUptime() = %#v, want uptime %s at %s", got, tt.want, at)
			}
		})
	}
}
