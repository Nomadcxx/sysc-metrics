//go:build linux

package metrics

import (
	"os"
	"path/filepath"
	"testing"
)

// writeLEDFixture builds one sysfs LED class directory.
func writeLEDFixture(t *testing.T, root, name string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for file, body := range files {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadLEDsReadsBacklights(t *testing.T) {
	root := t.TempDir()
	// The naming thinkpad_acpi uses, including the doubled colon.
	writeLEDFixture(t, root, "tpacpi::kbd_backlight", map[string]string{
		"brightness": "2\n", "max_brightness": "2\n",
	})
	writeLEDFixture(t, root, "tpacpi::lid_logo_dot", map[string]string{
		"brightness": "255\n", "max_brightness": "255\n", "brightness_hw_changed": "0\n",
	})

	got, err := readLEDs(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.LEDs) != 2 || len(got.Issues) != 0 {
		t.Fatalf("readLEDs() = %d LEDs, issues %v", len(got.LEDs), got.Issues)
	}
	// Sorted order keeps consecutive snapshots comparable.
	if got.LEDs[0].Name != "tpacpi::kbd_backlight" || got.LEDs[1].Name != "tpacpi::lid_logo_dot" {
		t.Fatalf("LED names = %v", LEDNames(got))
	}
	if kbd := got.LEDs[0]; kbd.Brightness != 2 || !kbd.BrightnessValid || kbd.MaxBrightness != 2 {
		t.Fatalf("kbd backlight = %#v", kbd)
	}
	if kbd := got.LEDs[0]; kbd.HWChangedValid {
		t.Fatalf("kbd backlight reported a firmware counter it does not have: %#v", kbd)
	}
	lid := got.LEDs[1]
	if !lid.HWChangedValid || lid.HWChanged != 0 || lid.MaxBrightness != 255 {
		t.Fatalf("lid logo = %#v", lid)
	}
}

func TestReadLEDsReportsPermissionErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the permission bits this test needs")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := readLEDs(root, false); err == nil {
		t.Fatal("readLEDs() hid a permission error as an empty LED class")
	}
}

func TestReadLEDsSkipsUnreadableEntriesWithoutFailingTheRead(t *testing.T) {
	root := t.TempDir()
	writeLEDFixture(t, root, "good", map[string]string{"brightness": "1\n", "max_brightness": "3\n"})
	// A directory with no brightness attribute is not a usable LED.
	writeLEDFixture(t, root, "partial", map[string]string{"max_brightness": "3\n"})

	got, err := readLEDs(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.LEDs) != 1 || got.LEDs[0].Name != "good" {
		t.Fatalf("readLEDs() = %v", LEDNames(got))
	}
	if len(got.Issues) != 1 {
		t.Fatalf("issues = %v, want one for the skipped entry", got.Issues)
	}
}

func TestReadLEDsReportsAbsenceWithoutError(t *testing.T) {
	got, err := readLEDs(filepath.Join(t.TempDir(), "missing"), false)
	if err != nil {
		t.Fatalf("readLEDs() error = %v, want nil for an absent LED class", err)
	}
	if len(got.LEDs) != 0 {
		t.Fatalf("readLEDs() = %v, want no LEDs", LEDNames(got))
	}
}

func TestReadLEDsProbesWriteAccessWithoutChangingHardware(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the permission bits the probe depends on")
	}
	root := t.TempDir()
	writeLEDFixture(t, root, "ro", map[string]string{"brightness": "1\n", "max_brightness": "3\n"})
	writeLEDFixture(t, root, "rw", map[string]string{"brightness": "2\n", "max_brightness": "3\n"})
	// Root-only is the stock mode for thinkpad_acpi brightness attributes:
	// reads always work, writes need a udev rule or the group the setup
	// script creates. Mode bits alone decide it, since the fixture is owned
	// by the test user.
	if err := os.Chmod(filepath.Join(root, "ro", "brightness"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "rw", "brightness"), 0o666); err != nil {
		t.Fatal(err)
	}

	got, err := readLEDs(root, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, led := range got.LEDs {
		want := led.Name == "rw"
		if led.Writable != want {
			t.Fatalf("LED %q writable = %v, want %v", led.Name, led.Writable, want)
		}
	}
}

func TestFindLED(t *testing.T) {
	snapshot := LEDSnapshot{LEDs: []LED{{Name: "a"}, {Name: "tpacpi::kbd_backlight"}}}
	got, ok := FindLED(snapshot, "tpacpi::kbd_backlight")
	if !ok || got.Name != "tpacpi::kbd_backlight" {
		t.Fatalf("FindLED() = %#v, %v", got, ok)
	}
	if _, ok := FindLED(snapshot, "missing"); ok {
		t.Fatal("FindLED() found an LED that is not in the snapshot")
	}
}
