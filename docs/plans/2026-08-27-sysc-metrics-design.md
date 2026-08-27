# sysc-metrics Design

Date: 2026-08-27
Status: Approved

## Purpose

`sysc-metrics` owns reusable, read-only Linux telemetry for the sysc projects. It replaces the proposed
direct `dgop` import with a focused library that does not inherit dgop's command, server, logging, or
unrelated dependency graph.

## Boundary

The library owns collection, parsing, counter deltas, units, timestamps, availability, and typed errors.
Consumers own polling policy, caching across widgets, presentation, alerts, and control actions.

Linux is the public platform contract. The project will not add portability interfaces without a real
second platform requirement.

## Initial data

- CPU aggregate/per-core utilization, load, and frequency.
- Memory and swap totals and usage.
- Filesystem mounts: source, type, read-only state, total, used, and available space.
- Block devices: bytes, operations, busy time, and derived rates.
- Network interfaces: counters and derived rates.
- Thermal zones.
- Battery and UPS percentage, state, energy, rate, and estimated time.
- Uptime and basic machine totals.

GPU metrics remain capability-based because Linux drivers expose different files and libraries. Process
sampling starts only when the shell ships a process consumer; it must not run for aggregate widgets.

## Collection rules

Snapshots carry their collection time. Rate collectors compare two monotonic samples and report that no
rate exists for the first sample. They treat counter resets, device replacement, suspend, and elapsed-time
errors as discontinuities instead of producing spikes.

Collectors return partial data when one device fails. They expose enough error context for a consumer to
mark one sensor unavailable without discarding the rest of the snapshot. Parsers bound input and reject
invalid numeric values at `/proc`, `/sys`, and D-Bus boundaries.

Battery collection prefers UPower's `DisplayDevice` because it aggregates multiple batteries and exposes
calibrated rate and time estimates. Direct power-supply sysfs collection takes over when UPower is absent
or loses its bus name.

## API direction

Start with concrete collector functions and value types. Add a stateful sampler only where rate
calculation requires previous counters. Do not add a collector registry, plugin API, daemon protocol, or
single-implementation interface.

Use bytes, durations, counters, and ratios consistently. UI-formatted strings do not belong in this
repository.

## Proof

Fixture tests cover parsers and discontinuities. Linux integration tests compare invariants rather than
machine-specific values: totals are non-negative, used capacity does not exceed the valid total, and a
removed device disappears without corrupting another device's result. The `v0.1.0` gate includes
suspend/resume and UPower-to-sysfs fallback on real hardware.
