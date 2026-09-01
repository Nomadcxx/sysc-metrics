//go:build linux

package metrics

import (
	"os"
	"path/filepath"
	"testing"
)

func writeDRMCard(t *testing.T, root, card, driver, vendor, device string, files map[string]string) string {
	t.Helper()
	dev := filepath.Join(root, card, "device")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		t.Fatal(err)
	}
	if driver != "" {
		drv := filepath.Join(root, "drivers", driver)
		if err := os.MkdirAll(drv, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(drv, filepath.Join(dev, "driver")); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(filepath.Join(dev, "vendor"), vendor)
	mustWrite(filepath.Join(dev, "device"), device)
	for name, body := range files {
		mustWrite(filepath.Join(dev, name), body)
	}
	return dev
}

func TestGPUReadsAmdBusyAndTempAndSkipsSimpleDRM(t *testing.T) {
	root := t.TempDir()
	writeDRMCard(t, root, "card0", "amdgpu", "0x1002", "0x67df", map[string]string{
		"gpu_busy_percent":         "14",
		"hwmon/hwmon0/temp1_input": "45000",
	})
	if err := os.MkdirAll(filepath.Join(root, "card0-DP-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDRMCard(t, root, "card1", "simpledrm", "0x0000", "0x0000", nil)

	snap, err := readGPU(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.GPUs) != 1 {
		t.Fatalf("GPUs = %#v", snap.GPUs)
	}
	g := snap.GPUs[0]
	if g.PCIID != "1002:67df" || g.Driver != "amdgpu" {
		t.Fatalf("identity = %#v", g)
	}
	if !g.Usage.Valid || g.Usage.Fraction != 0.14 {
		t.Fatalf("usage = %#v", g.Usage)
	}
	if !g.TempValid || g.Celsius != 45 {
		t.Fatalf("temp = %#v", g)
	}
}

func TestGPUFillsNvidiaFromSMI(t *testing.T) {
	root := t.TempDir()
	writeNvidiaCard(t, root, "card0", "0000:01:00.0", "0x10de", "0x2684")
	called := false
	smi := func() ([]byte, error) {
		called = true
		return []byte("0000:01:00.0, 37, 61, GeForce RTX 4090\n"), nil
	}
	snap, err := readGPU(root, nil, smi)
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("nvidia-smi was not invoked")
	}
	if len(snap.GPUs) != 1 {
		t.Fatalf("GPUs = %#v", snap.GPUs)
	}
	g := snap.GPUs[0]
	if !g.Usage.Valid || g.Usage.Fraction != 0.37 {
		t.Fatalf("usage = %#v", g.Usage)
	}
	if !g.TempValid || g.Celsius != 61 {
		t.Fatalf("temp = %#v", g)
	}
	if g.Name != "GeForce RTX 4090" {
		t.Fatalf("name = %q", g.Name)
	}
}

func TestGPUDoesNotInvokeSMIWithoutNvidia(t *testing.T) {
	root := t.TempDir()
	writeDRMCard(t, root, "card0", "i915", "0x8086", "0x5917", nil)
	smi := func() ([]byte, error) {
		t.Fatal("nvidia-smi invoked without an NVIDIA device")
		return nil, nil
	}
	if _, err := readGPU(root, nil, smi); err != nil {
		t.Fatal(err)
	}
}

func TestGPUMissingSMILeavesNvidiaInvalid(t *testing.T) {
	root := t.TempDir()
	writeNvidiaCard(t, root, "card0", "0000:01:00.0", "0x10de", "0x2684")
	snap, err := readGPU(root, nil, func() ([]byte, error) {
		return nil, os.ErrNotExist
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.GPUs) != 1 {
		t.Fatalf("GPUs = %#v", snap.GPUs)
	}
	g := snap.GPUs[0]
	if g.Usage.Valid || g.TempValid {
		t.Fatalf("missing smi still set rates: %#v", g)
	}
}

func TestGPUPCINameFromIDsFile(t *testing.T) {
	root := t.TempDir()
	writeDRMCard(t, root, "card0", "i915", "0x8086", "0x5917", nil)
	ids := filepath.Join(t.TempDir(), "pci.ids")
	if err := os.WriteFile(ids, []byte("8086  Intel Corporation\n\t5917  UHD Graphics 620\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap, err := readGPU(root, []string{ids}, func() ([]byte, error) {
		t.Fatal("smi")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.GPUs) != 1 || snap.GPUs[0].Name != "UHD Graphics 620" {
		t.Fatalf("GPUs = %#v", snap.GPUs)
	}
}

func writeNvidiaCard(t *testing.T, root, card, bdf, vendor, device string) {
	t.Helper()
	pci := filepath.Join(root, "pci", bdf)
	if err := os.MkdirAll(pci, 0o755); err != nil {
		t.Fatal(err)
	}
	drv := filepath.Join(root, "drivers", "nvidia")
	if err := os.MkdirAll(drv, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(drv, filepath.Join(pci, "driver")); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"vendor": vendor, "device": device} {
		if err := os.WriteFile(filepath.Join(pci, name), []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cardDir := filepath.Join(root, card)
	if err := os.MkdirAll(cardDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(pci, filepath.Join(cardDir, "device")); err != nil {
		t.Fatal(err)
	}
}
