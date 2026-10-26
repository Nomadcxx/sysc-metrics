//go:build linux

package metrics

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestReadBatteryNegativePowerNowStillReportsRateAndETA(t *testing.T) {
	root := t.TempDir()
	writeSupply(t, root, "BAT0", map[string]string{
		"type": "Battery", "status": "Discharging",
		"energy_now": "40000000", "energy_full": "80000000",
		"power_now": "-20000000",
	})
	snap, err := readBattery(root)
	if err != nil {
		t.Fatal(err)
	}
	// energy_* SOC is independent of the power sign.
	if !snap.ChargeValid || math.Abs(snap.Charge-0.5) > 1e-9 {
		t.Fatalf("charge = %v valid = %v, want 0.5", snap.Charge, snap.ChargeValid)
	}
	// -20 W magnitude; unsigned ParseUint dropped the rate and the ETA (issue #22).
	if !snap.RateValid || math.Abs(snap.RateWatts-20) > 1e-9 {
		t.Fatalf("rate = %v valid = %v, want 20 W", snap.RateWatts, snap.RateValid)
	}
	// 144 kJ / 20 W = 2 h.
	if !snap.TimeValid || snap.TimeRemaining != 2*time.Hour {
		t.Fatalf("time remaining = %v valid = %v, want 2h", snap.TimeRemaining, snap.TimeValid)
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

func TestReadBatterySOCWeightsByChargeFull(t *testing.T) {
	root := t.TempDir()
	writeSupply(t, root, "BAT0", map[string]string{
		"type": "Battery", "status": "Discharging",
		"charge_now": "90000", "charge_full": "100000",
	})
	writeSupply(t, root, "BAT1", map[string]string{
		"type": "Battery", "status": "Discharging",
		"charge_now": "5000", "charge_full": "10000",
	})
	snap, err := readBattery(root)
	if err != nil {
		t.Fatal(err)
	}
	// Σ now / Σ full = 95000/110000 ≈ 0.8636; averaging ratios gave 0.7 (issue #14).
	want := 95000.0 / 110000.0
	if !snap.ChargeValid || math.Abs(snap.Charge-want) > 1e-9 {
		t.Fatalf("charge = %v valid = %v, want %v", snap.Charge, snap.ChargeValid, want)
	}
}

func TestReadBatteryExcludesDeviceScopeAndAbsent(t *testing.T) {
	root := t.TempDir()
	writeSupply(t, root, "BAT0", map[string]string{
		"type": "Battery", "status": "Discharging", "scope": "System", "present": "1",
		"energy_now": "50000000", "energy_full": "100000000",
	})
	writeSupply(t, root, "hid-mouse", map[string]string{
		"type": "Battery", "status": "Discharging", "scope": "Device",
		"capacity": "10",
	})
	writeSupply(t, root, "BAT1", map[string]string{
		"type": "Battery", "status": "Full", "present": "0",
		"capacity": "100",
	})
	snap, err := readBattery(root)
	if err != nil {
		t.Fatal(err)
	}
	// Only BAT0 aggregates; mouse pack and empty bay stay out (issue #15).
	if !snap.ChargeValid || math.Abs(snap.Charge-0.5) > 1e-9 {
		t.Fatalf("charge = %v valid = %v, want 0.5 from BAT0 only", snap.Charge, snap.ChargeValid)
	}
	if snap.State != BatteryDischarging {
		t.Fatalf("state = %v, want Discharging (BAT1 present=0 excluded)", snap.State)
	}
}

func TestReadBatteryOnlyDeviceScopeIsNotPresent(t *testing.T) {
	root := t.TempDir()
	writeSupply(t, root, "hid-keyboard", map[string]string{
		"type": "Battery", "status": "Discharging", "scope": "device", "capacity": "55",
	})
	snap, err := readBattery(root)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Present || snap.ChargeValid {
		t.Fatalf("Device-scope supply treated as system battery: %#v", snap)
	}
}

func TestReadBatteryChargeSOCWhenEnergyFullUnusable(t *testing.T) {
	root := t.TempDir()
	// energy_now present but energy_full missing: charge_* must still
	// give SOC; the old else-if chain fell through to no SOC at all
	// (issue #17).
	writeSupply(t, root, "BAT0", map[string]string{
		"type": "Battery", "status": "Discharging",
		"energy_now": "40000000",
		"charge_now": "30000", "charge_full": "40000",
	})
	snap, err := readBattery(root)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.ChargeValid || math.Abs(snap.Charge-0.75) > 1e-9 {
		t.Fatalf("charge = %v valid = %v, want 0.75 from charge ratio", snap.Charge, snap.ChargeValid)
	}
	if want := 40000000 * 0.0036; math.Abs(snap.EnergyJoules-want) > 1e-9 {
		t.Fatalf("energy = %v, want %v kept from energy_now", snap.EnergyJoules, want)
	}
}

func TestBatteryChargeOnlySupplyDerivesEnergyAndETA(t *testing.T) {
	root := t.TempDir()
	writeSupply(t, root, "BAT0", map[string]string{
		"type":        "Battery",
		"status":      "Discharging",
		"charge_now":  "2000000",
		"charge_full": "4000000",
		"voltage_now": "12000000",
		"current_now": "-1000000",
	})

	snap, err := readBattery(root)
	if err != nil {
		t.Fatal(err)
	}
	// 2e6 µAh × 1.2e7 µV / 1e12 = 24 Wh = 86400 J; drain 12 W → 2 h.
	if want := 86400.0; math.Abs(snap.EnergyJoules-want) > 1e-9 {
		t.Fatalf("EnergyJoules = %v, want %v derived from charge_now × voltage_now", snap.EnergyJoules, want)
	}
	if !snap.TimeValid || snap.TimeRemaining != 2*time.Hour {
		t.Fatalf("time remaining = %v valid = %v, want 2h", snap.TimeRemaining, snap.TimeValid)
	}
}

func TestBatteryMixedFleetETAUsesEnergyAlignedWatts(t *testing.T) {
	root := t.TempDir()
	writeSupply(t, root, "BAT0", map[string]string{
		"type":        "Battery",
		"status":      "Discharging",
		"energy_now":  "3600000",
		"energy_full": "7200000",
		"power_now":   "6000000",
	})
	writeSupply(t, root, "BAT1", map[string]string{
		"type":      "Battery",
		"status":    "Discharging",
		"power_now": "6000000",
	})

	snap, err := readBattery(root)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.RateValid || math.Abs(snap.RateWatts-12) > 1e-9 {
		t.Fatalf("RateWatts = %v valid = %v, want the full 12 W", snap.RateWatts, snap.RateValid)
	}
	// 12960 J of metered energy against the 6 W that pack reports:
	// 36 min. The fleet-wide 12 W would claim 18 min for energy the
	// watts-only pack never accounted (issue #31).
	if !snap.TimeValid || snap.TimeRemaining != 36*time.Minute {
		t.Fatalf("time remaining = %v valid = %v, want 36m", snap.TimeRemaining, snap.TimeValid)
	}
}

func TestReadBatteryReportsEachDeviceSeparately(t *testing.T) {
	root := t.TempDir()
	writeSupply(t, root, "BAT0", map[string]string{
		"type": "Battery", "status": "Discharging",
		"energy_now": "40000000", "energy_full": "80000000",
		"power_now": "20000000",
	})
	writeSupply(t, root, "BAT1", map[string]string{
		"type": "Battery", "status": "Charging", "capacity": "40",
	})

	snap, err := readBattery(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Devices) != 2 {
		t.Fatalf("devices = %#v, want one entry per pack", snap.Devices)
	}
	if snap.Devices[0].Name != "BAT0" || snap.Devices[1].Name != "BAT1" {
		t.Fatalf("device names = %q, %q, want sysfs order BAT0, BAT1",
			snap.Devices[0].Name, snap.Devices[1].Name)
	}
	bat0, bat1 := snap.Devices[0], snap.Devices[1]
	if bat0.Kind != "Battery" || bat1.Kind != "Battery" {
		t.Fatalf("kinds = %q, %q, want Battery", bat0.Kind, bat1.Kind)
	}
	if !bat0.ChargeValid || bat0.Charge != 0.5 || bat0.State != BatteryDischarging {
		t.Fatalf("BAT0 = %#v, want charge 0.5 discharging", bat0)
	}
	if !bat0.RateValid || bat0.RateWatts != 20 {
		t.Fatalf("BAT0 rate = %v valid=%v, want 20 W", bat0.RateWatts, bat0.RateValid)
	}
	if !bat1.ChargeValid || bat1.Charge != 0.4 || bat1.State != BatteryCharging {
		t.Fatalf("BAT1 = %#v, want charge 0.4 charging", bat1)
	}
	// The catalog must not move the aggregate (issue #708).
	if !snap.Present || !snap.ChargeValid || snap.Charge != 0.5 {
		t.Fatalf("aggregate charge snapshot = %#v, want 0.5", snap)
	}
	if snap.State != BatteryCharging {
		t.Fatalf("aggregate state = %v, want charging", snap.State)
	}
}

func TestReadBatteryDeviceHealthFromEnergy(t *testing.T) {
	root := t.TempDir()
	writeSupply(t, root, "BAT0", map[string]string{
		"type": "Battery", "status": "Discharging",
		"energy_now":         "36000000",
		"energy_full":        "45000000",
		"energy_full_design": "60000000",
	})

	snap, err := readBattery(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Devices) != 1 {
		t.Fatalf("devices = %#v, want one", snap.Devices)
	}
	dev := snap.Devices[0]
	// 45 Wh of 60 Wh design: health 0.75. µWh × 0.0036 = J.
	if !dev.EnergyFullValid || !dev.HealthValid {
		t.Fatalf("device = %#v, want EnergyFullValid and HealthValid", dev)
	}
	if want := 45000000 * 0.0036; math.Abs(dev.EnergyFull-want) > 1e-9 {
		t.Fatalf("EnergyFull = %v, want %v", dev.EnergyFull, want)
	}
	if want := 60000000 * 0.0036; math.Abs(dev.EnergyFullDesign-want) > 1e-9 {
		t.Fatalf("EnergyFullDesign = %v, want %v", dev.EnergyFullDesign, want)
	}
	if math.Abs(dev.Health-0.75) > 1e-9 {
		t.Fatalf("Health = %v, want 0.75", dev.Health)
	}
}

func TestReadBatteryDeviceHealthMissingDesignStaysInvalid(t *testing.T) {
	root := t.TempDir()
	// energy_full_design is absent on many packs, and the charge_*
	// factory counter must not be mixed into the energy_* family
	// (issue #708).
	writeSupply(t, root, "BAT0", map[string]string{
		"type": "Battery", "status": "Discharging",
		"energy_now":         "36000000",
		"energy_full":        "45000000",
		"charge_now":         "3000000",
		"charge_full":        "4000000",
		"charge_full_design": "5000000",
		"voltage_now":        "12000000",
	})

	snap, err := readBattery(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Devices) != 1 {
		t.Fatalf("devices = %#v, want one", snap.Devices)
	}
	dev := snap.Devices[0]
	if dev.EnergyFullValid || dev.HealthValid {
		t.Fatalf("device = %#v, want invalid health without energy_full_design", dev)
	}
	if dev.Health != 0 {
		t.Fatalf("Health = %v, want 0 when invalid", dev.Health)
	}
	if !dev.ChargeValid || dev.Charge != 36000000.0/45000000.0 {
		t.Fatalf("Charge = %v valid=%v, want the energy-path SOC", dev.Charge, dev.ChargeValid)
	}
}

func TestReadBatteryDeviceHealthFromChargePack(t *testing.T) {
	root := t.TempDir()
	writeSupply(t, root, "BAT0", map[string]string{
		"type":        "Battery",
		"status":      "Discharging",
		"charge_now":  "1500000",
		"charge_full": "3000000",
		// 3000000 µAh × 12 V = 36 Wh of 48 Wh design → 0.75.
		"charge_full_design": "4000000",
		"voltage_now":        "12000000",
		"current_now":        "-1000000",
	})

	snap, err := readBattery(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Devices) != 1 {
		t.Fatalf("devices = %#v, want one", snap.Devices)
	}
	dev := snap.Devices[0]
	if !dev.EnergyFullValid || !dev.HealthValid {
		t.Fatalf("device = %#v, want EnergyFullValid and HealthValid via charge counters", dev)
	}
	if want := 3000000.0 * 12000000 / 1e12 * 3600; math.Abs(dev.EnergyFull-want) > 1e-9 {
		t.Fatalf("EnergyFull = %v, want %v derived from charge_full × voltage", dev.EnergyFull, want)
	}
	if want := 4000000.0 * 12000000 / 1e12 * 3600; math.Abs(dev.EnergyFullDesign-want) > 1e-9 {
		t.Fatalf("EnergyFullDesign = %v, want %v", dev.EnergyFullDesign, want)
	}
	if math.Abs(dev.Health-0.75) > 1e-9 {
		t.Fatalf("Health = %v, want 0.75", dev.Health)
	}
}

func TestReadBatteryDeviceHealthWithoutVoltageStaysInvalid(t *testing.T) {
	root := t.TempDir()
	// A charge_* pack with no voltage_now cannot be expressed in
	// joules; health stays invalid rather than dividing µAh into µWh
	// (issue #708).
	writeSupply(t, root, "BAT0", map[string]string{
		"type":               "Battery",
		"status":             "Discharging",
		"charge_now":         "1500000",
		"charge_full":        "3000000",
		"charge_full_design": "4000000",
	})

	snap, err := readBattery(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Devices) != 1 {
		t.Fatalf("devices = %#v, want one", snap.Devices)
	}
	dev := snap.Devices[0]
	if dev.EnergyFullValid || dev.HealthValid {
		t.Fatalf("device = %#v, want invalid health without voltage_now", dev)
	}
	if !dev.ChargeValid || dev.Charge != 0.5 {
		t.Fatalf("Charge = %v valid=%v, want 0.5", dev.Charge, dev.ChargeValid)
	}
}
