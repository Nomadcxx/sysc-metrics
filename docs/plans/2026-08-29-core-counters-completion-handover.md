# Core Counters Completion Handover

Date: 2026-08-29

## Branch

- Branch: `milestone/core-counters`
- Base branch: `main`
- Base commit: `b10a07ab2ce802786d18844f5f6fe39ae2213d27`
- Implementation head before this handover commit: `7ce7b471b778b848bd6b0142b6a6a361bfeaa1ab`

## Delivered

The branch defines and implements the M0/M1 public sampling contract for Linux:

- CPU aggregate and per-core usage, load, and best-effort frequency;
- memory, swap, and uptime;
- mounted-filesystem capacity from mountinfo and statfs;
- block-device counters, busy time, and rates;
- network-interface counters and rates.

Readers use `/proc`, `/sys`, and `syscall.Statfs` with no third-party dependencies. Stateful samplers
retain prior timestamps and counters, use kernel identities for lifecycle tracking, report invalid first
and discontinuous derived values, and preserve valid zero values. Partial entity failures return `Issue`
values with the healthy data.

## Public API

The public types and constructors are defined in `metrics.go`. Byte fields use `uint64`, durations use
`time.Duration`, and derived values use `float64` with validity flags. Samplers have sequential ownership;
the library does not schedule polling or start goroutines. Snapshot slices are caller-owned.

## Verification

The following fresh commands passed before this handover was written:

```text
go mod tidy -diff
go test -race -count=1 ./...
go vet ./...
go list -m all
git diff --check main...HEAD
git status --short
```

The module list contained only `github.com/Nomadcxx/sysc-metrics`. The live integration test checks
portable Linux invariants and does not require battery, cpufreq, physical-disk, or non-loopback hardware.

## Known gaps and M2 prerequisites

M1 does not collect thermal, battery, UPS, GPU, or process metrics. It does not provide a daemon, CLI,
polling service, presentation policy, or controls. M2 needs thermal-zone parsing, UPower DisplayDevice
collection, sysfs power-supply fallback, source-transition handling, multi-battery tests, and
suspend/resume qualification on real hardware.

Do not merge, tag, push, start M2, or integrate `sysc-shell` without owner review.
