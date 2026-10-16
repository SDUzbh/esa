#!/bin/sh
# Reuse deterministic tests and public API replay; never invoke a real Agent.
set -eu
cd "$(dirname "$0")/.."
ESA_GO_BIN=${ESA_GO_BIN:-"$PWD/.tools/go/bin/go"}
if [ ! -x "$ESA_GO_BIN" ]; then ESA_GO_BIN=go; fi
export ESA_GO_BIN
export GOCACHE="$PWD/.tools/go-cache" GOTOOLCHAIN=local
export ESA_TEST_RACE="${ESA_TEST_RACE:-1}"
export ESA_API_TOKEN= ESA_ANALYZER_URL= ESA_ANALYZER_TOKEN= ESA_REDMINE_URL= ESA_STATE_URL=
unset OPENAI_API_KEY CODEX_API_KEY
mkdir -p .tools
if [ -n "${ESA_WORKFLOW_OUTPUT:-}" ]; then
    mkdir "$ESA_WORKFLOW_OUTPUT"
else
    ESA_WORKFLOW_OUTPUT=$(mktemp -d "$PWD/.tools/workflow-checks-XXXXXX")
fi
ESA_WORKFLOW_OUTPUT=$(cd "$ESA_WORKFLOW_OUTPUT" && pwd)
export ESA_CI_AGENT_ATTEMPTS="$ESA_WORKFLOW_OUTPUT/agent-call-attempted"
# Intercept accidental default-CLI use even on an authenticated developer host.
# Unit tests supply their own fake executables and use real tool handlers.
mkdir "$ESA_WORKFLOW_OUTPUT/guard"
cat > "$ESA_WORKFLOW_OUTPUT/guard/codex" <<'GUARD'
#!/bin/sh
echo 'Real Agent execution is forbidden in workflow checks.' >&2
printf 'codex\n' >> "$ESA_CI_AGENT_ATTEMPTS"
exit 97
GUARD
chmod +x "$ESA_WORKFLOW_OUTPUT/guard/codex"
export PATH="$ESA_WORKFLOW_OUTPUT/guard:$PATH"
WORKFLOW_GOFMT="$(dirname "$ESA_GO_BIN")/gofmt"
if [ ! -x "$WORKFLOW_GOFMT" ]; then WORKFLOW_GOFMT=gofmt; fi
unformatted=$("$WORKFLOW_GOFMT" -l cmd internal tools system_replay)
if [ -n "$unformatted" ]; then
    printf 'Run gofmt on:\n%s\n' "$unformatted" >&2
    exit 1
fi
"$ESA_GO_BIN" vet ./...
"$ESA_GO_BIN" -C system_replay vet ./...
# Keep the store scaling benchmarks compiling and runnable (tiny size, one pass).
ESA_BENCH_SIZES=50 "$ESA_GO_BIN" test ./internal/esa -run '^$' -bench 'Store|TicketGET|ReceiveClosed' -benchtime 1x > /dev/null
sh scripts/build_esa.sh
sh scripts/build_system_replay.sh
python3 -B -m unittest discover -s scripts -p 'test_*.py'
ruby scripts/test_label_payload.rb
ruby scripts/test_esa_presentation.rb
ruby scripts/test_esa_acknowledgement.rb
"${ESA_NODE_BIN:-node}" --test scripts/test_esa_ticket_ui.cjs
python3 -B scripts/verify_shared_investigations.py
for fixture in esa-api-lifecycle esa-api-forwarding esa-shared-investigations; do
    python3 -B scripts/replay_pipeline.py --pipeline \
        --manifest "system_replay/examples/$fixture.json" \
        --output "$ESA_WORKFLOW_OUTPUT/$fixture"
done
test ! -e "$ESA_CI_AGENT_ATTEMPTS"
printf 'Workflow checks passed without real Agent calls. Reports: %s\n' "$ESA_WORKFLOW_OUTPUT"
