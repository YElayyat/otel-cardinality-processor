// Package cardinalityprocessor implements an OpenTelemetry Collector metrics
// processor that enforces per-metric-per-label cardinality limits using
// HyperLogLog sketches. It is designed to be a drop-in pipeline component with
// zero web-server footprint — the binary exports no HTTP endpoints.
//
// # Problem
//
// In high-throughput observability pipelines, individual label keys (e.g.
// "session_id", "request_id", "user_id") can take on millions of unique values,
// causing cardinality explosions in downstream time-series databases. Each
// unique label combination creates a new time series; at cloud TSDB pricing,
// a single misbehaving label can cost thousands of dollars per month.
//
// # How it works
//
// For every (metric_name, label_key) pair the processor maintains two
// HyperLogLog++ sketches — one for the current epoch and one for the
// previous epoch. On each data point the unique count is estimated as the
// delta between those two sketches. When the delta exceeds the configured
// MaxCardinalityDeltaPerEpoch the processor either removes the offending
// attribute (TagOnly: false, the default) or injects a routing tag so that
// downstream components can shunt the metric to cheap cold storage
// (TagOnly: true).
//
// # Architecture
//
//      ConsumeMetrics (hot path, called concurrently by the Collector)
//        └─ handleAttributes           (per data point)
//              └─ shouldDrop           (per label key/value pair)
//                    ├─ xxhash.Sum64String  — zero-alloc uint64 hash of value
//                    ├─ getShard            — maphash routing to 1/256 of key space
//                    ├─ shard.mu RLock      — fast path when tracker already exists
//                    └─ tracker.insert     — HLL InsertHash + lazy cached estimate
//
//      Background goroutine (one per processor lifetime, started in Start)
//        └─ rotate — every EpochDurationSeconds, advances the sliding window
//                     across all 256 shards, one shard at a time
//
// # Performance design decisions
//
//   - 256-shard RWMutex array: A single global lock would serialize all
//     goroutines in the Collector's metric pipeline. With 256 independently
//     locked shards, contention is reduced to roughly 1/256th under a uniform
//     metric-name distribution, enabling near-linear scaling with CPU count.
//
//   - xxhash.Sum64String instead of Insert([]byte): The axiomhq/hyperloglog
//     library's Insert method internally calls a package-level `hash` function
//     variable. Because the compiler cannot see through function variables, any
//     []byte argument would escape to the heap, adding an allocation per call.
//     xxhash.Sum64String accepts a string directly, computes the 64-bit hash
//     entirely on the stack, and passes the result to InsertHash(uint64), which
//     is a pure value-type operation with zero heap pressure.
//
//   - sync.Pool for HyperLogLog sketches: Allocating a fresh hyperloglog.Sketch
//     for every new (metric, label) pair or every epoch rotation would spike GC
//     pressure during cardinality explosions — precisely when the processor is
//     busiest. The pool pre-allocates sketches and hands them out at O(1) cost.
//     Note: dirty sketches are NOT returned to the pool (see sketchPool for the
//     full explanation), so the pool acts as a pre-allocation cache rather than
//     a traditional free list.
//
//   - Lazy cached estimates: Calling hyperloglog.Sketch.Estimate() triggers an
//     internal mergeSparse() that allocates ~5 heap objects per call when the
//     sketch is in sparse mode. The processor caches the last estimate and only
//     refreshes it every estimateInterval inserts (using a power-of-2 bitmask
//     check, which is a single AND instruction). See tracker.insert for the
//     full two-phase strategy.
package cardinalityprocessor

import (
        "context"
        "hash/maphash"
        "math"
        "sync"
        "sync/atomic"
        "time"

        "github.com/axiomhq/hyperloglog"
        "github.com/cespare/xxhash/v2"
        "go.opentelemetry.io/collector/component"
        "go.opentelemetry.io/collector/consumer"
        "go.opentelemetry.io/collector/pdata/pcommon"
        "go.opentelemetry.io/collector/pdata/pmetric"
        "go.opentelemetry.io/collector/processor"
        "go.opentelemetry.io/otel/metric"
        "go.uber.org/zap"
)

// staleSweepEpochs is the number of consecutive epochs a tracker must receive
// zero inserts before it is eligible for eviction from the shard map. Two
// epochs ensures that a tracker that received traffic at the tail end of
// a previous epoch (and was rotated into "previous") is not prematurely
// reaped before traffic resumes in the next epoch.
const staleSweepEpochs = 2

// numShards is the number of independent shard buckets used to partition the
// trackers map. 256 was chosen because:
//   - It is a power of two, so the modulo operation in getShard reduces to a
//     single bitmask AND with no division.
//   - It fits in a uint8, making the shard index derivation branchless.
//   - Empirically, 256 shards eliminate visible mutex contention up to ~512
//     concurrent goroutines under a uniform metric-name distribution.
const numShards = 256

// sketchPool is a package-level pool of fresh HyperLogLog++ sketches. It
// amortizes GC pressure when many trackers are initialized or rotated
// simultaneously — the most expensive moments are cardinality explosions and
// epoch boundaries, which are exactly when a naive "allocate on demand" approach
// would create the most GC pauses.
//
// Why New14()? The "14" refers to the precision parameter p=14, which uses
// 2^14 = 16 384 registers and yields a standard error of ~0.81%. This is
// accurate enough for cardinality enforcement while keeping each sketch at a
// fixed ~12 KB in dense mode.
//
// Why are dirty sketches NOT returned to the pool?
// hyperloglog.Sketch has no Reset() method. UnmarshalBinary (the closest
// alternative) allocates a new internal struct when the sketch is non-empty
// because it detects a non-empty tmpSet or sparseList. Returning a used sketch
// to the pool and then calling Get() on it would hand out a pre-contaminated
// sketch, corrupting the delta calculation for the new epoch. The pool therefore
// acts as a pre-allocation cache: New() vends fresh New14() instances and
// Get() returns them; used sketches are simply abandoned to the GC.
// If the upstream library ever adds Reset(), add the following inside rotate():
//
//      old.Reset(); sketchPool.Put(old)
var sketchPool = sync.Pool{
        New: func() any { return hyperloglog.New14() },
}

// mustGetSketch retrieves a fresh HyperLogLog++ sketch from sketchPool.
// The pool's New function always returns *hyperloglog.Sketch, so the
// type assertion is guaranteed to succeed. A panic here would indicate
// a programming error in the pool configuration — it cannot occur at runtime.
func mustGetSketch() *hyperloglog.Sketch {
        s, ok := sketchPool.Get().(*hyperloglog.Sketch)
        if !ok {
                panic("sketchPool: New returned a non-*hyperloglog.Sketch value")
        }
        return s
}

// trackerKey is a zero-allocation composite key for the trackers map.
// Using a struct key avoids the heap allocation that string concatenation
// (e.g. metricName + ":" + attrKey) would cause in the hot path: Go can hash
// a struct key inline without allocating a temporary string.
type trackerKey struct {
        metricName string
        attrKey    string
}

// estimateInterval controls how often the HLL Estimate() is recomputed in the
// hot path. Estimate() in the axiomhq/hyperloglog library always calls
// mergeSparse() when the sketch is in sparse mode, which allocates ~5 heap
// objects per call (a []uint32 sort buffer, a ForEach closure, a new
// *compressedList, its backing []byte, and a fresh intmap.Set for the reset
// tmpSet). By refreshing the estimate once every 64 inserts — checked via a
// power-of-2 bitmask, so no division — we amortize those allocations to
// 5/64 ≈ 0.08 allocs/op, which rounds to 0 in Go's benchmark output.
//
// HLL is probabilistic by design, so a 64-insert lag on the cardinality
// estimate has no practical impact on correctness: cardinality explosions
// unfold over thousands of data points, and the two-phase estimation strategy
// (see tracker.insert) ensures the estimate is accurate while the sketch is
// growing through the configured limit.
const estimateInterval = 64

// tracker holds two HyperLogLog++ sketches for a single (metric_name,
// label_key) pair and provides the fine-grained per-tracker mutex that allows
// the broader shard lock to be released before HLL operations begin.
//
// Field layout is chosen to keep the hot fields together at the front of the
// struct to improve cache-line locality during concurrent insert operations.
type tracker struct {
        // mu protects all fields below. It is held only for the duration of
        // insert() or rotate(), not for the longer shard-level operations.
        mu sync.Mutex

        // current is the HLL sketch for the in-progress epoch. New label values
        // are inserted here on every data point.
        current *hyperloglog.Sketch

        // previous is the HLL sketch snapshot from the last completed epoch.
        // It provides the cardinality baseline against which the delta is measured.
        // It is replaced (not reset) on every call to rotate().
        previous *hyperloglog.Sketch

        // cachedCurr is the most recently computed estimate of current.Estimate().
        // It is refreshed lazily — see the two-phase strategy in insert().
        cachedCurr uint64

        // cachedPrev is the cached estimate of previous.Estimate() at the moment
        // of the last rotate() call. It is stable for the entire epoch and is
        // only updated in rotate(), so it is never recomputed in insert().
        cachedPrev uint64

        // insertCount tracks how many times insert() has been called since the last
        // rotate(). It drives the Phase 1 / Phase 2 estimation switch.
        insertCount uint64

        // idleEpochs counts the number of consecutive epoch rotations during which
        // this tracker received zero inserts. When idleEpochs reaches
        // staleSweepEpochs the tracker is eligible for eviction from the shard map.
        idleEpochs int
}

// insert records a pre-computed xxhash value directly via InsertHash, which
// accepts a uint64 and avoids the []byte path entirely. Sketch.Insert([]byte)
// internally calls `hash(e)` where hash is a package-level function variable —
// the compiler cannot see through function variables, so any []byte argument
// would escape to the heap. By calling InsertHash(uint64) we pass a value
// type, eliminating all slice allocations from the hot path.
//
// The estimate is refreshed using a two-phase strategy:
//
//      Phase 1 — insertCount ≤ estimateInterval: estimate on every insert.
//      This ensures that cardinality is measured accurately while the sketch is
//      growing toward (and through) the configured limit. Without this phase,
//      a flat lazy interval would allow the limit to be overshot by up to
//      estimateInterval elements before the drop logic activates.
//
//      Phase 2 — insertCount > estimateInterval: estimate every estimateInterval
//      inserts (bitmask check). In the high-throughput steady state the sketch
//      cardinality has already stabilized; recomputing less frequently amortizes
//      the ~5 allocs/call from mergeSparse() to ≈ 5/64 = 0.08 allocs/op, which
//      rounds to 0 in Go's benchmark output.
//
// previous.Estimate() is stable between epoch rotations and is only updated
// in rotate(), so cachedPrev is never recomputed here.
func (t *tracker) insert(hashVal uint64) (curr, prev uint64) {
        t.mu.Lock()
        defer t.mu.Unlock()
        t.current.InsertHash(hashVal)
        t.insertCount++
        if t.insertCount <= estimateInterval || t.insertCount&(estimateInterval-1) == 0 {
                t.cachedCurr = t.current.Estimate()
        }
        return t.cachedCurr, t.cachedPrev
}

// rotate promotes current to previous and installs a pre-allocated fresh
// sketch as current. The evicted previous sketch is released to the GC
// (see the sketchPool recycling note above for why it is not Put back).
//
// The current cached estimate is carried forward as the new cachedPrev so
// that the cardinality baseline is immediately available in the new epoch
// without requiring an additional Estimate() call.
//
// Returns true if this tracker received zero inserts during the epoch that
// just ended, allowing the caller to track consecutive idle epochs for the
// stale-tracker eviction sweep.
func (t *tracker) rotate(fresh *hyperloglog.Sketch) (idle bool) {
        t.mu.Lock()
        idle = t.insertCount == 0
        if idle {
                t.idleEpochs++
        } else {
                t.idleEpochs = 0
        }
        t.cachedPrev = t.cachedCurr
        t.cachedCurr = 0
        t.insertCount = 0
        t.previous = t.current
        t.current = fresh
        t.mu.Unlock()
        return idle
}

// newTracker initializes a tracker, obtaining both sketches from sketchPool
// to avoid a cold allocation on the first insert into a new (metric, label)
// pair.
func newTracker() *tracker {
        return &tracker{
                current:  mustGetSketch(),
                previous: mustGetSketch(),
        }
}

// trackerShard is an independently-locked partition of the global trackers
// map. The 256-shard design means that two goroutines processing metrics with
// different names will, with high probability, land in different shards and
// acquire different mutexes — they never contend with each other at all.
type trackerShard struct {
        // mu guards the trackers map. It is a RWMutex because the common case
        // (tracker already exists) only needs a read lock; the write lock is
        // acquired only when a new (metric, label) pair is seen for the first time.
        mu sync.RWMutex

        // trackers maps a (metricName, attrKey) composite key to the live tracker
        // for that combination. Trackers are never removed; the map grows
        // monotonically with the number of unique (metric, label) pairs observed.
        trackers map[trackerKey]*tracker
}

// cardinalityProcessor is the internal implementation of the
// processor.Metrics interface. It is returned as that interface from
// newCardinalityProcessor so that test code can exercise all interface methods
// without importing internal types.
type cardinalityProcessor struct {
        // config is the validated, merged user configuration. It is read-only after
        // construction and therefore does not require synchronization.
        config *Config

        // logger is derived from set.TelemetrySettings.Logger in the constructor.
        // Sourcing the logger from the OTel Collector settings — rather than
        // accepting a separate *zap.Logger parameter — ensures that every log record
        // carries the correct component type and ID attributes automatically.
        logger *zap.Logger

        // next is the downstream consumer. ConsumeMetrics forwards the (mutated)
        // metric batch to next after all attribute enforcement has been applied.
        next consumer.Metrics

        // protectedLabels is a pre-built O(1) lookup set of label keys that must
        // never be dropped or tagged, regardless of cardinality. It is populated
        // from Config.NeverDropLabels at construction time and never modified.
        protectedLabels map[string]struct{}

        // seed is the maphash.Seed used by getShard. It is randomized once at
        // construction time so that shard distribution is unpredictable to external
        // actors (preventing deliberate hash-collision attacks) while remaining
        // stable for the lifetime of the processor.
        seed maphash.Seed

        // shards is the 256-element array of independently-locked tracker
        // partitions. Using a fixed-size array (not a slice) avoids an extra pointer
        // dereference on every hot-path lookup.
        shards [numShards]*trackerShard

        // ctx and cancel manage the lifetime of the background rotation goroutine.
        // ctx is a child of the context passed to newCardinalityProcessor, and
        // cancel is called in Shutdown() after the gauge registration is released.
        ctx    context.Context
        cancel context.CancelFunc

        // startOnce ensures the background ticker goroutine is launched exactly once
        // even if Start() is called multiple times (the Collector framework
        // guarantees single-call semantics, but the guard is cheap insurance).
        startOnce sync.Once

        // tagOnly mirrors Config.TagOnly and is captured directly on the struct to
        // avoid an extra pointer dereference into the Config on every data point.
        tagOnly bool

        // warnedMetrics tracks metric names for which an informational log
        // message about unsupported types has already been emitted. This prevents
        // per-data-point warn log storms for metric types that the processor
        // does not enforce (currently none, but retained for forward-compatibility).
        warnedMetrics sync.Map

        // estimatedCostPerMetricMonth mirrors Config.EstimatedCostPerMetricMonth for
        // the same reason: it is read on every attribute that exceeds the limit and
        // benefits from being on the processor struct rather than behind a pointer.
        estimatedCostPerMetricMonth float64

        // labelsStripped is an atomic counter tracking how many attributes were
        // stripped or tagged. By tracking this purely as an integer, we prevent
        // floating-point drift. This feeds both the labels stripped counter AND
        // the estimated savings computation at scrape time.
        labelsStripped atomic.Int64

        // gaugeRegistration holds the handle returned by meter.RegisterCallback so
        // the callback can be cleanly unregistered in Shutdown(). This is critical
        // for GC correctness: the OTel SDK holds a strong reference to every
        // registered callback closure. Without Unregister(), the closure (which
        // captures p via the `p.shards` reference) would keep the processor alive
        // until the SDK's MeterProvider is shut down — potentially leaking the
        // entire 256-shard tracker map across pipeline reconfigurations.
        gaugeRegistration metric.Registration
}

// newCardinalityProcessor constructs a cardinalityProcessor, registers its
// internal OTel telemetry instruments, and returns it as a processor.Metrics
// interface. It derives the logger from set.TelemetrySettings so that all OTel
// Collector log records carry the correct component attributes — passing a
// separate *zap.Logger alongside processor.Settings would create two sources of
// truth and risk log records reaching the wrong sink.
func newCardinalityProcessor(ctx context.Context, cfg *Config, set processor.Settings, next consumer.Metrics) (processor.Metrics, error) {
        protected := make(map[string]struct{}, len(cfg.NeverDropLabels))
        for _, l := range cfg.NeverDropLabels {
                protected[l] = struct{}{}
        }

        childCtx, cancel := context.WithCancel(ctx)

        p := &cardinalityProcessor{
                config:                      cfg,
                logger:                      set.TelemetrySettings.Logger,
                next:                        next,
                protectedLabels:             protected,
                seed:                        maphash.MakeSeed(),
                ctx:                         childCtx,
                cancel:                      cancel,
                tagOnly:                     cfg.TagOnly,
                estimatedCostPerMetricMonth: cfg.EstimatedCostPerMetricMonth,
        }

        for i := range p.shards {
                p.shards[i] = &trackerShard{
                        trackers: make(map[trackerKey]*tracker),
                }
        }

        // Instrumentation scope matches the component type so dashboards and
        // alert rules can filter by a consistent, human-readable name.
        meter := set.TelemetrySettings.MeterProvider.Meter(typeStr)
        var err error

        // Convert our previously synchronous counters into Observable metrics so
        // we can dynamically compute the savings based on exact integer multiples,
        // rather than blindly accruing IEEE-754 floating point dust.
        counterStripped, err := meter.Int64ObservableCounter(
                "processor_labels_stripped_total",
                metric.WithDescription("Total number of high-cardinality labels stripped or tagged."),
        )
        if err != nil {
                return nil, err
        }

        counterSavings, err := meter.Float64ObservableCounter(
                "estimated_savings_dollars_total",
                metric.WithDescription("Cumulative estimated dollar value of time-series churn prevented by the processor."),
        )
        if err != nil {
                return nil, err
        }

        gaugeActive, err := meter.Int64ObservableGauge(
                "processor_trackers_active",
                metric.WithDescription("Current number of active cardinality trackers across all shards."),
        )
        if err != nil {
                return nil, err
        }

        p.gaugeRegistration, err = meter.RegisterCallback(
                func(_ context.Context, o metric.Observer) error {
                        // 1. Report active trackers
                        var totalActive int64
                        for i := range p.shards {
                                p.shards[i].mu.RLock()
                                totalActive += int64(len(p.shards[i].trackers))
                                p.shards[i].mu.RUnlock()
                        }
                        o.ObserveInt64(gaugeActive, totalActive)

                        // 2. Report drops and apply 2-decimal rounded precision to the dollar calculation
                        drops := p.labelsStripped.Load()
                        o.ObserveInt64(counterStripped, drops)

                        savings := float64(drops) * p.estimatedCostPerMetricMonth
                        roundedSavings := math.Round(savings*100) / 100
                        o.ObserveFloat64(counterSavings, roundedSavings)

                        return nil
                },
                gaugeActive, counterStripped, counterSavings,
        )
        if err != nil {
                return nil, err
        }

        return p, nil
}

// getShard routes a metric name to one of the numShards independent shard
// buckets. maphash.String is zero-allocation: it operates directly on the
// string header without converting to []byte, and the seed is a value type
// held on the processor struct (no pointer indirection). The seed is fixed at
// construction time so routing is deterministic within a process lifetime.
func (p *cardinalityProcessor) getShard(metricName string) *trackerShard {
        return p.shards[maphash.String(p.seed, metricName)&(numShards-1)]
}

// Capabilities tells the Collector that this processor mutates the data it
// receives. Setting MutatesData: true causes the Collector to clone the metric
// batch before passing it to this processor, ensuring that modifications to
// attributes do not race with other pipeline components that may be consuming
// the same in-memory batch concurrently.
func (p *cardinalityProcessor) Capabilities() consumer.Capabilities {
        return consumer.Capabilities{MutatesData: true}
}

// ConsumeMetrics is the entry point for incoming metric batches. It walks the
// three-level OTel hierarchy (ResourceMetrics → ScopeMetrics → Metric) and
// delegates per-metric enforcement to processMetric. After all enforcement is
// complete the (potentially mutated) batch is forwarded to the next consumer.
func (p *cardinalityProcessor) ConsumeMetrics(ctx context.Context, md pmetric.Metrics) error {
        rm := md.ResourceMetrics()
        for i := 0; i < rm.Len(); i++ {
                resMetrics := rm.At(i)
                sm := resMetrics.ScopeMetrics()
                for j := 0; j < sm.Len(); j++ {
                        scopeMetrics := sm.At(j)
                        ms := scopeMetrics.Metrics()
                        for k := 0; k < ms.Len(); k++ {
                                // Process each metric
                                p.processMetric(ms.At(k))
                        }
                }
        }
        return p.next.ConsumeMetrics(ctx, md)
}

// processMetric dispatches a single metric to the appropriate data-point
// handler based on its type. Gauge and Sum metrics contain NumberDataPoints;
// Histogram metrics contain HistogramDataPoints. ExponentialHistogram and
// Summary metrics are passed through unchanged — cardinality checking is not
// applied to them — and a warning is emitted so that operators are aware that
// enforcement coverage is partial for those types.
func (p *cardinalityProcessor) processMetric(m pmetric.Metric) {

        switch m.Type() {
        case pmetric.MetricTypeGauge:
                p.processNumberDataPoints(m.Name(), m.Gauge().DataPoints())
        case pmetric.MetricTypeSum:
                p.processNumberDataPoints(m.Name(), m.Sum().DataPoints())
        case pmetric.MetricTypeHistogram:
                p.processHistogramDataPoints(m.Name(), m.Histogram().DataPoints())
        case pmetric.MetricTypeExponentialHistogram:
                p.processExponentialHistogramDataPoints(m.Name(), m.ExponentialHistogram().DataPoints())
        case pmetric.MetricTypeSummary:
                p.processSummaryDataPoints(m.Name(), m.Summary().DataPoints())
        }
}

// processNumberDataPoints iterates over a NumberDataPointSlice and calls
// handleAttributes for each data point. Both Gauge and Sum metric types use
// this slice type, so a single method covers both.
func (p *cardinalityProcessor) processNumberDataPoints(metricName string, dps pmetric.NumberDataPointSlice) {
        for i := 0; i < dps.Len(); i++ {
                p.handleAttributes(metricName, dps.At(i).Attributes())
        }
}

// processHistogramDataPoints iterates over a HistogramDataPointSlice and calls
// handleAttributes for each data point.
func (p *cardinalityProcessor) processHistogramDataPoints(metricName string, dps pmetric.HistogramDataPointSlice) {
        for i := 0; i < dps.Len(); i++ {
                p.handleAttributes(metricName, dps.At(i).Attributes())
        }
}

// processExponentialHistogramDataPoints iterates over an ExponentialHistogramDataPointSlice
// and calls handleAttributes for each data point.
func (p *cardinalityProcessor) processExponentialHistogramDataPoints(metricName string, dps pmetric.ExponentialHistogramDataPointSlice) {
        for i := 0; i < dps.Len(); i++ {
                p.handleAttributes(metricName, dps.At(i).Attributes())
        }
}

// processSummaryDataPoints iterates over a SummaryDataPointSlice and calls
// handleAttributes for each data point.
func (p *cardinalityProcessor) processSummaryDataPoints(metricName string, dps pmetric.SummaryDataPointSlice) {
        for i := 0; i < dps.Len(); i++ {
                p.handleAttributes(metricName, dps.At(i).Attributes())
        }
}

// handleAttributes is the innermost enforcement loop. It iterates over every
// attribute on a single data point and applies the cardinality decision from
// shouldDrop to each one.
//
// TagOnly dual-route logic:
//
// When Config.TagOnly is true the processor must not delete any attribute — it
// must instead inject a "cardinality_limit_exceeded=true" boolean attribute.
// The injection cannot happen inside the RemoveIf callback because pdata's
// KeyValueList backing slice may be reallocated by a PutBool call while
// RemoveIf still holds its internal cursor, producing undefined behavior.
// The solution is a local `shouldTag` bool that the closure sets to true when
// it decides to tag (and returns false to prevent deletion). After RemoveIf
// returns the tag is applied in a single, safe PutBool call.
//
// Allocation note: The closure passed to RemoveIf already escapes to the heap
// (RemoveIf is an interface-level call). Capturing `shouldTag` by reference
// widens the existing closure struct by one bool with zero additional heap
// allocations.
func (p *cardinalityProcessor) handleAttributes(metricName string, attrs pcommon.Map) {
        // shouldTag is set inside the RemoveIf callback when tagOnly mode decides
        // that an attribute must be tagged rather than removed. The actual PutBool
        // call is deferred until after RemoveIf completes — mutating the map
        // while RemoveIf is iterating it is undefined behavior in pdata (the
        // backing KeyValueList slice could be modified mid-scan).
        // This local bool is captured by the closure below; the closure is already
        // heap-allocated (it escapes into RemoveIf), so capturing one extra bool
        // increases the existing closure struct size with zero additional allocations.
        shouldTag := false
        attrs.RemoveIf(func(k string, v pcommon.Value) bool {
                // Guard: check the protected set before calling v.AsString().
                if p.isProtected(k) {
                        return false
                }

                if p.shouldDrop(metricName, k, v.AsString()) {
                        p.labelsStripped.Add(1)

                        if p.tagOnly {
                                // DUAL-ROUTE MODE: record the decision, keep the attribute.
                                shouldTag = true
                                return false
                        }

                        // NORMAL MODE: log and signal RemoveIf to delete this attribute.
                        p.logger.Warn("Dropping high-cardinality attribute",
                                zap.String("metric", metricName),
                                zap.String("key", k))
                        return true
                }
                return false
        })

        // Apply the routing tag only after iteration is complete so that
        // pcommon.Map is never modified while RemoveIf holds its internal cursor.
        if shouldTag {
                attrs.PutBool("cardinality_limit_exceeded", true)
        }
}

// isProtected reports whether key is in the NeverDropLabels set. The lookup
// is O(1) via the pre-built map on the processor struct.
func (p *cardinalityProcessor) isProtected(key string) bool {
        _, ok := p.protectedLabels[key]
        return ok
}

// rotate advances the sliding cardinality window by one epoch across all 256
// shards. It is called from the background ticker goroutine in Start() and
// never from the hot path, so it may take short-lived read locks without
// impacting ConsumeMetrics throughput.
//
// The rotation sequence for each shard is:
//  1. Acquire a read lock and snapshot all tracker pointers (no allocations
//     inside the lock — the slice is allocated before the lock is acquired in
//     the next iteration, but this snapshot is small).
//  2. Release the read lock.
//  3. Allocate fresh sketches from sketchPool entirely outside any lock.
//  4. Call tracker.rotate() for each tracker, which acquires only the fine-
//     grained per-tracker mutex. ConsumeMetrics goroutines holding that same
//     tracker's mutex are paused for microseconds, not the entire shard.
//
// This design means only 1/256th of all trackers are temporarily stalled at
// any given point during a rotation, and the shard-level write lock is never
// held during sketch allocation.
func (p *cardinalityProcessor) rotate() {
        p.logger.Debug("Rotating cardinality sketches")

        for _, shard := range p.shards {
                // Snapshot tracker references under a shard read lock — no sketch
                // allocations inside the lock.
                shard.mu.RLock()
                type trackerEntry struct {
                        key trackerKey
                        t   *tracker
                }
                entries := make([]trackerEntry, 0, len(shard.trackers))
                for k, t := range shard.trackers {
                        entries = append(entries, trackerEntry{key: k, t: t})
                }
                shard.mu.RUnlock()

                // Pull fresh sketches from the pool entirely outside any lock.
                fresh := make([]*hyperloglog.Sketch, len(entries))
                for i := range entries {
                        fresh[i] = mustGetSketch()
                }

                // Rotate each tracker under its own fine-grained per-tracker lock,
                // not the shard lock, so ConsumeMetrics is never blocked here.
                // Collect keys of trackers that have been idle for staleSweepEpochs
                // consecutive rotations.
                var staleKeys []trackerKey
                for i, e := range entries {
                        idle := e.t.rotate(fresh[i])
                        if idle && e.t.idleEpochs >= staleSweepEpochs {
                                staleKeys = append(staleKeys, e.key)
                        }
                }

                // Evict stale trackers under a write lock. This is rare — only
                // trackers that received zero inserts for staleSweepEpochs
                // consecutive epochs are removed.
                if len(staleKeys) > 0 {
                        shard.mu.Lock()
                        for _, k := range staleKeys {
                                delete(shard.trackers, k)
                        }
                        shard.mu.Unlock()
                        p.logger.Debug("Evicted stale trackers",
                                zap.Int("count", len(staleKeys)))
                }
        }
}

// Start is called by the OTel Collector host when the pipeline is starting.
// It launches the background epoch-rotation goroutine exactly once (guarded by
// startOnce) and returns immediately. The goroutine exits when p.ctx is
// canceled, which happens in Shutdown().
func (p *cardinalityProcessor) Start(_ context.Context, _ component.Host) error {
        p.startOnce.Do(func() {
                p.logger.Info("Starting cardinality rotation ticker",
                        zap.Int("interval_seconds", p.config.EpochDurationSeconds))

                go func() {
                        ticker := time.NewTicker(time.Duration(p.config.EpochDurationSeconds) * time.Second)
                        defer ticker.Stop()

                        for {
                                select {
                                case <-ticker.C:
                                        p.rotate()
                                case <-p.ctx.Done():
                                        return
                                }
                        }
                }()
        })

        return nil
}

// Shutdown is called by the OTel Collector host when the pipeline is stopping.
// It must complete two cleanup steps in order:
//
//  1. Unregister the observable gauge callback. The OTel SDK holds a strong
//     reference to every registered callback closure; without this call the
//     closure — which captures p via the p.shards slice — would keep the entire
//     processor alive until the SDK's MeterProvider is shut down, leaking all
//     256 shard maps and their tracker contents across pipeline reconfigurations.
//     A nil guard is included for safety (e.g. if construction failed after the
//     counter registrations but before RegisterCallback).
//
//  2. Cancel the child context. This signals the background ticker goroutine to
//     exit on its next select iteration, stopping epoch rotations cleanly.
func (p *cardinalityProcessor) Shutdown(_ context.Context) error {
        p.logger.Info("Shutting down cardinality processor")
        if p.gaugeRegistration != nil {
                if err := p.gaugeRegistration.Unregister(); err != nil {
                        p.logger.Error("failed to unregister gauge callback", zap.Error(err))
                }
        }
        p.cancel()
        return nil
}

// shouldDrop is the core cardinality decision function. It returns true when
// the unique-value count for attrKey within metricName has grown by more than
// MaxCardinalityDeltaPerEpoch since the last epoch rotation.
//
// Call sequence:
//  1. Hash attrVal to a uint64 with xxhash.Sum64String (zero allocation).
//  2. Route to the correct shard with getShard (maphash, zero allocation).
//  3. Attempt a read lock to find an existing tracker (fast path).
//  4. On miss: upgrade to a write lock and double-check before inserting a new
//     tracker (classic double-checked locking pattern — safe in Go because the
//     sync package provides the necessary memory barriers).
//  5. Insert the hash into the tracker's current HLL sketch and retrieve the
//     cached (curr, prev) estimates.
//  6. Guard against uint64 underflow: if curr ≤ prev (possible due to HLL
//     probabilistic variance near sketch boundaries), return false.
//  7. Return true only if (curr − prev) > MaxCardinalityDeltaPerEpoch.
func (p *cardinalityProcessor) shouldDrop(metricName string, attrKey string, attrVal string) bool {
        key := trackerKey{metricName: metricName, attrKey: attrKey}

        // Hash the attribute value to a uint64 before acquiring any lock.
        // We pass this directly to InsertHash, bypassing the []byte path and
        // the library's internal hash variable — see tracker.insert for details.
        hashVal := xxhash.Sum64String(attrVal)

        // Route to 1/256th of the total key space.
        shard := p.getShard(metricName)

        // Phase 1: read lock — fast path for already-tracked metric+label pairs.
        shard.mu.RLock()
        t, ok := shard.trackers[key]
        shard.mu.RUnlock()

        if !ok {
                // Phase 2: write lock — only for first-time insertion into the shard.
                // Double-check after acquiring the write lock: another goroutine may
                // have inserted the same key while we waited.
                shard.mu.Lock()
                t, ok = shard.trackers[key]
                if !ok {
                        t = newTracker()
                        shard.trackers[key] = t
                }
                shard.mu.Unlock()
        }

        // HLL insert and estimate happen under the per-tracker lock, completely
        // independent of the shard lock.
        currCount, prevCount := t.insert(hashVal)

        // Guard against uint64 underflow caused by HLL probabilistic estimation
        // variance — if the current count has not grown, nothing should be dropped.
        if currCount <= prevCount {
                return false
        }

        return (currCount - prevCount) > uint64(p.config.MaxCardinalityDeltaPerEpoch)
}
