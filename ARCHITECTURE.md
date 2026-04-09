# Architecture & Internals

A deep dive into Cardinality Guardian's design decisions, internal telemetry, and operational details for contributors and advanced evaluators.

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

## Supported Metric Types

Cardinality Guardian processes all five OpenTelemetry metric data types: **Gauge**, **Sum**, **Histogram**, **ExponentialHistogram**, and **Summary**. Each data point's attributes are evaluated independently against the cardinality limit. The processor never modifies the metric value, type, or temporality — only the attribute set on individual data points that breach the configured threshold.

---

## Telemetry Deep Dive

### How to Access Internal Metrics

OpenTelemetry handles internal telemetry entirely separately from the `pipelines` block. To access Cardinality Guardian's metrics, you must explicitly enable a telemetry reader in the `service.telemetry` section of your configuration.

#### Option 1: Prometheus Endpoint (cURL or Scrape)
Expose the metrics over a standard HTTP Prometheus endpoint:
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
Test locally:
```bash
curl -s http://localhost:8888/metrics | grep -E '^estimated_savings|^processor_'
```
Alternatively, configure your main Prometheus server to point a static scrape job at `ip:8888`.

#### Option 2: Routing into your regular OTLP pipeline
If you want these internal metrics to ride the exact same exporter pipeline (e.g., straight to Datadog) as your application metrics:
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
Once the metrics are flowing to your TSDB, extrapolate the current 5-minute drop rate into an estimated monthly dollar figure:
```promql
rate(estimated_savings_dollars_total[5m]) * 60 * 60 * 24 * 30
```

### Standard Pipeline Metrics
Alongside Cardinality Guardian's custom metrics, the OpenTelemetry Collector automatically emits standard pipeline telemetry:

| Metric | Type | Description |
|---|---|---|
| `otelcol_receiver_accepted_metric_points_total` | Counter | Total metrics ingested by the collector (the "before" count). |
| `otelcol_exporter_sent_metric_points_total` | Counter | Total metrics exported to your final TSDB (the "after" count). |

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
