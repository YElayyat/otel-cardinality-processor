# Contributing to Cardinality Guardian

First off, thank you for considering contributing to Cardinality Guardian! This processor is designed to protect OpenTelemetry pipelines from cardinality explosions, and community contributions are vital to keeping it highly performant and secure.

## 📋 Scope of Contributions
To keep the project focused, please adhere to the following guidelines:
* **Bug Fixes & Performance:** Optimizations to the core sharding logic, memory management, or CPU overhead are highly encouraged. Open a Pull Request (PR) directly.
* **New Features:** Before writing code for new dropping strategies (e.g., LFU vs. LRU) or major architectural shifts, please open a GitHub Issue first to discuss the design.
* **Refactoring:** We do not accept refactor-only PRs unless they are tied to a specific bug fix or performance improvement.

## 🛠️ Development Environment
We maintain strict version requirements to ensure optimal performance and security.

* **Go:** Version `1.25` or higher is strictly required.
* **Tooling:** You will need OpenTelemetry Collector Builder (OCB) to compile custom collector distributions, and Docker for running End-to-End (E2E) sandbox tests.

### Getting Started
1. Fork the repository and clone it locally.
2. Install dependencies: `go mod tidy`
3. Verify your environment by running the test suite via the provided Makefile.

## ✅ The Quality Gate (CI)
All Pull Requests must pass our automated CI pipeline before they can be merged. We recommend running these checks locally before pushing your code.

We use a `Makefile` to standardize these commands:
* **Unit Tests:** Run `make test`
* **E2E Tests:** Run `make e2e` (Requires Docker)
* **Linting:** Run `make lint` 
  * *Note:* Due to the bleeding-edge Go 1.25 requirement, we utilize `golangci-lint` v1.66.0+. Ensure your local linter matches this version to avoid discrepancies with GitHub Actions.
* **Vulnerability Scan:** We run `govulncheck` on all PRs. 

## 🚀 Pull Request Process
1. Create a focused branch for your changes (e.g., `fix/memory-leak` or `feat/lru-eviction`).
2. Keep your PRs small and tightly scoped. Do not mix unrelated changes.
3. Update the `README.md` and any relevant documentation if your changes affect user-facing configurations.
4. Ensure all CI checks (Lint, Test, Vulncheck) are green. 
5. A maintainer will review your code. Please respond promptly to any review comments.

## 🔒 Security Vulnerabilities
If you discover a vulnerability that could allow cardinality limits to be bypassed or cause a denial of service (DoS) within the collector, **do not open a public issue.** Please use [GitHub's Private Vulnerability Reporting](https://github.com/YElayyat/otel-cardinality-processor/security/advisories/new) feature to responsibly disclose the issue directly to the maintainers. We will review and respond to your report promptly through that secure channel.
