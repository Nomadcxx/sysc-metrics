# sysc-metrics Roadmap

Date: 2026-08-27

## M0: Foundation (complete)

- approve units, timestamps, partial-error behavior, and rate semantics;
- record dgop provenance used as behavioral reference;
- create the Go module only when the first collector starts.

Gate: public types do not contain shell presentation or cross-platform abstractions.

## M1: Core counters (complete)

- CPU, memory, swap, uptime, filesystem capacity, block-device counters, and network counters;
- fixture parsers and stateful rate tests;
- counter-reset, device-removal, and malformed-input handling.

Gate: `go test -race ./...` passes and Linux integration checks establish sane invariants.

## M2: Power and thermal

- thermal zones;
- UPower `DisplayDevice` battery and UPS state;
- `/sys/class/power_supply` fallback;
- source transition, multi-battery, suspend/resume, and missing-service tests.

Gate: one shell consumer can swap data sources without changing its view model.

## M3: Consumer-gated collectors

- GPU metrics required by a shipped widget;
- process CPU and memory required by a shipped process view.

Gate: collectors remain idle when no consumer requests them.

## M4: First release

- qualify the API from `sysc-shell`;
- record supported Linux interfaces and known driver gaps;
- tag `v0.1.0` after real-hardware checks.

Audio, MPRIS, power controls, indexing, SMART, and storage administration remain outside this repository.
