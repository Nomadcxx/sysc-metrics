//go:build linux

package metrics

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const netDevHeader = "Inter-|   Receive                                                |  Transmit\n face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed\n"

func netDevRow(name string, receive, receivePackets, receiveErrors, receiveDropped, transmit, transmitPackets, transmitErrors, transmitDropped uint64) string {
	return name + ":" + " " + strings.Repeat("0 ", 0) +
		strings.TrimSpace(strings.Join([]string{
			toString(receive), toString(receivePackets), toString(receiveErrors), toString(receiveDropped), "0", "0", "0", "0",
			toString(transmit), toString(transmitPackets), toString(transmitErrors), toString(transmitDropped), "0", "0", "0", "0",
		}, " ")) + "\n"
}

func toString(value uint64) string {
	return fmt.Sprintf("%d", value)
}

func TestParseNetDev(t *testing.T) {
	input := netDevHeader + netDevRow("eth0", 100, 10, 1, 2, 200, 20, 3, 4) + netDevRow("lo", 5, 6, 0, 0, 7, 8, 0, 0)
	interfaces, issues, err := parseNetDev(strings.NewReader(input))
	if err != nil || len(issues) != 0 || len(interfaces) != 2 {
		t.Fatalf("parseNetDev() = %#v, %#v, %v", interfaces, issues, err)
	}
	if interfaces[0].Name != "eth0" || interfaces[0].ReceiveBytes != 100 || interfaces[0].ReceivePackets != 10 || interfaces[0].TransmitBytes != 200 || interfaces[0].TransmitDropped != 4 {
		t.Fatalf("parsed interface = %#v", interfaces[0])
	}
	if _, _, err := parseNetDev(strings.NewReader(netDevHeader + netDevRow("eth0", 1, 1, 0, 0, 1, 1, 0, 0) + netDevRow("eth0", 2, 2, 0, 0, 2, 2, 0, 0))); err == nil {
		t.Fatal("duplicate interface unexpectedly succeeded")
	}
}

func TestParseNetDevMalformedAndEmpty(t *testing.T) {
	if interfaces, issues, err := parseNetDev(strings.NewReader(netDevHeader)); err != nil || len(interfaces) != 0 || len(issues) != 0 {
		t.Fatalf("empty netdev = %#v, %#v, %v", interfaces, issues, err)
	}
	valid := netDevRow("eth0", 1, 1, 0, 0, 1, 1, 0, 0)
	interfaces, issues, err := parseNetDev(strings.NewReader(netDevHeader + valid + "bad: 1 nope\n"))
	if err != nil || len(interfaces) != 1 || len(issues) != 1 || !strings.Contains(issues[0].Source, "bad") {
		t.Fatalf("partial netdev = %#v, %#v, %v", interfaces, issues, err)
	}
	for _, input := range []string{"", "bad header\n", netDevHeader + "eth0: 1 nope\n", netDevHeader + strings.Repeat("x", maxScannerToken+1) + "\n"} {
		if _, _, err := parseNetDev(strings.NewReader(input)); err == nil {
			t.Errorf("parseNetDev(%q) unexpectedly succeeded", input[:min(len(input), 20)])
		}
	}
}

func TestParseIfindex(t *testing.T) {
	if got, err := parseIfindex(strings.NewReader("12\n")); err != nil || got != 12 {
		t.Fatalf("parseIfindex() = %d, %v", got, err)
	}
	for _, input := range []string{"", "0\n", "nope\n", "4294967296\n"} {
		if _, err := parseIfindex(strings.NewReader(input)); err == nil {
			t.Errorf("parseIfindex(%q) unexpectedly succeeded", input)
		}
	}
}

func TestReadNetworkPreservesCountersWhenIfindexFails(t *testing.T) {
	interfaces, issues, err := readNetwork(strings.NewReader(netDevHeader+netDevRow("eth0", 10, 2, 1, 3, 20, 4, 5, 6)), func(string) (uint32, error) {
		return 0, fmt.Errorf("gone")
	}, time.Unix(1, 0))
	if err != nil || len(interfaces) != 1 || interfaces[0].ReceiveBytes != 10 || interfaces[0].Index != 0 || len(issues) != 1 {
		t.Fatalf("readNetwork() = %#v, %#v, %v", interfaces, issues, err)
	}
}

func TestNetworkSamplerRatesAndIdentity(t *testing.T) {
	sampler := NewNetworkSampler()
	iface := func(name string, index uint32, receive, receivePackets, transmit, transmitPackets uint64) NetworkInterface {
		return NetworkInterface{Name: name, Index: index, ReceiveBytes: receive, ReceivePackets: receivePackets, TransmitBytes: transmit, TransmitPackets: transmitPackets}
	}
	at := time.Unix(1, 0)
	first := sampler.sampleNetwork([]NetworkInterface{iface("eth0", 3, 100, 10, 200, 20)}, nil, at)
	if first.Interfaces[0].Rates.Valid {
		t.Fatal("first network sample has valid rates")
	}
	second := sampler.sampleNetwork([]NetworkInterface{iface("eth0", 3, 300, 14, 600, 26)}, nil, at.Add(2*time.Second))
	rates := second.Interfaces[0].Rates
	if !rates.Valid || rates.ReceiveBytesPerSecond != 100 || rates.ReceivePacketsPerSecond != 2 || rates.TransmitBytesPerSecond != 200 || rates.TransmitPacketsPerSecond != 3 {
		t.Fatalf("network rates = %#v", rates)
	}
	zero := sampler.sampleNetwork([]NetworkInterface{iface("renamed", 3, 300, 14, 600, 26)}, nil, at.Add(3*time.Second))
	if !zero.Interfaces[0].Rates.Valid || zero.Interfaces[0].Rates.ReceiveBytesPerSecond != 0 {
		t.Fatalf("zero network rates = %#v", zero.Interfaces[0].Rates)
	}
	newIndex := sampler.sampleNetwork([]NetworkInterface{iface("renamed", 4, 400, 15, 700, 27)}, nil, at.Add(4*time.Second))
	if newIndex.Interfaces[0].Rates.Valid {
		t.Fatal("new ifindex retained an old baseline")
	}
	missing := NetworkInterface{Name: "missing", ReceiveBytes: 1}
	got := sampler.sampleNetwork([]NetworkInterface{missing}, nil, at.Add(5*time.Second))
	if got.Interfaces[0].Rates.Valid || got.Interfaces[0].Index != 0 {
		t.Fatalf("missing ifindex = %#v", got.Interfaces[0])
	}
}
