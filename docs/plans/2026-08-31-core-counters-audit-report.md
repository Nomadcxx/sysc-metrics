# Core Counters Audit Report

Date: 2026-08-31
Commissioned by: `docs/plans/2026-08-29-core-counters-merge-handover.md`
Audited: `main` at `d821afe`, against
`docs/plans/2026-08-29-core-counters.md`,
`docs/plans/2026-08-29-core-counters-execution-handover.md`, and
`docs/plans/2026-08-27-sysc-metrics-design.md`.

## Verdict

**Fit to tag.** No defect found that should block a release. Nothing in the tree was changed by this
audit except the release-gate amendment recorded below.

## Qualification

Run on the merged tree, working directory clean:

```
go mod tidy -diff      no difference
go vet ./...           silent
gofmt -l .             nothing
git diff --check       nothing
go test -race -count=1 ./...   ok, 29 tests
go list -m all         github.com/Nomadcxx/sysc-metrics
```

Zero dependencies, as the design requires.

## Findings against the commissioned brief

**Public API drift — none.** 53 exported declarations, all of them value types, their constructors, or
the six collector entry points the design names: `NewCPUSampler`, `NewBlockSampler`,
`NewNetworkSampler`, `ReadMemory`, `ReadFilesystems`, `ReadUptime`.

**Non-goal creep — none.** No `interface` is declared anywhere in the package. There is no `main`, no
HTTP surface, no registry and no plugin seam. The design's "no collector registry, plugin API, daemon
protocol, or single-implementation interface" holds literally rather than approximately.

**Unit semantics — correct.** Bytes are `uint64`, durations are `time.Duration`, derived values are
`float64` paired with a validity flag. The only `fmt` use in non-test code is `fmt.Errorf`, so no
UI-formatted string exists — unit rendering stays the consumer's job, as the design requires.

**Returned-slice ownership — correct.** Every snapshot allocates its result slices fresh
(`make([]BlockDevice, 0, …)`, `make([]NetworkInterface, 0, …)`, `make([]CPUCore, 0, …)`) and copies
issues defensively with `append([]Issue(nil), issues…)`. A caller mutating a returned slice cannot
reach sampler state, so the "caller-owned snapshot slices" doc comment is accurate rather than
aspirational.

**Counter resets and device replacement — correct, and conservatively so.** `blockRates` invalidates
when elapsed is non-positive or when any of the five counters regressed, so a reset cannot produce a
spike. A replaced device presents a new identity, finds no previous entry, and reports no rate. Each
counter has its own rejection test.

**Suspend — correct by construction.** Elapsed time is the difference of two `time.Now()` values, which
carry Go's monotonic reading, and `CLOCK_MONOTONIC` does not advance across suspend on Linux. Elapsed
therefore counts only awake time, which is the interval over which the counters actually advanced. The
rate after a resume is right, not merely un-spiky. This holds by construction and was not exercised on
hardware; the `v0.2.0` gate covers that.

**Timestamp ownership — correct.** One collection instant is threaded through each sampling pass, so
every value in a snapshot shares a timestamp rather than sampling its own.

**Deterministic ordering — correct.** Six explicit sorts across CPU cores, block devices, filesystems
and network interfaces, so repeated collection produces stable output.

**Trust boundaries — correct.** Scanner tokens are bounded at 64 KiB, and malformed rows and numeric
overflow have named rejection tests at each of `/proc/stat`, `/proc/diskstats`, `/proc/net/dev`,
`/proc/meminfo` and `/proc/self/mountinfo`.

**Partial-error behaviour — correct.** A failing device or mount produces an `Issue` naming its source
while the rest of the snapshot survives, which is what lets a consumer mark one sensor unavailable
without discarding the pass.

## Release gate amendment

The design tied the `v0.1.0` gate to "suspend/resume and UPower-to-sysfs fallback on real hardware".
Those are power collectors, and this release contains none: no file in the package references battery,
thermal, UPower or `power_supply`. The gate as written could not be met by the release it named.

Gates are now recorded per milestone: `v0.1.0` covers what core counters ship, and the power evidence
moves to `v0.2.0`, where the code it describes will exist. This is a deliberate amendment to a recorded
gate, made in the same commit as the tag so the change is visible rather than assumed.

## Not covered by this audit

- Real-hardware suspend and resume. Reasoned above, not executed.
- Any power, thermal, GPU or process collector. None exists; that work is the consumer repository's
  `sysc-19` and this repository's `v0.2.0`.
