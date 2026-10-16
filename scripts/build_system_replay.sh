#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
REPLAY_GO_BIN=${ESA_GO_BIN:-"$PWD/.tools/go/bin/go"}
if [ ! -x "$REPLAY_GO_BIN" ]; then REPLAY_GO_BIN=go; fi
mkdir -p bin .tools/go-cache
set --
if [ "${ESA_TEST_RACE:-0}" = 1 ]; then set -- -race; fi
GOCACHE="$PWD/.tools/go-cache" GOTOOLCHAIN=local "$REPLAY_GO_BIN" -C system_replay test "$@" ./...
GOCACHE="$PWD/.tools/go-cache" GOTOOLCHAIN=local "$REPLAY_GO_BIN" -C system_replay build -trimpath -o ../bin/system-replay ./cmd/system-replay
