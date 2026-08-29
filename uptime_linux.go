//go:build linux

package metrics

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

func parseUptime(r io.Reader, at time.Time) (UptimeSnapshot, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxScannerToken)
	var token string
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) > 0 {
			token = fields[0]
			break
		}
	}
	if err := scanner.Err(); err != nil {
		return UptimeSnapshot{}, fmt.Errorf("scan: %w", err)
	}
	if token == "" {
		return UptimeSnapshot{}, fmt.Errorf("uptime: missing seconds")
	}
	uptime, err := time.ParseDuration(token + "s")
	if err != nil {
		return UptimeSnapshot{}, fmt.Errorf("uptime: invalid seconds: %w", err)
	}
	if uptime < 0 {
		return UptimeSnapshot{}, fmt.Errorf("uptime: negative seconds")
	}
	return UptimeSnapshot{CollectedAt: at, Uptime: uptime}, nil
}

// ReadUptime returns one uptime snapshot from Linux.
func ReadUptime() (UptimeSnapshot, error) {
	at := time.Now()
	file, err := os.Open("/proc/uptime")
	if err != nil {
		return UptimeSnapshot{}, fmt.Errorf("/proc/uptime: %w", err)
	}
	defer file.Close()
	snapshot, err := parseUptime(file, at)
	if err != nil {
		return UptimeSnapshot{}, fmt.Errorf("/proc/uptime: %w", err)
	}
	return snapshot, nil
}
