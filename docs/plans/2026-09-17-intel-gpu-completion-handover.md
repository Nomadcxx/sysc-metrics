# sysc-metrics Intel GPU usage completion handover

Date: 2026-09-17

Plan: `docs/plans/2026-09-17-intel-gpu-pmu.md`, commissioning handover:
`sysc-shell/docs/plans/2026-09-17-sysc-metrics-intel-gpu-execution-handover.md`.
Design amendment: `docs/plans/2026-09-02-thermal-and-gpu-design.md`
(amendment 2026-09-17).

## Outcome

`sysc-metrics` now reports truthful Intel i915 GPU utilization through the new
stateful `GPUSampler`. The existing public contract is preserved: `GPU`,
`GPUUsage`, `GPUSnapshot`, and `ReadGPU` are source-compatible, and `ReadGPU`
still never reports Intel usage. The tag for the shell to consume is
`v0.5.0`.

## API contract for the shell

```go
sampler := metrics.NewGPUSampler()      // owned by the one metrics goroutine
snapshot, err := sampler.Sample()       // synchronous, no goroutine inside
...
err = sampler.Close()                   // when the metrics run stops
```

- `GPUSampler.Sample` returns the same snapshot shape as `ReadGPU` (identity,
  driver, name, temperature, AMD usage, NVIDIA fallback) and additionally
  fills Intel i915 `Usage` from the **second** sample on, as the maximum of
  the measured engine-busy fractions over the monotonic sample interval.
- The first sample, the first sample after `Close`, and the first sample after
  a GPU (re)appearance always have `Usage.Valid == false`.
- A measured zero busy delta is a valid zero.
- Counter decrease/reset, short or failed reads, non-positive intervals, and
  impossible deltas rebaseline the engine and invalidate that one sample.
- Any engine open/read failure invalidates the whole GPU percentage for that
  sample and records an `Issue`; partial data never fabricates a percentage.
- GPU removal closes and drops that GPU's counters; reappearance rebaselines.
- One sequential owner per sampler; no concurrent `Sample` calls; no package
  globals. Close the sampler when the metrics loop stops — it owns open
  counter descriptors (close-on-exec, so exec'd helpers never inherit them).
- The shell must stop using `metrics.ReadGPU()` for Intel usage and bump its
  module pin to `github.com/Nomadcxx/sysc-metrics@v0.5.0`. No `replace`
  directive, no second reader, no shell-side `perf` call.

## Permission contract (measured, kernel 7.2.4-arch1-2)

Opening the i915 system-wide PMU counters requires **`CAP_PERFMON` or
`CAP_SYS_ADMIN`** on the process. Lowering `kernel.perf_event_paranoid` to 1
did **not** lift the gate: any unprivileged system-wide `perf_event_open`
(`pid=-1`) returns `EACCES` on this kernel, with and without
`exclude_kernel`/`exclude_hv`; only task-attached self-measurement remains
unprivileged, and the i915 PMU rejects task attachment (`EINVAL`). The shell's
metrics goroutine therefore needs the capability (for example
`setcap cap_perfmon+ep` on the shell binary) or must accept invalid Intel
usage with an `Issue` — identity and temperature still arrive.

## Commits

| Commit | Subject |
|---|---|
| `df98cfa` | docs: plan the Intel GPU i915 PMU sampler |
| `28975e4` | feat: parse i915 PMU busy events and engine deltas |
| `ce2977f` | feat: sample Intel GPU usage from the i915 PMU |
| `c4b83f0` | docs: document the Intel GPU PMU sampler |
| `44b0560` | test: add the opt-in Intel GPU live check |
| `1eadf20` | fix: discover PMUs through symlinked event_source entries |
| `f091241` | docs: record the measured i915 permission gate |

`v0.5.0` is tagged on `f091241`. Zero new modules: `go.mod`/`go.sum` are
unchanged (gate below), the syscall shim uses the standard library
`syscall.SYS_PERF_EVENT_OPEN` with the 64-byte ABI-0 `perf_event_attr`.

## Gates

```
gofmt -l .                     → no output (clean)
go vet ./...                   → clean
go test -race -count=1 ./...   → ok  github.com/Nomadcxx/sysc-metrics 1.461s
git diff --exit-code -- go.mod go.sum → clean
```

Test additions: 19 `gpu_pmu_linux_test.go` parsing/mapping/delta tests, 10
`GPUSampler` fixture tests (two-sample validity, valid zero, read-error and
reset recovery, open failure, missing PMU, AMD passthrough, device
disappearance, idempotent `Close`, `ReadGPU` Intel contract), plus a
`GPUSampler` pass in `TestLinuxIntegration` and the opt-in live check
`TestIntelGPULive` (`SYSC_METRICS_INTEL_GPU_LIVE=1`).

## Live qualification on the target laptop

Target: Intel i915, PCI `0000:00:02.0`, device `0x3ea0` (Whiskey Lake iGPU),
`ssh -p 7777 nomadx@192.168.0.64`. Run from the transferred tree at
`/tmp/sysc-metrics-live` (commit `f091241`).

```
kernel release: 7.2.4-arch1-2
PMU i915 root=/sys/bus/event_source/devices/i915 type=13 BDF=""
  engine bcs0 config=0x1000
  engine rcs0 config=0x0
  engine vcs0 config=0x2000
  engine vecs0 config=0x3000
first sample:  PCIID=8086:3ea0 driver=i915 temp=0 tempValid=false usageValid=false
second sample: PCIID=8086:3ea0 fraction=0.021866545483376656 valid=true
--- PASS: TestIntelGPULive (0.51s)          [run with sudo: CAP_SYS_ADMIN]
```

- First sample invalid, second sample valid with a real non-zero fraction
  (~2.2% engine-busy on an idle desktop). A valid zero is accepted by the
  same path (fixture-tested); an idle valid zero would have been a success.
- Unprivileged run (uid 1000, both `perf_event_paranoid` 2 and 1): all four
  counters fail with `perf_event_open: permission denied`; the GPU stays in
  the snapshot with invalid usage, four per-engine `Issue` entries, identity
  and driver intact — never a fabricated zero.
- GPU temperature: this iGPU exposes no hwmon temperature, so `TempValid`
  is false — identical to the shipped v0.4.0 behavior and unchanged by this
  work.
- Device removal was exercised by fixture tests (live hot-unplug of an iGPU
  is not possible); PMU discovery through the kernel's symlinked
  `event_source/devices` entries was exercised live and is fixture-tested
  (`TestPMUDiscoversSymlinkedPMUDir`).
- Engine sets differ across kernels and generations; discovery is fully
  runtime-driven and nothing above is hard-coded.

## Boundary

Only the `i915` driver is supported. The `xe` driver may expose a different
PMU contract and is a separate driver-gated extension. No `intel_gpu_top`, no
`perf` binary, no daemon, no CGO, no new module was introduced.
