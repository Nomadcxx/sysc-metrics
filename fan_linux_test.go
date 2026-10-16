//go:build linux

package metrics

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFanReadsEveryField(t *testing.T) {
	body := "status:\tenabled\nspeed:\t\t4432\nlevel:\t\tauto\n"
	got, err := parseFan(strings.NewReader(body), "test")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Present || got.Status != "enabled" || got.SpeedRPM != 4432 || !got.SpeedValid {
		t.Fatalf("parseFan() = %#v", got)
	}
	if got.Level != FanLevelAuto || !got.LevelValid {
		t.Fatalf("parseFan() level = %q valid %v", got.Level, got.LevelValid)
	}
	if len(got.Issues) != 0 {
		t.Fatalf("parseFan() issues = %v", got.Issues)
	}
}

func TestParseFanWithoutAControllableLevel(t *testing.T) {
	// The level line is absent unless the module loaded with fan_control=1.
	// Its absence is the signal that reads work but writes will be refused.
	body := "status:\tenabled\nspeed:\t\t3100\n"
	got, err := parseFan(strings.NewReader(body), "test")
	if err != nil {
		t.Fatal(err)
	}
	if got.LevelValid || got.Level != "" {
		t.Fatalf("parseFan() level = %q valid %v, want an uncontrollable level", got.Level, got.LevelValid)
	}
	if !got.SpeedValid || got.SpeedRPM != 3100 {
		t.Fatalf("parseFan() speed = %d valid %v", got.SpeedRPM, got.SpeedValid)
	}
}

func TestParseFanTreatsAStoppedFanAsAValidZero(t *testing.T) {
	// A parked laptop reports speed 0. That is a reading, not a failure, and
	// only an unparsable word is an issue.
	got, err := parseFan(strings.NewReader("status:\tenabled\nspeed:\t\t0\n"), "test")
	if err != nil {
		t.Fatal(err)
	}
	if !got.SpeedValid || got.SpeedRPM != 0 || len(got.Issues) != 0 {
		t.Fatalf("parseFan() = %#v", got)
	}
}

func TestParseFanKeepsGoodFieldsWhenOneIsUnusable(t *testing.T) {
	got, err := parseFan(strings.NewReader("status:\tdisabled\nspeed:\t\tunknown\nlevel:\t\tturbo\n"), "test")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "disabled" {
		t.Fatalf("status = %q, want disabled", got.Status)
	}
	if got.SpeedValid || got.LevelValid {
		t.Fatalf("parseFan() = %#v, want both fields rejected", got)
	}
	if len(got.Issues) != 2 {
		t.Fatalf("issues = %v, want one per unusable field", got.Issues)
	}
}

func TestValidFanLevel(t *testing.T) {
	for _, level := range []string{"auto", "disengaged", "0", "3", "7"} {
		if !ValidFanLevel(level) {
			t.Errorf("ValidFanLevel(%q) = false, want true", level)
		}
	}
	// The driver takes the string verbatim, so anything outside the set has
	// to be refused before a write, not discovered as an EIO afterwards.
	for _, level := range []string{"", "8", "9", "-1", "AUTO", " max", "7\n", "auto ", "disengage"} {
		if ValidFanLevel(level) {
			t.Errorf("ValidFanLevel(%q) = true, want false", level)
		}
	}
}

func TestFanLevelNumber(t *testing.T) {
	if got := FanLevelNumber("5"); got != 5 {
		t.Errorf("FanLevelNumber(\"5\") = %d, want 5", got)
	}
	// The symbolic levels have no fixed encoder position.
	for _, level := range []string{FanLevelAuto, FanLevelDisengaged, "nope"} {
		if got := FanLevelNumber(level); got != -1 {
			t.Errorf("FanLevelNumber(%q) = %d, want -1", level, got)
		}
	}
}

func TestReadFanReportsAbsenceWithoutError(t *testing.T) {
	got, err := readFan(filepath.Join(t.TempDir(), "missing", "fan"))
	if err != nil {
		t.Fatalf("readFan() error = %v, want nil for an absent interface", err)
	}
	if got.Present {
		t.Fatalf("readFan() = %#v, want an absent fan", got)
	}
}

func TestReadFanReadsAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fan")
	if err := os.WriteFile(path, []byte("status:\tenabled\nspeed:\t\t5000\nlevel:\t\t7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readFan(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Present || got.Level != "7" || got.SpeedRPM != 5000 {
		t.Fatalf("readFan() = %#v", got)
	}
}
