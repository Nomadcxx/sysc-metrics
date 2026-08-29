# sysc-metrics Core Counters Execution Handover

Date: 2026-08-29

## Assignment

Implement roadmap M0 and M1 for `sysc-metrics`: settle the public sampling contract, then build the
Linux CPU, memory, swap, uptime, filesystem, block-device, and network collectors. Work in the
`sysc-metrics` repository while another agent implements the stable multi-output bar in `sysc-shell`.

This lane has no dependency on `OutputHost`, Wayland, rendering, or Milestone 2 shell code. Do not edit
`sysc-shell` from this worktree.

## Authoritative implementation plan

Follow this file task by task:

```text
/home/nomadx/sysc-metrics/docs/plans/2026-08-29-core-counters.md
```

Repository-relative path:

```text
docs/plans/2026-08-29-core-counters.md
```

The plan contains the proposed public API, exact files, failing tests, commands, expected results, commit
boundaries, acceptance gate, and reconsider conditions. Use `superpowers:executing-plans` in the
implementation session. Stop after Task 1 for owner approval of the public M0 contract unless the owner
approves that contract during this handover review.

This handover supplies context. If it conflicts with the approved design or implementation plan, stop
and resolve the documents before writing code.

## Repository state before planning documents

| Item | State |
|---|---|
| Repository | `/home/nomadx/sysc-metrics` |
| Remote | `https://github.com/Nomadcxx/sysc-metrics.git` |
| Branch | `main` |
| Baseline commit | `ea6a54ca20ec55574be183836e019a685c4573a0` |
| Baseline remote state | local `main` matches `origin/main` |
| Production Go code | none |
| Go module | not created |
| Licence | BSD 3-Clause |

Check the current state instead of assuming the planning commit was pushed. Create implementation work
on branch `milestone/core-counters` in a dedicated worktree. Keep `main` for accepted documentation and
merged gates.

## Read these files first

Inside `sysc-metrics`:

1. `README.md` describes the repository boundary and development gates.
2. `docs/plans/2026-08-27-sysc-metrics-design.md` is the approved architecture and ownership design.
3. `docs/roadmap.md` defines M0 through M4 and the M1 exit gate.
4. `docs/plans/2026-08-29-core-counters.md` is the executable M0-M1 implementation plan.
5. `LICENSE` is the BSD 3-Clause project licence.

Relevant cross-repository context in `/home/nomadx/sysc-shell`:

- `AGENTS.md` records the Go, Linux, dependency, testing, and scope rules followed across this project;
- `docs/prior-art.md`, section `dgop`, records the inspected source, commit, licence, and extraction
  decision;
- `docs/plans/2026-08-26-sysc-shell-design.md` defines the `sysc-metrics` ownership boundary;
- `docs/plans/2026-08-26-development-orchestration.md`, section `Cross-repository lanes`, explains why
  metrics can proceed independently and when shell integration may begin;
- `docs/roadmap.md`, Milestone 3, lists the eventual shell consumers.

Do not edit those shell documents or pull their presentation types into this library.

## Decisions already made

### Product boundary

- `sysc-metrics` is a Go library, not a daemon or application.
- Linux is the public platform contract. Do not add portability interfaces or non-Linux implementations.
- The library owns collection, parsing, units, timestamps, counter deltas, availability, and source
  errors.
- Consumers own polling cadence, caching across widgets, presentation, alerts, filtering, and control
  actions.
- Version one does not include a CLI, TUI, JSON service, HTTP server, logging framework, or collector
  registry.

### Dependency policy

- M1 uses the Go standard library only.
- Use native `/proc`, `/sys`, and `syscall.Statfs` interfaces.
- Do not import or fork dgop. Its inspected commit `473bc52` is a behavioral reference for units, cursor
  behavior, and live results.
- Do not add a dependency without owner approval and evidence that the standard library cannot meet a
  named requirement.

### Public sampling model

- Stateless readers cover memory, uptime, and filesystem capacity.
- Stateful concrete samplers cover CPU, block devices, and network interfaces because rates require a
  previous observation.
- Samplers have one sequential owner. They do not start goroutines, schedule polling, or add mutexes.
- Each snapshot has `CollectedAt time.Time`.
- Byte values use `uint64`; durations use `time.Duration`; derived ratios and rates use `float64` plus a
  validity boolean.
- First observations and one sample after a reset have invalid derived values. Valid zero remains
  distinguishable from unavailable.
- Removed devices disappear. Reappearing devices start a new baseline. Mount IDs, block major/minor
  numbers, and network `ifindex` values provide runtime identity; display names remain attributes.
- Returned slices are caller-owned immutable snapshots.

The exact proposed structs and function signatures are in the implementation plan under `Contract
decisions`. Treat that section as the M0 approval surface.

### Errors and trust boundaries

- A missing or unusable primary source returns an error.
- One bad mount, device, optional load source, or frequency source produces a typed `Issue`; other valid
  entries remain available.
- Bound scanner tokens and reject malformed, missing, duplicate, negative, non-finite, and overflowing
  fields at the parser boundary.
- Wrap errors with the source path and field or entity name.
- Never turn malformed input into a rate spike or discard healthy sibling devices.

### Linux semantics that must survive review

- CPU total uses `/proc/stat` fields `user` through `steal`. Exclude `guest` and `guest_nice` because the
  kernel already includes them in `user` and `nice`.
- CPU idle is `idle+iowait`; utilization is the busy delta divided by total delta.
- `/proc/diskstats` sector counters use 512 bytes per sector, independent of device block size.
- Filesystem used bytes use `(Blocks-Bfree)*Bsize`; available bytes use `Bavail*Bsize`. Reserved blocks
  make `used` different from `total-available`.
- Mountinfo decoding handles the kernel escapes for space, tab, newline, and backslash.
- The library reports filesystem types and does not choose which mounts a widget should show. It
  preserves overmounts that share a mount point by their distinct mount IDs.
- Block samplers key state by major/minor device number, not name. Network samplers key state by
  `ifindex`, not interface name.
- Results use deterministic sorting so unchanged host state produces deterministic snapshots.

## Work sequence and ownership gate

The plan has eight tasks:

1. Create the module and public M0 contract, then stop for owner review.
2. Implement memory, swap, and uptime.
3. Implement CPU parsing, load, frequency, delta state, and resets.
4. Implement mountinfo and filesystem capacity.
5. Implement block-device counters and rates.
6. Implement network-interface counters and rates.
7. Add live Linux invariants and usage documentation.
8. Review scope, run the final gate, and write the completion handover.

Do not parallelize Tasks 1 through 3. Task 1 fixes the API and Task 3 establishes the stateful-sampler
rules reused by later rate collectors. After Task 3 passes review, separate agents may implement
filesystem, block, and network tasks in isolated worktrees only if each keeps the approved public
contract. One integration owner merges and reruns the full race gate.

The simplest execution is one agent following all eight tasks. The codebase starts empty, so extra lanes
will save little time before Task 3 and create merge overhead.

## Required development method

- Use `superpowers:executing-plans` and follow the TDD steps in the implementation plan.
- Write one focused failing check before each parser or state transition.
- Use inline fixture strings. Do not create a fixture framework or public fake filesystem.
- Keep Linux I/O in `_linux.go` files and pure parsing/delta logic callable from package tests.
- Use checked additions and multiplications before converting kernel counters or units.
- Preserve partial data when one identifiable entity fails.
- Run the focused test first, then `go test -race -count=1 ./...` and `go vet ./...` at each task gate.
- Commit after each passing task with the plan's commit boundary.
- Ask before running CodeRabbit or another token-intensive external reviewer.

## Required proof

Pure tests must cover:

- parser bounds, malformed values, missing and duplicate required fields;
- numeric, byte, and duration overflow;
- first sample, valid zero, normal delta, counter regression, and non-positive elapsed time;
- device/core/interface addition, removal, reset, and reappearance;
- partial failure without loss of healthy sibling data;
- deterministic ordering and returned-slice ownership;
- exact CPU, disk-sector, filesystem-capacity, and mount-escape semantics.

The Linux integration test must use the public API and assert portable invariants. It must not record or
expect a machine's core count, memory size, disks, interfaces, mount count, frequencies, or rates. Empty
block and network sets are valid. Missing cpufreq, physical disks, non-loopback networking, battery
hardware, and UPower are valid environments for M1.

Before the completion handover, run fresh:

```bash
go mod tidy -diff
go test -race -count=1 ./...
go vet ./...
go list -m all
git diff --check main...HEAD
git status --short
```

`go list -m all` must show only `github.com/Nomadcxx/sysc-metrics`. A standard-library-only module should
not create `go.sum`.

## Non-goals

Do not add:

- thermal, battery, UPS, UPower, GPU, or process collectors in this branch;
- audio, MPRIS, power controls, SMART, quotas, recursive directory sizing, filesystem indexing, or
  storage administration;
- a daemon, CLI, TUI, HTTP API, JSON model, logging package, cache service, or background poller;
- shell widgets, formatted strings, colors, thresholds, graph history, icons, or notifications;
- cross-platform build layers, a collector plugin system, or a single-implementation interface;
- an exported test root, fake filesystem, clock interface, or generalized rate engine without a second
  concrete need.

## Stop and ask the owner

Stop before proceeding if:

- Task 1's public types need a breaking change after approval;
- the standard library cannot read a required value correctly;
- a collector appears to require its own goroutine or persistent daemon;
- a kernel interface disagrees with the plan's unit or delta rule;
- correct partial errors require discarding otherwise valid data;
- tests require privileged access, fixed hardware, or a public injection API;
- implementation needs a dependency, CGO, D-Bus, shell code, or work from M2 and later.

## Completion handoff

At Task 8 create:

```text
docs/plans/2026-08-29-core-counters-completion-handover.md
```

Record the implementation branch and commits, fresh command results, supported sources, public API
changes, live gaps, and M2 power/thermal prerequisites. Stop for review. Do not merge, tag, push, start
power/thermal, or edit `sysc-shell` unless the owner directs it.

## Resume commands

Start read-only:

```bash
cd /home/nomadx/sysc-metrics
git status --short --branch
git log -5 --oneline --decorate
git remote -v
sed -n '1,240p' docs/plans/2026-08-27-sysc-metrics-design.md
sed -n '1,260p' docs/roadmap.md
sed -n '1,420p' docs/plans/2026-08-29-core-counters.md
```

Confirm the owner approved the M0 contract, then create or enter the dedicated
`milestone/core-counters` worktree and execute the plan from Task 1.
