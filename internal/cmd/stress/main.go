package main

import (
	"bytes"
	"log"
	"math/rand/v2"
	"net/http"
	_ "net/http/pprof" //nolint:gosec // G108: pprof endpoint is intentional in this dev-only stress tool.
	"os"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

func main() {
	// 1. Boot up the pprof server on port 6060 in the background.
	go func() {
		log.Println("Starting pprof server on http://localhost:6060")
		srv := &http.Server{
			Addr:         "localhost:6060",
			ReadTimeout:  5 * time.Second,
			WriteTimeout: 10 * time.Second,
		}
		log.Println(srv.ListenAndServe())
	}()

	// 2. Setup OTLP HTTP Client.
	client := &http.Client{Timeout: 5 * time.Second}
	log.Println("Starting OTLP firehose to http://localhost:4318/v1/metrics...")

	// 3. Failsafe: Auto-kill.
	go func() {
		time.Sleep(1 * time.Hour)
		log.Println("1-Hour Stress Test Complete!")
		os.Exit(0)
	}()

	// 4. The Firehose: Generate metrics continuously.
	ticker := time.NewTicker(10 * time.Millisecond) // Slightly slower for network stability
	defer ticker.Stop()

	marshaler := &pmetric.ProtoMarshaler{}

	for range ticker.C {
		md := generateMockMetrics()
		buf, err := marshaler.MarshalMetrics(md)
		if err != nil {
			log.Printf("Marshal error: %v", err)
			continue
		}
		req, err := http.NewRequest("POST", "http://localhost:4318/v1/metrics", bytes.NewReader(buf))
		if err != nil {
			log.Printf("NewRequest error: %v", err)
			continue
		}
		req.Header.Set("Content-Type", "application/x-protobuf")

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("Export error: %v", err)
			continue
		}
		resp.Body.Close()
	}
}

// generateMockMetrics creates a fake OTel payload with a mix of static and
// high-cardinality labels.
func generateMockMetrics() pmetric.Metrics {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	sm := rm.ScopeMetrics().AppendEmpty()
	m := sm.Metrics().AppendEmpty()

	m.SetName("api.request.duration")
	m.SetEmptyGauge()
	dp := m.Gauge().DataPoints().AppendEmpty()

	// Add static, safe labels.
	dp.Attributes().PutStr("region", "us-east")
	dp.Attributes().PutStr("status_code", "200")

	// Inject Chaos: Add a totally unique UUID 80% of the time to trigger the cardinality killer.
	if rand.Float32() > 0.2 { //nolint:gosec // G404: math/rand/v2 is sufficient for non-security use in a stress tool.
		dp.Attributes().PutStr("session_id", uuid.New().String())
	}

	return md
}
