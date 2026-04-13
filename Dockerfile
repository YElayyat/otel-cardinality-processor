# Use Go 1.25 on Bookworm
FROM golang:1.25.9-bookworm AS builder

# Set up environment
ENV OCB_VERSION=0.148.0
WORKDIR /app

# Install OpenTelemetry Collector Builder (ocb)
RUN go install go.opentelemetry.io/collector/cmd/builder@v${OCB_VERSION}

# Copy the entire project
COPY . .

# Build the custom collector binary statically
# We must run it from the examples folder so the `../../` paths in builder.yaml resolve properly.
WORKDIR /app/examples
RUN CGO_ENABLED=0 builder --config builder.yaml

# Final minimal stage
FROM debian:bookworm-slim

# Create a non-privileged user and install curl for the healthcheck
RUN groupadd -r otel && useradd -r -g otel otel && \
    apt-get update && \
    apt-get install -y --no-install-recommends curl ca-certificates && \
    rm -rf /var/lib/apt/lists/*

# Copy the static binary from the builder stage
# Output path specified in builder.yaml is ./build, meaning /app/examples/build
COPY --from=builder /app/examples/build/otelcol-custom /otelcol

# Drop down to the unprivileged user
USER otel

# Expose standard OTLP and extension ports
# 4317: OTLP gRPC
# 4318: OTLP HTTP
# 8888: Prometheus Metrics
# 13133: Health Check Extension
EXPOSE 4317 4318 8888 13133

# Add a healthcheck to monitor the collector's internal state
HEALTHCHECK --interval=10s --timeout=5s --retries=3 \
  CMD curl -f http://localhost:13133/ || exit 1

# Require configuration file to be mounted
ENTRYPOINT ["/otelcol", "--config=/etc/otelcol/config.yaml"]
