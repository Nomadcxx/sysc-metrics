//go:build linux

package metrics

import (
	"bytes"
	"fmt"
	"math"
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
	// chargeFromEnergy records that charge came from energy_now/energy_full
	// rather than a capacity fallback; SOC is only aggregated across
	// same-unit supplies (issue #6).
	chargeFromEnergy bool
	// raw µAh counters behind a charge_* SOC, kept for weighted
	// multi-pack aggregation (issue #14).
	chargeAh      float64
	chargeFullAh  float64
	hasChargeFull bool
	energyJ       float64
	hasEnergy     bool
	energyFullJ   float64
	hasEnergyFull bool
	watts         float64
	hasWatts      bool
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
		if !systemScopeSupply(path) {
			continue
		}
		supplies = append(supplies, reading)
	}
	if len(supplies) == 0 {
		return BatterySnapshot{CollectedAt: now, Issues: issues}, nil
	}

	snap := aggregateSupplies(now, supplies, &issues)
	snap.Issues = issues
	return snap, nil
}

// systemScopeSupply reports whether a power_supply node counts toward the
// whole-system battery. Per sysfs-class-power: missing present ⇒ present,
// present=0 ⇒ absent; missing scope ⇒ System, scope=Device ⇒ peripheral
// (HID mouse etc.) and excluded (issue #15).
func systemScopeSupply(dir string) bool {
	if present, err := readSysfsString(filepath.Join(dir, "present")); err == nil && present == "0" {
		return false
	}
	if scope, err := readSysfsString(filepath.Join(dir, "scope")); err == nil && strings.EqualFold(scope, "Device") {
		return false
	}
	return true
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

	// energy_* is microWatt-hours. 1 µWh = 0.0036 J.
	if energyNow, err := readSysfsUint(filepath.Join(dir, "energy_now")); err == nil {
		out.energyJ = float64(energyNow) * 0.0036
		out.hasEnergy = true
		if energyFull, err := readSysfsUint(filepath.Join(dir, "energy_full")); err == nil && energyFull > 0 {
			out.energyFullJ = float64(energyFull) * 0.0036
			out.hasEnergyFull = true
			out.charge = float64(energyNow) / float64(energyFull)
			out.chargeFromEnergy = true
			out.hasCharge = true
		}
	}
	// charge_* is tried whenever the energy path did not establish SOC —
	// an earlier else-if skipped it when energy_now existed but
	// energy_full was missing/0, despite valid µAh counters (issue #17).
	// Energy Joules from energy_now are kept regardless.
	if !out.hasCharge {
		if chargeNow, err := readSysfsUint(filepath.Join(dir, "charge_now")); err == nil {
			if chargeFull, err := readSysfsUint(filepath.Join(dir, "charge_full")); err == nil && chargeFull > 0 {
				out.charge = float64(chargeNow) / float64(chargeFull)
				out.chargeAh = float64(chargeNow)
				out.chargeFullAh = float64(chargeFull)
				out.hasChargeFull = true
				out.hasCharge = true
			}
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
	} else if currentNow, err := readSysfsInt(filepath.Join(dir, "current_now")); err == nil {
		if voltageNow, err := readSysfsUint(filepath.Join(dir, "voltage_now")); err == nil {
			// µA * µV / 1e12 = W. current_now is signed (negative while
			// discharging, per sysfs-class-power ABI); the rate is a
			// magnitude either way (issue #13).
			out.watts = math.Abs(float64(currentNow)) * float64(voltageNow) / 1e12
			out.hasWatts = true
		}
	}
	return out, nil
}

func aggregateSupplies(now time.Time, supplies []supplyReading, issues *[]Issue) BatterySnapshot {
	snap := BatterySnapshot{CollectedAt: now, Present: true}
	var (
		energyNowSum   float64
		energyFullSum  float64
		capacityCharge float64
		chargeNowSum   float64
		chargeFullSum  float64
		watts          float64
	)
	var nEnergy, nCapacity, nChargeFull, nWatts int
	var sawCharging, sawDischarging, sawFull, sawUnknown bool

	for _, s := range supplies {
		switch {
		case s.chargeFromEnergy:
			energyNowSum += s.energyJ
			energyFullSum += s.energyFullJ
			nEnergy++
		case s.hasCharge:
			capacityCharge += s.charge
			nCapacity++
			if s.hasChargeFull {
				chargeNowSum += s.chargeAh
				chargeFullSum += s.chargeFullAh
				nChargeFull++
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
		case "full", "not charging", "idle":
			// "Not charging"/"idle" is the steady AC state of a
			// charge-threshold battery; it must not poison the Full
			// aggregation the way an unknown word does (issue #6).
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

	if nEnergy > 0 {
		// Multi-pack SOC by capacity (Σ energy_now / Σ energy_full):
		// weighting by energy_now alone let one nearly-empty pack drag
		// the fleet percentage down (issue #10).
		snap.Charge = energyNowSum / energyFullSum
		snap.ChargeValid = true
		if nCapacity > 0 {
			// Mixed units are surfaced, never silently dropped (issue #6).
			*issues = append(*issues, Issue{
				Source: "power_supply",
				Err: fmt.Errorf("mixed energy/capacity supplies: SOC aggregated %d energy-based supply(s), dropped %d capacity-only supply(s)",
					nEnergy, nCapacity),
			})
		}
	} else if nCapacity > 0 {
		// Capacity-path SOC: when every supply exposed µAh counters,
		// weight by capacity (Σ charge_now / Σ charge_full) like the
		// energy path; a plain mean of ratios mis-sizes unequal packs
		// (issue #14). Percentage-only packs keep the mean.
		if nChargeFull == nCapacity && chargeFullSum > 0 {
			snap.Charge = chargeNowSum / chargeFullSum
		} else {
			snap.Charge = capacityCharge / float64(nCapacity)
		}
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
		// A pathological rate (e.g. power_now=1µW) overflows Duration and
		// wraps negative with TimeValid true; implausible ETAs stay
		// invalid (issue #11).
		if !math.IsNaN(hours) && !math.IsInf(hours, 0) && hours > 0 &&
			hours <= float64(math.MaxInt64)/float64(time.Hour) {
			if remaining := time.Duration(hours * float64(time.Hour)); remaining > 0 {
				snap.TimeRemaining = remaining
				snap.TimeValid = true
			}
		}
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

// readSysfsInt (shared, thermal_linux.go) parses signed sysfs values such
// as current_now.
