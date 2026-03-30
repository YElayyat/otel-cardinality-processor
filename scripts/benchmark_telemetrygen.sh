#!/usr/bin/env bash
# benchmark_telemetrygen.sh — Load test the Cardinality Guardian with telemetrygen.
#
# This script builds a custom OTel Collector (via OCB), starts it with pprof
# enabled, blasts it with telemetrygen across multiple metric types, captures
# a CPU profile for flame graph generation, and reports results.
#
# Usage:  ./scripts/benchmark_telemetrygen.sh
# Requires: go, ocb (install via 'make install-ocb')

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
BUILD_DIR="$PROJECT_ROOT/examples/build"
COLLECTOR_BIN="$BUILD_DIR/otelcol-custom"
COLLECTOR_CONFIG="$PROJECT_ROOT/test/benchmark/otel-collector-bench.yaml"
RESULTS_DIR="$PROJECT_ROOT/test/benchmark/results"
PPROF_PORT=1777
OTLP_PORT=4317
DURATION="10s"
WORKERS=8

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

log()  { echo -e "${GREEN}[BENCH]${NC} $*"; }
warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
info() { echo -e "${CYAN}[INFO]${NC} $*"; }

cleanup() {
    log "Cleaning up..."
    if [ -n "${COLLECTOR_PID:-}" ] && kill -0 "$COLLECTOR_PID" 2>/dev/null; then
        kill "$COLLECTOR_PID" 2>/dev/null || true
        wait "$COLLECTOR_PID" 2>/dev/null || true
    fi
    log "Done."
}
trap cleanup EXIT

# -------------------------------------------------------------------
# 1. Install telemetrygen
# -------------------------------------------------------------------
log "Installing telemetrygen..."
go install github.com/open-telemetry/opentelemetry-collector-contrib/cmd/telemetrygen@v0.148.0

# -------------------------------------------------------------------
# 2. Build custom collector (if not already built)
# -------------------------------------------------------------------
if [ ! -f "$COLLECTOR_BIN" ]; then
    log "Building custom collector via OCB..."
    cd "$PROJECT_ROOT/examples"
    ocb --config builder.yaml
    cd "$PROJECT_ROOT"
else
    log "Using existing collector binary: $COLLECTOR_BIN"
fi

# -------------------------------------------------------------------
# 3. Start the collector
# -------------------------------------------------------------------
mkdir -p "$RESULTS_DIR"
log "Starting collector with pprof on :$PPROF_PORT ..."
"$COLLECTOR_BIN" --config "$COLLECTOR_CONFIG" > "$RESULTS_DIR/collector.log" 2>&1 &
COLLECTOR_PID=$!
sleep 3

if ! kill -0 "$COLLECTOR_PID" 2>/dev/null; then
    warn "Collector failed to start. Check $RESULTS_DIR/collector.log"
    exit 1
fi
log "Collector running (PID: $COLLECTOR_PID)"

# -------------------------------------------------------------------
# 4. Run telemetrygen — 3 passes for all metric types
# -------------------------------------------------------------------
run_pass() {
    local label="$1"
    local extra_args="${2:-}"
    
    info "Pass: $label (workers=$WORKERS, duration=$DURATION, rate=unlimited)"
    telemetrygen metrics \
        --otlp-endpoint="localhost:$OTLP_PORT" \
        --otlp-insecure \
        --workers "$WORKERS" \
        --rate 0 \
        --duration "$DURATION" \
        --otlp-attributes='region="us-east-1"' \
        --otlp-attributes='session_id="bench-unique-value"' \
        $extra_args \
        2>&1 | tee "$RESULTS_DIR/telemetrygen_${label}.log"
    
    log "Pass '$label' complete."
}

run_pass "gauge" ""
run_pass "sum" "--metric-type Sum"
run_pass "histogram" "--metric-type Histogram"

# -------------------------------------------------------------------
# 5. Capture CPU profile (10 seconds)
# -------------------------------------------------------------------
log "Capturing 10-second CPU profile from pprof..."
if curl -s "http://localhost:$PPROF_PORT/debug/pprof/profile?seconds=10" \
    -o "$RESULTS_DIR/cpu.prof" 2>/dev/null; then
    log "CPU profile saved to $RESULTS_DIR/cpu.prof"
    info "Generate flame graph with: go tool pprof -http=:8080 $RESULTS_DIR/cpu.prof"
else
    warn "Could not capture CPU profile (pprof may not be available)."
fi

# -------------------------------------------------------------------
# 6. Capture heap profile
# -------------------------------------------------------------------
log "Capturing heap profile..."
if curl -s "http://localhost:$PPROF_PORT/debug/pprof/heap" \
    -o "$RESULTS_DIR/heap.prof" 2>/dev/null; then
    log "Heap profile saved to $RESULTS_DIR/heap.prof"
else
    warn "Could not capture heap profile."
fi

# -------------------------------------------------------------------
# 7. Report
# -------------------------------------------------------------------
echo ""
echo "============================================================"
echo "  BENCHMARK LOAD TEST RESULTS"
echo "============================================================"
echo ""
echo "  Collector config:  $COLLECTOR_CONFIG"
echo "  Workers:           $WORKERS"
echo "  Duration per pass: $DURATION"
echo "  Metric types:      Gauge, Sum, Histogram"
echo ""
echo "  Results directory: $RESULTS_DIR/"
echo "    - collector.log"
echo "    - telemetrygen_gauge.log"
echo "    - telemetrygen_sum.log"
echo "    - telemetrygen_histogram.log"
echo "    - cpu.prof  (flame graph: go tool pprof -http=:8080 cpu.prof)"
echo "    - heap.prof (memory:     go tool pprof -http=:8080 heap.prof)"
echo ""
echo "============================================================"
