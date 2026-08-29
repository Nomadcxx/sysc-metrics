# sysc-metrics Core Counters Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Build the dependency-free Linux telemetry library needed by the first `sysc-shell` system-monitor widgets.

**Architecture:** The root `metrics` package reads Linux kernel interfaces directly and returns typed,
renderer-neutral snapshots. Stateful CPU, block-device, and network samplers retain only the previous
counters needed to calculate rates; stateless memory, uptime, and filesystem readers return one snapshot
per call. Pure parsers accept `io.Reader` or values so tests need no fake filesystem or collector interface.

**Tech Stack:** Go 1.26, Linux `/proc` and `/sys`, Go standard library, `syscall.Statfs`, table tests, and
Linux invariant tests. No third-party dependency, daemon, CLI, CGO, D-Bus, or shell presentation code.

---

Status: Proposed M0 contract and executable M1 plan. Obtain owner approval of Task 1's public types before
starting collector implementation.

## Fixed scope

This plan implements `sysc-metrics` roadmap M0 and M1:

- aggregate and per-core CPU usage, load, and best-effort current frequency;
- memory, swap, and uptime;
- mounted-filesystem capacity;
- block-device cumulative counters and rates;
- network-interface cumulative counters and rates;
- parser fixtures, reset/removal tests, race tests, and Linux invariant checks.

Stop after the M1 gate. Thermal zones, UPower, sysfs battery fallback, GPU metrics, process metrics, a
daemon, CLI, HTTP server, formatting, alerts, polling policy, and shell integration belong to later work.

## Contract decisions

1. The module path is `github.com/Nomadcxx/sysc-metrics`; the package name is `metrics`.
2. Linux is the compile-time and runtime platform. Linux readers use `_linux.go` files. Do not create a
   portability interface or non-Linux stubs.
3. M1 uses only the Go standard library. Use `syscall.Statfs` instead of adding `x/sys` for one call.
4. Public readers use the real `/proc` and `/sys` paths. Tests call unexported parser and delta functions;
   do not add public root-path options or a filesystem interface.
5. Every snapshot records one `time.Time` in `CollectedAt`. Stateful samplers compare its monotonic
   component through `time.Sub`.
6. Byte fields use `uint64` and include `Bytes` in their names. Durations use `time.Duration`. CPU usage
   and rates use `float64`; a separate validity boolean distinguishes a real zero from an unavailable
   first or discontinuous sample.
7. CPU utilization uses the first eight Linux `/proc/stat` counters. It excludes `guest` and
   `guest_nice`, which Linux already includes in `user` and `nice`. Idle time is `idle+iowait`.
8. `/proc/diskstats` sector counts use the kernel ABI's fixed 512-byte sector unit. Do not substitute a
   device's logical block size.
9. A sampler's first observation has invalid derived values. A new device also starts invalid. Counter
   regression marks that entity's derived values invalid for one observation and installs the new
   counters as its next baseline. A removed entity disappears from the snapshot and sampler state.
10. Samplers support sequential use by one polling owner. They are not safe for concurrent calls and do
    not add mutexes. Returned slices belong to the caller and the sampler never mutates them later.
11. An unreadable or structurally unusable primary source returns an error. A bad individual device,
    mount, optional load file, or frequency file produces an `Issue` while other valid data remains.
12. Results use deterministic ordering: CPU cores by numeric ID, filesystems by mount point then mount
    ID, block devices by name then major/minor number, and network interfaces by name then `ifindex`.
13. Library code does not clamp valid counter rates or format display strings. `sysc-shell` owns view
    policy such as meter clamping, decimal precision, and units shown to users.

The M0 public value types are:

```go
package metrics

import "time"

type Issue struct {
	Source string
	Err    error
}

func (i Issue) Error() string
func (i Issue) Unwrap() error

type Capacity struct {
	TotalBytes     uint64
	UsedBytes      uint64
	AvailableBytes uint64
}

type CPUUsage struct {
	Fraction float64
	Valid    bool
}

type CPUCore struct {
	ID             int
	Usage          CPUUsage
	FrequencyHz    uint64
	FrequencyValid bool
}

type CPUSnapshot struct {
	CollectedAt time.Time
	Usage       CPUUsage
	Cores       []CPUCore
	Load1       float64
	Load5       float64
	Load15      float64
	LoadValid   bool
	Issues      []Issue
}

type MemorySnapshot struct {
	CollectedAt time.Time
	Memory      Capacity
	Swap        Capacity
}

type UptimeSnapshot struct {
	CollectedAt time.Time
	Uptime      time.Duration
}

type Filesystem struct {
	MountID    uint64
	MountPoint string
	Source     string
	Type       string
	ReadOnly   bool
	Capacity   Capacity
}

type FilesystemSnapshot struct {
	CollectedAt time.Time
	Filesystems []Filesystem
	Issues      []Issue
}

type BlockRates struct {
	ReadBytesPerSecond       float64
	WriteBytesPerSecond      float64
	ReadOperationsPerSecond  float64
	WriteOperationsPerSecond float64
	BusyFraction             float64
	Valid                    bool
}

type BlockDevice struct {
	Name            string
	Major           uint32
	Minor           uint32
	ReadBytes       uint64
	WriteBytes      uint64
	ReadOperations  uint64
	WriteOperations uint64
	Busy            time.Duration
	Rates           BlockRates
}

type BlockSnapshot struct {
	CollectedAt time.Time
	Devices     []BlockDevice
	Issues      []Issue
}

type NetworkRates struct {
	ReceiveBytesPerSecond    float64
	TransmitBytesPerSecond   float64
	ReceivePacketsPerSecond  float64
	TransmitPacketsPerSecond float64
	Valid                    bool
}

type NetworkInterface struct {
	Name             string
	Index            uint32
	ReceiveBytes     uint64
	ReceivePackets   uint64
	ReceiveErrors    uint64
	ReceiveDropped   uint64
	TransmitBytes    uint64
	TransmitPackets  uint64
	TransmitErrors   uint64
	TransmitDropped  uint64
	Rates            NetworkRates
}

type NetworkSnapshot struct {
	CollectedAt time.Time
	Interfaces  []NetworkInterface
	Issues      []Issue
}

type CPUSampler struct { /* unexported state */ }
type BlockSampler struct { /* unexported state */ }
type NetworkSampler struct { /* unexported state */ }

func NewCPUSampler() *CPUSampler
func (s *CPUSampler) Sample() (CPUSnapshot, error)
func ReadMemory() (MemorySnapshot, error)
func ReadUptime() (UptimeSnapshot, error)
func ReadFilesystems() (FilesystemSnapshot, error)
func NewBlockSampler() *BlockSampler
func (s *BlockSampler) Sample() (BlockSnapshot, error)
func NewNetworkSampler() *NetworkSampler
func (s *NetworkSampler) Sample() (NetworkSnapshot, error)
```

If review changes a name or field, update this section before implementation. Do not let implementation
silently redefine the contract.

### Task 1: Establish the module and M0 public contract

**Files:**

- Create: `go.mod`
- Create: `metrics.go`
- Create: `metrics_test.go`
- Modify: `README.md`

**Step 1: Create the implementation worktree**

From a clean `main`, create branch `milestone/core-counters` in a dedicated worktree. Do not implement on
`main` and do not reuse the Milestone 2 shell worktree.

**Step 2: Write the public-contract test**

Add tests that construct every public value type, verify `Issue.Error()` includes its source and wrapped
error, verify `errors.Is` works through `Issue.Unwrap`, and confirm the zero value of each derived-value
validity flag is false.

Use a compile-time API check so accidental signature changes fail in this package:

```go
var (
	_ func() *CPUSampler                              = NewCPUSampler
	_ func(*CPUSampler) (CPUSnapshot, error)          = (*CPUSampler).Sample
	_ func() (MemorySnapshot, error)                  = ReadMemory
	_ func() (UptimeSnapshot, error)                  = ReadUptime
	_ func() (FilesystemSnapshot, error)              = ReadFilesystems
	_ func() *BlockSampler                            = NewBlockSampler
	_ func(*BlockSampler) (BlockSnapshot, error)      = (*BlockSampler).Sample
	_ func() *NetworkSampler                          = NewNetworkSampler
	_ func(*NetworkSampler) (NetworkSnapshot, error)  = (*NetworkSampler).Sample
)
```

**Step 3: Run the test and confirm the expected failure**

Run:

```bash
go test -count=1 ./...
```

Expected: failure because `go.mod` and the public API do not exist.

**Step 4: Add the minimum contract implementation**

Create `go.mod` with:

```go
module github.com/Nomadcxx/sysc-metrics

go 1.26
```

Add the public types and signatures from **Contract decisions** to `metrics.go`. Constructors may return
empty samplers and reader methods may return `errors.New("not implemented")` only for this contract
commit. Add doc comments that state byte units, validity semantics, sequential sampler ownership, and
snapshot-slice ownership.

Update the README status to say that M0 contract work is in progress. Do not claim that collectors work.

**Step 5: Run the M0 checks**

Run:

```bash
gofmt -w metrics.go metrics_test.go
go mod tidy -diff
go test -race -count=1 ./...
go vet ./...
go list -m all
```

Expected: all checks pass; `go list -m all` prints only `github.com/Nomadcxx/sysc-metrics`; no `go.sum`
appears.

**Step 6: Stop for the public API gate**

Show the owner the contract diff. Continue only after the owner approves the field names, units,
first-sample behavior, partial-error model, and sequential sampler ownership.

**Step 7: Commit**

```bash
git add go.mod metrics.go metrics_test.go README.md
git commit -m "feat: define metrics sampling contract"
```

### Task 2: Implement memory, swap, and uptime

**Files:**

- Create: `memory_linux.go`
- Create: `memory_linux_test.go`
- Create: `uptime_linux.go`
- Create: `uptime_linux_test.go`
- Modify: `metrics.go`

**Step 1: Write failing table tests for `/proc/meminfo`**

Call an unexported `parseMeminfo(io.Reader, time.Time)` with inline strings. Cover:

- valid `MemTotal`, `MemAvailable`, `SwapTotal`, and `SwapFree` values in `kB`;
- zero swap;
- arbitrary field ordering and ignored unknown fields;
- missing required fields;
- duplicate required fields;
- non-`kB` units, negative values, malformed numbers, and multiplication overflow;
- `MemAvailable > MemTotal` and `SwapFree > SwapTotal`.

Assert `UsedBytes = TotalBytes - AvailableBytes` and `kB * 1024` conversion.

**Step 2: Write failing uptime tests**

Call `parseUptime(io.Reader, time.Time)` and cover whole and fractional seconds, missing fields, negative
values, non-numbers, and `time.Duration` overflow. Parse the first token with
`time.ParseDuration(token + "s")`; do not pass through floating-point seconds.

**Step 3: Run the focused tests and confirm failure**

```bash
go test -count=1 -run 'TestParse(Meminfo|Uptime)' ./...
```

Expected: compile failure because the parsers do not exist.

**Step 4: Implement the parsers and Linux readers**

`ReadMemory` opens `/proc/meminfo`, captures one timestamp, and delegates to `parseMeminfo`.
`ReadUptime` does the same with `/proc/uptime`. Bound scanner tokens to 1 MiB. Wrap open, scan, and parse
errors with the source path and field name. Do not expose raw kernel pages or UI percentages.

Remove the corresponding temporary `not implemented` bodies from `metrics.go`; keep public type
definitions there.

**Step 5: Run checks**

```bash
gofmt -w memory_linux.go memory_linux_test.go uptime_linux.go uptime_linux_test.go metrics.go
go test -race -count=1 ./...
go vet ./...
```

Expected: all checks pass.

**Step 6: Commit**

```bash
git add memory_linux.go memory_linux_test.go uptime_linux.go uptime_linux_test.go metrics.go
git commit -m "feat: collect memory and uptime"
```

### Task 3: Implement CPU sampling

**Files:**

- Create: `cpu_linux.go`
- Create: `cpu_linux_test.go`
- Modify: `metrics.go`

**Step 1: Write failing `/proc/stat` parser tests**

Test an unexported `parseCPUStat(io.Reader)` with inline fixtures. Require one aggregate `cpu` row and
accept numeric `cpuN` rows. Cover malformed numbers, too few fields, duplicate labels, a missing
aggregate, oversized lines, sparse core IDs, and guest counters that must not enter the total twice.

**Step 2: Write failing delta tests**

Test a pure `cpuUsage(previous, current cpuTimes) CPUUsage`. Cover:

- a known busy/idle ratio;
- a legitimate zero-usage interval with `Valid=true`;
- identical totals;
- each relevant counter regressing;
- `idle+iowait` overflow and total overflow.

Invalid or zero-total deltas return the zero `CPUUsage`. Do not clamp a valid ratio.

**Step 3: Write failing sampler-state tests**

Exercise an unexported `sampleCPU(parsedCPU, load, frequencies, at)` path. Prove:

- first aggregate and core usage values are invalid;
- the second sample produces expected ratios;
- a new core starts invalid while existing cores remain valid;
- a removed core disappears from both output and retained state;
- a reset core is invalid for one sample and valid after the following monotonic sample;
- cores remain numerically sorted;
- one malformed or unavailable core frequency does not remove another core.

**Step 4: Write failing load and frequency tests**

Parse the first three `/proc/loadavg` fields as finite, non-negative `float64` values. Parse each
`/sys/devices/system/cpu/cpuN/cpufreq/scaling_cur_freq` value as kHz and multiply by 1000 with an overflow
check. Missing frequency files mean `FrequencyValid=false` without an issue; malformed or unreadable
existing files add an `Issue` for that core.

**Step 5: Run the focused tests and confirm failure**

```bash
go test -count=1 -run 'Test(CPU|ParseCPU|ParseLoad|ParseFrequency)' ./...
```

Expected: compile failure for missing CPU implementation.

**Step 6: Implement `CPUSampler`**

Open `/proc/stat` as the required source. Read `/proc/loadavg` and core frequency files as optional
sources. Use the first eight CPU counters and define:

```text
total = user + nice + system + idle + iowait + irq + softirq + steal
idle  = idle + iowait
busy fraction = (delta(total) - delta(idle)) / delta(total)
```

Use checked additions. Install current counters as the next baseline after each successfully parsed
sample, including a discontinuity. Keep no raw data for removed cores.

**Step 7: Run checks**

```bash
gofmt -w cpu_linux.go cpu_linux_test.go metrics.go
go test -race -count=1 ./...
go vet ./...
```

Expected: all checks pass.

**Step 8: Commit**

```bash
git add cpu_linux.go cpu_linux_test.go metrics.go
git commit -m "feat: sample CPU usage and load"
```

### Task 4: Implement mounted-filesystem capacity

**Files:**

- Create: `filesystem_linux.go`
- Create: `filesystem_linux_test.go`
- Modify: `metrics.go`

**Step 1: Write failing mountinfo tests**

Parse `/proc/self/mountinfo` around the required ` - ` separator. Cover:

- source, mount point, filesystem type, and `ro`/`rw` options;
- valid kernel escapes `\\040`, `\\011`, `\\012`, and `\\134`;
- optional pre-separator fields;
- malformed separators, short rows, invalid mount IDs, and invalid escapes;
- two overmounted filesystems with the same decoded mount point, both preserved under distinct mount IDs;
- deterministic sorting by decoded mount point then mount ID.

Do not filter pseudo, network, overlay, removable, or read-only filesystems. The caller owns selection
policy and has the filesystem type needed to apply it.

**Step 2: Write failing capacity tests**

Test an unexported conversion from `syscall.Statfs_t` or a small internal numeric value. Require:

```text
total     = Blocks * Bsize
used      = (Blocks - Bfree) * Bsize
available = Bavail * Bsize
```

Cover negative block size, `Bfree > Blocks`, `Bavail > Blocks`, and multiplication overflow. Used space
must include blocks reserved from unprivileged callers; do not calculate it as `total-available`.

**Step 3: Write the partial-error test**

Call an unexported `readFilesystems(mountinfo, statfsFunc, at)` where one mount's injected stat call
fails. Assert that valid mounts remain, the failed mount does not, and one `Issue` names its decoded
mount point.

**Step 4: Run the focused tests and confirm failure**

```bash
go test -count=1 -run 'Test(Mount|Filesystem|Statfs)' ./...
```

Expected: compile failure for missing filesystem implementation.

**Step 5: Implement the reader**

Open `/proc/self/mountinfo`, parse bounded lines, and call `syscall.Statfs` for each decoded mount point.
Treat failure to open or structurally parse mountinfo as the returned error. Treat an individual statfs
failure as an `Issue`. Do not add `golang.org/x/sys`.

**Step 6: Run checks**

```bash
gofmt -w filesystem_linux.go filesystem_linux_test.go metrics.go
go test -race -count=1 ./...
go vet ./...
```

Expected: all checks pass.

**Step 7: Commit**

```bash
git add filesystem_linux.go filesystem_linux_test.go metrics.go
git commit -m "feat: collect filesystem capacity"
```

### Task 5: Implement block-device counters and rates

**Files:**

- Create: `block_linux.go`
- Create: `block_linux_test.go`
- Modify: `metrics.go`

**Step 1: Write failing `/proc/diskstats` parser tests**

Use fixtures for the documented Linux fields through time doing I/O. Cover major/minor numbers, whole
disks, partitions, device-mapper names, extra trailing fields, short rows, malformed numbers, duplicate
major/minor identities, and sector or millisecond conversion overflow.

Assert sectors convert with the fixed factor `512`, and milliseconds convert to `time.Duration` without
overflow.

**Step 2: Write failing rate tests**

Exercise pure delta code with fixed timestamps. Cover:

- expected byte, operation, and busy rates across a two-second interval;
- a real zero rate with `Valid=true`;
- first sample invalid;
- zero and negative elapsed time invalid without replacing a valid baseline;
- regression of each tracked counter invalid for one sample;
- new, removed, and reappearing devices;
- the same name under a different major/minor identity starting a fresh baseline;
- deterministic ordering by name then major/minor identity.

Do not clamp `BusyFraction`; the view may clamp a meter while retaining the measured value.

**Step 3: Write the malformed-device test**

A malformed nonblank row with an identifiable device becomes an `Issue` while valid rows remain. An
empty file is a valid empty snapshot; a file containing data rows but no valid row returns an error.

**Step 4: Run the focused tests and confirm failure**

```bash
go test -count=1 -run 'Test(Block|ParseDiskstats)' ./...
```

Expected: compile failure for missing block implementation.

**Step 5: Implement `BlockSampler`**

Read `/proc/diskstats`, retain one baseline per major/minor device identity, and return fresh value
slices. Replace the baseline after a counter reset, drop removed devices, and retain the old baseline
when elapsed time is non-positive. Treat a name as an attribute, not identity. Do not classify physical
versus virtual devices or partitions in M1.

**Step 6: Run checks**

```bash
gofmt -w block_linux.go block_linux_test.go metrics.go
go test -race -count=1 ./...
go vet ./...
```

Expected: all checks pass.

**Step 7: Commit**

```bash
git add block_linux.go block_linux_test.go metrics.go
git commit -m "feat: sample block device rates"
```

### Task 6: Implement network-interface counters and rates

**Files:**

- Create: `network_linux.go`
- Create: `network_linux_test.go`
- Modify: `metrics.go`

**Step 1: Write failing `/proc/net/dev` parser tests**

Cover receive/transmit bytes, packets, errors, and drops; ignored FIFO/frame/compressed/multicast fields;
whitespace; malformed headers; short rows; malformed numbers; duplicate interface names; oversized
lines; and deterministic sorting. Split each data row at its first colon and reject an empty name. Parse
`/sys/class/net/<name>/ifindex` as a non-zero `uint32` identity.

**Step 2: Write failing rate and lifecycle tests**

With fixed timestamps, cover expected byte and packet rates, a valid zero rate, first sample, each tracked
counter regressing, zero/negative elapsed time, interface addition, removal, reappearance, rename under
the same `ifindex`, and name reuse under a new `ifindex`.

Error/drop counters remain cumulative in M1. Do not add error/drop rates without a shell consumer.

**Step 3: Write the malformed-interface test**

A malformed identifiable interface becomes an `Issue` while other interfaces remain. Correct headers
with no data rows produce a valid empty snapshot; data rows with no valid interface produce an error. A
missing or malformed `ifindex` preserves cumulative counters but adds an `Issue` and leaves rates invalid.

**Step 4: Run the focused tests and confirm failure**

```bash
go test -count=1 -run 'Test(Network|ParseNetDev)' ./...
```

Expected: compile failure for missing network implementation.

**Step 5: Implement `NetworkSampler`**

Read `/proc/net/dev` and each interface's sysfs `ifindex`, retain baselines by non-zero `ifindex`, apply
the same reset rules as `BlockSampler`, and return fresh sorted values. Keep the implementation local;
two samplers do not justify a generic collector registry or generic rate engine.

**Step 6: Run checks**

```bash
gofmt -w network_linux.go network_linux_test.go metrics.go
go test -race -count=1 ./...
go vet ./...
```

Expected: all checks pass.

**Step 7: Commit**

```bash
git add network_linux.go network_linux_test.go metrics.go
git commit -m "feat: sample network interface rates"
```

### Task 7: Add Linux integration invariants and usage documentation

**Files:**

- Create: `integration_linux_test.go`
- Modify: `README.md`
- Modify: `docs/roadmap.md`

**Step 1: Write the live invariant test**

Run the public API against the current Linux host. Assert only portable invariants:

- CPU aggregate exists; each valid usage fraction is finite and between 0 and 1;
- core IDs are unique and sorted;
- memory total is positive, used and available do not exceed total, and swap values remain consistent;
- uptime is non-negative;
- every filesystem satisfies `used <= total`, `available <= total`, and has a non-empty mount point;
- block and network entities have non-empty names and valid identities; derived values are finite and
  non-negative when marked valid;
- on a second CPU, block, and network sample after at least 100 ms, entities retained under the same
  identity have valid rates when their counters remain monotonic; new or reset entities remain invalid;
- no test requires a battery, cpufreq driver, physical disk, non-loopback interface, fixed mount count, or
  machine-specific value.

Use `testing.Short()` to skip the two-sample delay during short runs. Keep the delay below one second.

**Step 2: Run the focused live test**

```bash
go test -race -count=1 -run TestLinuxIntegration ./...
```

Expected: pass on the development Linux host. Record failures as parser or invariant defects; do not
weaken the check to match one machine.

**Step 3: Update documentation**

README must show one short sampling example and state:

- Linux-only, standard-library-only M1 scope;
- sequential sampler ownership;
- first/discontinuous sample validity;
- partial `Issue` behavior;
- presentation and polling remain consumer responsibilities.

Mark roadmap M0 and M1 complete only after every gate in Task 8 passes. Do not mark M2 power/thermal in
progress.

**Step 4: Run all checks**

```bash
gofmt -w integration_linux_test.go
go mod tidy -diff
go test -race -count=1 ./...
go vet ./...
go list -m all
git diff --check
```

Expected: all commands pass, the module list contains no dependency, and the diff has no whitespace
errors.

**Step 5: Commit**

```bash
git add integration_linux_test.go README.md docs/roadmap.md
git commit -m "test: qualify core Linux collectors"
```

### Task 8: Review and close the M1 branch

**Files:**

- Create: `docs/plans/2026-08-29-core-counters-completion-handover.md`
- Modify only files required by confirmed review findings

**Step 1: Inspect scope and public API drift**

Compare the branch against its base and this plan. Confirm:

- no dependency, daemon, CLI, polling scheduler, UI string, alert policy, D-Bus, power, GPU, or process
  code entered the branch;
- every exported symbol appears in the approved Task 1 contract or has an owner-approved reason;
- sampler state contains only prior timestamp and counters;
- every non-trivial parser or delta rule has one focused runnable check.

Run:

```bash
git diff --stat main...HEAD
git diff --check main...HEAD
go doc .
go list -m all
```

**Step 2: Run a manual local review**

Review integer conversions, counter additions, `time.Duration` conversion, scanner bounds, error
wrapping, first-sample behavior, resets, removals, ordering, and slice ownership. Do not run CodeRabbit
without owner approval.

**Step 3: Run the final fresh gate**

```bash
go mod tidy -diff
go test -race -count=1 ./...
go vet ./...
git diff --check main...HEAD
git status --short
```

Expected: every command exits zero and the worktree is clean after the completion handover commit.

**Step 4: Write the completion handover**

Record branch, base and head commits, exact commands and results, supported Linux sources, known hardware
gaps, API changes from this plan, and the remaining M2 power/thermal work. Do not include machine-specific
telemetry values.

**Step 5: Commit the handover**

```bash
git add docs/plans/2026-08-29-core-counters-completion-handover.md
git commit -m "docs: hand off core metrics milestone"
```

**Step 6: Stop for review**

Ask the owner to review the branch. Do not merge, tag, push, begin M2 power/thermal, or integrate
`sysc-shell` without explicit direction.

## M1 acceptance gate

The milestone is ready for review only when:

- the owner approved the M0 API contract;
- public Linux readers work without third-party dependencies;
- first samples, counter resets, elapsed-time errors, device addition/removal, malformed rows, overflow,
  and partial errors have focused tests;
- `go test -race -count=1 ./...`, `go vet ./...`, and `go mod tidy -diff` pass;
- live tests assert portable invariants without embedding host values;
- the completion handover records remaining hardware gaps;
- the branch contains none of the stated non-goals.

## Reconsider conditions

Stop and return to owner review if:

- the public M0 types cannot express a required core or power consumer without formatted strings or
  shell-owned policy;
- a required kernel value cannot be read correctly with the standard library;
- accurate CPU or rate sampling requires a background goroutine inside the library;
- a parser needs an exported filesystem abstraction only to support tests;
- dgop behavior conflicts with current Linux ABI documentation;
- reliable filesystem identity requires a mount or block-device policy that belongs to `sysc-shell`;
- integration tests require privileged access or fixed hardware values.

Use dgop commit `473bc52` as a behavioral reference for units and cursor semantics. Do not copy its CLI,
TUI, HTTP, logging, process, or dependency structure. If code is adapted, preserve MIT attribution in the
file and document the exact source.
