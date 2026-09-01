//go:build linux

package metrics

import (
	"os"
	"path/filepath"
	"testing"
)

func writeHwmon(t *testing.T, root, dir, name string, files map[string]string) {
	t.Helper()
	chip := filepath.Join(root, dir)
	if err := os.MkdirAll(chip, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chip, "name"), []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for file, body := range files {
		if err := os.WriteFile(filepath.Join(chip, file), []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestThermalPrefersK10TctlOverCoretemp(t *testing.T) {
	root := t.TempDir()
	writeHwmon(t, root, "hwmon0", "coretemp", map[string]string{
		"temp1_label": "Package id 0", "temp1_input": "80000",
	})
	writeHwmon(t, root, "hwmon1", "k10temp", map[string]string{
		"temp1_label": "Tctl", "temp1_input": "42000",
	})
	snap, err := readThermal(root, filepath.Join(t.TempDir(), "none"))
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Valid || snap.Celsius != 42 {
		t.Fatalf("got %#v", snap)
	}
}

func TestThermalPrefersCoretempPackageOverCore(t *testing.T) {
	root := t.TempDir()
	writeHwmon(t, root, "hwmon0", "coretemp", map[string]string{
		"temp1_label": "Package id 0", "temp1_input": "51000",
		"temp2_label": "Core 0", "temp2_input": "70000",
	})
	snap, err := readThermal(root, filepath.Join(t.TempDir(), "none"))
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Valid || snap.Celsius != 51 {
		t.Fatalf("got %#v", snap)
	}
}

func TestThermalFallsBackToPackageThermalZone(t *testing.T) {
	hwmon := t.TempDir()
	writeHwmon(t, hwmon, "hwmon0", "acpitz", map[string]string{
		"temp1_input": "30000",
	})
	thermal := t.TempDir()
	writeThermalZone(t, thermal, "thermal_zone0", "x86_pkg_temp", "49000")
	snap, err := readThermal(hwmon, thermal)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Valid || snap.Celsius != 49 {
		t.Fatalf("got %#v", snap)
	}
}

func TestThermalAbsentTreeIsNotAnError(t *testing.T) {
	snap, err := readThermal(filepath.Join(t.TempDir(), "missing"), filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatal(err)
	}
	if snap.Valid {
		t.Fatalf("got %#v", snap)
	}
}

func writeThermalZone(t *testing.T, root, dir, kind, milli string) {
	t.Helper()
	zone := filepath.Join(root, dir)
	if err := os.MkdirAll(zone, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(zone, "type"), []byte(kind+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(zone, "temp"), []byte(milli+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
