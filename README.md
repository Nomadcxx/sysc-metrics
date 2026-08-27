# sysc-metrics

`sysc-metrics` is a small Go library for read-only Linux system telemetry. It supplies the built-in
monitoring widgets in [`sysc-shell`](https://github.com/Nomadcxx/sysc-shell) without importing a CLI,
TUI, HTTP server, or unrelated application framework.

The repository is in the design stage. It contains no production Go package yet.

## Scope

The first releases will collect:

- aggregate and per-core CPU usage, load, and frequency;
- memory and swap;
- mounted-filesystem capacity and block-device I/O rates;
- per-interface network counters and rates;
- thermal sensors and available GPU metrics;
- battery and UPS state, energy, rate, and estimated time;
- uptime and basic totals;
- process CPU and memory only when a consumer requests it.

UPower supplies the preferred battery aggregate. `/sys/class/power_supply` provides the fallback.
Collectors use Linux interfaces such as `/proc`, `/sys`, `statfs`, and D-Bus directly.

The library will not provide power controls, recursive directory sizes, filesystem indexing, SMART,
quotas, vendor administration, a daemon, or cross-platform abstractions.

## Development gates

1. Define snapshot and sampling semantics, including first-sample behavior and counter reset handling.
2. Implement CPU, memory, filesystem, block-device, and network collectors with fixture tests.
3. Add thermal and battery collection, including UPower loss and sysfs fallback tests.
4. Add GPU and process collectors only for a confirmed shell consumer.
5. Qualify suspend/resume, device removal, permission errors, and real hardware before `v0.1.0`.

See the [design](docs/plans/2026-08-27-sysc-metrics-design.md) and [roadmap](docs/roadmap.md).
Package directories will be added with their first tested behavior; the repository will not track empty
scaffolding.

## Licence

`sysc-metrics` uses the [BSD 3-Clause License](LICENSE).
