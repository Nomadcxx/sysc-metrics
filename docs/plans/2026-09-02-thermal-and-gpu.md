# Thermal and GPU Collectors Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Ship `ReadThermal` and `ReadGPU` in `sysc-metrics` so the shell System pane can show CPU °C, GPU usage, GPU °C, and a GPU name.

**Architecture:** Unexported readers take fixture roots, same as `readBattery`. Public `ReadThermal` / `ReadGPU` bind `/sys`. NVIDIA is an injectable `nvidiaSMI` func. Zero new modules.

**Tech Stack:** Go 1.26, `/sys/class/hwmon`, `/sys/class/thermal`, `/sys/class/drm`, `pci.ids`, optional `nvidia-smi` via `os/exec`. Table tests on temp dirs. `@superpowers:test-driven-development`

Design: `docs/plans/2026-09-02-thermal-and-gpu-design.md`.

---

### Task 1: Public types

**Files:**
- Modify: `metrics.go`
- Modify: `metrics_test.go`

**Step 1: Write the failing compile assertions**

Add to `metrics_test.go` var block:

```go
_ func() (ThermalSnapshot, error) = ReadThermal
_ func() (GPUSnapshot, error)     = ReadGPU
```

In `TestPublicValueTypesHaveInvalidDerivedZeroValues`, assert a zero `ThermalSnapshot` and `GPUUsage` have `Valid == false`, and a zero `GPU` has `TempValid == false`.

**Step 2: Run test to verify it fails**

Run: `go test -count=1 -run 'TestPublicValueTypes' .`

Expected: FAIL, `ReadThermal` / `ThermalSnapshot` undefined.

**Step 3: Write minimal types and stubs**

In `metrics.go`, add the types from the design and:

```go
func ReadThermal() (ThermalSnapshot, error) { return ThermalSnapshot{}, errNotImplemented }
func ReadGPU() (GPUSnapshot, error)         { return GPUSnapshot{}, errNotImplemented }
```

`errNotImplemented` is temporary; Task 2/5 replace the bodies. Or return empty snapshots from the public funcs and put real work in unexported readers so integration does not fail. Prefer empty snapshots (Valid false) as the stub — missing data is not an error.

**Step 4: Run tests**

Run: `go test -count=1 .`
Expected: PASS existing tests; new assertions pass.

**Step 5: Commit**

```bash
git add metrics.go metrics_test.go
git commit -m "feat: export thermal and GPU snapshot types"
```

---

### Task 2: CPU temperature from known hwmon

**Files:**
- Create: `thermal_linux.go`
- Create: `thermal_linux_test.go`

**Step 1: Failing test**

```go
func TestThermalPrefersK10TctlOverCoretemp(t *testing.T) {
	root := t.TempDir()
	writeHwmon(t, root, "hwmon0", "coretemp", map[string]string{
		"temp1_label": "Package id 0", "temp1_input": "80000",
	})
	writeHwmon(t, root, "hwmon1", "k10temp", map[string]string{
		"temp1_label": "Tctl", "temp1_input": "42000",
	})
	snap, err := readThermal(root, filepath.Join(t.TempDir(), "none"))
	if err != nil || !snap.Valid || snap.Celsius != 42 {
		t.Fatalf("got %#v err=%v", snap, err)
	}
}
```

Helper `writeHwmon` mirrors `writeSupply`.

**Step 2: Run** `go test -count=1 -run TestThermalPrefersK10TctlOverCoretemp .`

Expected: FAIL, `readThermal` undefined.

**Step 3: Implement `readThermal(hwmonRoot, thermalRoot)` scoring per the design.**

**Step 4: Run that test. PASS.**

**Step 5: Commit** `feat: score CPU temperature from known hwmon chips`

---

### Task 3: coretemp Package, thermal_zone fallback, absent tree

**Files:**
- Modify: `thermal_linux.go`
- Modify: `thermal_linux_test.go`

**Step 1: Three failing tests**

1. coretemp only, Package id 0 at 51°C and Core 0 at 70°C → 51.
2. hwmon named `acpitz` only, thermal_zone `x86_pkg_temp` at 49°C → 49.
3. both roots missing → `Valid == false`, `err == nil`.

**Step 2: Run, confirm FAIL on the new cases.**

**Step 3: Implement thermal_zone walk and coretemp label scoring.**

**Step 4: PASS. Wire `ReadThermal` to `/sys/class/hwmon` and `/sys/class/thermal`.**

**Step 5: Commit** `feat: fall back CPU temperature to thermal_zone`

---

### Task 4: AMD GPU usage and temp, skip simpledrm

**Files:**
- Create: `gpu_linux.go`
- Create: `gpu_linux_test.go`

**Step 1: Failing tests**

Fixture: fake drm root with `card0` (amdgpu, vendor `0x1002`, device `0x67df`, `gpu_busy_percent` `14`, hwmon `temp1_input` `45000`) and `card0-DP-1` connector dir plus `card1` with `DRIVER=simpledrm`.

`readGPU` returns one GPU, usage 0.14, 45°C, PCI `1002:67df`.

**Step 2: FAIL undefined `readGPU`.**

**Step 3: Enumerate drm cards, skip connectors and simpledrm, read busy and hwmon temp.**

**Step 4: PASS.**

**Step 5: Commit** `feat: read amdgpu usage and temperature from drm`

---

### Task 5: nvidia-smi injection

**Files:**
- Modify: `gpu_linux.go`
- Modify: `gpu_linux_test.go`

**Step 1: Failing tests**

1. NVIDIA card `10de:2684`, no hwmon. Fake smi returns `0000:01:00.0, 37, 61, GeForce RTX 4090`. Sysfs device basename `0000:01:00.0`. Usage 0.37, 61°C, name set. Record that smi was invoked.
2. Same tree but vendor `8086` only: smi must not be invoked.
3. NVIDIA card, smi func returns `errMissing`: GPU listed, usage/temp invalid, no error from `readGPU`.

**Step 2: FAIL.**

**Step 3: Implement `nvidiaSMI`, BDF match (strip leading `0000:` on either side), 400ms timeout in the production wrapper. `ReadGPU` uses real drm root.**

**Step 4: PASS.**

**Step 5: Commit** `feat: fill NVIDIA usage and temp from nvidia-smi`

---

### Task 6: pci.ids name and integration

**Files:**
- Modify: `gpu_linux.go`, `gpu_linux_test.go`, `integration_linux_test.go`, `README.md`

**Step 1: Test pci.ids fixture:** file `8086  Intel\n\t5917  UHD Graphics 620\n`, GPU vendor/device those ids, no smi → `Name == "UHD Graphics 620"`.

**Step 2: FAIL until lookup exists.**

**Step 3: Implement lookup. Integration: `ReadThermal` and `ReadGPU` return err == nil; if thermal Valid then 0 < C < 150.**

**Step 4: `go test -race -count=1 ./...` PASS. Update README scope line for thermal/GPU as implemented.**

**Step 5: Commit** `feat: name GPUs from pci.ids and qualify live reads`

---

### Task 7: Tag and pin

Not code. After Task 6:

1. Merge `feat/thermal-gpu` into `milestone/power-collectors` and into `main` (fast-forward or merge commit; `main` is an ancestor of `v0.2.0`).
2. Tag `v0.3.0` on that commit, push branch + tag to origin.
3. In `sysc-shell`: `go get github.com/Nomadcxx/sysc-metrics@v0.3.0`, `go mod tidy`.
4. Shell System card: lease thermal + GPU while the monitor panel is open; project CPU °C, GPU usage/temp, GPU name. Identity rows already drafted in the working tree.
