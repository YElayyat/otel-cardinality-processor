# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.3.1] - 2026-04-13

### Added
- Docker layer caching in CI to speed up builds.
- Explicit `latest` tag management in GitHub Actions.
- Release checklist and `act` testing instructions in `CONTRIBUTING.md`.

### Changed
- **Security**: Migrated Docker base image to `gcr.io/distroless/static-debian12:nonroot` (~80MB -> ~2MB).
- **Hardening**: Removed `curl` and `shell` from the Docker image; shifted health monitoring to OTel's `health_check` extension.

## [1.3.0] - 2026-04-12

### Changed
- **Breaking Change**: Default `max_cardinality_delta_per_epoch` changed from `500` to `100`.
- Standardized documentation and examples to use the new `100` threshold.

### Added
- Enterprise Rollout Strategy documentation in `README.md`.
- Deployment flowchart (Mermaid) in `README.md`.
- Multi-arch Docker build support (amd64, arm64).

## [1.2.0] - 2026-04-11
### Added
- Drop log sampling to bound enforcement log volume.

## [1.1.0] - 2026-04-10
### Added
- Top-N offender gauge for cardinality observability.
- Per-metric cardinality overrides via metric_overrides config map.
- max_tracker_count to prevent unbounded memory growth.

## [1.0.2] - 2026-04-09
### Fixed
- Removed unused warnedMetrics sync.Map.
- Bumped otel/sdk to v1.43.0 to resolve PATH hijacking CVE.

## [1.0.1] - 2026-04-08
### Fixed
- Security update: Bumped Go toolchain requirements to 1.25.9/1.26.2 to mitigate upstream x509 and TLS standard library vulnerabilities (GO-2026-4947, GO-2026-4946, GO-2026-4870).
