// Package e2e contains black-box end-to-end integration tests for the
// cardinality_guardian processor. Unlike the unit tests in
// cardinalityprocessor/, these tests spin up a real OpenTelemetry Collector
// binary with the processor wired into a live pipeline and verify observable
// behavior from the outside — no access to internal types or state.
//
// # Build tag
//
// All test files in this package carry the build tag `//go:build e2e`. They
// are therefore excluded from the default `go test ./...` run and from the
// `make test` Makefile target. To execute them, use:
//
//	make e2e
//
// or equivalently:
//
//	go test -tags e2e -timeout 5m ./test/e2e/...
//
// # Prerequisites
//
// Before running the e2e suite you need:
//
//  1. A compiled ocb (OpenTelemetry Collector Builder) binary, or a pre-built
//     Collector binary that includes this processor registered via NewFactory().
//  2. A writable temporary directory for the Collector's config file and
//     output artifacts.
//  3. (Optional) A running Prometheus-compatible scrape target on localhost if
//     any test verifies metric ingestion from an external source.
//
// # Adding new tests
//
// Follow these conventions when adding tests to this package:
//   - Each test must be fully self-contained: it starts its own Collector
//     subprocess and tears it down via t.Cleanup(), even on test failure.
//   - Use t.TempDir() for all Collector config and output files — never write
//     to the repository root or to hard-coded paths.
//   - Assert on observable outputs only (e.g. exported metrics, log lines,
//     forwarded OTLP payloads) — never import internal processor types.
//   - Name tests TestE2E_<Scenario> so they are easy to filter with -run.
package e2e
