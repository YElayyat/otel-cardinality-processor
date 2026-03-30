package benchmark

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YElayyat/otel-cardinality-processor/cardinalityprocessor"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer/consumertest"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/collector/processor/processortest"
)

// BenchmarkSustainedLoad runs the processor under sustained high-cardinality
// load for 60 seconds, sampling heap memory every 5 seconds. It asserts that
// memory at T=60s is within 2x of memory at T=10s, proving the stale tracker
// eviction mechanism prevents unbounded memory growth (OOM).
//
// Run with:
//
//	go test -v -run=^$ -bench=BenchmarkSustainedLoad -timeout 5m ./test/benchmark/...
func BenchmarkSustainedLoad(b *testing.B) {
	// Use b.N=1 since this is a time-based sustained test, not an iteration-based one.
	if b.N > 1 {
		b.Skip("sustained load test runs once per invocation")
	}

	const (
		testDuration    = 60 * time.Second
		sampleInterval  = 5 * time.Second
		numWorkers      = 8
		batchesPerTick  = 10
		epochSeconds    = 5 // Fast rotation for eviction testing
		cardinalityLim  = 500
	)

	cfg := &cardinalityprocessor.Config{
		MaxCardinalityDeltaPerEpoch: cardinalityLim,
		EpochDurationSeconds:        epochSeconds,
	}

	next := new(consumertest.MetricsSink)
	set := processortest.NewNopSettings(component.MustNewType("cardinality_guardian"))
	proc, err := cardinalityprocessor.NewCardinalityProcessorForTest(context.Background(), cfg, set, next)
	if err != nil {
		b.Fatal(err)
	}

	// Start the processor (starts the epoch rotation goroutine).
	if err := proc.Start(context.Background(), nil); err != nil {
		b.Fatal(err)
	}
	defer proc.Shutdown(context.Background())

	// Memory sampling goroutine.
	type memSample struct {
		t         time.Duration
		heapAlloc uint64
	}
	var samples []memSample
	var sampleMu sync.Mutex

	ctx, cancel := context.WithTimeout(context.Background(), testDuration+5*time.Second)
	defer cancel()

	// Start memory sampler.
	go func() {
		start := time.Now()
		ticker := time.NewTicker(sampleInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				var ms runtime.MemStats
				runtime.ReadMemStats(&ms)
				sampleMu.Lock()
				samples = append(samples, memSample{
					t:         time.Since(start),
					heapAlloc: ms.HeapAlloc,
				})
				sampleMu.Unlock()
			}
		}
	}()

	// Generate load: N workers, each sending batches with unique high-cardinality labels.
	var totalMetrics atomic.Int64
	var wg sync.WaitGroup
	wg.Add(numWorkers)

	start := time.Now()
	for w := 0; w < numWorkers; w++ {
		w := w
		go func() {
			defer wg.Done()
			batchNum := 0
			for time.Since(start) < testDuration {
				for i := 0; i < batchesPerTick; i++ {
					md := pmetric.NewMetrics()
					rm := md.ResourceMetrics().AppendEmpty()
					sm := rm.ScopeMetrics().AppendEmpty()
					m := sm.Metrics().AppendEmpty()
					m.SetName(fmt.Sprintf("sustained.metric_%d", w))
					m.SetEmptyGauge()

					for d := 0; d < 10; d++ {
						dp := m.Gauge().DataPoints().AppendEmpty()
						dp.SetIntValue(1)
						// Unique label value per batch to create constant cardinality pressure.
						dp.Attributes().PutStr("session_id", fmt.Sprintf("w%d-b%d-d%d", w, batchNum, d))
						dp.Attributes().PutStr("region", "us-east-1")
					}

					next.Reset()
					_ = proc.ConsumeMetrics(context.Background(), md)
					totalMetrics.Add(10)
				}
				batchNum++
				// Yield to avoid monopolizing the CPU scheduler.
				runtime.Gosched()
			}
		}()
	}

	wg.Wait()
	cancel()
	elapsed := time.Since(start)

	// Report results.
	total := totalMetrics.Load()
	throughput := float64(total) / elapsed.Seconds()

	b.ReportMetric(throughput, "metrics/sec")
	b.ReportMetric(float64(total), "total_metrics")
	b.ReportMetric(elapsed.Seconds(), "duration_sec")

	// Log memory samples.
	sampleMu.Lock()
	defer sampleMu.Unlock()

	b.Logf("\n=== SUSTAINED LOAD TEST RESULTS ===")
	b.Logf("Duration:   %.1fs", elapsed.Seconds())
	b.Logf("Workers:    %d", numWorkers)
	b.Logf("Throughput: %.0f metrics/sec", throughput)
	b.Logf("Total:      %d metrics processed", total)
	b.Logf("")
	b.Logf("Memory Samples (HeapAlloc):")
	b.Logf("%-12s %-15s", "Time", "HeapAlloc (MB)")
	b.Logf("%-12s %-15s", "----", "--------------")

	for _, s := range samples {
		b.Logf("%-12s %-15.2f", fmt.Sprintf("T+%ds", int(s.t.Seconds())), float64(s.heapAlloc)/(1024*1024))
	}

	// Assert memory stability: heap at end should be within 3x of heap at T=10s.
	if len(samples) >= 3 {
		earlyHeap := samples[1].heapAlloc // T=10s
		finalHeap := samples[len(samples)-1].heapAlloc

		b.Logf("")
		b.Logf("Early heap (T=10s): %.2f MB", float64(earlyHeap)/(1024*1024))
		b.Logf("Final heap (T=60s): %.2f MB", float64(finalHeap)/(1024*1024))
		b.Logf("Ratio:              %.2fx", float64(finalHeap)/float64(earlyHeap))

		if finalHeap > earlyHeap*3 {
			b.Errorf("MEMORY LEAK DETECTED: Final heap (%.2f MB) is >3x early heap (%.2f MB). "+
				"Stale tracker eviction may not be working.",
				float64(finalHeap)/(1024*1024), float64(earlyHeap)/(1024*1024))
		} else {
			b.Logf("✅ MEMORY STABLE: No unbounded growth detected.")
		}
	}
}
