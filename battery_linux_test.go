//go:build linux

package metrics

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSupply(t *testing.T, root, name string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadBatteryAbsentIsNotAnError(t *testing.T) {
	root := t.TempDir()
	writeSupply(t, root, "AC", map[string]string{"type": "Mains", "online": "1"})

	snap, err := readBattery(root)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Present {
		t.Fatalf("Present = true on a machine with only mains")
	}
}

func TestReadBatteryAggregatesChargeAndState(t *testing.T) {
	root := t.TempDir()
	writeSupply(t, root, "BAT0", map[string]string{
		"type": "Battery", "status": "Discharging",
		"energy_now": "40000000", "energy_full": "80000000",
		"power_now": "20000000",
	})

	snap, err := readBattery(root)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Present || !snap.ChargeValid || snap.Charge != 0.5 {
		t.Fatalf("charge snapshot = %#v", snap)
	}
	if snap.State != BatteryDischarging {
		t.Fatalf("state = %v, want discharging", snap.State)
	}
	if !snap.RateValid || snap.RateWatts != 20 {
		t.Fatalf("rate = %v valid=%v, want 20 W", snap.RateWatts, snap.RateValid)
	}
	if !snap.TimeValid || snap.TimeRemaining <= 0 {
		t.Fatalf("time remaining = %v valid=%v", snap.TimeRemaining, snap.TimeValid)
	}
}

func TestReadBatteryChargingWinsOverDischarging(t *testing.T) {
	root := t.TempDir()
	writeSupply(t, root, "BAT0", map[string]string{
		"type": "Battery", "status": "Charging", "capacity": "40",
	})
	writeSupply(t, root, "BAT1", map[string]string{
		"type": "Battery", "status": "Discharging", "capacity": "80",
	})

	snap, err := readBattery(root)
	if err != nil {
		t.Fatal(err)
	}
	if snap.State != BatteryCharging {
		t.Fatalf("state = %v, want charging when any pack is charging", snap.State)
	}
	if !snap.ChargeValid || snap.Charge < 0.59 || snap.Charge > 0.61 {
		t.Fatalf("mean charge = %v, want 0.6", snap.Charge)
	}
}

func TestReadBatteryMissingDirectory(t *testing.T) {
	snap, err := readBattery(filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatal(err)
	}
	if snap.Present {
		t.Fatal("a missing sysfs tree reported a battery")
	}
}

func TestReadBatterySOCWeightsByEnergyFull(t *testing.T) {
	root := t.TempDir()
	writeSupply(t, root, "BAT0", map[string]string{
		"type": "Battery", "status": "Discharging",
		"energy_now": "90000000", "energy_full": "100000000", "power_now": "10000000",
	})
	writeSupply(t, root, "BAT1", map[string]string{
		"type": "Battery", "status": "Discharging",
		"energy_now": "10000000", "energy_full": "100000000", "power_now": "10000000",
	})
	snap, err := readBattery(root)
	if err != nil {
		t.Fatal(err)
	}
	// Σ now / Σ full = 100/200 = 0.5; energy_now weighting gave 0.82 (issue #10).
	if !snap.ChargeValid || math.Abs(snap.Charge-0.5) > 1e-9 {
		t.Fatalf("charge = %v valid = %v, want 0.5", snap.Charge, snap.ChargeValid)
	}
}

func TestReadBatteryMixedUnitsAggregateAndReport(t *testing.T) {
	root := t.TempDir()
	writeSupply(t, root, "BAT0", map[string]string{
		"type": "Battery", "status": "Not charging",
		"energy_now": "50000000", "energy_full": "100000000",
	})
	writeSupply(t, root, "BAT1", map[string]string{
		"type": "Battery", "status": "Not charging", "capacity": "80",
	})
	snap, err := readBattery(root)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.ChargeValid || snap.Charge != 0.5 {
		t.Fatalf("charge = %v valid = %v, want 0.5 from the energy pack", snap.Charge, snap.ChargeValid)
	}
	var mixed bool
	for _, is := range snap.Issues {
		if strings.Contains(is.Source, "power_supply") {
			mixed = true
		}
	}
	if !mixed {
		t.Fatalf("dropped capacity-only supply not reported: %#v", snap.Issues)
	}
	// "Not charging" is a steady AC state, not an unknown that poisons Full.
	if snap.State != BatteryFull {
		t.Fatalf("state = %v, want Full", snap.State)
	}
}

func TestReadBatteryImplausibleRateKeepsTimeInvalid(t *testing.T) {
	root := t.TempDir()
	writeSupply(t, root, "BAT0", map[string]string{
		"type": "Battery", "status": "Discharging",
		"energy_now": "50000000", "energy_full": "100000000", "power_now": "1",
	})
	snap, err := readBattery(root)
	if err != nil {
		t.Fatal(err)
	}
	// 1 µW over 50 Wh is ~57 years; the Duration multiply overflowed to a
	// negative TimeRemaining that was still marked valid (issue #11).
	if snap.TimeValid {
		t.Fatalf("overflowing ETA accepted: %v valid", snap.TimeRemaining)
	}
}

func TestReadBatteryNegativeCurrentNowStillReportsRate(t *testing.T) {
	root := t.TempDir()
	writeSupply(t, root, "BAT0", map[string]string{
		"type": "Battery", "status": "Discharging",
		"charge_now": "50000000", "charge_full": "100000000",
		"current_now": "-2000000", "voltage_now": "11000000",
	})
	snap, err := readBattery(root)
	if err != nil {
		t.Fatal(err)
	}
	// -2 A * 11 V = 22 W magnitude; unsigned parse lost the rate (issue #13).
	if !snap.RateValid || math.Abs(snap.RateWatts-22) > 1e-9 {
		t.Fatalf("rate = %v valid = %v, want 22 W", snap.RateWatts, snap.RateValid)
	}
}
