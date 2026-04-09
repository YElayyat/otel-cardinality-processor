# Cardinality Guardian

[![Build](https://github.com/YElayyat/otel-cardinality-processor/actions/workflows/ci.yml/badge.svg)](https://github.com/YElayyat/otel-cardinality-processor/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/YElayyat/otel-cardinality-processor)](https://goreportcard.com/report/github.com/YElayyat/otel-cardinality-processor)
[![Coverage](https://codecov.io/gh/YElayyat/otel-cardinality-processor/branch/main/graph/badge.svg)](https://codecov.io/gh/YElayyat/otel-cardinality-processor)
[![Go Reference](https://pkg.go.dev/badge/github.com/YElayyat/otel-cardinality-processor.svg)](https://pkg.go.dev/github.com/YElayyat/otel-cardinality-processor)
[![Release](https://img.shields.io/github/v/tag/YElayyat/otel-cardinality-processor?label=Release&color=blue)](https://github.com/YElayyat/otel-cardinality-processor/releases)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)
[![Status](https://img.shields.io/badge/Status-Development-yellow)](./CONTRIBUTING.md)
[![Benchmarks](https://img.shields.io/badge/Benchmarks-~91ns%2Fop%20|%20827K%20metrics%2Fs-brightgreen)](./BENCHMARKS.md)

An OpenTelemetry Collector processor that enforces per-metric, per-attribute cardinality limits using HyperLogLog++ sketches. It stops cardinality explosions from reaching expensive time-series databases — before the bill does.

---

## The Problem

It only takes one bad deployment. A bug accidentally logs raw database exceptions into the `error.type` attribute instead of a clean string:

* ✅ **Expected:** `error.type = "db_timeout"`
* 💥 **The Bug:** `error.type = "Lock wait timeout exceeded; txn_id=8f7d6a5b..."`

Because every transaction ID is unique, your timeseries count skyrockets from 5 to 50,000 overnight. Your Datadog bill spikes 1.5x.

Cardinality Guardian catches this surgically — it strips only the exploding `error.type` attribute while leaving your other dimensions (`api`, `region`, `status_code`) completely untouched. Your dashboards stay alive, your on-call stays asleep.

---

## How It Works

The processor limits cardinality at the **attribute level**, not the metric level. It creates a separate HyperLogLog++ sketch for every `(metric_name, attribute_key)` pair and enforces limits on the *delta* (new unique values per epoch), not the absolute count.

> **Hot path:** Metric arrives → hash metric name → pick 1 of 256 shards → hash the attribute value on-stack (0 allocs) → insert into HLL sketch → enforce if over limit → unlock.

**Result:** ~48 ns/op, 0 allocs/op at 1M+ data points per second. See [ARCHITECTURE.md](ARCHITECTURE.md) for the full design rationale.

---

## Operation Modes

### Enforcement Mode (default)

Attributes that breach the cardinality limit are stripped from the data point before export. The metric itself is preserved.

```
Before:  {region="us-east", status="200", error.type="Lock wait timeout; txn=a3f9c..."}  ← over limit
After:   {region="us-east", status="200"}
```

The 50,000 unique `error.type` values are flattened instantly. Your `region` and `status` dimensions survive untouched, so your P99 latency and traffic dashboards keep working.

### Tag-Only Mode

When `tag_only: true`, nothing is deleted. The processor injects `otel.metric.overflow: true` so a downstream routing processor can divert tagged metrics to cheap storage while clean metrics flow to your primary TSDB.

```
Before:  {region="us-east", status="200", error.type="Lock wait timeout; txn=a3f9c..."}  ← over limit
After:   {region="us-east", status="200", error.type="Lock wait timeout; txn=a3f9c...", otel.metric.overflow: true}
```

No data is lost. You can route the tagged overflow to S3 or a dev TSDB for debugging, while clean metrics continue flowing to production. This is ideal for initial rollout — observe first, enforce later.

---

## Building the Collector

Because this is a custom processor, you must compile it into your binary using the OpenTelemetry Collector Builder (OCB). See the [official documentation](https://opentelemetry.io/docs/collector/extend/ocb/) for full details and release mapping.

### 1. Download OCB

You must download the specific `ocb` binary that matches both your operating system, your chipset, and your desired OpenTelemetry version. Be very careful to select the right asset from the releases page (e.g., Linux vs macOS, AMD64 vs ARM64).

For example, to download OTel `v0.148.0` on macOS ARM64:
```bash
curl --proto '=https' --tlsv1.2 -fL -o ocb \
https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/cmd%2Fbuilder%2Fv0.148.0/ocb_0.148.0_darwin_arm64
chmod +x ocb
```

### 2. Create `builder.yaml`

Create a manifest file named `builder.yaml`. Ensure the component versions exactly match the version of your downloaded `ocb` binary (e.g., `v0.148.0`). You must also include the `name` and `import` overrides to correctly handle the hyphenated module path for the Cardinality Guardian.

```yaml
dist:
  name: otelcol-custom
  description: Custom OTel Collector with Cardinality Guardian
  output_path: ./build

exporters:
  - gomod: go.opentelemetry.io/collector/exporter/debugexporter v0.148.0

receivers:
  - gomod: go.opentelemetry.io/collector/receiver/otlpreceiver v0.148.0

processors:
  - gomod: go.opentelemetry.io/collector/processor/batchprocessor v0.148.0
  - gomod: github.com/YElayyat/otel-cardinality-processor v1.0.1
    name: cardinalityprocessor
    import: github.com/YElayyat/otel-cardinality-processor/cardinalityprocessor
```

### 3. Compile the Binary

```bash
./ocb --config=builder.yaml
```

Once the build successfully completes, OCB will create a new directory directly under the project root called `build/`. Inside this directory, you will find your compiled, static binary named `otelcol-custom`.

### 4. Configure and Run

Before running the built collector, you must create a configuration file (`otel-collector-config.yaml`) that defines your Cardinality Guardian pipeline parameters. Add the processor to your pipeline:

```yaml
# otel-collector-config.yaml

processors:
  cardinality_guardian:
    max_cardinality_delta_per_epoch: 500    # Max new unique values per (metric, attribute) per epoch
    epoch_duration_seconds: 300              # Length of the sliding window
    never_drop_labels:                       # Labels that are never stripped
      - region
      - http.status_code
      - service.name
    tag_only: false                           # true = observe only, false = enforce
    estimated_cost_per_metric_month: 0.05    # For ROI tracking ($/series/month)

service:
  pipelines:
    metrics:
      receivers:  [otlp]
      processors: [cardinality_guardian]
      exporters:  [prometheusremotewrite]
```

Once your configuration is ready, run your custom binary:

```bash
./build/otelcol-custom --config=otel-collector-config.yaml
```

---

## Built-In Telemetry

| Metric | Type | Description |
|---|---|---|
| `processor_labels_stripped_total` | Counter | Attributes stripped or tagged per data point. Use `rate()` for spike detection. |
| `estimated_savings_dollars_total` | Counter | Dollar value of series prevented from reaching your TSDB. |
| `processor_trackers_active` | Gauge | Live `(metric, attribute)` trackers across all 256 shards. |

See [ARCHITECTURE.md](ARCHITECTURE.md#telemetry-deep-dive) for how to expose these metrics via Prometheus or OTLP, and ROI monitoring queries.

---

## Example Configurations

The `examples/` directory includes production-ready templates:

* **`examples/prometheus/`** — Docker Compose stack with pre-configured Grafana dashboard
* **`examples/datadog/`** — Datadog native export pipeline
* **`examples/builder.yaml`** — OCB build manifest

---

## Getting Started (Development)

**Prerequisites:** Go 1.25+, GNU Make.

```bash
git clone https://github.com/YElayyat/otel-cardinality-processor.git
cd otel-cardinality-processor

make build          # Compile all packages
make test           # Unit tests with race detector
make install-lint   # Install golangci-lint
make lint           # Static analysis
make fuzz FUZZ_TIME=60s   # Fuzz the core decision logic
make stress-test STRESS_COUNT=1000   # Concurrency stress test
make e2e            # Build custom collector + black-box E2E test
```

---

## Project Layout

```
cardinality-guardian/
├── cardinalityprocessor/       # Core processor package
│   ├── config.go               # Config struct with field-level documentation
│   ├── factory.go              # OTel Collector factory registration
│   ├── processor.go            # Hot path, HLL brain, 256-shard architecture
│   ├── processor_test.go       # Unit and benchmark tests
│   └── processor_fuzz_test.go  # Fuzz harness for shouldDrop
├── internal/cmd/stress/        # Long-running stress tool with pprof support
├── test/
│   ├── e2e/                    # Black-box integration test scaffold
│   └── benchmark/              # Sustained load & memory stability tests
├── examples/
│   ├── builder.yaml            # OCB build manifest
│   ├── otel-collector-config.yaml
│   ├── prometheus/             # Docker Compose stack for Prometheus + Grafana
│   └── datadog/                # Datadog native export pipeline config
├── scripts/
│   ├── install-lint.sh         # Installs golangci-lint via go install
│   └── benchmark_telemetrygen.sh  # telemetrygen load test with pprof
├── .golangci.yml               # Strict linter configuration
├── Makefile                    # Build, test, bench, fuzz, lint, stress, e2e targets
├── ARCHITECTURE.md             # Design decisions, internals, and telemetry deep dive
├── BENCHMARKS.md               # Reproducible performance data
├── FAQ.md                      # Pragmatic Q&A for evaluators and adopters
├── SECURITY.md                 # Vulnerability reporting policy
└── go.mod
```

---

## Further Reading

* **[ARCHITECTURE.md](ARCHITECTURE.md)** — Design decisions, HLL math, sharding, zero-alloc hot path, telemetry setup, component naming
* **[FAQ.md](FAQ.md)** — Safety, accuracy, production rollout, comparison with SDK/TSDB limits
* **[BENCHMARKS.md](BENCHMARKS.md)** — Full benchmark suite with reproducible numbers
* **[CONTRIBUTING.md](CONTRIBUTING.md)** — Development workflow and submission guidelines

---

## Contributing

We welcome issues and pull requests! Please open an issue before submitting large architectural changes. See [CONTRIBUTING.md](CONTRIBUTING.md) for details.

---

## License

This project is licensed under the Apache License 2.0 — see the [LICENSE](LICENSE) file for details.
