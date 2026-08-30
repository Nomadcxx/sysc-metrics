//go:build linux

package metrics

import (
	"os"
	"path/filepath"
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
