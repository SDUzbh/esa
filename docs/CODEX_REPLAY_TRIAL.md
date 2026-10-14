# Codex historical replay trial

This document describes the focused application-provider trial, not the current external system tester. Its retained reports are historical artifacts. For current replay and evaluation, use [external system replay](TICKET_BACKTEST.md).

This trial connects the installed, already authenticated Codex CLI through a Go HTTP adapter. ESA owns the asynchronous worker. In the current provider, Engineering Agent returns specific code findings and a grouping request; Routing Agent supplies the final Issue assignment. Existing display fields are derived for engineer review. Ruby remains the Redmine integration layer. No Kafka or original Jira files are edited.

## Selected issue and scope

The selected public Bug is **KAFKA-2880**, reported on **2015-11-24T00:13:27.387Z**: a consumer metadata lookup throws a NullPointerException when the broker cannot be reached. The affected version is **0.9.0.0**. The replay snapshot uses commit `fc7243c2af4b2b4affcdf14d80826986fe6d4ce8`, authored and committed before the cutoff.

`cmd/esa-replay` rewinds available diagnostic field changes, excludes later comments and comments edited after the cutoff, clears resolution evidence, and exports a source snapshot using Git archive. The snapshot contains no `.git` directory, future commits, fix versions, issue links, attachments or post-cutoff discussion. It records raw and source-file hashes. The original description supplies the no-broker startup scenario; no customer OS/JDK is invented.

The test compiles all historical clients Java sources plus the historical shipped MockClient in a pinned Java 8 image. It exercises actual Fetcher, ConsumerNetworkClient and MetadataResponse classes. A successful metadata response is the control; a disconnected ClientResponse with an absent response body is the failure scenario. The container has no network, a read-only root filesystem, bounded temporary storage, memory/CPU/process limits, and no added capabilities.

This is a deterministic client request replay. It does not start a broker cluster or the complete Kafka Connect distributed process. The original frozen trial used source windows selected by the test operator. New managed sessions can search the registered historical source and rerun the fixture through restricted tools; their results are audited separately from the original report.

## Provider

`cmd/esa-codex` listens on host loopback port 8091. Colima containers can reach it through `host.lima.internal`. It authenticates requests with the existing backend service token, accepts only the registered project/ticket diagnostic fingerprint, and invokes the installed Codex CLI without inheriting user MCPs, plugins, hooks, user instructions or previous sessions. Only the operator-registered replay MCP is enabled. Model and reasoning preferences are read from the user's existing local configuration. A Redmine CRLF conversion is normalized for fingerprinting without changing preserved raw text.

The session uses `codex exec`, an ephemeral read-only sandbox, disabled native host tools, a custom diagnosis instruction and a strict final JSON schema. With managed tools enabled, four replay MCP tools provide source inspection, container commands, registered installation and environment change requests. Every citation must match a frozen document or audited tool response, including the exact source identifier, line number and quote. Results are left **Awaiting review** in Redmine and do not close the source issue.

Private audit artifacts under `.esa/codex-runs/<run-id>` preserve input, exact prompt, schema, CLI events, usage, errors and the final report. Repeating the same completed run returns its cached report. Interrupted or failed attempts cannot automatically restart with the same run ID; a new forced run is explicit. Missing usage from an interrupted CLI run is unknown, not zero.

The original trial used rule version 3. The current workflow excludes admission classification and requires explicit manual forwarding and an overall 900-second execution deadline. The native provider now retrieves local Issue records through Routing Agent; reuse skips engineering, while new diagnosis invokes engineering once. See [Issue directory and routing](ISSUE_DIRECTORY.md) for stage usage. Input 16,000, output 6,000 and total 22,000 weighted units trigger warnings without terminating analysis. The current defaults count uncached input/output at 1 and cached input at 0.1; raw usage and cache hits remain available. These weights are operator estimates, not a billing statement. The CLI can report usage only at completion; no hard generation-token ceiling is applied. See [pipeline operations](REPLAY_PIPELINE.md) for automatic setup, delta reuse, cleanup and permissions. Historical trial reports retain their original usage and conditions.

## Reproduce

Build the host tools with the existing project-local Go toolchain:

```sh
GOCACHE="$PWD/.tools/go-cache" GOTOOLCHAIN=local .tools/go/bin/go build -o bin/esa-replay ./cmd/esa-replay
GOCACHE="$PWD/.tools/go-cache" GOTOOLCHAIN=local .tools/go/bin/go build -o bin/esa-codex ./cmd/esa-codex
```

Prepare a **new**, nonexisting bundle directory. This historical fixture uses `.esa/replays/KAFKA-2880/trial-1`.

```sh
bin/esa-replay -raw data/jira/raw/2015/11/24/KAFKA-2880.json -repo kafka -ref 0.9.0.0 \
  -at 2015-11-24T00:13:27.387Z -out .esa/replays/KAFKA-2880/trial-1 \
  -environment '{"reported_scenario":"Kafka Connect distributed startup with no broker running, as stated in the issue description"}'
docker --context colima-redmine pull eclipse-temurin:8-jdk
python3 scripts/replay_trial.py prepare
python3 scripts/manage_codex_provider.py start
```

The former live Redmine import verifiers were removed because they depended on retired automatic intake and case matching. Current native routing coverage uses `go test ./internal/codex -run TestNativeRouting`; it exercises the public HTTP provider with fixture subprocesses and does not consume model tokens or modify live tickets. Use the external pipeline below for system replay.

The previous imported-report evaluator has been removed. The current command below starts a **new system replay** through public APIs. It does not ingest or evaluate the diagnosis from this historical provider trial:

```sh
python3 scripts/replay_pipeline.py --pipeline --manifest system_replay/examples/esa-api-kafka.json --output reports/new-system-run
```

The default manifest disables the engineering provider and expects `awaiting_configuration` after explicit forwarding. It exports baseline, upstream parent and repaired snapshots below the new report directory, runs the same neutral client fixture, and checks external failure/healthy assertions. It verifies product behavior without claiming Agent accuracy.


To stop only the host adapter, use `python3 scripts/manage_codex_provider.py stop`. The adapter does not install a system-wide service. Registered evidence for other tickets and a production service supervisor are future work.

## Limits of interpretation

- The export has no independently captured initial description snapshot. Reconstruction relies on complete changelog metadata and available old field values. Ambiguous histories fail rather than invent values.
- One selected, stack-rich public bug cannot establish classification or localization accuracy across the archive.
- Historical input isolation does not remove knowledge the model may have learned during training.
- Universal historical build profiles, attachment replay, archive-wide ground-truth scoring and release lifecycle verification remain future work. Controlled source/log retrieval is available only within the registered replay environment.

The Codex execution mechanism follows [official non-interactive documentation](https://learn.chatgpt.com/docs/non-interactive-mode).

Current ESA intake requires explicit `forwarded_by` and `forwarded_at`. External fixtures declare the manual action; the tester does not infer it. Classification and label learning are removed from the active application. See [manual forwarding](MANUAL_FORWARDING.md).
