//go:build linux

package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	hwmonRoot       = "/sys/class/hwmon"
	thermalZoneRoot = "/sys/class/thermal"
)

func readThermal(hwmonRoot, thermalRoot string) (ThermalSnapshot, error) {
	now := time.Now()
	var issues []Issue
	if snap, ok := pickKnownHwmon(hwmonRoot, &issues); ok {
		snap.CollectedAt = now
		snap.Issues = issues
		return snap, nil
	}
	if snap, ok := pickThermalZone(thermalRoot, &issues); ok {
		snap.CollectedAt = now
		snap.Issues = issues
		return snap, nil
	}
	return ThermalSnapshot{CollectedAt: now, Issues: issues}, nil
}

type thermalCandidate struct {
	driverPri int
	sensorPri int
	celsius   float64
	source    string
}

func pickKnownHwmon(root string, issues *[]Issue) (ThermalSnapshot, bool) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			*issues = append(*issues, Issue{Source: root, Err: err})
		}
		return ThermalSnapshot{}, false
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	bestOK := false
	var best thermalCandidate
	for _, name := range names {
		chip := filepath.Join(root, name)
		driver, err := readSysfsString(filepath.Join(chip, "name"))
		if err != nil {
			*issues = append(*issues, Issue{Source: chip, Err: err})
			continue
		}
		driverPri, ok := hwmonDriverPriority(driver)
		if !ok {
			continue
		}
		matches, err := filepath.Glob(filepath.Join(chip, "temp*_input"))
		if err != nil {
			*issues = append(*issues, Issue{Source: chip, Err: err})
			continue
		}
		sort.Strings(matches)
		for _, input := range matches {
			index := tempInputIndex(filepath.Base(input))
			label, _ := readSysfsString(strings.TrimSuffix(input, "_input") + "_label")
			milli, err := readSysfsInt(input)
			if err != nil {
				*issues = append(*issues, Issue{Source: input, Err: err})
				continue
			}
			cand := thermalCandidate{
				driverPri: driverPri,
				sensorPri: hwmonSensorPriority(driver, label, index),
				celsius:   float64(milli) / 1000,
				source:    formatHwmonSource(driver, label),
			}
			if !bestOK || cand.driverPri < best.driverPri || (cand.driverPri == best.driverPri && cand.sensorPri < best.sensorPri) {
				best, bestOK = cand, true
			}
		}
	}
	if !bestOK {
		return ThermalSnapshot{}, false
	}
	return ThermalSnapshot{Celsius: best.celsius, Valid: true, Source: best.source}, true
}

func pickThermalZone(root string, issues *[]Issue) (ThermalSnapshot, bool) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			*issues = append(*issues, Issue{Source: root, Err: err})
		}
		return ThermalSnapshot{}, false
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	bestOK := false
	var best thermalCandidate
	for _, name := range names {
		zone := filepath.Join(root, name)
		kind, err := readSysfsString(filepath.Join(zone, "type"))
		if err != nil {
			*issues = append(*issues, Issue{Source: zone, Err: err})
			continue
		}
		pri, ok := thermalZonePriority(kind)
		if !ok {
			continue
		}
		milli, err := readSysfsInt(filepath.Join(zone, "temp"))
		if err != nil {
			*issues = append(*issues, Issue{Source: zone, Err: err})
			continue
		}
		celsius := float64(milli) / 1000
		cand := thermalCandidate{driverPri: pri, celsius: celsius, source: kind}
		if !bestOK || cand.driverPri < best.driverPri || (cand.driverPri == best.driverPri && celsius > best.celsius) {
			best, bestOK = cand, true
		}
	}
	if !bestOK {
		return ThermalSnapshot{}, false
	}
	if best.celsius <= 0 {
		return ThermalSnapshot{}, false
	}
	return ThermalSnapshot{Celsius: best.celsius, Valid: true, Source: best.source}, true
}

func hwmonDriverPriority(name string) (int, bool) {
	switch strings.ToLower(name) {
	case "k10temp":
		return 0, true
	case "zenpower":
		return 1, true
	case "coretemp":
		return 2, true
	case "ibmpowernv":
		return 3, true
	}
	return 0, false
}

func hwmonSensorPriority(driver, label string, index int) int {
	driver = strings.ToLower(driver)
	switch driver {
	case "k10temp", "zenpower":
		switch {
		case strings.HasPrefix(label, "Tctl"):
			return 0
		case index == 1:
			return 1
		case strings.HasPrefix(label, "Tdie"):
			return 2
		case strings.HasPrefix(label, "Package id"):
			return 3
		case strings.HasPrefix(label, "SoC Temperature"):
			return 4
		case strings.HasPrefix(label, "Core") || strings.HasPrefix(label, "Tccd"):
			return 5
		}
	case "coretemp":
		switch {
		case strings.HasPrefix(label, "Package id"):
			return 0
		case index == 1:
			return 1
		case strings.HasPrefix(label, "Core"):
			return 2
		}
	}
	if index == 1 {
		return 1
	}
	return 20
}

func thermalZonePriority(kind string) (int, bool) {
	switch kind {
	case "cpu-thermal":
		return 0, true
	case "x86_pkg_temp":
		return 1, true
	case "acpitz":
		return 2, true
	}
	return 0, false
}

func tempInputIndex(base string) int {
	base = strings.TrimSuffix(strings.TrimPrefix(base, "temp"), "_input")
	n, err := strconv.Atoi(base)
	if err != nil {
		return 0
	}
	return n
}

func formatHwmonSource(driver, label string) string {
	if label == "" {
		return driver
	}
	return driver + " " + label
}

func readSysfsInt(path string) (int64, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(body)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	return n, nil
}
