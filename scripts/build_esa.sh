#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
GO_BIN=${ESA_GO_BIN:-.tools/go/bin/go}
if [ ! -x "$GO_BIN" ]; then GO_BIN=go; fi
mkdir -p bin .tools/go-cache
GOCACHE="$PWD/.tools/go-cache" GOTOOLCHAIN=local "$GO_BIN" test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=${ESA_GO_ARCH:-arm64} GOCACHE="$PWD/.tools/go-cache" GOTOOLCHAIN=local "$GO_BIN" build -trimpath -o bin/esa ./cmd/esa
for ESA_HOST_COMMAND in esa-replay esa-codex; do
  GOCACHE="$PWD/.tools/go-cache" GOTOOLCHAIN=local "$GO_BIN" build -trimpath -o "bin/$ESA_HOST_COMMAND" "./cmd/$ESA_HOST_COMMAND"
done
