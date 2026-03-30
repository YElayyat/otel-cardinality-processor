//go:build e2e

package e2e_test

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	otlpTagOnlyHTTPAddr  = "http://localhost:4320/v1/metrics"
	e2eTagOnlyOutputFile = "/tmp/cardinality-e2e-tagonly-output.json"
)

func TestE2E_CardinalityTagOnlyMode(t *testing.T) {
	ocbBin, err := exec.LookPath("ocb")
	require.NoError(t, err, "ocb binary not found in PATH")

	buildDir := t.TempDir()
	t.Logf("Building custom collector into %s", buildDir)

	buildCmd := exec.Command(ocbBin,
		"--config", "builder.yaml",
		"--output-path", buildDir,
	)
	buildOut, buildErr := buildCmd.CombinedOutput()
	require.NoError(t, buildErr, "OCB build failed:\n%s", buildOut)

	collectorBin := filepath.Join(buildDir, collectorBinName)
	require.FileExists(t, collectorBin, "expected compiled binary at %s", collectorBin)

	_ = os.Remove(e2eTagOnlyOutputFile)

	collectorCmd := exec.Command(collectorBin, "--config", "config_tag_only.yaml")
	collectorCmd.Stderr = os.Stderr

	require.NoError(t, collectorCmd.Start(), "failed to start collector subprocess")

	t.Cleanup(func() {
		if collectorCmd.Process != nil {
			_ = collectorCmd.Process.Kill()
			_ = collectorCmd.Wait()
		}
		_ = os.Remove(e2eTagOnlyOutputFile)
	})

	t.Log("Waiting for collector OTLP HTTP port 4320...")
	require.Eventually(t, func() bool {
		conn, dialErr := net.DialTimeout("tcp", "localhost:4320", 200*time.Millisecond)
		if dialErr != nil {
			return false
		}
		_ = conn.Close()
		return true
	}, 30*time.Second, 200*time.Millisecond, "collector did not open port 4320 within 30s")

	time.Sleep(500 * time.Millisecond)

	t.Log("Sending OTLP metrics payload (3 unique session_id values)...")
	require.NoError(t, sendGaugeMetrics(t, otlpTagOnlyHTTPAddr, "session.duration",
		map[string]string{"session_id": "s1"},
		map[string]string{"session_id": "s2"},
		map[string]string{"session_id": "s3"},
	))

	t.Log("Waiting for file exporter to flush output...")
	require.Eventually(t, func() bool {
		info, statErr := os.Stat(e2eTagOnlyOutputFile)
		return statErr == nil && info.Size() > 0
	}, 10*time.Second, 200*time.Millisecond, "file exporter did not write to %s", e2eTagOnlyOutputFile)

	time.Sleep(200 * time.Millisecond)

	dps := collectGaugeDataPoints(t, e2eTagOnlyOutputFile, "session.duration")

	require.Len(t, dps, 3, "all 3 data points must be forwarded in tag_only mode")

	withSessionID := 0
	withLimitFlag := 0
	for _, dp := range dps {
		if _, ok := dp.Attributes().Get("session_id"); ok {
			withSessionID++
		}
		if val, ok := dp.Attributes().Get("cardinality_limit_exceeded"); ok && val.Bool() == true {
			withLimitFlag++
		}
	}

	assert.Equal(t, 3, withSessionID, "all 3 data points should retain session_id in tag_only mode")
	assert.Equal(t, 1, withLimitFlag, "exactly 1 data point should have the cardinality_limit_exceeded flag set to true")

	t.Logf("SUCCESS: %d/3 data points retained session_id and %d was tagged.", withSessionID, withLimitFlag)
}
