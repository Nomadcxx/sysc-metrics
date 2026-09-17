# Intel GPU i915 PMU Sampler Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Ship `GPUSampler` in `sysc-metrics` reporting truthful Intel i915 GPU usage from PMU
engine-busy counters, qualify it on the target laptop, and publish `v0.5.0`.

**Architecture:** The existing `readGPU` DRM enumeration stays the single discovery layer. A new
`gpu_pmu_linux.go` adds pure PMU parsing/delta math plus a narrow `perf_event_open` shim behind
an injectable opener. `GPUSampler` (type + constructor in `metrics.go`, `Sample`/`Close` in the
Linux files) layers the PMU state onto each DRM snapshot. `ReadGPU` is unchanged and never
reports Intel usage. Zero new modules.

**Tech Stack:** Go 1.26 standard library only: `syscall.SYS_PERF_EVENT_OPEN`, sysfs files,
existing fixture patterns. Design: `docs/plans/2026-09-02-thermal-and-gpu-design.md`
(amendment 2026-09-17). Handover:
`sysc-shell/docs/plans/2026-09-17-sysc-metrics-intel-gpu-execution-handover.md`.

Probe evidence (2026-09-17, target laptop, kernel 7.2.4-arch1-2): PMU `i915` type `13`, format
`i915_eventid: config:0-20`, busy events `rcs0-busy` 0x0 / `bcs0-busy` 0x1000 / `vcs0-busy`
0x2000 / `vecs0-busy` 0x3000, unit `ns`; no `device` link on the PMU dir;
`perf_event_paranoid=2` gives unprivileged `EACCES`. All discovery is dynamic; nothing above is
hard-coded.

---

### Task 1: Pure PMU parsing and delta math (failing tests first)

**Files:**
- Create: `gpu_pmu_linux_test.go`
- Create: `gpu_pmu_linux.go`
- Modify: `metrics_test.go`

**Step 1: Failing tests.** In `metrics_test.go`, add compile assertions:

```go
_ func() *GPUSampler                           = NewGPUSampler
_ func(*GPUSampler) (GPUSnapshot, error)       = (*GPUSampler).Sample
_ func(*GPUSampler) error                      = (*GPUSampler).Close
```

In `gpu_pmu_linux_test.go` cover, with fixture sysfs trees and an injected counter
opener/reader (never real `perf_event_open`):

- parsing `type`, `format`, and `events/*-busy` definitions including `config=0x...` bodies;
- malformed event definitions, missing `unit`, non-`ns` units;
- engine names that differ from the laptop's four (`engine0-busy` etc.);
- first sample invalid, second sample valid over a positive interval;
- valid zero busy delta stays valid;
- counter decrease/reset and impossible delta (busy delta > elapsed) rebaseline and invalidate
  one sample, then recover;
- non-positive elapsed time and short/failed reads invalidate without panicking;
- stable max aggregation across engine order changes;
- bounded finite output (no NaN/Inf, 0..1).

**Step 2: Run** `go test -count=1 .` — expect FAIL (undefined `gpuPMU*` symbols, compile
assertions).

**Step 3: Implement the smallest pure core** in `gpu_pmu_linux.go`: attr type + open shim
behind an injectable opener, sysfs discovery/parse functions, engine delta function with the
rebaseline rules, max aggregation, PMU-to-GPU mapping (device link, else single-driver-GPU
rule; ambiguous stays unmapped).

**Step 4: Run** `go test -count=1 -run 'TestPMU|TestGPU' .` — expect PASS for the new tests
(sampler compile assertions may stay red until Task 2; keep them in this commit only if green,
else move them to Task 2).

**Step 5: Commit** `feat: parse i915 PMU busy events and engine deltas`

### Task 2: Stateful GPUSampler and integration

**Files:**
- Modify: `metrics.go`
- Modify: `gpu_linux.go`
- Modify: `gpu_linux_test.go`
- Modify: `metrics_test.go`

**Step 1: Failing fixture tests** (`gpu_linux_test.go`):

- an Intel GPU retains identity/name/temp while usage becomes valid after two sampler calls;
- no `nvidia-smi` invocation occurs for an Intel-only fixture;
- a valid zero remains valid on the second call;
- device disappearance closes and drops that GPU's PMU state; reappearance rebaselines;
- `Close` is idempotent and closes every opened counter;
- `ReadGPU` still returns `Usage.Valid == false` for i915.

**Step 2: Run** `go test -count=1 .` — expect FAIL.

**Step 3: Implement** `GPUSampler` (type + `NewGPUSampler` in `metrics.go`; `Sample`/`Close` in
the Linux GPU files) layering PMU state keyed by PCI BDF onto `readGPU`'s snapshot. `ReadGPU`
keeps its current body.

**Step 4: Run** `go test -race -count=1 ./...` — PASS, including
`integration_linux_test.go` extended with a `GPUSampler` two-sample pass and `Close`.

**Step 5: Commit** `feat: sample Intel GPU usage from the i915 PMU`

### Task 3: Documentation and gates

**Files:**
- Modify: `README.md` (sampler in usage/scope: i915 PMU source, stateful requirement,
  first-sample/valid-zero semantics, permission behavior, i915-only boundary)

**Gates:**

```bash
gofmt -w .
test -z "$(gofmt -l .)"
go vet ./...
go test -race -count=1 ./...
git diff --exit-code -- go.mod go.sum
```

**Commit:** `docs: document the Intel GPU PMU sampler`

### Task 4: Opt-in live check and laptop qualification

**Files:**
- Modify: `integration_linux_test.go` (env-gated `TestIntelGPULive`, skipped unless
  `SYSC_METRICS_INTEL_GPU_LIVE=1`, requires an i915 GPU; takes two samples, prints PCI identity
  and discovered PMU events, asserts the second sample's `Usage.Valid`)

Transfer the tree to the laptop (`192.168.0.64:7777`, no checkout exists there yet), run the
live check with privileges sufficient for i915 counters (the probe recorded unprivileged
`EACCES` at `perf_event_paranoid=2`), and record: kernel/driver, BDF + device id, PMU type +
events, first-sample invalid, second-sample valid (a valid zero is a success), permission
behavior.

**Commit:** `test: add the opt-in Intel GPU live check`

### Task 5: Release and completion handover

Tag `v0.5.0` after gates pass, write
`docs/plans/2026-09-17-intel-gpu-completion-handover.md` (commit hashes, gate output, tag,
permission findings, exact shell migration contract: one `GPUSampler` owned by the metrics
goroutine, `Sample` per tick, `Close` at shutdown, module pin bump), then push branch + tag.
