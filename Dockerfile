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

# Final minimal stage: True Distroless
FROM gcr.io/distroless/static-debian12:nonroot

# Copy certificates for secure outbound connections
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy the static binary from the builder stage
COPY --from=builder /app/examples/build/otelcol-custom /otelcol

# Ports for OTLP (4317, 4318), Prometheus (8888), and Healthcheck (13133)
EXPOSE 4317 4318 8888 13133

# Run as the unprivileged 'nonroot' user provided by distroless
USER nonroot

# Use OTel's health_check extension for external health monitoring
ENTRYPOINT ["/otelcol", "--config=/etc/otelcol/config.yaml"]
