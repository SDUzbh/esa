#!/bin/sh
# Build documentation only; never start ESA, an Agent or a replay environment.
set -eu
cd "$(dirname "$0")/.."
command -v doxygen >/dev/null 2>&1 || {
  echo 'Doxygen is required. Install it separately (macOS: brew install doxygen).' >&2
  exit 1
}
DOCS_GO_BIN=${ESA_GO_BIN:-.tools/go/bin/go}
if [ ! -x "$DOCS_GO_BIN" ]; then DOCS_GO_BIN=go; fi
mkdir -p .tools/go-cache
GOCACHE="$PWD/.tools/go-cache" GOTOOLCHAIN=local "$DOCS_GO_BIN" build -trimpath -o .tools/doxygen-go ./tools/doxygen-go
# This fixed output belongs to this generator; remove obsolete reference pages.
rm -rf .tools/doxygen
mkdir -p .tools/doxygen
doxygen Doxyfile
echo "Documentation: $PWD/.tools/doxygen/html/index.html"
