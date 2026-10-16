//go:build linux

package metrics

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

const acpiFanPath = "/proc/acpi/ibm/fan"

// Fan levels the IBM ACPI driver accepts. The driver rejects anything else,
// so a caller that writes to the interface must filter with ValidFanLevel
// first.
const (
	// FanLevelAuto hands the fan back to the BIOS thermal policy.
	FanLevelAuto = "auto"
	// FanLevelDisengaged pins the fan to full speed.
	FanLevelDisengaged = "disengaged"
)

// maxNumericFanLevel is the highest level a modern ThinkPad exposes. Older
// machines stop lower, and the driver rejects a level its hardware has no
// encoder position for, so callers should prefer the levels the driver
// reports in FanSnapshot.Level.
const maxNumericFanLevel = 7

// FanSnapshot is one reading of the IBM ACPI thinkpad fan interface.
//
// Present is false on a machine without the interface, which is not an error:
// desktop hardware has no such file and the caller should carry on.
type FanSnapshot struct {
	CollectedAt time.Time
	Present     bool

	// Status is the driver's raw status word, normally "enabled" or
	// "disabled".
	Status string

	// SpeedRPM is the reported tachometer reading. SpeedValid is false when
	// the driver cannot produce a number, which is what a laptop that has
	// parked its fan reports; a genuine zero fan is a valid zero.
	SpeedRPM   int
	SpeedValid bool

	// Level is the cooling level the interface currently requests: "auto",
	// "disengaged" or "0".."7".
	//
	// LevelValid is false when the driver does not accept a level at all.
	// That is the default kernel behaviour -- the thinkpad_acpi module comes
	// up with fan_control=0 -- and it is the signal that reads work but
	// writes will be refused until the module is reloaded with the option
	// set.
	Level      string
	LevelValid bool

	// Issues holds per-field parse failures. The snapshot is still returned
	// so a partial reading reaches the caller.
	Issues []Issue
}

// ReadFan returns one IBM ACPI fan snapshot. A machine without the
// thinkpad_acpi interface reports Present false rather than an error.
func ReadFan() (FanSnapshot, error) {
	return readFan(acpiFanPath)
}

func readFan(path string) (FanSnapshot, error) {
	file, err := os.Open(path)
	if err != nil {
		// A missing or unreadable interface is an absent feature, not a
		// failure: the caller draws a "not supported" panel and stops.
		if os.IsNotExist(err) || os.IsPermission(err) {
			return FanSnapshot{CollectedAt: time.Now()}, nil
		}
		return FanSnapshot{}, fmt.Errorf("%s: %w", path, err)
	}
	defer file.Close()

	snapshot, err := parseFan(file, path)
	if err != nil {
		return FanSnapshot{}, fmt.Errorf("%s: %w", path, err)
	}
	return snapshot, nil
}

// parseFan reads the "key:\tvalue" lines the driver emits:
//
//	status:	enabled
//	speed:		4432
//	level:		auto
//
// The level line only appears when the module was loaded with fan_control=1.
func parseFan(r io.Reader, source string) (FanSnapshot, error) {
	snapshot := FanSnapshot{CollectedAt: time.Now(), Present: true}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), maxScannerToken)
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "status":
			snapshot.Status = strings.ToLower(value)
		case "speed":
			rpm, err := strconv.Atoi(value)
			if err != nil || rpm < 0 {
				snapshot.Issues = append(snapshot.Issues, Issue{
					Source: source + ":speed",
					Err:    fmt.Errorf("unusable tachometer reading %q", value),
				})
				continue
			}
			snapshot.SpeedRPM, snapshot.SpeedValid = rpm, true
		case "level":
			level := strings.ToLower(value)
			if !ValidFanLevel(level) {
				snapshot.Issues = append(snapshot.Issues, Issue{
					Source: source + ":level",
					Err:    fmt.Errorf("unknown fan level %q", level),
				})
				continue
			}
			snapshot.Level, snapshot.LevelValid = level, true
		}
	}
	if err := scanner.Err(); err != nil {
		return FanSnapshot{}, fmt.Errorf("scan: %w", err)
	}
	return snapshot, nil
}

// ValidFanLevel reports whether level is a value the IBM ACPI fan driver
// accepts: "auto", "disengaged", or a numeric level. Callers that write to
// the interface must check first, because the driver takes the string
// verbatim and an unexpected level is refused.
func ValidFanLevel(level string) bool {
	switch level {
	case FanLevelAuto, FanLevelDisengaged:
		return true
	}
	n, err := strconv.Atoi(level)
	return err == nil && n >= 0 && n <= maxNumericFanLevel
}

// FanLevelNumber returns the numeric part of a fan level. It returns -1 for
// the symbolic levels, which have no fixed encoder position: "auto" tracks the
// BIOS thermal policy and "disengaged" runs the fan flat out.
func FanLevelNumber(level string) int {
	n, err := strconv.Atoi(level)
	if err != nil {
		return -1
	}
	return n
}
