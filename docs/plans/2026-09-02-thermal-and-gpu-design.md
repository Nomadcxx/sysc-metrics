# Thermal and GPU collectors

Date: 2026-09-02
Status: Approved (amended 2026-09-17 for Intel GPU usage; see the amendment at the end)

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

Process list. Per-core temperatures. VRAM. Power limits. `nvidia-smi` pretty-print. Folding
these fields into `CPUSnapshot`. (Intel GPU usage was originally out of scope; the 2026-09-17
amendment at the end adds it.)

## Amendment 2026-09-17: Intel GPU usage via the i915 PMU

The original design left Intel GPU usage out of scope: i915 exposes utilization as PMU counters
rather than a percentage file, and the stateless `ReadGPU` API cannot form a counter delta. This
amendment adds a stateful sampler while preserving the existing public contract.

### Source

Intel GPUs managed by the Linux `i915` driver expose engine-busy time as PMU events under
`/sys/bus/event_source/devices/i915`. The sampler opens those counters with `perf_event_open`
and converts busy-time deltas over the monotonic sample interval into a 0..1 fraction. No
`intel_gpu_top`, no `perf` binary, no daemon, no CGO, no new module: the syscall is a narrow
shim in `gpu_pmu_linux.go` using the standard library `syscall` package with the ABI-0
`perf_event_attr` (64 bytes). `gt_act_freq_mhz` is a frequency, not utilization, and is not a
substitute.

### Public shape

```go
type GPUSampler struct { /* owned by one sequential caller */ }

func NewGPUSampler() *GPUSampler
func (s *GPUSampler) Sample() (GPUSnapshot, error)
func (s *GPUSampler) Close() error
```

- One sequential owner, same rule as the CPU, block, network, and process samplers. `Sample` is
  synchronous and starts no goroutine.
- `Close` is idempotent and closes every opened counter file descriptor; open PMU descriptors
  have this explicit owner and lifetime.
- `ReadGPU` is unchanged and stays source-compatible for one-shot identity, temperature, AMD,
  and NVIDIA snapshots. It never returns Intel usage (`Usage.Valid == false` for i915) and never
  hides a process-global sampler or a fake one-counter percentage. A consumer that needs Intel
  usage must own a `GPUSampler` per metrics loop.

### Discovery and mapping

1. `Sample` first produces the normal DRM snapshot (identity, driver, name, temperature, AMD
   busy, NVIDIA fallback) through the existing reader; the PMU layer is applied on top, without
   duplicating enumeration.
2. For each GPU whose driver is `i915`, the sampler looks for a PMU directory named after the
   driver (exactly `i915`, or `i915_...` when a kernel names per-device PMUs).
3. A PMU maps to a GPU through its sysfs `device` link to a PCI device when the kernel exposes
   one. When no link exists, a driver-named PMU may only be mapped while exactly one GPU carries
   that driver. Ambiguity is never resolved by arbitrary assignment: unmatched GPUs keep invalid
   usage and an `Issue`.
4. Busy engines are the entries under `<pmu>/events/` ending in `-busy`. Each definition must
   parse as `config=<hex>` consistent with the `format` files (observed: `config:0-20`), and the
   matching `<event>.unit` file must say `ns`. Unparseable definitions or non-ns units are an
   `Issue`, and a failed engine makes the whole GPU percentage invalid (see aggregation). Engine
   sets differ across kernel and GPU generations, so events are discovered at runtime, never
   hard-coded.
5. Each engine counter is opened once (`pid=-1`, `cpu=0`, group=-1, disabled) and stays open
   across `Sample` calls; values are read as 8-byte counters.

### Aggregation

`GPUUsage.Fraction` for an Intel GPU is the **maximum of the measured engine busy fractions**.
Max stays within the 0..1 contract and does not double-count parallel engines. If kernel
evidence later supports a device-total event, that is a new recorded decision here, not a silent
change; engine percentages are never summed.

Partial coverage is handled conservatively: if any defined engine cannot be opened, or any open
counter read fails, or an engine yields an impossible delta, that GPU's usage is invalid for the
sample and the failure is an `Issue`. A fabricated device-wide percentage from partial data is
worse than no percentage.

### Required semantics

- The first sample after `NewGPUSampler`, after `Close`, or after a GPU (re)appearance has
  `Usage.Valid == false` because no delta exists yet.
- A second sample over a positive monotonic interval with readable counters has
  `Usage.Valid == true`. A measured zero busy delta is a valid zero.
- Counter decrease or reset (i915 resets on GPU suspend and module reload), short or failed
  reads, non-positive elapsed time, and an impossible delta (busy delta exceeding the interval)
  rebaseline the engine and leave that sample invalid. The next complete pair is valid again.
- A missing PMU, missing `-busy` events, `EPERM`, or `EACCES` leaves the Intel GPU in the
  snapshot with invalid usage and a useful `Issue`; identity and temperature survive, and the
  reading is never turned into zero.
- `GPUUsage.Fraction` is finite and within 0..1 whenever `Valid` is true.
- GPU removal closes and drops that GPU's PMU state; reappearance starts a fresh baseline. Old
  history never resumes across the gap.

### Permissions

Measured on the target laptop (kernel 7.2.4-arch1-2, `perf_event_paranoid=2`, uid 1000, no
`perf` or `intel_gpu_top` installed): `perf_event_open` on the i915 counters returns `EACCES`
unprivileged, with and without `exclude_kernel`/`exclude_hv` flags. Reading i915 counters
therefore requires `CAP_PERFMON` (or `CAP_SYS_ADMIN`) on the consumer binary, or
`kernel.perf_event_paranoid <= 1`. The sampler reports the failure as an `Issue` and keeps the
rest of the snapshot intact.

Observed PMU evidence (2026-09-17): PMU type `13`; format `i915_eventid: config:0-20`; busy
events `rcs0-busy` (config 0x0), `bcs0-busy` (0x1000), `vcs0-busy` (0x2000), `vecs0-busy`
(0x3000), all with unit `ns`. This PMU directory exposes no `device` link on this kernel, which
is why the single-GPU mapping rule above exists.

### Driver boundary

Only the `i915` driver is supported. The newer `xe` driver may expose a different public PMU
contract; if it does, that is a separate driver-gated extension, and no claim is made that i915
support covers `xe`.
