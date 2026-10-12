# Codex historical replay trial

This trial connects the installed, already authenticated Codex CLI through a Go HTTP adapter. ESA keeps deterministic routing hints and the asynchronous worker; the Agent returns its classification, issue assignment and specific code findings. Existing display fields are derived for engineer review. Ruby remains the Redmine integration layer. No Kafka or original Jira files are edited.

## Selected issue and scope

The selected public Bug is **KAFKA-2880**, reported on **2015-11-24T00:13:27.387Z**: a consumer metadata lookup throws a NullPointerException when the broker cannot be reached. The affected version is **0.9.0.0**. The replay snapshot uses commit `fc7243c2af4b2b4affcdf14d80826986fe6d4ce8`, authored and committed before the cutoff.

`cmd/esa-replay` rewinds available diagnostic field changes, excludes later comments and comments edited after the cutoff, clears resolution evidence, and exports a source snapshot using Git archive. The snapshot contains no `.git` directory, future commits, fix versions, issue links, attachments or post-cutoff discussion. It records raw and source-file hashes. The original description supplies the no-broker startup scenario; no customer OS/JDK is invented.

The test compiles all historical clients Java sources plus the historical shipped MockClient in a pinned Java 8 image. It exercises actual Fetcher, ConsumerNetworkClient and MetadataResponse classes. A successful metadata response is the control; a disconnected ClientResponse with an absent response body is the failure scenario. The container has no network, a read-only root filesystem, bounded temporary storage, memory/CPU/process limits, and no added capabilities.

This is a deterministic client request replay. It does not start a broker cluster or the complete Kafka Connect distributed process. The original frozen trial used source windows selected by the test operator. New managed sessions can search the registered historical source and rerun the fixture through restricted tools; their results are audited separately from the original report.

## Provider

`cmd/esa-codex` listens on host loopback port 8091. Colima containers can reach it through `host.lima.internal`. It authenticates requests with the existing backend service token, accepts only the registered project/ticket diagnostic fingerprint, and invokes the installed Codex CLI without inheriting user MCPs, plugins, hooks, user instructions or previous sessions. Only the operator-registered replay MCP is enabled. Model and reasoning preferences are read from the user's existing local configuration. A Redmine CRLF conversion is normalized for fingerprinting without changing preserved raw text.

The session uses `codex exec`, an ephemeral read-only sandbox, disabled native host tools, a custom diagnosis instruction and a strict final JSON schema. With managed tools enabled, four replay MCP tools provide source inspection, container commands, registered installation and environment change requests. Every citation must match a frozen document or audited tool response, including the exact source identifier, line number and quote. Results are left **Awaiting review** in Redmine and do not close the source issue.

Private audit artifacts under `.esa/codex-runs/<run-id>` preserve input, exact prompt, schema, CLI events, usage, errors and the final report. Repeating the same completed run returns its cached report. Interrupted or failed attempts cannot automatically restart with the same run ID; a new forced run is explicit. Missing usage from an interrupted CLI run is unknown, not zero.

Rule version 3 retains classification rules, one provider invocation and a separate 900-second execution deadline. Input 16,000, output 6,000 and total 22,000 tokens trigger warnings without terminating analysis. The CLI can report usage only at completion; no hard generation-token ceiling is applied. See [pipeline operations](REPLAY_PIPELINE.md) for automatic setup, delta reuse, cleanup and permissions. Historical trial reports retain their original usage and conditions.

## Reproduce

Build the host tools with the existing project-local Go toolchain:

```sh
GOCACHE="$PWD/.tools/go-cache" GOTOOLCHAIN=local .tools/go/bin/go build -o bin/esa-replay ./cmd/esa-replay
GOCACHE="$PWD/.tools/go-cache" GOTOOLCHAIN=local .tools/go/bin/go build -o bin/esa-codex ./cmd/esa-codex
```

Prepare a **new**, nonexisting bundle directory. The current trial uses `.esa/replays/KAFKA-2880/trial-1`; the scripts select that directory explicitly.

```sh
bin/esa-replay -raw data/jira/raw/KAFKA-2880.json -repo kafka -ref 0.9.0.0 \
  -at 2015-11-24T00:13:27.387Z -out .esa/replays/KAFKA-2880/trial-1 \
  -environment '{"reported_scenario":"Kafka Connect distributed startup with no broker running, as stated in the issue description"}'
docker --context colima-redmine pull eclipse-temurin:8-jdk
python3 scripts/replay_trial.py prepare
python3 scripts/manage_codex_provider.py start
```

Set the public endpoint `ESA_ANALYZER_URL=http://host.lima.internal:8091/analyze` in the private Compose `.env`, rebuild/recreate ESA, then run `python3 scripts/verify_codex_replay.py`. This script imports one historical ticket through Redmine; it does not import the Jira archive. `--retry` explicitly forces a new run and must only be used after inspecting previous provider activity.

After the diagnosis is complete and frozen, invoke the separate evaluator:

```sh
python3 scripts/replay_pipeline.py --pipeline --manifest system_replay/examples/esa-api-kafka.json --output reports/new-system-run
```

The evaluator exports the later upstream fix into `.esa/evaluation`, outside the Agent bundle, and runs the **same unchanged test**. It preserves the frozen diagnosis hash, upstream patch, fixed revision and test logs. Diagnostic assertions compare cited locations and recommendations with the actual upstream correction; they are not a general accuracy score.

To stop only the host adapter, use `python3 scripts/manage_codex_provider.py stop`. The adapter does not install a system-wide service. Registered evidence for other tickets and a production service supervisor are future work.

## Limits of interpretation

- The export has no independently captured initial description snapshot. Reconstruction relies on complete changelog metadata and available old field values. Ambiguous histories fail rather than invent values.
- One selected, stack-rich public bug cannot establish classification or localization accuracy across the archive.
- Historical input isolation does not remove knowledge the model may have learned during training.
- Universal historical build profiles, attachment replay, archive-wide ground-truth scoring and release lifecycle verification remain future work. Controlled source/log retrieval is available only within the registered replay environment.

The Codex execution mechanism follows [official non-interactive documentation](https://learn.chatgpt.com/docs/non-interactive-mode).

The imported-report evaluator has been removed. The [external system tester](TICKET_BACKTEST.md) now observes the actual backend workflow through public APIs. Its default examples do not invoke Codex; configure the system under test with its own provider and environment adapter to evaluate the actual engineering Agent. Earlier trial reports and metrics remain historical artifacts.
