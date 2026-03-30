#!/bin/bash
set -e

LINT_VER="v1.64.5"

if command -v golangci-lint &> /dev/null && golangci-lint version | grep -q "$LINT_VER"; then
    echo "golangci-lint $LINT_VER is already installed."
    exit 0
fi

echo "Installing golangci-lint $LINT_VER from source using $(go version)..."
go install github.com/golangci/golangci-lint/cmd/golangci-lint@$LINT_VER
echo "Installed successfully to $(go env GOPATH)/bin/golangci-lint"
