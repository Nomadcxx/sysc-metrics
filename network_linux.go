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
	"time"
)

func parseNetDev(r io.Reader) ([]NetworkInterface, []Issue, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxScannerToken)
	interfaces := []NetworkInterface{}
	issues := []Issue{}
	seen := make(map[string]bool)
	header := 0
	dataRows := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if header < 2 {
			if (header == 0 && (!strings.HasPrefix(line, "Inter-|") || !strings.Contains(line, "Receive") || !strings.Contains(line, "Transmit"))) || (header == 1 && (!strings.HasPrefix(line, "face |") || !strings.Contains(line, "bytes") || !strings.Contains(line, "packets"))) {
				return nil, nil, fmt.Errorf("/proc/net/dev: malformed header")
			}
			header++
			continue
		}
		dataRows = true
		name, values, err := parseNetDevRow(line)
		if err != nil {
			if separator := strings.IndexByte(line, ':'); separator > 0 {
				name = strings.TrimSpace(line[:separator])
			}
			if name == "" {
				name = "netdev"
			}
			issues = append(issues, Issue{Source: "/proc/net/dev:" + name, Err: err})
			continue
		}
		if seen[name] {
			return nil, nil, fmt.Errorf("/proc/net/dev: duplicate interface %q", name)
		}
		seen[name] = true
		interfaces = append(interfaces, values)
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("/proc/net/dev: scan: %w", err)
	}
	if header != 2 {
		return nil, nil, fmt.Errorf("/proc/net/dev: missing headers")
	}
	if dataRows && len(interfaces) == 0 {
		return nil, issues, fmt.Errorf("/proc/net/dev: no valid interface rows")
	}
	sort.Slice(interfaces, func(i, j int) bool { return interfaces[i].Name < interfaces[j].Name })
	return interfaces, issues, nil
}

func parseNetDevRow(line string) (string, NetworkInterface, error) {
	separator := strings.IndexByte(line, ':')
	if separator <= 0 {
		return "", NetworkInterface{}, fmt.Errorf("missing interface separator")
	}
	name := strings.TrimSpace(line[:separator])
	if name == "" {
		return "", NetworkInterface{}, fmt.Errorf("empty interface name")
	}
	fields := strings.Fields(line[separator+1:])
	if len(fields) < 16 {
		return name, NetworkInterface{}, fmt.Errorf("short interface row")
	}
	values := make([]uint64, 16)
	for i := range values {
		value, err := strconv.ParseUint(fields[i], 10, 64)
		if err != nil {
			return name, NetworkInterface{}, fmt.Errorf("field %d: %w", i, err)
		}
		values[i] = value
	}
	return name, NetworkInterface{Name: name, ReceiveBytes: values[0], ReceivePackets: values[1], ReceiveErrors: values[2], ReceiveDropped: values[3], TransmitBytes: values[8], TransmitPackets: values[9], TransmitErrors: values[10], TransmitDropped: values[11]}, nil
}

func parseIfindex(r io.Reader) (uint32, error) {
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
		return 0, fmt.Errorf("scan: %w", err)
	}
	if token == "" {
		return 0, fmt.Errorf("ifindex: missing value")
	}
	value, err := strconv.ParseUint(token, 10, 32)
	if err != nil || value == 0 {
		return 0, fmt.Errorf("ifindex: invalid value")
	}
	return uint32(value), nil
}

func readNetwork(r io.Reader, indexFunc func(string) (uint32, error), at time.Time) ([]NetworkInterface, []Issue, error) {
	interfaces, issues, err := parseNetDev(r)
	if err != nil {
		return nil, nil, err
	}
	for i := range interfaces {
		index, err := indexFunc(interfaces[i].Name)
		if err != nil {
			issues = append(issues, Issue{Source: "/sys/class/net/" + interfaces[i].Name + "/ifindex", Err: err})
			continue
		}
		interfaces[i].Index = index
	}
	return interfaces, issues, nil
}

type networkState struct {
	at    time.Time
	iface NetworkInterface
}

func networkRates(previous, current NetworkInterface, elapsed time.Duration) NetworkRates {
	if elapsed <= 0 || current.ReceiveBytes < previous.ReceiveBytes || current.ReceivePackets < previous.ReceivePackets || current.TransmitBytes < previous.TransmitBytes || current.TransmitPackets < previous.TransmitPackets {
		return NetworkRates{}
	}
	seconds := elapsed.Seconds()
	rates := NetworkRates{
		ReceiveBytesPerSecond:    float64(current.ReceiveBytes-previous.ReceiveBytes) / seconds,
		TransmitBytesPerSecond:   float64(current.TransmitBytes-previous.TransmitBytes) / seconds,
		ReceivePacketsPerSecond:  float64(current.ReceivePackets-previous.ReceivePackets) / seconds,
		TransmitPacketsPerSecond: float64(current.TransmitPackets-previous.TransmitPackets) / seconds,
		Valid:                    true,
	}
	if math.IsNaN(rates.ReceiveBytesPerSecond) || math.IsInf(rates.ReceiveBytesPerSecond, 0) || math.IsNaN(rates.TransmitBytesPerSecond) || math.IsInf(rates.TransmitBytesPerSecond, 0) || math.IsNaN(rates.ReceivePacketsPerSecond) || math.IsInf(rates.ReceivePacketsPerSecond, 0) || math.IsNaN(rates.TransmitPacketsPerSecond) || math.IsInf(rates.TransmitPacketsPerSecond, 0) {
		return NetworkRates{}
	}
	return rates
}

func (s *NetworkSampler) sampleNetwork(interfaces []NetworkInterface, issues []Issue, at time.Time) NetworkSnapshot {
	snapshot := NetworkSnapshot{CollectedAt: at, Interfaces: make([]NetworkInterface, 0, len(interfaces)), Issues: append([]Issue(nil), issues...)}
	elapsed := time.Duration(0)
	if !s.previousAt.IsZero() {
		elapsed = at.Sub(s.previousAt)
	}
	next := make(map[uint32]networkState, len(interfaces))
	for _, iface := range interfaces {
		if iface.Index != 0 {
			if previous, ok := s.previous[iface.Index]; ok && elapsed > 0 {
				iface.Rates = networkRates(previous.iface, iface, elapsed)
			}
			if s.previousAt.IsZero() || elapsed > 0 {
				next[iface.Index] = networkState{at: at, iface: iface}
			} else if previous, ok := s.previous[iface.Index]; ok {
				next[iface.Index] = previous
			}
		}
		snapshot.Interfaces = append(snapshot.Interfaces, iface)
	}
	if s.previousAt.IsZero() || elapsed > 0 {
		s.previousAt = at
	}
	s.previous = next
	sort.Slice(snapshot.Interfaces, func(i, j int) bool {
		if snapshot.Interfaces[i].Name != snapshot.Interfaces[j].Name {
			return snapshot.Interfaces[i].Name < snapshot.Interfaces[j].Name
		}
		return snapshot.Interfaces[i].Index < snapshot.Interfaces[j].Index
	})
	return snapshot
}

// Sample returns one network-interface snapshot. The sampler is not safe for concurrent use.
func (s *NetworkSampler) Sample() (NetworkSnapshot, error) {
	at := time.Now()
	file, err := os.Open("/proc/net/dev")
	if err != nil {
		return NetworkSnapshot{}, fmt.Errorf("/proc/net/dev: %w", err)
	}
	defer file.Close()
	interfaces, issues, err := readNetwork(file, func(name string) (uint32, error) {
		path := "/sys/class/net/" + name + "/ifindex"
		indexFile, err := os.Open(path)
		if err != nil {
			return 0, err
		}
		defer indexFile.Close()
		return parseIfindex(indexFile)
	}, at)
	if err != nil {
		return NetworkSnapshot{}, err
	}
	return s.sampleNetwork(interfaces, issues, at), nil
}
