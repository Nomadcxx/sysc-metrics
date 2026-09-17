# sysc-metrics

`sysc-metrics` is a small Go library for read-only Linux system telemetry. It supplies the built-in
monitoring widgets in [`sysc-shell`](https://github.com/Nomadcxx/sysc-shell) without importing a CLI,
TUI, HTTP server, or unrelated application framework.

The M0 contract, M1 core collectors, sysfs battery, CPU temperature, GPU
usage/temperature, and opt-in process sampling are implemented.

## Usage

```go
package main

import (
	"fmt"
	"github.com/Nomadcxx/sysc-metrics"
)

func main() {
	sampler := metrics.NewGPUSampler()
	snapshot, err := sampler.Sample()
	if err != nil {
		panic(err)
	}
	if snapshot.Usage.Valid {
		fmt.Println(snapshot.Usage.Fraction)
	}
	sampler.Close()
}
```

M1 is Linux-only and uses only the Go standard library. Samplers belong to one sequential polling
owner; they do not start goroutines. First and discontinuous rate samples have `Valid == false`, while
valid zero values remain valid. A snapshot may contain partial data and `Issue` values for failed
individual sources or entities.

`GPUSampler` reports the same GPU snapshot as `ReadGPU` and additionally fills Intel i915 usage
from the second sample on, derived from PMU engine-busy counters opened with `perf_event_open`.
On measured kernels, opening those system-wide counters requires `CAP_PERFMON` (or
`CAP_SYS_ADMIN`) on the process; lowering `kernel.perf_event_paranoid` did not lift the gate.
Without privilege the sampler records an `Issue` and leaves Intel usage invalid while identity
and temperature stay filled. `ReadGPU` never reports Intel usage. Close a `GPUSampler` when its
polling stops; it owns open counter descriptors.

Polling cadence, caching, presentation, units shown to users, alerts, and filtering remain consumer
responsibilities.

## Scope

The first releases will collect:

- aggregate and per-core CPU usage, load, and frequency;
- memory and swap;
- mounted-filesystem capacity and block-device I/O rates;
- per-interface network counters and rates;
- thermal sensors and available GPU metrics;
- battery and UPS state, energy, rate, and estimated time;
- uptime and basic totals;
- process CPU and memory only when a consumer requests it.

Battery is the sysfs power-supply aggregate. CPU temperature is one scored
hwmon / thermal_zone reading. GPU usage and temperature come from drm sysfs,
with AMD usage from `gpu_busy_percent`, NVIDIA falling back to `nvidia-smi`
when a `10de:` device is present, and Intel i915 usage from PMU engine-busy
counters through the stateful `GPUSampler`. Collectors use Linux interfaces
such as `/proc`, `/sys`, `statfs`, and `os/exec` for that optional NVIDIA
binary.

The library will not provide power controls, recursive directory sizes, filesystem indexing, SMART,
quotas, vendor administration, a daemon, or cross-platform abstractions.

## Development gates

1. Define snapshot and sampling semantics, including first-sample behavior and counter reset handling.
2. Implement CPU, memory, filesystem, block-device, and network collectors with fixture tests.
3. Add thermal and battery collection, including UPower loss and sysfs fallback tests.
4. Add GPU and process collectors only for a confirmed shell consumer.
5. Qualify suspend/resume, device removal, permission errors, and real hardware before `v0.1.0`.

See the [design](docs/plans/2026-08-27-sysc-metrics-design.md) and [roadmap](docs/roadmap.md).
Package directories will be added with their first tested behavior; the repository will not track empty
scaffolding.

## Licence

`sysc-metrics` uses the [BSD 3-Clause License](LICENSE).
