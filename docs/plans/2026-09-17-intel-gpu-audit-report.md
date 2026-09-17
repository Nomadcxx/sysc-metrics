# sysc-metrics Intel GPU work audit report

Date: 2026-09-17

Audit commission: `docs/plans/2026-09-17-intel-gpu-audit-commission.md`.
Audited artifact: `github.com/Nomadcxx/sysc-metrics` tag `v0.5.0`
(annotated tag object `32e95765…` on the remote, peeled commit
`f09124173d90d6faffbc57243efc89d07f56c1bf`), plus documentation commits
`388cd8e` and `dc287be` on `main`.
Source of requirements (read from disk, not taken on trust):
`/home/nomadx/sysc-shell/docs/plans/2026-09-17-sysc-metrics-intel-gpu-execution-handover.md`.

Auditor's environment:

- Audit host (`archPC`): kernel `7.2.4-arch1-2`, `perf_event_paranoid=2`, Go
  `1.27.1 linux/amd64`. GPU is an NVIDIA dGPU (`10de:2808`), **no i915 PMU**;
  i915-dependent paths skip here. (The commission calls this "an AMD desktop";
  it is actually NVIDIA. Immaterial to the audit.)
- Target laptop (`ssh -p 7777 nomadx@192.168.0.64`): kernel `7.2.4-arch1-2`,
  `perf_event_paranoid=1`, i915 `8086:3ea0` at PCI `0000:00:02.0`, Go `1.27.1`.

## Verdict

**Not clean — one hard defect falsifies an explicit documented safety claim and
one documented mapping rule is implemented more coarsely than written.** Nine of
the ten claim groups reproduce. No tracked file was modified, no tag moved, no
force operation, no sysctl changed, no binary installed, and no `sysc-shell`
product code touched.

## Claim-by-claim results

| # | Claim | Result |
|---|---|---|
| 1 | API compatibility (`GPU`/`GPUUsage`/`GPUSnapshot`/`ReadGPU`; `ReadGPU` never Intel usage) | **Reproduced** |
| 2 | Sampling semantics (first invalid, second valid, valid zero, rebaseline rules, recovery) | **Reproduced** |
| 3 | Mapping rules (BDF+driver, linkless single-GPU fallback, ambiguity unmapped) | **Reproduced, with a documented-rule divergence → F2** |
| 4 | Aggregation (max of engines, never summed, engine failure invalidates) | **Reproduced** |
| 5 | Permission contract (unprivileged system-wide EACCES at paranoid 2 *and* 1; self works; i915 task attach EINVAL) | **Reproduced on real hardware** |
| 6 | Live evidence (PMU type 13, events, first invalid, second valid under sudo) | **Reproduced except the privileged positive, which is corroborated but not re-run** |
| 7 | No new modules / stdlib-only / ABI-0 layout | **Reproduced** |
| 8 | Existing GPU paths unchanged (only the `readGPUs` split) | **Reproduced** |
| 9 | fd lifecycle (close-on-exec, idempotent `Close`, removal, failed-read retention) | **FALSIFIED → F1** |
| 10 | Ownership/concurrency (one owner, no goroutines, no package globals) | **Reproduced with a caveat** |

Baseline gates on the audit host, at `v0.5.0` content:

```
gofmt -l .                     → no output
go vet ./...                   → clean
go test -race -count=1 ./...   → ok  github.com/Nomadcxx/sysc-metrics 1.464s
git diff v0.4.0..v0.5.0 -- go.mod go.sum → empty
```

All confirmed independently. `git diff v0.5.0 -- . ':(exclude)docs'` is empty:
the checked-out code equals the tag, and `git log v0.5.0..HEAD` is exactly two
documentation commits.

---

## Defect F1 (Moderate) — the PMU counter descriptors are **not** close-on-exec, and `exclusive` is set instead

**Claim falsified:** design amendment §Discovery-and-mapping 5 and the
completion handover §API contract both state the counters are opened
close-on-exec "so exec'd helpers never inherit them".

**Root cause.** `openGPUCounter` (`gpu_pmu_linux.go`) puts
`perfFlagFDCloexec = 1 << 3` into `perfEventAttr.Flags` and passes `0` as the
`perf_event_open` syscall's `flags` argument:

```go
attr := perfEventAttr{ Type: ..., Size: ..., Config: config, Flags: perfFlagFDCloexec }
fd, err := perfEventOpen(&attr, -1, 0, -1, 0)   // <-- syscall flags == 0
```

`perfEventAttr.Flags` is the ABI-0 bitfield at offset 40, where bit 3 is
`exclusive`, **not** a CLOEXEC control. `PERF_FLAG_FD_CLOEXEC` is only
effective as the fifth syscall argument, which the code sets to `0`. So the
descriptor is left non-CLOEXEC and, as a second, undocumented consequence, the
event requests `exclusive` counter use.

**Evidence (audit host, kernel 7.2.4-arch1-2, unprivileged).** Reproducing the
repo's exact construction on a self-attached software event (so it opens
without CAP_PERFMON):

```
attr.Flags=1<<3, syscall flags=0   -> fd=4 err=nil  FD_CLOEXEC=false
syscall flags=1<<3, attr.Flags=0   -> fd=4 err=nil  FD_CLOEXEC=true
```

**Evidence (target laptop, kernel 7.2.4-arch1-2, unprivileged, real i915 box).**
Identical result: `(A) attr.Flags=1<<3, syscall flags=0 → FD_CLOEXEC=false`;
`(B) syscall flags=1<<3 → FD_CLOEXEC=true`.

**Concrete leak.** Opening a counter the way `openGPUCounter` does and then
exec'ing a child (exactly what `readGPUs` does to `nvidia-smi` on a hybrid
system) shows the child inheriting the descriptor:

```
child /proc/self/fd after exec (repo-style, non-CLOEXEC):
  lrwx------ 1 nomadx nomadx 64 ... 4 -> anon_inode:[perf_event]
child /proc/self/fd after exec (syscall flag CLOEXEC):
  (fd 4 absent)
```

On a hybrid Intel+NVIDIA machine the second and later `Sample` calls exec
`nvidia-smi` while the i915 counters are already open, so the counter fds are
inherited by the child — precisely the outcome the documentation says cannot
happen. On the Intel-only target laptop no helper is exec'd, so the defect is
latent there and the live evidence could not have surfaced it.

**Failing test** (verified: FAILs on the audit host; the production-path variant
skips unprivileged and FAILs where the open succeeds):

```go
//go:build linux

package metrics

import (
	"syscall"
	"testing"
	"unsafe"
)

func TestAuditF1CounterFDIsCloseOnExecMechanism(t *testing.T) {
	const excludeKernelHV = 1<<5 | 1<<6 // paranoid gate; orthogonal to CLOEXEC
	attr := perfEventAttr{
		Type:   1, // PERF_TYPE_SOFTWARE
		Size:   uint32(unsafe.Sizeof(perfEventAttr{})),
		Config: 0, // PERF_COUNT_SW_CPU_CLOCK
		Flags:  perfFlagFDCloexec | excludeKernelHV,
	}
	fd, err := perfEventOpen(&attr, 0, -1, -1, 0) // attr carries 1<<3, syscall flags 0
	if err != nil {
		t.Skipf("perf_event_open unavailable here: %v", err)
	}
	defer syscall.Close(fd)
	got, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_GETFD), 0)
	if errno != 0 {
		t.Fatalf("fcntl(F_GETFD): %v", errno)
	}
	if got&uintptr(syscall.FD_CLOEXEC) == 0 {
		t.Fatalf("F1: counter fd is not close-on-exec: perfFlagFDCloexec=1<<3 landed in perfEventAttr.Flags (bit 3 = exclusive) and 0 was passed as the perf_event_open flags argument")
	}
}

func TestAuditF1OpenGPUCounterIsCloseOnExec(t *testing.T) {
	counter, err := openGPUCounter(1, 0) // system-wide: needs CAP_PERFMON/CAP_SYS_ADMIN
	if err != nil {
		t.Skipf("openGPUCounter needs CAP_PERFMON/CAP_SYS_ADMIN: %v", err)
	}
	defer counter.close()
	fd := int(counter.(*perfCounter).file.Fd())
	got, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_GETFD), 0)
	if errno != 0 {
		t.Fatalf("fcntl(F_GETFD): %v", errno)
	}
	if got&uintptr(syscall.FD_CLOEXEC) == 0 {
		t.Fatalf("F1: openGPUCounter returned a non-close-on-exec descriptor")
	}
}
```

**Suggested fix (for the follow-up commission, not applied here):** pass the
flag where the kernel honours it and stop setting the attr bitfield —

```go
attr := perfEventAttr{Type: uint32(pmuType), Size: uint32(unsafe.Sizeof(perfEventAttr{})), Config: config}
fd, err := perfEventOpen(&attr, -1, 0, -1, perfFlagFDCloexec)
```

This both restores close-on-exec and removes the accidental `exclusive`
attribute. Note the change is behavioural: the live `sudo` run succeeded *with*
`exclusive` set, so removing it should be re-qualified, not assumed neutral.

---

## Defect F2 (Low) — the linkless-PMU fallback is all-or-nothing, coarser than the documented rule

**Claim diverged from:** design amendment §Discovery-and-mapping 3 promises a
per-PMU rule: "A PMU maps to a GPU through its sysfs `device` link … When no
link exists, a driver-named PMU may only be mapped while exactly one GPU carries
that driver." The commissioning handover says the same.

`mapGPUPMUs` instead computes one global `hasLinks` flag: if **any** candidate
carries a `device` link, the links-only branch runs and returns, so a linkless
candidate that should fall back is never considered, even when it is the only
candidate whose driver matches a unique GPU.

**Evidence.** With one i915 GPU and two candidates — a linkless `i915` and an
unrelated `i915_…` linked to `0000:99:99.9` — the lone GPU is left unmapped:

```
mapGPUPMUs([{0000:00:02.0, i915}], [{i915,""},{i915_…,"0000:99:99.9"}]) = [-1]
```

The failure mode is safe (invalid usage + `Issue`, never a fabricated
percentage), so this is a policy divergence rather than a correctness bug; the
implementer judged it defensible, and it is. It is reported because the
commission explicitly asked whether "any link present ⇒ links-only mode" is the
rule that was requested, and it is not literally the rule that was written.

**Failing test** (verified FAIL against `v0.5.0`):

```go
//go:build linux

package metrics

import "testing"

func TestAuditF2LinklessSingleGPUStillMaps(t *testing.T) {
	gpus := []gpuRef{{bdf: "0000:00:02.0", driver: "i915"}}
	pmus := []pmuCandidate{
		{name: "i915", driver: "i915", bdf: ""},
		{name: "i915_0000_99_99_9", driver: "i915", bdf: "0000:99:99.9"},
	}
	mapped := mapGPUPMUs(gpus, pmus)
	if mapped[0] != 0 {
		t.Fatalf("F2: the only i915 GPU was left unmapped (%v) because an unrelated candidate had a device link", mapped)
	}
}
```

---

## Claim detail and evidence

### 1. API compatibility — reproduced
`git diff v0.4.0..v0.5.0 -- metrics.go` appends only the `GPUSampler` type and
`NewGPUSampler`. `GPU`, `GPUUsage`, `GPUSnapshot`, and `ReadGPU` are untouched;
`ReadGPU` still routes through `readGPUs`, which only ever fills `Usage` from
`gpu_busy_percent` (amdgpu) or the injected `nvidia-smi`, so i915 stays invalid.
`TestReadGPUKeepsIntelUsageInvalid` and the `metrics_test.go` `var` block pass.

### 2. Sampling semantics — reproduced
Re-derived the rules from the amendment rather than the tests. `engineBusy`
rejects a missing previous value, `elapsed <= 0`, a decrease, and
`delta > uint64(elapsed)`; the `uint64(elapsed)` cast is guarded by the
`elapsed <= 0` check, so it is safe. The fixture suite covers first/second
sample, valid zero, read error, decrease, impossible delta, non-positive
interval, order-stable max, and bounded output; all pass and none was found that
should fail but passes. Overlong busies would need ~584 years to wrap, treated
as a decrease → rebaseline.

### 3. Mapping rules — reproduced, divergence F2
BDF+driver link mapping, single-linkless-GPU fallback, two-GPU ambiguity left
unmapped, cross-driver and unknown-link rejection, and symlinked
`event_source/devices` discovery all reproduce. The extra `hasLinks` gating is
F2.

### 4. Aggregation — reproduced
`maxBusyFraction` has one call site (`sampleGPU`) and is never summed. Any open
failure, read failure, or rebaseline sets `valid = false`, so the whole GPU
percentage is withheld; the amendment's conservative "Aggregation" rule matches
the code. `TestPMUAggregationIsOrderStableMaximum` passes.

### 5. Permission contract — reproduced on real hardware
The commission's "surprising" claim is correct and was verified against the
kernel rather than accepted:
- Audit host, `perf_event_paranoid=2`, unprivileged: system-wide software
  (with/without `exclude_kernel`, with/without `exclude_hv`), `exclusive`, and a
  hardware event **all return EACCES**.
- Target laptop, `perf_event_paranoid=1`, unprivileged: system-wide software
  `EACCES`; i915 `rcs0` system-wide `EACCES`; i915 **task attach (`pid=0,
  cpu=-1`) returns EINVAL**; self-attached software event succeeds.
- This matches `perf_event_paranoid ≥ 1 ⇒ no CPU-wide events without
  CAP_PERFMON`; the paranoid-1 half is therefore reproduced directly, not
  inferred. The probe left the sysctl untouched.

### 6. Live evidence — reproduced except the privileged positive
On the laptop the sysfs inventory is exactly as recorded: PMU `i915` type `13`,
`format/i915_eventid = config:0-20`, `rcs0-busy=0x0`, `bcs0-busy=0x1000`,
`vcs0-busy=0x2000`, `vecs0-busy=0x3000`, every unit `ns`, **no `device` link**,
`card1 → 0000:00:02.0` `i915` `8086:3ea0`, no hwmon temperature, and neither
`intel_gpu_top` nor `perf` installed. Running the actual v0.5.0 code
unprivileged (`GPUSampler` two-sample harness) reproduced the handover's
unprivileged evidence exactly: identity and PCI name retained, `usageValid=false`,
and four per-engine `perf_event_open: permission denied` `Issue`s — never a
fabricated valid value. The opt-in `TestIntelGPULive` **skips** on the i915-free
audit host and **fails by design** unprivileged on the laptop.

The privileged positive (`fraction≈0.02187 valid=true` under sudo) was **not
re-run** — sudo on the laptop is the human's. It is corroborated by the retained
root-owned artifact `/tmp/intel-gpu-live-sudo.txt` (750 bytes, still present),
which matches the handover line-for-line including `fraction=0.021866545483376656
valid=true` and `--- PASS: TestIntelGPULive (0.51s)`.

### 7. stdlib-only / layout — reproduced
`git diff v0.4.0..v0.5.0 -- go.mod go.sum` is empty; the only imports in the new
code are stdlib (`syscall`, `unsafe`, `encoding/binary`, `io`, `os`,
`path/filepath`, `sort`, `strconv`, `strings`, `time`); no cgo. `perfEventAttr`
is 64 bytes with fields at offsets 0/4/8/16/24/32/40/48/52/56, measured on both
machines — so the amd64 claim holds, and the layout is identical for the other
32/64-bit Linux ABIs by construction.

### 8. Existing paths unchanged — reproduced
`git diff v0.4.0..v0.5.0 -- gpu_linux.go` is the `readGPU`→`readGPUs` split,
two added imports, and appended sampler code; the enumeration body moved
verbatim. `git diff --numstat` shows the test files are additions only (470/343/113/3 insertions, 0 deletions). AMD busy, NVIDIA injection, hwmon temp,
pci.ids naming, simpledrm/connector skips, and deterministic ordering are intact;
the `-race` suite is green.

### 9. fd lifecycle — FALSIFIED
Close-on-exec is false (F1). The rest holds: `Close` is idempotent and closes
every counter; GPU removal closes and drops that GPU's state; reappearance
rebaselines (`TestGPUSamplerCloseIsIdempotentAndClosesCounters`,
`TestGPUSamplerDeviceDisappearanceDropsState`). A failed `read` intentionally
keeps the fd and retries next sample — bounded and acceptable. One residual
wart: `closeCounters` sets `counter = nil` even when `counter.close()` errors,
so a failed `close(2)` leaks the fd with no handle left to retry; `close(2)`
failure on a valid perf fd is essentially impossible, so this is noted, not
scored.

### 10. Ownership — reproduced with a caveat
`GPUSampler` documents one sequential owner, starts no goroutine, holds no sync
primitives, and returns freshly allocated caller-owned slices. Counter state is
per sampler, satisfying the commission's actual constraint ("do not use package
globals to retain counter state"). The handover's unqualified phrase "no package
globals" is technically false — `perfEventOpen`, `nvidiaSMI`, and `pciIDsPaths`
are mutable package-level vars — but `perfEventOpen` is a test seam mirroring the
existing `nvidiaSMI` pattern, not retained state.

---

## Other observations (not defects)

- **Short reads are implemented but untested.** `perfCounter.read` returns
  `io.ErrUnexpectedEOF` on `n != 8`, but the injectable `gpuCounter` fake can
  only return errors, so the commission's "short read" red case is not actually
  exercised; same for "PMU present but zero `*-busy` events" (only a wholly
  missing PMU is tested).
- **Missing `format` files weaken validation.** If `format/config` is
  unreadable, `discoverBusyEvents` accepts events without the mask-fit check the
  amendment requires instead of raising an `Issue`. Fail-open on a malformed
  PMU, but only reachable when the kernel exposes no `config` format at all.
- **Endianness.** `perfCounter.read` uses `binary.LittleEndian`. On a
  big-endian Linux target the count is byte-swapped, but the value would almost
  always exceed the interval and be rejected as an impossible delta, so it fails
  safe rather than fabricating a fraction. The handover's "argued by
  construction" claim would be airtight with `binary.NativeEndian`.
- **Interval skew is understated in the deviation list.** `now` is sampled
  before `readGPUs`, but the counters are read after it, so on a hybrid machine
  the numerator window (counter reads) and the denominator window (sample
  starts) diverge by the variation in `readGPUs` latency — up to the 400 ms
  `nvidia-smi` timeout, not merely "per-sample syscall time". It cannot produce
  a fraction > 1 (rejected as impossible), but it can drop a legitimate sample.
- **Uncommitted probes.** The handover's `TestDebugPerfVariants` outputs are not
  in the tree; this audit independently reproduced every quoted behaviour on the
  real target, so the omission is a traceability gap, not a fidelity gap.
- **Tag scope.** `v0.5.0` contains the code, tests, README, plan, and design
  amendment, but **not** the completion handover (committed at `388cd8e` after
  the tag). Consistent with the commission ("docs commits through `dc287be`"),
  worth stating so the shell's pin is not expected to include the handover.

## Not independently reproduced

- The privileged live positive (needs the human's sudo; corroborated by the
  retained artifact above).
- `xe`-driver behaviour (out of scope; no `xe` hardware available).
- Big-endian `GOARCH` behaviour (noted as a portability risk only).

## Boundaries honoured

Read-only on `sysc-metrics`: no tracked file was modified, no tag moved, no
force operation, no `sysc-shell` product code edited. The temporary audit test
files were added, run, and removed; `git status` shows only the pre-existing
untracked `.commandcode/` and this commission. No sysctl was changed and nothing
was installed. The laptop's `/tmp` tree created for the unprivileged harness was
removed.
