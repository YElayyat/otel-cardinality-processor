# Benchmarks

Production-grade performance data for the **Cardinality Guardian** OpenTelemetry processor.

> **All results are reproducible.** Exact commands are provided for every number.

## Environment

| Property | Value |
|---|---|
| **CPU** | Apple M2 (8 cores: 4P + 4E) |
| **OS** | macOS (darwin/arm64) |
| **Go** | 1.25.0 |
| **OTel SDK** | v1.54.0 / v0.148.0 |
| **Shards** | 256 |
| **HLL Precision** | p=14 (~0.81% standard error) |

---

## 1. Micro-Benchmarks (`consumertest` Pipeline)

These benchmarks exercise the **full `ConsumeMetrics` pipeline** through a `consumertest.MetricsSink` consumer, isolating the processor's CPU and allocation cost from network I/O.

```
go test -bench=Benchmark -benchmem -count=3 -timeout 120s ./cardinalityprocessor/...
```

### Results

| Benchmark | ns/op | B/op | allocs/op | What it measures |
|---|---:|---:|---:|---|
| `ShouldDrop_HighThroughput` | **~91** | 1 | **0** | Pure hot-path cost (pre-warmed trackers) |
| `ConsumeMetrics_Passthrough` | **~1,323** | 84 | 4 | Full pipeline, no labels dropped |
| `ConsumeMetrics_MixedMetricTypes` | **~353** | 40 | 2 | All 5 metric types in one batch |
| `ConsumeMetrics_HighCardinality` | ~131,111 | ~37,871 | 88 | Full pipeline, every label dropped |
| `ConsumeMetrics_LargeBatch` | ~101,677 | 6,924 | 205 | 1,000 data points in one payload |

### Key Takeaways

- **Happy path overhead: ~1.3 μs per batch** (10 data points × 3 labels each). This is the tax every metric pays when nothing is being dropped.
- **Type dispatch is free:** Processing all 5 OTel metric types (Gauge, Sum, Histogram, ExponentialHistogram, Summary) in a single batch costs only **353 ns** — proving the `switch` dispatch adds no measurable overhead.
- **Large batch amortization:** 1,000 data points process in ~102 μs, yielding an amortized cost of **~102 ns per data point**.
- **Hot-path (shouldDrop): ~91 ns, 0 allocs.** This is the steady-state uncontended cost measured with pre-warmed trackers and per-goroutine key isolation.

---

## 2. Sustained Load Test (Memory Stability)

This macro-benchmark runs the processor under **60 seconds of sustained high-cardinality load** from 8 concurrent workers, sampling heap memory every 5 seconds. It proves the stale tracker eviction mechanism prevents unbounded memory growth (OOM).

```
go test -v -run='^$' -bench=BenchmarkSustainedLoad -benchmem -timeout 5m ./test/benchmark/...
```

### Results

| Metric | Value |
|---|---|
| **Duration** | 60.0 seconds |
| **Workers** | 8 |
| **Total Metrics Processed** | 52,249,400 |
| **Throughput** | **870,814 metrics/sec** |
| **Epoch Duration** | 5s (fast rotation for eviction testing) |
| **Cardinality Limit** | 500 per metric/key |

### Memory Profile

| Time | HeapAlloc (MB) |
|---:|---:|
| T+5s | 5.19 |
| T+10s | 5.31 |
| T+15s | 5.93 |
| T+20s | 5.83 |
| T+25s | 6.13 |
| T+30s | 5.18 |
| T+35s | 4.75 |
| T+40s | 4.84 |
| T+45s | 5.69 |
| T+50s | 7.89 |
| T+55s | 4.77 |
| T+60s | 5.92 |

**Early heap (T=10s): 5.31 MB → Final heap (T=60s): 5.92 MB → Ratio: 1.12x**

✅ **MEMORY STABLE:** After processing 52 million metrics over 60 seconds with constant cardinality pressure and 12 epoch rotations, the heap grew by only 12%. The stale tracker eviction mechanism is working correctly — no unbounded growth detected.

---

## 3. Load Testing (`telemetrygen`)

A custom-built OTel Collector (with Cardinality Guardian in the pipeline) is blasted by `telemetrygen` using **8 concurrent workers at unlimited rate** (`--rate 0`). Three passes cover all supported metric types.

```
make bench-load
```

### Results

| Metric Type | Total Metrics | Throughput | Duration |
|---|---:|---:|---|
| **Gauge** | 7,853,465 | **785,346 metrics/sec** | 10s |
| **Sum** | 9,190,694 | **919,069 metrics/sec** | 10s |
| **Histogram** | 7,765,961 | **776,596 metrics/sec** | 10s |
| **Combined** | **24,810,120** | **~827K metrics/sec avg** | 30s |

✅ **Zero errors, zero backpressure, zero dropped spans.** The 256-shard architecture handles unlimited concurrent load without choking.

### Profiling

The collector config includes the `pprof` extension on `:1777` for CPU profiling and flame graph generation:

```bash
# Capture a 10-second CPU profile during a load test:
curl -s "http://localhost:1777/debug/pprof/profile?seconds=10" -o cpu.prof

# View as an interactive flame graph:
go tool pprof -http=:8080 cpu.prof
```

---

## Reproducing These Results

```bash
# All micro-benchmarks
make bench

# Sustained load test only
go test -v -run='^$' -bench=BenchmarkSustainedLoad -benchmem -timeout 5m ./test/benchmark/...

# Full load test with telemetrygen + pprof (requires ocb)
make bench-load
```

> **Note:** Processor architectures containing asymmetrical Efficiency Cores (like Apple Silicon M1/M2/M3 chips) may exhibit higher artificial per-operation latency in parallel macrobenchmarks as the Go scheduler overflows from Performance cores into Efficiency cores.
