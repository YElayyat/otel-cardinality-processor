# Cardinality Guardian

[![Build](https://github.com/YElayyat/otel-cardinality-processor/actions/workflows/ci.yml/badge.svg)](https://github.com/YElayyat/otel-cardinality-processor/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/YElayyat/otel-cardinality-processor)](https://goreportcard.com/report/github.com/YElayyat/otel-cardinality-processor)
[![Coverage](https://codecov.io/gh/YElayyat/otel-cardinality-processor/branch/main/graph/badge.svg)](https://codecov.io/gh/YElayyat/otel-cardinality-processor)
[![Go Reference](https://pkg.go.dev/badge/github.com/YElayyat/otel-cardinality-processor.svg)](https://pkg.go.dev/github.com/YElayyat/otel-cardinality-processor)
[![Release](https://img.shields.io/github/v/tag/YElayyat/otel-cardinality-processor?label=Release&color=blue)](https://github.com/YElayyat/otel-cardinality-processor/releases)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)
[![Status](https://img.shields.io/badge/Status-Development-yellow)](./CONTRIBUTING.md)
[![Benchmarks](https://img.shields.io/badge/Benchmarks-~91ns%2Fop%20|%20827K%20metrics%2Fs-brightgreen)](./BENCHMARKS.md)

An OpenTelemetry Collector processor that enforces per-metric label cardinality limits using HyperLogLog++ sketches. It is designed to stop cardinality explosions from reaching expensive time-series databases before the bill does.

---

## Why This Exists

In high-throughput observability pipelines, a single misbehaving label can silently destroy a budget. Labels such as `user_id`, `session_id`, or `request_id` can each produce millions of unique values per day. Every unique label combination creates a new time series. At the per-series pricing of managed TSDBs like Datadog, Honeycomb, or Grafana Cloud, a single unchecked `session_id` label can cost thousands of dollars per month — and the first sign of the problem is often the invoice.

Cardinality Guardian sits inside the OTel Collector pipeline, directly upstream of your TSDB exporter. It tracks the rate of new unique label values per metric using a probabilistic sketch, and enforces a configurable ceiling. Labels that exceed the ceiling are either stripped before export (Enforcement Mode) or tagged with a routing attribute so a downstream processor can divert them to cheap object storage (Tag-Only Mode).

The result is a hard cardinality budget with full observability into what is being enforced and an estimated dollar value of what was saved.

---

## Design Decisions

> **Summary of the Hot Path Flow (Life of a Metric):**
> 1. Metric arrives -> Hash the metric name -> Pick 1 of 256 independent shards.
> 2. Lock the shard -> Hash the label string directly on the stack (`xxhash`, 0 allocations).
> 3. Insert the hash into the current epoch's HyperLogLog++ sketch.
> 4. If it's the 64th insert, update the cached size estimate.
> 5. If the estimate > limit, strip the label (or tag it in `tag_only` mode).
> 6. Unlock the shard.

### 256-Way Sharded Mutexes

A naive implementation uses a single `sync.RWMutex` to protect the global map of cardinality trackers. Under concurrent load from the Collector's goroutine pool, every metric data point would contend on that one lock. Throughput plateaus regardless of CPU count.

Cardinality Guardian partitions the tracker map into 256 independent shards. Each shard has its own `sync.RWMutex`. Incoming data points are routed to a shard by hashing the metric name with `maphash.String` (zero allocation, fixed seed per process). Under a uniform metric-name distribution, the probability that two concurrent goroutines land on the same shard is 1/256. Lock contention becomes negligible and throughput scales near-linearly with core count.

The shard count is a power of two, so the routing operation is a single bitmask `AND` with no division. Shard boundaries are also respected during epoch rotation: each shard is locked independently and for the minimum possible duration — only long enough to snapshot tracker pointers, never during sketch allocation.

### HyperLogLog++ Math

Counting exact unique values requires memory proportional to the number of unique values seen — impractical at scale. HyperLogLog++ is a probabilistic algorithm that estimates the cardinality of a set using a fixed amount of memory, regardless of how many elements are inserted.

Cardinality Guardian uses precision parameter `p=14`, which allocates 2^14 = 16,384 registers per sketch and yields a standard error of approximately 0.81%. Each sketch occupies roughly 12 KB in dense mode. The processor maintains two sketches per `(metric_name, label_key)` pair — one for the current epoch and one for the previous epoch — and enforces limits on the *delta* (new unique values seen this epoch) rather than the absolute cardinality. This means the processor only penalizes metrics that are actively growing, not metrics that have reached a stable high-cardinality state.

### Zero-Allocation Hot Path

The processor is called on every data point, at rates that can exceed one million per second in production pipelines. Any heap allocation in the hot path increases GC pressure and latency variance.

Two specific design choices keep allocations at zero in steady state:

**`xxhash.Sum64String` instead of `Insert([]byte)`.**
The underlying HLL library's `Insert([]byte)` method calls an internal `hash` function variable. Because the Go compiler cannot inline through a function variable, any `[]byte` argument escapes to the heap. By hashing the attribute value with `xxhash.Sum64String` before acquiring any lock — returning a `uint64` on the stack — and passing that directly to `InsertHash(uint64)`, the entire hash operation is allocation-free.

**`sync.Pool` for HLL sketch allocation.**
Allocating a fresh `hyperloglog.Sketch` for every new `(metric, label)` pair or every epoch rotation would generate GC pressure during cardinality explosions, precisely when the processor is busiest. A package-level `sync.Pool` pre-allocates sketches and vends them at O(1) cost. The pool's `New` function always returns a `*hyperloglog.Sketch`, so the type assertion is guaranteed and is performed through a dedicated `mustGetSketch()` helper that panics explicitly on violation rather than silently using a zero value.

**Lazy cached estimates.**
Calling `Sketch.Estimate()` in the axiomhq/hyperloglog library triggers an internal `mergeSparse()` that allocates approximately five heap objects per call when the sketch is in sparse mode. The processor caches the last estimate in the tracker struct and refreshes it at most once every 64 inserts using a power-of-two bitmask check (a single `AND` instruction, no division). Phase 1 — the first 64 inserts — estimates on every insert to ensure the cardinality limit is enforced accurately during the initial growth period. This two-phase strategy reduces the allocation rate from 5 allocs/op to 0 allocs/op as reported by Go's benchmark tooling.

The measured result: **~48 ns/op, 0 allocs/op** on a `BenchmarkShouldDrop_HighThroughput` run with 6 parallel goroutines on a Go 1.25 / AMD EPYC host. 

> [!NOTE]
> Processor architectures containing asymmetrical Efficiency Cores (like Apple Silicon M1/M2/M3 chips) may exhibit higher artificial per-operation latency in parallel macrobenchmarks as the Go scheduler overflows from Performance cores into Efficiency cores. Single-threaded (`-cpu=1`) runs on top-tier silicon consistently benchmark beneath **35 ns/op**.

📊 **For the full benchmark suite** — including `consumertest` pipeline benchmarks, `telemetrygen` load tests (827K metrics/sec), and sustained memory stability results — see **[BENCHMARKS.md](./BENCHMARKS.md)**.

---

## Operation Modes

### Enforcement Mode (default)

When `tag_only: false`, attributes that breach the cardinality limit are silently removed from the data point before it reaches the downstream exporter. The metric itself is preserved; only the high-cardinality label is stripped.

This is the recommended mode when the goal is pure cost control and the stripped labels are not required for query correctness in the primary TSDB.

```
Before:  {region="us-east", status="200", session_id="a3f9c..."}  ← over limit
After:   {region="us-east", status="200"}
```

### Tag-Only Mode

When `tag_only: true`, no attribute is ever deleted. Instead, the processor injects a boolean attribute `cardinality_limit_exceeded: true` into any data point where at least one label breached the limit.

```
Before:  {region="us-east", status="200", session_id="a3f9c..."}  ← over limit
After:   {region="us-east", status="200", session_id="a3f9c...", cardinality_limit_exceeded: true}
```

A downstream OTel routing processor can then match on this attribute and forward the tagged metric to cheap object storage (S3, GCS, etc.) while clean metrics continue flowing to the primary TSDB. This makes cardinality enforcement non-destructive and reversible, which is valuable during initial rollout or in regulated environments where data must not be dropped.

---
## Building the Collector
Because this is a custom processor, you must compile it into your binary using the OpenTelemetry Collector Builder (OCB). See the [official documentation](https://opentelemetry.io/docs/collector/extend/ocb/) for full details and release mapping.

### 1. Download OpenTelemetry Collector Builder (OCB)
You must download the specific `ocb` binary that matches both your operating system, your chipset, and your desired OpenTelemetry version. Be very careful to select the right asset from the releases page (e.g., Linux vs macOS, AMD64 vs ARM64).

For example, to download OTel `v0.148.0` on macOS ARM64:
```bash
curl --proto '=https' --tlsv1.2 -fL -o ocb \
https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/cmd%2Fbuilder%2Fv0.148.0/ocb_0.148.0_darwin_arm64
chmod +x ocb
```

### 2. Configure the Builder (`builder.yaml`)
Create a manifest file named `builder.yaml`. Ensure the component versions exactly match the version of your downloaded `ocb` binary (e.g., `v0.148.0`). You must also include the `name` and `import` overrides to correctly handle the hyphenated module path for the Cardinality Guardian.

```yaml
dist:
  name: otelcol-custom
  description: Custom OTel Collector with Cardinality Guardian
  output_path: ./build

# Add your receivers, exporters, and other processors here
exporters:
  - gomod: go.opentelemetry.io/collector/exporter/debugexporter v0.148.0

receivers:
  - gomod: go.opentelemetry.io/collector/receiver/otlpreceiver v0.148.0

processors:
  - gomod: go.opentelemetry.io/collector/processor/batchprocessor v0.148.0
  - gomod: github.com/YElayyat/otel-cardinality-processor v1.0.0
    name: cardinalityprocessor
    import: github.com/YElayyat/otel-cardinality-processor/cardinalityprocessor
```

### 3. Compile the Binary
Use the `ocb` binary you downloaded in step 1 to compile your custom collector:

```bash
# Build the custom binary
./ocb --config=builder.yaml
```

Once the build successfully completes, OCB will create a new directory directly under the project root called `build/`. Inside this directory, you will find your compiled, static binary named `otelcol-custom`.

### 4. Configure and Run
Before running the built collector, you must create a configuration file (`otel-collector-config.yaml`) that defines your Cardinality Guardian pipeline parameters. Add the processor to your pipeline:

```yaml
# otel-collector-config.yaml

processors:
  cardinality_guardian:
    # Maximum number of new unique values allowed for a single (metric, label)
    # pair within one epoch. Only the delta — new values seen this epoch — is
    # counted, not the absolute lifetime cardinality of the metric.
    max_cardinality_delta_per_epoch: 500

    # Length of the sliding cardinality window in seconds. At the end of each
    # epoch the current HLL sketch is promoted to "previous" and a fresh sketch
    # starts accumulating. Shorter epochs react faster to explosions but
    # produce noisier decisions for naturally bursty label spaces.
    epoch_duration_seconds: 300

    # Labels that are never stripped or tagged, regardless of cardinality.
    # Include any label whose values are essential for query correctness.
    never_drop_labels:
      - region
      - http.status_code
      - service.name

    # Set to true to inject 'cardinality_limit_exceeded: true' instead of
    # stripping the attribute. Enables dual-route cold-storage patterns.
    # Set to false (default) for hard enforcement.
    tag_only: false

    # Dollar value assigned to each unique time series prevented from entering
    # a paid TSDB. Used exclusively to populate the estimated_savings_dollars_total
    # counter for cost dashboards. Has no effect on enforcement logic.
    estimated_cost_per_metric_month: 0.05

service:
  pipelines:
    metrics:
      receivers:  [otlp]
      processors: [cardinality_guardian]
      exporters:  [prometheusremotewrite]
```

Once your configuration is ready, run your custom binary:

```bash
# Run the collector with your pipeline configuration
./build/otelcol-custom --config=otel-collector-config.yaml
```

### 5. Example Configurations & Visualization

For your convenience, the repository includes a dedicated `examples/` directory containing complete, production-ready templates:

#### 📊 Local Visualization Stack (Prometheus + Grafana)
Located in `examples/prometheus/`, this is a completely automated Docker Compose stack that spins up a pre-configured Grafana dashboard. It is auto-provisioned to scrape your custom collector and immediately visualize your cardinality savings.
- **`docker-compose.yaml`**: One-click boot for the visualization infrastructure.
- **`provisioning/`**: Automated data source linking for a zero-config experience.

#### 🐶 Datadog Native Export
Located in `examples/datadog/`, this template shows the production-grade way to route both your application metrics and the Guardian's internal ROI telemetry directly into the Datadog SaaS using a `DD_API_KEY`.
- **`otel-datadog-config.yaml`**: Optimized pipeline for Datadog ingestion.

#### 🏗️ Builder Manifest
- **`examples/builder.yaml`**: The exact manifest used to compile the `otelcol-custom` binary containing this module.

---

## Built-In Telemetry and ROI Tracking

Cardinality Guardian emits three internal OTel metrics under the instrumentation scope `cardinality_guardian` to help you monitor its behavior and your savings.

| Metric | Type | Description |
|---|---|---|
| `processor_labels_stripped_total` | Counter | Increments once per attribute key that is stripped or tagged per data point. Use this to detect enforcement spikes and build alerts. |
| `estimated_savings_dollars_total` | Counter | Accumulates the dollar value of time series prevented from reaching a paid TSDB, based on `estimated_cost_per_metric_month`. Apply `rate()` in your monitoring platform to see the current savings rate. |
| `processor_trackers_active` | Gauge | Current number of live `(metric, label_key)` cardinality trackers across all 256 shards. Useful for capacity planning and detecting tracker map growth. |

### Standard Pipeline Metrics
Alongside the custom metrics above, the OpenTelemetry Collector automatically emits standard pipeline telemetry. To build a complete dashboard or calculate what percentage of your total ingest was safely guarded, you should also monitor these standard metrics:

| Metric | Type | Description |
|---|---|---|
| `otelcol_receiver_accepted_metric_points_total` | Counter | Total number of metrics successfully ingested by the collector (the "before" count). |
| `otelcol_exporter_sent_metric_points_total` | Counter | Total number of metrics successfully exported to your final TSDB (the "after" count). |

### How to Access These Metrics

OpenTelemetry handles internal telemetry entirely separately from the `pipelines` block. To access these metrics, you must explicitly enable a telemetry reader in the `service.telemetry` section of your configuration.

#### Option 1: Prometheus Endpoint (cURL or Scrape)
You can expose the metrics over a standard HTTP Prometheus endpoint. Add this to the very bottom of your `otel-collector-config.yaml`:
```yaml
service:
  telemetry:
    metrics:
      readers:
        - pull:
            exporter:
              prometheus:
                host: 0.0.0.0
                port: 8888
```
Once the collector is running, you can manually test it locally using `cURL` and filter for the processor's custom metrics:
```bash
curl -s http://localhost:8888/metrics | grep -E '^estimated_savings|^processor_'
```
Alternatively, configure your main Prometheus server to point a static scrape job at `ip:8888`.

#### Option 2: Routing into your regular OTLP pipeline
If you want these internal metrics to ride the exact same exporter pipeline (e.g., straight to Datadog) as your application metrics, configure an OTLP reader bound to your internal metrics pipeline:
```yaml
service:
  telemetry:
    metrics:
      readers:
        - periodic:
            exporter:
              otlp:
                endpoint: "localhost:4317" # Loops back into your main receiver
```

### ROI Monitoring
Once the metrics are flowing to your TSDB, you can run this PromQL query to extrapolate the current 5-minute drop rate into an estimated monthly dollar figure:
```promql
rate(estimated_savings_dollars_total[5m]) * 60 * 60 * 24 * 30
```

---

## Getting Started

**Prerequisites:** Go 1.25 or later, GNU Make.

```bash
# Clone and enter the repository
git clone https://github.com/YElayyat/otel-cardinality-processor.git
cd otel-cardinality-processor

# Compile all packages (confirms the build is clean)
make build

# Run the full unit test suite with the race detector
make test

# Install golangci-lint and run static analysis
make install-lint
make lint

# Fuzz the core cardinality decision for 60 seconds
make fuzz FUZZ_TIME=60s

# Hammer the concurrency paths under the race detector
make stress-test STRESS_COUNT=1000

# Build a custom collector and run the black-box E2E test
make e2e
```

To integrate the processor into a custom Collector distribution, register the factory in your builder configuration:

```go
import "github.com/YElayyat/otel-cardinality-processor/cardinalityprocessor"

// Pass to your ocb configuration or Go-based Collector builder.
cardinalityprocessor.NewFactory()
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
├── BENCHMARKS.md               # Reproducible performance data
├── FAQ.md                      # Pragmatic Q&A for evaluators and adopters
├── SECURITY.md                 # Vulnerability reporting policy
└── go.mod
```

---

## Supported Metric Types

Cardinality Guardian processes all five OpenTelemetry metric data types: **Gauge**, **Sum**, **Histogram**, **ExponentialHistogram**, and **Summary**. Each data point's attributes are evaluated independently against the cardinality limit. The processor never modifies the metric value, type, or temporality — only the attribute set on individual data points that breach the configured threshold.

---

## Understanding the Component Names

Four different identifiers appear across the project. They serve completely different purposes in different systems — nothing is wrong or inconsistent.

**`otel-cardinality-processor`** — the Go module name, declared in `go.mod`. This is the repository and module identifier used by the Go toolchain and module proxy. It appears in `builder.yaml` under `gomod:` and in import paths. You never type this in a collector config file.

**`cardinalityprocessor`** — the Go package name, which is the name of the `cardinalityprocessor/` subdirectory. It appears in two places: as the `name:` alias in `builder.yaml` (because OCB needs a valid Go identifier to use in generated code — the module name above has hyphens, which Go forbids as identifiers), and in any Go code that imports the factory directly (`import "…/cardinalityprocessor"`). Again, never appears in a collector config file.

**`cardinality_guardian`** — the OTel component type string, registered inside `factory.go` with `component.MustNewType("cardinality_guardian")`. This is the only name that customers put in their `otel-collector-config.yaml` under `processors:`. It is completely independent of the Go module name or package name.

**`otelcol-custom`** — the name of the compiled collector binary, set by `dist.name` in `builder.yaml`. This is just the output filename of the binary OCB produces. Customers can rename it to anything — `otelcol-mycompany`, `collector`, whatever. It has no effect on how the processor works.

| Name | Where it appears | Set by |
|---|---|---|
| `otel-cardinality-processor` | `go.mod`, `builder.yaml gomod:` field | `go.mod` module declaration |
| `cardinalityprocessor` | `builder.yaml name:` field, Go import statements | Go package name of the subdirectory |
| `cardinality_guardian` | `otel-collector-config.yaml processors:` block | `factory.go` component type registration |
| `otelcol-custom` | Output binary filename | `builder.yaml dist.name` |

---

## Troubleshooting

### Zombie Collector Processes
When rapidly iterating on the Collector configuration or recompiling the binary, using `SIGTERM` (like `Ctrl+C` in a shell script) might occasionally leave the OTLP gRPC port bound, or leave a ghost process running in the background. Because the Collector can run mutely, your test scripts might actually be hitting an old version of the Collector with outdated cardinality limits.

**The Fix:** Always forcefully kill the custom collector between test runs using `pkill -9 otelcol-custom`.

---

## Further Reading

See [FAQ.md](FAQ.md) for answers to common questions about safety, performance, production rollout, and how Cardinality Guardian compares to SDK limits, static Collector processors, and TSDB-level enforcement.

---

## Contributing

We welcome issues and pull requests! Please prioritize a conversation by opening an issue before submitting massive architectural changes. 

Please see [CONTRIBUTING.md](CONTRIBUTING.md) for instructions on our development workflow, formatting guidelines, and standards for submitting changes.

Local development requires Go 1.25+ and `make`. The `make test` suite strictly enforces data-race detection and deterministic cardinality evaluations.

---

## License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.


