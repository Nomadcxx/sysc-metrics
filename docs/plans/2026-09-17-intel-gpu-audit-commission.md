# sysc-metrics Intel GPU work audit commission

Date: 2026-09-17

Audited work: `github.com/Nomadcxx/sysc-metrics` tag `v0.5.0` (commit
`f091241`), plus documentation commits through `dc287be` on `main`.

Auditor's goal: adversarially verify the claims in
`docs/plans/2026-09-17-intel-gpu-completion-handover.md` against the
original commission in
`/home/nomadx/sysc-shell/docs/plans/2026-09-17-sysc-metrics-intel-gpu-execution-handover.md`.
An audit passes when every claim is either reproduced independently or a
defect is filed with a failing test. Trust nothing quoted; rerun it.

## What the work claims, in one paragraph

`sysc-metrics` gained a stateful `GPUSampler` that reports Intel i915 GPU
utilization by opening the i915 PMU's `*-busy` engine counters with a raw
`perf_event_open` syscall shim and dividing busy-time deltas by the monotonic
sample interval (max across engines). `ReadGPU` is unchanged and still never
reports Intel usage. The release is qualified on the target laptop and
published as `v0.5.0`. No new modules were added.

## Documents in the chain

1. Commission (source of requirements): `sysc-shell` repo,
   `docs/plans/2026-09-17-sysc-metrics-intel-gpu-execution-handover.md`.
2. Design amendment: this repo, `docs/plans/2026-09-02-thermal-and-gpu-design.md`,
   section "Amendment 2026-09-17" (at the end).
3. Executable plan: `docs/plans/2026-09-17-intel-gpu-pmu.md`.
4. Completion handover: `docs/plans/2026-09-17-intel-gpu-completion-handover.md`
   — includes the "Commission coverage" table mapping each requirement to
   where it landed, commit hashes, gate output, and live evidence.

If the auditor cannot read the `sysc-shell` checkout (workspace boundaries
blocked the implementing session too), audit against the coverage table plus
the requirement quotes embedded in this document.

## Code inventory (all new GPU work)

- `gpu_pmu_linux.go`: pure parsing (`parsePMUType`, `parseEventConfig`,
  `formatConfigMask`), discovery (`discoverGPUPMUs`, `discoverBusyEvents`),
  mapping (`mapGPUPMUs`), delta math (`engineBusy`, `maxBusyFraction`), and
  the syscall shim (`perfEventAttr`, `perfEventOpen` var, `openGPUCounter`,
  `perfCounter`).
- `gpu_linux.go`: `readGPU` → `readGPUs` refactor returning `[]gpuFound`;
  `GPUSampler` methods `Sample`, `Close`, `applyIntelPMU`, `sampleGPU`; state
  types `gpuPMUState`, `gpuEngineState`.
- `metrics.go`: public `GPUSampler` type + `NewGPUSampler` only.
- Tests: `gpu_pmu_linux_test.go` (19 tests), `gpu_linux_test.go` (10 sampler
  fixture tests + 1 `ReadGPU` contract test), `metrics_test.go` (3 compile
  assertions), `integration_linux_test.go` (sampler pass +
  opt-in `TestIntelGPULive`).

## Claims to falsify, with the fastest falsification

1. **API compatibility.** `GPU`, `GPUUsage`, `GPUSnapshot`, `ReadGPU` are
   source-compatible with `v0.4.0`; `ReadGPU` never reports Intel usage.
   Check: `git diff v0.4.0..v0.5.0 -- metrics.go` (types), and run
   `TestReadGPUKeepsIntelUsageInvalid` plus the `metrics_test.go` var block.
2. **Sampling semantics.** First sample invalid; second valid over a positive
   interval; valid zero stays valid; decrease/reset/short-or-failed
   read/non-positive interval/impossible delta rebaseline and invalidate one
   sample; recovery on the next complete pair. Check: `TestPMUEngineBusy*`,
   `TestGPUSampler{ReadError,CounterDecrease,ImpossibleDelta}...`,
   `TestGPUSamplerValidZeroStaysValid`. Then re-derive the rules from the
   design amendment and hunt for a fixture that should fail but passes.
3. **Mapping rules.** Device-link mapping by BDF+driver; linkless PMU serves a
   single driver GPU; any ambiguity stays unmapped with an `Issue`; a linked
   candidate pointing elsewhere disables the fallback entirely. Check:
   `TestPMUMaps*`, `TestPMUDoesNotMap*`. Scrutinize whether
   "any link present ⇒ links-only mode" is actually the conservative rule the
   commission asked for, and whether a PMU linked to a GPU hidden from drm
   should instead unmapped-map by elimination (the implementation says no).
4. **Aggregation policy.** Fraction = max of engine fractions; never summed;
   no new public field; any engine failure invalidates the whole GPU
   percentage for that sample (the conservative partial-coverage rule).
   Check: `maxBusyFraction` call sites, `sampleGPU`, the amendment's
   "Aggregation" section, `TestPMUAggregationIsOrderStableMaximum`.
5. **Permission contract (the surprising one).** On kernel
   `7.2.4-arch1-2`, ANY unprivileged system-wide `perf_event_open` (`pid=-1`)
   returns EACCES — at `perf_event_paranoid` 2 **and** 1, with and without
   `exclude_kernel`/`exclude_hv`; task-attached self-measurement still works;
   the i915 PMU rejects task attach with EINVAL; therefore i915 counters
   require `CAP_PERFMON`/`CAP_SYS_ADMIN`. This contradicts the older
   "paranoid <= 1 suffices" folk contract and is stated in the design
   amendment and README. Check: rerun the variant probe via
   `TestDebugPerfVariants`-style opens (self `pid=0,cpu=-1` hardware event
   vs system-wide `pid=-1,cpu=0`), both unprivileged.
6. **Live evidence.** On the laptop: PMU i915 type 13, busy events
   `rcs0=0x0 bcs0=0x1000 vcs0=0x2000 vecs0=0x3000` (unit ns), first sample
   invalid, second sample `fraction≈0.02187 valid=true` under sudo;
   unprivileged runs showed per-engine EACCES Issues with identity retained.
   Check: rerun `TestIntelGPULive` yourself (commands below). Note the
   evidence was gathered from a tar-copied tree at `/tmp/sysc-metrics-live`
   claimed identical to the tag — verify with
   `git diff --stat v0.5.0` against a fresh clone if you doubt it (the
   only post-tag commits are docs).
7. **No new modules / stdlib-only.** Check: `git diff v0.4.0..v0.5.0 --
   go.mod go.sum` must be empty; grep for cgo and third-party imports in the
   new files (only `syscall`, `unsafe`, `encoding/binary`, `io`, std lib).
   Also check the `perfEventAttr` layout claim: 64-byte ABI-0 with fields at
   offsets 0/4/8/16/24/32/40/48/52/56 — live-verified on linux/amd64 only;
   other GOARCH are argued by construction, not measured.
8. **Existing GPU paths unchanged.** AMD busy, NVIDIA `nvidia-smi` injection,
   hwmon temperature, pci.ids naming, connector/simpledrm skips, deterministic
   ordering. Check: `git diff v0.4.0..v0.5.0 -- gpu_linux.go` — the only
   intended change to `readGPU` is the `readGPUs` split (signature + returns);
   everything else moved verbatim. The old test file's assertions are
   untouched.
9. **fd lifecycle.** Counters open close-on-exec (so exec'd `nvidia-smi`
   never inherits them), `Close` is idempotent and closes everything, GPU
   removal closes and drops that GPU's state, reappearance rebaselines.
   Check: `TestGPUSamplerCloseIsIdempotentAndClosesCounters`,
   `TestGPUSamplerDeviceDisappearanceDropsState`, and read `closeCounters`.
   A failed `read` intentionally keeps the fd (retried next sample) — assess
   whether that bounded-lifetime choice is acceptable or a leak vector.
10. **Ownership/concurrency.** One sequential owner, no goroutines, no
    package globals, snapshot slices caller-owned. Check: doc comments on
    `GPUSampler`, absence of sync primitives, and accept that nothing
    *enforces* single ownership (same as every other sampler here) — decide
    whether that satisfies the commission's "documented ownership rule".

## Verification commands

On any machine (audit machine `archPC` is an AMD desktop without i915; the
live check skips there):

```bash
git clone https://github.com/Nomadcxx/sysc-metrics && cd sysc-metrics
git checkout v0.5.0
gofmt -l .                                   # expect: no output
go vet ./...                                 # expect: clean
go test -race -count=1 ./...                 # expect: ok
git diff --exit-code v0.4.0 -- go.mod go.sum # expect: empty (no new modules)
go test -count=1 -run 'TestPMU|TestGPUSampler|TestReadGPUKeepsIntel' -v .
SYSC_METRICS_INTEL_GPU_LIVE=1 go test -count=1 -run TestIntelGPULive -v .
# → SKIP unless an i915 machine; on AMD machines it must skip, not fail
```

On the Intel laptop (`ssh -p 7777 nomadx@192.168.0.64`, Whiskey Lake iGPU,
PCI `0000:00:02.0`, device `0x3ea0`): transfer the checkout (the laptop has
Go, no rsync; use tar over ssh). Unprivileged run must reproduce the EACCES
Issue path with identity/temp retained and no fabricated zero. The privileged
run needs sudo — coordinate with the human (password entry is theirs); the
the implementing session used a root-owned output file pattern:

```
sudo sh -c 'SYSC_METRICS_INTEL_GPU_LIVE=1 go test -count=1 -run=TestIntelGPULive -v . > /tmp/out.txt 2>&1'
```

Expect the second sample valid; an idle valid zero is also a pass.

## Known deviations and judgment calls (assess, don't absorb)

- The conventions file named in the commission's reading list does not exist in this repo;
  conventions came from `README.md` and existing code.
- The shell consumer `sysc-shell/internal/services/metrics.go` was outside
  the implementing session's readable workspace; the shell-side contract was
  taken from the commission text itself.
- Counters are opened **enabled** (no disabled+ioctl dance); the design
  amendment was corrected to match rather than adding the ioctl.
- Interval skew between counter accrual and the wall-clock denominator is
  bounded by per-sample syscall time; no correction is applied.
- Read failures keep the fd open and retry; open failures retry the open
  next sample (self-heals if privileges change at runtime).
- `TestDebugPerfVariants`-style probes lived in the throwaway tree and were
  never committed; their outputs are quoted in the handover only.
- Commit messages carry no assistant attribution — the commit guard rejects
  it; do not "fix" this.
- The laptop's `/tmp/intel-gpu-live-sudo.txt` is root-owned (sudo-created
  evidence file); untracked `.commandcode/` in the repo is session tooling.

## Suggested attack surfaces (where the implementing session is least sure)

- The `mapGPUPMUs` links-present fallback interaction (claim 3) — the
  fixture in `TestPMUDoesNotMapAcrossDriversOrUnknownLinks` encodes a policy
  decision that is defensible but debatable.
- `engineBusy`'s `uint64(elapsed)` cast after the positive check, and
  overflow behavior of busy counters near wraparound (treated as decrease →
  rebaseline; i915 busy-time in ns wraps after ~584 years).
- Whether the fixture suite pins semantics or merely mirrors the
  implementation (e.g., are the rebaseline rules falsifiable by a test
  written from the commission text alone?).
- Whether `readGPUs`' `found` slice order invariant (index i of `found` ==
  index i of `snapshot.GPUs`) is guaranteed everywhere `sampleGPU` relies on
  it.
- The claim that the tar-copied evidence tree equals the tag.

## Boundaries for the auditor

Audit work is read-only on this repository: no rewrites, no tag moves, no
force operations. Do not change sysctls, install binaries, or edit
`sysc-shell` product code. Sudo on the laptop is the human's, always. File
findings as a report with a failing test per defect; the implementing session
fixes under a new commission.
