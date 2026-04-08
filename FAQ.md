# Frequently Asked Questions

A pragmatic Q&A for Platform Engineers, SREs, and Observability leads evaluating Cardinality Guardian for a production OpenTelemetry Collector pipeline.

---

### 1. How do I know this won't silently strip labels my dashboards depend on?

Cardinality Guardian provides an explicit safelist via the `never_drop_labels` configuration field. Any label key listed there — for example `region`, `service.name`, or `http.status_code` — is unconditionally exempt from enforcement. Internally, the safelist is stored in a hash map for O(1) lookup, so there is zero performance cost regardless of how many labels you protect.

Beyond the safelist, the processor only strips labels whose unique-value growth rate exceeds the `max_cardinality_delta_per_epoch` threshold. Labels that have reached a stable, high-cardinality state (many unique values that stopped growing) are not penalized. This is because enforcement is based on the *delta* — the difference between the current epoch's HyperLogLog++ sketch estimate and the previous epoch's — not on the absolute cardinality. A label with 100,000 stable values will never be touched; a label that added 501 new values in the last five minutes will.

**Does the limit apply to the whole metric, or individual labels?**
The processor is an **Attribute-level Cardinality Limiter**, not an overarching series limiter. It creates a totally separate HyperLogLog sketch for every single `(metric_name, label_key)` pair it sees. 

For example, if you have a metric `http_requests` with labels `api` (20 values), `region` (10 values), and `error.type` (5 values like `"timeout"`, `"not_found"`, etc.):
* The tracker for `(http_requests, api)` sees **20** unique values.
* The tracker for `(http_requests, region)` sees **10** unique values.
* The tracker for `(http_requests, error.type)` sees **5** unique values.

None of the individual dimensions exceed a hypothetical `500` limit, so everything passes cleanly.

Now imagine a rogue deployment introduces a bug that accidentally logs raw database exceptions into the `error.type` attribute instead of the standard error code:

* **Expected:** `error.type = "db_timeout"`
* **The Bug:** `error.type = "Lock wait timeout exceeded; txn_id=8f7d6a5b..."`

Because every transaction ID is unique, `error.type` suddenly spikes to **50,000** unique values. Only the `(http_requests, error.type)` tracker breaches the limit. The processor surgically strips just the `error.type` label, flattening the cardinality explosion instantly, while leaving your `api` and `region` dimensions perfectly intact so your core traffic dashboards don't break.

If you are still uncomfortable, use `tag_only: true` mode first (see Question 4). In that mode nothing is ever deleted — the processor only tags data points, giving you full visibility before you flip to hard enforcement.

---

### 2. Will this add meaningful latency to my pipeline?

The processor's hot path — the code executed once per metric data point — was designed for zero heap allocations in steady state. Two specific choices make this possible:

- **`xxhash.Sum64String` instead of `Insert([]byte)`.** The underlying HyperLogLog library's `Insert` method accepts a byte slice, which forces the Go compiler to escape the argument to the heap. Instead, the processor hashes each attribute value with `xxhash.Sum64String` (which operates entirely on the stack and returns a `uint64`) and passes the result to `InsertHash(uint64)`. No allocation occurs.

- **Lazy cached estimates.** Calling `Estimate()` on the HyperLogLog sketch triggers an internal merge that allocates roughly five objects per call in sparse mode. The processor caches the last estimate and only refreshes it every 64 inserts (using a bitmask check — a single AND instruction). During the first 64 inserts of a new tracker it estimates on every insert to maintain accuracy near the limit, then shifts to the amortized schedule.

The measured result is **48 ns/op, 0 allocs/op** on a benchmark with 6 parallel goroutines on Go 1.25. At one million data points per second, that adds roughly 0.05 ms of total CPU time per second — effectively invisible next to serialization, network I/O, and exporter batching.

---

### 3. Could a cardinality explosion cause the processor itself to OOM?

Each cardinality tracker (one per unique `metric_name + label_key` pair) holds two HyperLogLog++ sketches at precision `p=14`. Each sketch uses 2^14 = 16,384 registers and occupies approximately 12 KB in dense mode. A tracker therefore costs roughly 24 KB.

Ten thousand tracked metric-label pairs — a very large deployment — would consume about 240 MB. One hundred thousand pairs would consume about 2.4 GB. These numbers are bounded by the number of distinct metric names multiplied by the number of distinct label keys, not by the number of unique label values. HyperLogLog++ sketches are fixed-size regardless of how many values are inserted.

New sketch allocations are served from a `sync.Pool`, which amortizes GC pressure during epoch rotation and during cardinality spikes when many new trackers are created at once. The pool acts as a pre-allocation cache: it vends ready-to-use sketches in O(1) time without hitting the allocator on the fast path.

Concurrency is handled by 256 independent shards, each with its own `sync.RWMutex`. Data points are routed to shards by hashing the metric name with `maphash.String` (zero allocation, fixed seed). The probability that two concurrent goroutines contend on the same lock is 1/256, which keeps throughput near-linear with core count and avoids the kind of mutex convoy that could amplify memory pressure under load.

---

### 4. Can I test this in production without risking data loss?

Yes. Set `tag_only: true` in the processor configuration. In this mode, no attribute is ever removed. Instead, data points where at least one label exceeds the cardinality limit receive a boolean attribute `otel.metric.overflow: true`. Your existing pipeline continues to export every label unchanged.

You can then add a downstream OTel routing processor that matches on `otel.metric.overflow` and forks those metrics to a secondary destination — a cheap object store like S3 or GCS, a debug exporter, or a dev TSDB — while clean metrics flow to your production TSDB as before. This makes the enforcement decision fully visible and completely reversible.

A recommended rollout sequence:

1. Deploy with `tag_only: true` and monitor the `processor_labels_stripped_total` counter for a few days. This tells you which metrics and labels would be affected.
2. Add the labels you consider essential to `never_drop_labels`.
3. When the tagged set matches your expectations, flip `tag_only: false` to begin hard enforcement.

---

### 5. Why can't I just install this as a plugin? Why do I have to compile a custom Collector?

The OpenTelemetry Collector does not support runtime plugins. Every processor, receiver, and exporter must be compiled into the binary at build time. This is a deliberate design choice by the OTel project — it ensures type safety, avoids dynamic linking issues, and produces a single static binary with no external dependencies.

The OpenTelemetry Collector Builder (OCB) is the official tool for this. You declare your components in a `builder.yaml` file, run a single command, and OCB generates the Go source, resolves dependencies, and compiles the binary. The result is a purpose-built Collector that contains exactly the components you need — no more, no less.

The project README contains a complete, tested `builder.yaml` example with the correct `name` and `import` overrides for the hyphenated module path, plus the OCB install and build commands. The entire process is two shell commands.

---

### 6. How do I know how much money this processor is actually saving me?

Cardinality Guardian emits a counter called `estimated_savings_dollars_total` under the `cardinality_guardian` instrumentation scope. Every time a label is stripped (or tagged, in tag-only mode), the processor adds the value of `estimated_cost_per_metric_month` to this counter. That configuration field represents the dollar cost of one unique time series per month in your TSDB.

To see the current savings rate in Prometheus or any PromQL-compatible backend:

```promql
rate(estimated_savings_dollars_total[5m]) * 60 * 60 * 24 * 30
```

This extrapolates the five-minute rate to a monthly dollar figure. You can alert on it, graph it on a cost dashboard, or feed it into a FinOps report.

Two companion metrics round out the picture:

- `processor_labels_stripped_total` — a counter that increments once per attribute stripped or tagged. Use `rate()` to detect enforcement spikes.
- `processor_trackers_active` — a gauge showing the current number of live `(metric, label_key)` trackers across all 256 shards. Useful for capacity planning and for spotting tracker growth that could indicate a new cardinality source.

All three metrics are standard OTel SDK instruments and are automatically collected by any Collector with self-telemetry enabled.

---

### 7. What happens when an epoch rotates? Is there a spike of dropped metrics during the transition?

No. Epoch rotation is designed to be non-disruptive. At the end of each `epoch_duration_seconds` window, a background goroutine walks all 256 shards. For each shard, it acquires the write lock only long enough to swap two pointers — the current sketch becomes the previous sketch, and a fresh sketch (drawn from the `sync.Pool`) becomes the new current. No sketch allocation, no estimation, and no enforcement decision happens while the lock is held.

Immediately after rotation, the new epoch starts with an empty current sketch. The delta — current minus previous — is initially zero, which means every label is within budget. As new unique values arrive during the epoch, the delta grows. A label that was over-limit in the previous epoch must re-exceed the limit in the new epoch before being enforced again. This provides a natural cooldown window and avoids permanent bans on labels that experienced a transient spike.

---

### 8. How accurate is the cardinality counting? Could it strip a label that is actually under the limit?

The processor uses HyperLogLog++ at precision `p=14`, which provides a standard error of approximately 0.81% in optimal ranges. However, variance is not linear across all cardinalities. 

Based on empirical benchmark testing across different scales:

**Small Scale Burst (1,000 unique IDs):**
- **At very low limits (100 - 200):** Variance can be up to **~27%** (e.g., a limit of 100 might permit ~127 values before enforcing).
- **At medium limits (400):** Variance drops to **~11%** (e.g., a limit of 400 permits ~447 values before enforcing).

**Large Scale Burst (20,000 unique IDs):**
- **At high limits (1,000 - 5,000):** Variance tightens to the theoretical **1-2%** (e.g., a limit of 5,000 permitted 5,055 values).
- **At massive limits (10,000+):** Variance approaches **~0%** (e.g., a limit of 10,000 permitted 10,047 values, an error rate of < 0.5%).

> [!NOTE] 
> **Does the total volume of incoming data affect variance?** No. HyperLogLog standard error is a mathematical property of the *enforcement limit*, not the total data volume. Whether your cluster is hit with a spike of 20,000 unique values or a massive attack of 20,000,000 unique values, if your limit is set to 500, the algorithm's variance and the exact point it triggers enforcement will remain computationally identical.

This tradeoff is deliberate. Exact counting would require memory proportional to the number of unique values — potentially gigabytes for high-cardinality labels — and would need a lock-protected hash set per tracker. HyperLogLog++ provides a fixed 12 KB footprint per sketch regardless of how many values are inserted, and the `InsertHash` operation is constant-time with no allocation.

If the sub-1% error margin is a concern for a specific label, add it to `never_drop_labels` to exempt it entirely. The error margin affects only the enforcement boundary, never the labels you have explicitly protected.

---

### 9. What if I need to change the cardinality limit or add protected labels? Do I have to restart the Collector?

Yes, configuration changes require a Collector restart. The processor reads its configuration once at startup and builds its internal data structures (the protected-labels map, the per-shard tracker maps, the epoch ticker) from that configuration.

However, a restart is fast and safe. The processor carries no persistent state — all HyperLogLog sketches are rebuilt from scratch after restart. The first epoch after restart is a clean slate: no label is dropped until it re-exceeds the configured delta threshold during the new epoch window. There is no risk of a restart causing a burst of incorrect enforcement decisions.

For zero-downtime configuration changes, deploy the updated Collector configuration as a rolling restart behind your load balancer or Kubernetes deployment. Each new instance starts with fresh sketches and converges to accurate enforcement within a single epoch.

---

### 10. How does Cardinality Guardian compare to existing limits in OTel SDKs, standard Collector processors, and TSDBs?

Cardinality management requires defense-in-depth. Here is how Cardinality Guardian fills the gap in the standard observability pipeline:

* **Layer 1: OpenTelemetry Client SDKs (The Application Guard)**
  Client-side SDKs have cardinality limits designed strictly to protect the *application* from crashing (OOM) due to memory exhaustion. When triggered, the SDK uses a blunt approach: it drops the new data points entirely or lumps them into a generic `otel.metric.overflow` bucket.
* **Layer 2: Vanilla OTel Collector (The Static Guard)**
  Out-of-the-box Collector processors (like `filter` or `transform`) allow you to drop attributes, but they are *static*. You must know exactly which labels to drop ahead of time. They cannot dynamically track state or react to sudden, unexpected cardinality explosions.
* **Layer 3: TSDBs like Prometheus or Datadog (The Destructive Last Resort)**
  If high-cardinality data reaches your TSDB, the results are destructive. Prometheus enforces limits (like `series_limit`) by dropping the series or failing the entire scrape, creating massive monitoring blind spots. Commercial TSDBs like Datadog will either accept the data (resulting in a massive surprise invoice) or rate-limit the metric, destroying your dashboard visibility.
* **The Missing Piece: Cardinality Guardian (The Surgical Guard)**
  Cardinality Guardian acts as a dynamic safety net just before the TSDB. Instead of relying on static rules or dropping entire metrics, it uses HyperLogLog++ to track cardinality in real-time. When a specific label explodes, it *surgically strips only the offending label*, ensuring the core metric (like overall HTTP request rate) still reaches your TSDB safely and cheaply.
