# Thermal and GPU collectors

Date: 2026-09-02
Status: Approved

Consumer: `sysc-shell` System pane (`noctalia-sysmon.png`). CPU card footer needs package
temperature. GPU card needs usage and temperature. System identity card needs a GPU name.

This is roadmap M2 thermal plus M3 GPU, tagged `v0.3.0`. Battery already shipped in `v0.2.0`.
`main` is still the core-counters audit; this work continues the `v0.2.0` /
`milestone/power-collectors` lineage and then lands on `main` so the next pin is not another
orphan tag.

## Boundary

Same contract as the 2026-08-27 design. Collection, parsing, units, timestamps, availability,
partial errors. No presentation strings, no polling loop, no daemon, no CGO, no dgop import.

DMS does not contain these collectors. It execs `dgop`. We copy the Linux files dgop and
Noctalia already read.

## Public API

Concrete readers, like `ReadBattery`. Not folded into `CPUSnapshot`.

```go
type ThermalSnapshot struct {
	CollectedAt time.Time
	Celsius     float64
	Valid       bool
	Source      string // hwmon name + label, or thermal_zone type; for Issues, not UI
	Issues      []Issue
}

func ReadThermal() (ThermalSnapshot, error)

type GPUUsage struct {
	Fraction float64 // 0..1
	Valid    bool
}

type GPU struct {
	PCIID  string // "vendor:device", lowercase hex, no 0x
	Driver string
	Name   string // pci.ids best-effort; empty if unknown
	Usage  GPUUsage
	Celsius float64
	TempValid bool
}

type GPUSnapshot struct {
	CollectedAt time.Time
	GPUs        []GPU
	Issues      []Issue
}

func ReadGPU() (GPUSnapshot, error)
```

A missing sysfs tree is not an error: `Valid` stays false / `GPUs` empty, matching battery
`Present == false`. An unreadable individual sensor is an `Issue`; the rest of the snapshot
stands.

Returned slices belong to the caller.

## CPU temperature

One number. The library scores; the shell does not.

Walk `/sys/class/hwmon/*/temp*_input` whose `name` is `k10temp`, `zenpower`, `coretemp`, or
`ibmpowernv`. Prefer in that driver order. Inside a driver, prefer:

| Driver | Label / input |
|---|---|
| k10temp, zenpower | Tctl, then `temp1`, then Tdie, then Package id |
| coretemp | Package id, then `temp1`, then Core * |
| ibmpowernv | `temp1` |

Values are millidegrees Celsius. Divide by 1000. Skip unparseable files.

If no known hwmon chip exists, walk `/sys/class/thermal/thermal_zone*/temp` for types
`cpu-thermal`, then `x86_pkg_temp`, then `acpitz`. Prefer a positive reading over a zero.

Do not export every zone. Do not clamp.

## GPU

Enumerate `/sys/class/drm/card*` whose name has no `-` (skip `card0-DP-1` connectors). Skip
`DRIVER=simpledrm`. Identity is the PCI vendor/device pair from `device/vendor` and
`device/device`.

**Name.** First match in `/usr/share/hwdata/pci.ids`, then `/usr/share/misc/pci.ids`. Empty
on miss. Not a UI string; the shell may show it raw.

**Usage.**

- `amdgpu` / `radeon`: `device/gpu_busy_percent` (0–100) → fraction.
- NVIDIA (`10de:`): `nvidia-smi` (below).
- Anything else, including Intel iGPU: `Usage.Valid == false`.

**Temperature.** Prefer `device/hwmon/hwmon*/temp1_input` (millidegrees). NVIDIA falls back
to `nvidia-smi` when hwmon is missing. Do not use `acpitz` as a GPU temp; that is a board
sensor.

Deterministic order: PCI id, then drm card name.

## nvidia-smi

Only when at least one enumerated GPU has vendor `10de`. Look up `nvidia-smi` on `PATH`.
Missing binary: those GPUs stay usage/temp invalid, no error.

```
nvidia-smi --query-gpu=pci.bus_id,utilization.gpu,temperature.gpu,name --format=csv,noheader,nounits
```

Timeout 400ms. Match rows to sysfs by PCI BDF (sysfs `device` basename vs `pci.bus_id`,
ignoring domain-zero vs `0000:` prefix). `utilization.gpu` is 0–100. `name` fills `GPU.Name`
when pci.ids missed.

The command is injected behind an unexported `var nvidiaSMI func(ctx) ([]byte, error)` so
tests never spawn a process. Production default uses `os/exec`.

Calling `nvidia-smi` wakes a hybrid dGPU. That is accepted. The shell must lease GPU only
while the monitor panel is open, never from a bar widget, unless a later widget accepts that
cost.

No NVML, no ROCm SMI, no extra module.

## Tests

Fixture trees under `t.TempDir()`, same pattern as `readBattery(root)`.

- k10temp Tctl wins over a coretemp Package sitting beside it.
- coretemp Package wins over Core 0 when k10temp is absent.
- thermal_zone `x86_pkg_temp` used when hwmon has no known driver.
- missing hwmon and thermal roots → `Valid == false`, no error.
- drm `card0` amdgpu with `gpu_busy_percent` 14 and `temp1_input` 45000 → usage 0.14, 45°C.
- `simpledrm` skipped.
- NVIDIA GPU present, fake smi CSV → usage and temp; smi not called when no `10de:` device.
- missing `nvidia-smi` with an NVIDIA device → GPU listed, usage/temp invalid.

Linux integration: `ReadThermal` and `ReadGPU` do not error on this machine. If thermal is
valid, Celsius is in (0, 150). GPU slice may be empty.

## Out of scope

Process list. Per-core temperatures. VRAM. Intel GPU usage. Power limits. `nvidia-smi`
pretty-print. Folding these fields into `CPUSnapshot`.
