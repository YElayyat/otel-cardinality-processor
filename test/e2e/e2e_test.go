//go:build e2e

// Package e2e_test contains black-box end-to-end integration tests for the
// cardinality_guardian processor. Tests in this file are only compiled and run
// when the e2e build tag is provided (e.g. make e2e).
package e2e_test

import (
        "bufio"
        "bytes"
        "fmt"
        "io"
        "net"
        "net/http"
        "os"
        "os/exec"
        "path/filepath"
        "strings"
        "testing"
        "time"

        "github.com/stretchr/testify/assert"
        "github.com/stretchr/testify/require"
        "go.opentelemetry.io/collector/pdata/pcommon"
        "go.opentelemetry.io/collector/pdata/pmetric"
)

const (
        // otlpHTTPAddr is the OTLP HTTP receiver address used by the test to
        // send metrics. The collector config.yaml also enables gRPC on port 4317.
        otlpHTTPAddr = "http://localhost:4318/v1/metrics"

        // e2eOutputFile matches the file exporter path in config.yaml.
        e2eOutputFile = "/tmp/cardinality-e2e-output.json"

        // collectorBinName is the binary name OCB produces from dist.name in builder.yaml.
        collectorBinName = "otelcol-cardinality-guardian"
)

// TestE2E_CardinalityLimitEnforced is a black-box end-to-end test that verifies
// the cardinality_guardian processor enforces its per-label cardinality limit
// against a real, running OpenTelemetry Collector binary.
//
// Test sequence:
//
//  1. Compile a custom collector from builder.yaml using the ocb tool.
//  2. Start the compiled binary with config.yaml (limit=2, file exporter).
//  3. Send one OTLP HTTP payload carrying three gauge data points for the same
//     metric, each with a unique session_id value: s1, s2, s3.
//  4. Wait for the file exporter to flush the processed batch to disk.
//  5. Assert that all three data points are forwarded (hard-drop removes the
//     attribute, not the data point) but that only two retain session_id:
//     s1 and s2 are within the limit; s3 exceeds it and is stripped.
func TestE2E_CardinalityLimitEnforced(t *testing.T) {
        // --- Step 1: locate the ocb binary ----------------------------------------
        // "make e2e" installs ocb before running this test. If it is absent the
        // user must run "make install-ocb" (or "make e2e") first.
        ocbBin, err := exec.LookPath("ocb")
        require.NoError(t, err,
                "ocb binary not found in PATH; run 'make install-ocb' or 'make e2e' first")

        // --- Step 2: compile the custom collector ----------------------------------
        // go test sets cwd to the package source directory (test/e2e/), so relative
        // paths such as "builder.yaml" and the path: ../../ in builder.yaml resolve
        // from test/e2e/ as expected.
        buildDir := t.TempDir()
        t.Logf("Building custom collector into %s (may take several minutes on first run)", buildDir)

        buildCmd := exec.Command(ocbBin,
                "--config", "builder.yaml",
                "--output-path", buildDir,
        )
        buildOut, buildErr := buildCmd.CombinedOutput()
        require.NoError(t, buildErr, "OCB build failed:\n%s", buildOut)
        t.Logf("OCB build complete")

        collectorBin := filepath.Join(buildDir, collectorBinName)
        require.FileExists(t, collectorBin,
                "expected compiled binary at %s after OCB build", collectorBin)

        // --- Step 3: remove any output file left by a previous run ----------------
        _ = os.Remove(e2eOutputFile)

        // --- Step 4: start the collector in the background ------------------------
        // config.yaml is in the same directory as this test file (test/e2e/).
        collectorCmd := exec.Command(collectorBin, "--config", "config.yaml")
        collectorCmd.Stderr = os.Stderr

        require.NoError(t, collectorCmd.Start(),
                "failed to start collector subprocess")

        t.Cleanup(func() {
                if collectorCmd.Process != nil {
                        _ = collectorCmd.Process.Kill()
                        _ = collectorCmd.Wait()
                }
                _ = os.Remove(e2eOutputFile)
        })

        // --- Step 5: wait for the OTLP HTTP port to accept connections -------------
        t.Log("Waiting for collector OTLP HTTP port 4318...")
        require.Eventually(t, func() bool {
                conn, dialErr := net.DialTimeout("tcp", "localhost:4318", 200*time.Millisecond)
                if dialErr != nil {
                        return false
                }
                _ = conn.Close()
                return true
        }, 30*time.Second, 200*time.Millisecond,
                "collector did not open port 4318 within 30s")

        // Brief pause to let the full pipeline initialize after the port opens.
        time.Sleep(500 * time.Millisecond)

        // --- Step 6: send three gauge data points with unique session_id values ---
        //
        // max_cardinality_delta_per_epoch = 2 in config.yaml.
        //
        //   s1 → unique count 1  (1 ≤ 2, attribute kept)
        //   s2 → unique count 2  (2 ≤ 2, attribute kept)
        //   s3 → unique count 3  (3 > 2, session_id attribute stripped)
        t.Log("Sending OTLP metrics payload (3 unique session_id values)...")
        require.NoError(t, sendGaugeMetrics(t, otlpHTTPAddr, "session.duration",
                map[string]string{"session_id": "s1"},
                map[string]string{"session_id": "s2"},
                map[string]string{"session_id": "s3"},
        ))

        // --- Step 7: wait for the file exporter to write the output ---------------
        t.Log("Waiting for file exporter to flush output...")
        require.Eventually(t, func() bool {
                info, statErr := os.Stat(e2eOutputFile)
                return statErr == nil && info.Size() > 0
        }, 10*time.Second, 200*time.Millisecond,
                "file exporter did not write to %s within 10s", e2eOutputFile)

        // Allow a brief moment for the OS write buffer to fully flush.
        time.Sleep(200 * time.Millisecond)

        // --- Step 8: parse the file exporter output --------------------------------
        dps := collectGaugeDataPoints(t, e2eOutputFile, "session.duration")

        // --- Step 9: assert the cardinality policy was enforced -------------------
        //
        // Hard-drop mode forwards every data point but removes the over-limit
        // attribute. All three data points must appear in the output.
        require.Len(t, dps, 3,
                "all 3 data points must be forwarded (hard-drop strips the attribute, not the data point)")

        withSessionID := 0
        for _, dp := range dps {
                if _, ok := dp.Attributes().Get("session_id"); ok {
                        withSessionID++
                }
        }

        assert.Equal(t, 2, withSessionID,
                "exactly 2 data points should retain session_id: s1 and s2 are within the "+
                        "limit of 2; s3 exceeded it and had its session_id attribute stripped")

        t.Logf("SUCCESS: %d/3 data points retained session_id; the 3rd was correctly stripped.",
                withSessionID)
}

// sendGaugeMetrics builds a pmetric.Metrics payload with one gauge data point
// per element of attrs and POSTs it to the specified OTLP HTTP endpoint as
// a protobuf-encoded ExportMetricsServiceRequest.
func sendGaugeMetrics(t *testing.T, addr string, metricName string, attrs ...map[string]string) error {
        t.Helper()

        md := pmetric.NewMetrics()
        rm := md.ResourceMetrics().AppendEmpty()
        sm := rm.ScopeMetrics().AppendEmpty()
        m := sm.Metrics().AppendEmpty()
        m.SetName(metricName)
        m.SetEmptyGauge()

        for i, attrSet := range attrs {
                dp := m.Gauge().DataPoints().AppendEmpty()
                dp.SetDoubleValue(float64(i + 1))
                dp.SetTimestamp(pcommon.NewTimestampFromTime(time.Now()))
                for k, v := range attrSet {
                        dp.Attributes().PutStr(k, v)
                }
        }

        marshaler := &pmetric.ProtoMarshaler{}
        protoBytes, err := marshaler.MarshalMetrics(md)
        if err != nil {
                return fmt.Errorf("marshal OTLP payload: %w", err)
        }

        resp, err := http.Post(addr, "application/x-protobuf", bytes.NewReader(protoBytes))
        if err != nil {
                return fmt.Errorf("POST to %s: %w", addr, err)
        }
        defer resp.Body.Close()
        _, _ = io.Copy(io.Discard, resp.Body)

        if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
                return fmt.Errorf("collector returned unexpected HTTP status %d", resp.StatusCode)
        }

        return nil
}

// collectGaugeDataPoints reads the file exporter output line by line, unmarshals
// each non-empty line as OTLP JSON, and returns every NumberDataPoint belonging
// to the named gauge metric found across all batches.
func collectGaugeDataPoints(t *testing.T, path, metricName string) []pmetric.NumberDataPoint {
        t.Helper()

        f, err := os.Open(path)
        require.NoError(t, err, "open file exporter output at %s", path)
        defer f.Close()

        scanner := bufio.NewScanner(f)
        scanner.Buffer(make([]byte, 16*1024*1024), 16*1024*1024)

        unmarshaler := pmetric.JSONUnmarshaler{}
        var dps []pmetric.NumberDataPoint

        for scanner.Scan() {
                line := strings.TrimSpace(scanner.Text())
                if line == "" {
                        continue
                }

                md, unmarshalErr := unmarshaler.UnmarshalMetrics([]byte(line))
                if unmarshalErr != nil {
                        t.Logf("collectGaugeDataPoints: skipping unparseable line: %v", unmarshalErr)
                        continue
                }

                for i := 0; i < md.ResourceMetrics().Len(); i++ {
                        rm := md.ResourceMetrics().At(i)
                        for j := 0; j < rm.ScopeMetrics().Len(); j++ {
                                for k := 0; k < rm.ScopeMetrics().At(j).Metrics().Len(); k++ {
                                        m := rm.ScopeMetrics().At(j).Metrics().At(k)
                                        if m.Name() == metricName && m.Type() == pmetric.MetricTypeGauge {
                                                for l := 0; l < m.Gauge().DataPoints().Len(); l++ {
                                                        dps = append(dps, m.Gauge().DataPoints().At(l))
                                                }
                                        }
                                }
                        }
                }
        }

        require.NoError(t, scanner.Err(), "scanner error while reading %s", path)
        return dps
}
