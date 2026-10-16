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

const ledsRoot = "/sys/class/leds"

// LED is one device in the sysfs class LED hierarchy, most of which the
// thinkpad_acpi driver publishes under the "tpacpi::" prefix.
type LED struct {
	// Name is the directory name under /sys/class/leds, which is also the
	// kernel's device name.
	Name string

	// Brightness is the current level and MaxBrightness the driver's ceiling.
	// Most backlights expose a ceiling of 1, 2 or 3 rather than 255, so a
	// caller must never assume a range.
	Brightness      int
	BrightnessValid bool
	MaxBrightness   int

	// HWChanged counts brightness changes the firmware made behind the
	// driver's back, which is how a hardware Fn-key press shows up. The
	// attribute only exists on some drivers.
	HWChanged      int
	HWChangedValid bool

	// Writable reports whether this process can open the brightness
	// attribute for writing. The probe opens and immediately closes the
	// file: writing to a sysfs attribute is what changes hardware, so the
	// open alone has no effect and the answer is exact, with none of the
	// guessing a permission-bit check would need.
	Writable bool
}

// LEDSnapshot lists the LEDs found at one time. LEDs and Issues are
// caller-owned snapshot slices.
type LEDSnapshot struct {
	CollectedAt time.Time
	LEDs        []LED
	Issues      []Issue
}

// ReadLEDs collects one reading of every device in the sysfs LED class. A
// machine with no class LED directory reports an empty snapshot, not an
// error.
func ReadLEDs() (LEDSnapshot, error) {
	return readLEDs(ledsRoot, true)
}

func readLEDs(root string, probeWrite bool) (LEDSnapshot, error) {
	snapshot := LEDSnapshot{CollectedAt: time.Now()}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) || os.IsPermission(err) {
			return snapshot, nil
		}
		return LEDSnapshot{}, fmt.Errorf("%s: %w", root, err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	// The class directory reads back in hash order, which would make two
	// consecutive snapshots differ in node order for no reason.
	sort.Strings(names)

	snapshot.LEDs = make([]LED, 0, len(names))
	for _, name := range names {
		led, ok := readLED(filepath.Join(root, name), name, probeWrite, &snapshot.Issues)
		if !ok {
			continue
		}
		snapshot.LEDs = append(snapshot.LEDs, led)
	}
	return snapshot, nil
}

// readLED fills one LED. It reports false for an entry that is not a usable
// LED class device, which is not worth an Issue: some drivers publish
// directories whose brightness attribute is absent.
func readLED(path, name string, probeWrite bool, issues *[]Issue) (LED, bool) {
	led := LED{Name: name}

	if max, err := readLEDInt(filepath.Join(path, "max_brightness")); err == nil {
		led.MaxBrightness = max
	}
	value, err := readLEDInt(filepath.Join(path, "brightness"))
	if err != nil {
		*issues = append(*issues, Issue{Source: filepath.Join(path, "brightness"), Err: err})
		return LED{}, false
	}
	led.Brightness, led.BrightnessValid = value, true

	if changed, err := readLEDInt(filepath.Join(path, "brightness_hw_changed")); err == nil {
		led.HWChanged, led.HWChangedValid = changed, true
	}
	if probeWrite {
		led.Writable = canWrite(filepath.Join(path, "brightness"))
	}
	return led, true
}

// readLEDInt reads a sysfs integer attribute, tolerating a missing file so
// one absent attribute never sinks a whole device.
func readLEDInt(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	return value, nil
}

// canWrite reports whether the attribute can be opened for writing.
func canWrite(path string) bool {
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	_ = file.Close()
	return true
}

// FindLED returns the LED with the given kernel device name.
func FindLED(snapshot LEDSnapshot, name string) (LED, bool) {
	for _, led := range snapshot.LEDs {
		if led.Name == name {
			return led, true
		}
	}
	return LED{}, false
}

// LEDNames lists the kernel device names in the snapshot, in order.
func LEDNames(snapshot LEDSnapshot) []string {
	names := make([]string, 0, len(snapshot.LEDs))
	for _, led := range snapshot.LEDs {
		names = append(names, led.Name)
	}
	return names
}
