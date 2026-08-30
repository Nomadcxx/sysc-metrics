//go:build linux

package metrics

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const powerSupplyRoot = "/sys/class/power_supply"

type supplyReading struct {
	name      string
	kind      string
	status    string
	charge    float64
	hasCharge bool
	energyJ   float64
	hasEnergy bool
	watts     float64
	hasWatts  bool
}

func readBattery(root string) (BatterySnapshot, error) {
	now := time.Now()
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return BatterySnapshot{CollectedAt: now}, nil
		}
		return BatterySnapshot{}, err
	}

	var (
		supplies []supplyReading
		issues   []Issue
	)
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		reading, err := readSupply(path)
		if err != nil {
			issues = append(issues, Issue{Source: path, Err: err})
			continue
		}
		if reading.kind != "Battery" && reading.kind != "UPS" {
			continue
		}
		supplies = append(supplies, reading)
	}
	if len(supplies) == 0 {
		return BatterySnapshot{CollectedAt: now, Issues: issues}, nil
	}

	snap := aggregateSupplies(now, supplies)
	snap.Issues = issues
	return snap, nil
}

func readSupply(dir string) (supplyReading, error) {
	kind, err := readSysfsString(filepath.Join(dir, "type"))
	if err != nil {
		return supplyReading{}, err
	}
	out := supplyReading{name: filepath.Base(dir), kind: kind}
	if status, err := readSysfsString(filepath.Join(dir, "status")); err == nil {
		out.status = status
	}

	if energyNow, err := readSysfsUint(filepath.Join(dir, "energy_now")); err == nil {
		if energyFull, err := readSysfsUint(filepath.Join(dir, "energy_full")); err == nil && energyFull > 0 {
			out.charge = float64(energyNow) / float64(energyFull)
			out.hasCharge = true
		}
		// energy_* is microWatt-hours. 1 µWh = 0.0036 J.
		out.energyJ = float64(energyNow) * 0.0036
		out.hasEnergy = true
	} else if chargeNow, err := readSysfsUint(filepath.Join(dir, "charge_now")); err == nil {
		if chargeFull, err := readSysfsUint(filepath.Join(dir, "charge_full")); err == nil && chargeFull > 0 {
			out.charge = float64(chargeNow) / float64(chargeFull)
			out.hasCharge = true
		}
	}
	if !out.hasCharge {
		if capacity, err := readSysfsUint(filepath.Join(dir, "capacity")); err == nil {
			out.charge = float64(capacity) / 100
			out.hasCharge = true
		}
	}
	if powerNow, err := readSysfsUint(filepath.Join(dir, "power_now")); err == nil {
		out.watts = float64(powerNow) / 1e6
		out.hasWatts = true
	} else if currentNow, err := readSysfsUint(filepath.Join(dir, "current_now")); err == nil {
		if voltageNow, err := readSysfsUint(filepath.Join(dir, "voltage_now")); err == nil {
			// µA * µV / 1e12 = W
			out.watts = float64(currentNow) * float64(voltageNow) / 1e12
			out.hasWatts = true
		}
	}
	return out, nil
}

func aggregateSupplies(now time.Time, supplies []supplyReading) BatterySnapshot {
	snap := BatterySnapshot{CollectedAt: now, Present: true}
	var energy, energyCharged, watts float64
	var nCharge, nWatts int
	var sawCharging, sawDischarging, sawFull, sawUnknown bool

	for _, s := range supplies {
		if s.hasCharge {
			if s.hasEnergy {
				energyCharged += s.charge * s.energyJ
				energy += s.energyJ
			} else {
				snap.Charge += s.charge
				nCharge++
			}
		}
		if s.hasEnergy {
			snap.EnergyJoules += s.energyJ
		}
		if s.hasWatts {
			watts += s.watts
			nWatts++
		}
		switch strings.ToLower(s.status) {
		case "charging":
			sawCharging = true
		case "discharging":
			sawDischarging = true
		case "full":
			sawFull = true
		default:
			sawUnknown = true
		}
	}

	switch {
	case sawCharging:
		snap.State = BatteryCharging
	case sawDischarging:
		snap.State = BatteryDischarging
	case sawFull && !sawUnknown:
		snap.State = BatteryFull
	default:
		snap.State = BatteryUnknown
	}

	if energy > 0 {
		snap.Charge = energyCharged / energy
		snap.ChargeValid = true
	} else if nCharge > 0 {
		snap.Charge /= float64(nCharge)
		snap.ChargeValid = true
	}
	if snap.Charge < 0 {
		snap.Charge = 0
	}
	if snap.Charge > 1 {
		snap.Charge = 1
	}
	if nWatts > 0 {
		snap.RateWatts = watts
		snap.RateValid = true
	}
	if snap.State == BatteryDischarging && snap.RateValid && snap.RateWatts > 0 && snap.EnergyJoules > 0 {
		hours := snap.EnergyJoules / (snap.RateWatts * 3600)
		snap.TimeRemaining = time.Duration(hours * float64(time.Hour))
		snap.TimeValid = true
	}
	return snap
}

func readSysfsString(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(data) > 4096 {
		return "", fmt.Errorf("sysfs: %s exceeds 4096 bytes", path)
	}
	return string(bytes.TrimSpace(data)), nil
}

func readSysfsUint(path string) (uint64, error) {
	s, err := readSysfsString(path)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("sysfs: %s: %w", path, err)
	}
	return v, nil
}
