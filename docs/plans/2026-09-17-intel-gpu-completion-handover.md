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
`v0.5.1`, which supersedes `v0.5.0` (the audit below found `v0.5.0`'s counter
descriptors were not close-on-exec; do not pin `v0.5.0`).

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

## Audit and re-qualification

An independent audit (commission:
`2026-09-17-intel-gpu-audit-commission.md`; report:
`2026-09-17-intel-gpu-audit-report.md`) reproduced nine of ten claim groups
and filed two defects, both fixed on `main`:

- **F1 (Moderate).** `PERF_FLAG_FD_CLOEXEC` had been written into the
  `perf_event_attr` bitfield (offset 40, bit 3 = `exclusive`) while the
  syscall's flags argument received `0`: counter descriptors were **not**
  close-on-exec and every event requested exclusive use. On a hybrid
  Intel+NVIDIA machine the counters leaked into exec'd `nvidia-smi`
  children — exactly what this document promised could not happen. Fixed in
  `1e2e806`: the flag rides the `perf_event_open` flags argument,
  `attr.Flags` stays zero. Regression tests: flag routing (unprivileged),
  syscall-flags CLOEXEC end-to-end (unprivileged), and the production path
  `TestAuditF1OpenGPUCounterIsCloseOnExec` (privileged).
- **F2 (Low).** The linkless-PMU fallback was all-or-nothing on the presence
  of any linked candidate. It now applies per candidate: the single unclaimed
  GPU carrying the driver maps when exactly one unclaimed linkless
  driver-named candidate remains. Ambiguity still never maps.

Re-qualification on the target laptop with the fixed code (full package under
sudo, commit `1e2e806`): every test passes, the production-path CLOEXEC test
passes, and `TestIntelGPULive` reports first sample invalid and second sample
`fraction=0.11954942390818976 valid=true` — the counters open without the
accidental `exclusive` attribute.

## Boundary

Only the `i915` driver is supported. The `xe` driver may expose a different
PMU contract and is a separate driver-gated extension. No `intel_gpu_top`, no
`perf` binary, no daemon, no CGO, no new module was introduced.

## Commission coverage

How this work addresses each section of the commissioning handover.

| Commission requirement | Where it landed |
|---|---|
| Truthful Intel i915 utilization | `GPUSampler` (`metrics.go`, `gpu_linux.go`, `gpu_pmu_linux.go`) from i915 PMU engine-busy deltas; live-qualified above |
| Preserve `GPU`/`GPUUsage`/`GPUSnapshot`/`ReadGPU` contract | Types untouched; `ReadGPU` body unchanged except the shared `readGPUs` refactor; `TestReadGPUKeepsIntelUsageInvalid` and the `metrics_test.go` compile assertions pin the API |
| Smallest stateful sampler, recommended shape | `NewGPUSampler`/`Sample`/`Close` exactly as specified; synchronous, one sequential owner, explicit fd lifetime, no goroutine |
| Shell stays on `v0.4.0`; no shell-side reader, `perf`, or `replace` | Nothing in `sysc-shell` was touched; the migration contract above waits for this tag |
| Probe the kernel interface, record evidence | Read-only ssh probes: PMU type 13, `format/config:0-20`, four `*-busy` events with ns units, no PMU→PCI link, `perf_event_paranoid` 2 and 1, open/read variants; no sysctl change or install during probing |
| Amend the thermal/GPU design + executable plan | Amendment 2026-09-17 in `2026-09-02-thermal-and-gpu-design.md`; `2026-09-17-intel-gpu-pmu.md` |
| Failing tests first (parsing, delta, semantics) | `gpu_pmu_linux_test.go` written and watched failing before `gpu_pmu_linux.go`; sampler fixture tests watched failing before the integration commit; the symlink-discovery bug was regression-caught by a failing test before the fix (`1eadf20`) |
| PMU→GPU mapping via device link, ambiguity unavailable | `mapGPUPMUs` with the measured no-link fallback rule; `TestPMUMaps*`, `TestPMUDoesNotMap*` |
| Max-of-engines aggregation, never summed, no new UI field | `maxBusyFraction`; design amendment "Aggregation"; no new public field |
| No `intel_gpu_top`/`perf`/daemon/CGO/dependency/frequency heuristic | `go.mod`/`go.sum` unchanged (gate); stdlib `syscall` shim only |
| First-sample invalid, second valid, valid zero | `TestGPUSampler*` first/second-sample and zero tests; live PASS |
| Decrease/reset/short read/non-positive/impossible rebaseline | `TestPMUEngineBusy*`, `TestGPUSamplerReadErrorRebaselinesAndRecovers`, `...CounterDecrease...`, `...ImpossibleDelta...` |
| Missing PMU/events, `EPERM`/`EACCES` keep identity + `Issue`, never zero | `TestGPUSamplerMissingPMUIssuesAndKeepsIdentity`, `TestGPUSamplerOpenFailureKeepsIdentityAndIssues`; live unprivileged runs recorded |
| Failed engine → no fabricated device-wide percentage (conservative rule) | `sampleGPU` invalidates the whole GPU on any engine failure; documented in the amendment "Aggregation" |
| Removal closes/drops state, reappearance fresh | `TestGPUSamplerDeviceDisappearanceDropsState`; idempotent `Close` closes every counter (`TestGPUSamplerCloseIsIdempotentAndClosesCounters`) |
| AMD/NVIDIA behavior, ordering, `nvidia-smi` injection stay green | Existing `gpu_linux_test.go` suite untouched and passing; `TestGPUSamplerPassesAMDThroughWithoutPMU`; `TestLinuxIntegration` |
| Fraction finite in 0..1 when valid | `assertFraction` in integration + live checks; `TestPMUEngineBusyFractionStaysBounded` |
| Opt-in live check with recorded evidence | `TestIntelGPULive`; outputs recorded in this document |
| Publish next additive release, not a moving branch | Tag `v0.5.0` on `f091241` |
| Completion handover with hashes, gates, tag, migration contract | This document |

Deviations worth noting: the conventions file named in the commission's reading
list does not exist in this repository (conventions were taken from `README.md`
and the existing code), and the shell consumer file was outside this
commission's readable workspace, so the shell ownership contract was taken
from the commissioning handover's own text.
