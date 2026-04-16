# 🚀 Cardinality Guardian Quickstart

This demo environment provides a pre-configured OpenTelemetry Collector, Prometheus TSDB, and a fully automated Grafana dashboard to showcase real-time high-cardinality label management natively in action.

## 1. Start the Environment
Bring up the entire observability stack natively.

```bash
docker compose up -d
```

## 2. Access the Dashboard
The Zero-Friction setup will automatically orchestrate and bypass the login window! 
Open your browser to [http://localhost:3000](http://localhost:3000). The **Cardinality Guardian Demo** dashboard will be beautifully loaded automatically.

## 3. Trigger a Multi-Service Cardinality Event
To see the `cardinality_guardian` rigorously isolating and managing multiple independent services and metric streams concurrently, blast the OpenTelemetry Collector with distinct background loads by running this block in your terminal:

```bash
# Terminal 1: Auth Service Load
go run github.com/open-telemetry/opentelemetry-collector-contrib/cmd/telemetrygen@latest metrics \
  --otlp-insecure --metrics 5000 --rate 50 --service auth-provider \
  --unique-timeseries --otlp-metric-name auth_time &

# Terminal 2: Checkout Latency Load
go run github.com/open-telemetry/opentelemetry-collector-contrib/cmd/telemetrygen@latest metrics \
  --otlp-insecure --metrics 5000 --rate 50 --service checkout-service \
  --unique-timeseries --otlp-metric-name checkout_latency &

# Terminal 3: Inventory Updates Load
go run github.com/open-telemetry/opentelemetry-collector-contrib/cmd/telemetrygen@latest metrics \
  --otlp-insecure --metrics 5000 --rate 50 --service inventory-manager \
  --unique-timeseries --otlp-metric-name inventory_update_count &
```

## 4. What to look for in the Dashboard

*   **Active Trackers (Current):** This will cleanly jump exactly up to `3` almost immediately. Because you configured uniquely named metrics, the Guardian automatically spawns independent HyperLogLog trackers in memory for each distinct metric boundary.
*   **Pipeline Throughput:** You will securely see the 150 ops/s throughput visualizing safely managed data allowed structurally through.
*   **Stripped Attributes Rate:** The graph aggressively spikes, highlighting the excess explosive labels violently matching the limits and being proactively discarded in real-time. 
*   **Top Cardinality Offenders:** All three unique metrics (`auth_time`, `checkout_latency`, `inventory_update_count`) will populate instantly side-by-side in this table format, proving accurately exactly how heavily unique boundaries were forcefully intercepted. *(Note: This data structure undergoes "Epoch Rotation" and will definitively display when the 5-minute config loop processes!)*
*   **Total Savings (Cumulative):** Watch your theoretical cost ROI dynamically climb exponentially as the Guardian ruthlessly neutralizes exactly 15,000 potential catastrophic billing events!
