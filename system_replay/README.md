# External system replay tester

This directory is a standalone Go module (`systemreplay`) with no application imports, SDKs or third-party Go modules. It can be copied out of the ESA repository and built independently. Python standard-library runtime infrastructure and a registered Kafka observation fixture are included. The application is an opaque executable or externally managed HTTP service, configured by a manifest.

The tester delivers historical inputs through public interfaces and records what the system actually did. Classification, issue assignment, case retrieval, routing, queues, engineering providers and business state transitions belong to the system under test. The tester never invokes those application functions, builds substitute issues, imports Agent predictions or directly launches an engineering Agent.

## Build and run independently

```sh
cd system_replay
GOCACHE="/tmp/system-replay-go-cache" GOTOOLCHAIN=local go test -race ./...
go build -o /tmp/system-replay ./cmd/system-replay
/tmp/system-replay --pipeline --manifest /absolute/path/test.json --output /absolute/path/new-report
```

The examples reference a separately built application artifact and test data. These references are operator configuration, not build dependencies of this module. Build the ESA host artifact separately from the application root before using them:

```sh
GOCACHE="$PWD/.tools/go-cache" GOTOOLCHAIN=local .tools/go/bin/go build -o bin/esa-host ./cmd/esa
sh scripts/build_system_replay.sh
python3 scripts/replay_pipeline.py --pipeline --manifest system_replay/examples/esa-api-lifecycle.json --output reports/new-lifecycle-run
python3 scripts/replay_pipeline.py --pipeline --manifest system_replay/examples/esa-api-kafka.json --output reports/new-kafka-run
```

The compatibility script only builds this independent module and execs its binary. The former `--agent`, `--predictions`, `--issue-history`, positional ticket shortcuts and direct-provider evaluation flow have been removed. A public target manifest is required. `--plan` validates input and source selection without executing the target, runtime or model. `--track-only` follows local Git references without target calls; it is explicitly not a system test.

## Replay stages

1. Read the manifest. Reconstruct original Jira fields from complete, count-checked history, exclude future comments/edited bodies and discard fix/resolution metadata. Explicit prepared tickets are also accepted with an operator-provenance limitation. Resolve the affected tag or explicit Git ref, reject future commits, export read-only source and retain hashes.
2. Start a fresh system-under-test process with its own state path and random service token. An already responding target URL is rejected before any write unless `--attach` is explicit. Attach mode does not assert clean state or ownership and never shuts down that target.
3. Prepare a registered product scenario in an independent runtime. Only the historical ticket, historical code path/commit and observed baseline logs can be delivered to the target's optional environment-setup endpoint or command. Assertions and future repair evidence are withheld.
4. Submit each event through the configured public request. Poll public results until the configured terminal state, record raw replies and evaluate external assertions. The first event observation remains the job's initial prediction; later updates or other tickets in the event stream cannot replace it. Optional clock requests advance a target-provided test clock; without such an API, only event order is simulated and business-clock behavior is explicitly untested.
5. Freeze every public-system output, pin its hash and verify source hashes before future Git evidence is read. Issue IDs and findings come from observed responses; the tester does not synthesize business classification or issue membership.
6. Resolve the mainline head once and share successful Git reads across tickets, only after the output-freeze boundary. Follow that immutable mainline graph over the next N release observation points. Exact source-key references or changes to observed finding files produce candidates. Follow line changes, renames and merge landing commits. Unmerged branches and pre-receipt updates are excluded. Retain local PR references and actual patches.
7. Reuse one independent runtime across candidate parent/fix snapshots in mainline order. Synchronize only source/dependency differences, reset mutable task state and run the same registered scenario. A repair passes only when the baseline and PR parent reproduce the failure, controls pass throughout, and the repaired snapshot meets the healthy assertions without still meeting the bug assertion.
8. Save all results and cleanup evidence. Invalid plans fail before target startup. Assertion, report-write, runtime reset or cleanup failures cannot return a successful test exit; final summaries include a run error when available. Patch overlap is an explicit metric, not proof of root-cause or solution correctness. An optional separate HTTP evaluator can supply semantic judgments from frozen public predictions, PR/source evidence and already-verified runtime behavior. Its judgments cannot override the runtime gate and require exact supplied evidence citations.

No universal scenario generator is claimed. A missing scenario produces `system_observed_replay_unregistered`, not a product reproduction claim. Startup alone cannot be selected as a bug replay profile. Controls, bug assertions and healthy assertions are mandatory for registered scenarios. False controls mean an environment failure, not a product failure.

## Manifest contract

`schema_version` is 1. All file paths are resolved relative to the manifest directory. `repo`, `mainline`, `versions` and `until` configure product history. Jobs have unique `id` and sanitized report paths, numeric `version`, `at`, exactly one of `raw` or prepared `ticket`, optional explicit `ref`, `environment`, `expect`, `events` and `scenario`.

The `target` has:

- `url`, `health_path`, `start` (`args`, optional `env`, `dir`) and optional `token_env`.
- `headers`, `submit` and `observe` requests. Requests specify `method`, relative `path`, headers and optional JSON `body`.
- `extract` JSON pointers for `state`, `category`, `route`, `run_id`, `issue_id`, `findings` and `usage`. Pointer lists provide protocol fallbacks. These are observed properties, never a classifier.
- `terminal_states`, `timeout_seconds`, `poll_milliseconds` and optional `capture` mappings from public observation JSON to subsequent request variables.
- Optional `advance_clock`, `prepare` and `prepare_command`. Environment setup remains a target-owned capability; the tester does not emulate a missing application management endpoint.

Templates use `{{ticket}}`, `{{ticket_id}}`, `{{source_key}}`, `{{project_id}}`, `{{event_id}}`, `{{event_time}}`, `{{receipt.run_id}}`, captured names such as `{{case_id}}`, and infrastructure values `{{run_dir}}`, `{{manifest_dir}}`, `{{token}}`, `{{target_url}}`. Whole-value JSON templates preserve objects instead of turning tickets into text. `{{historical_context}}` and its file path are available to target preparation. Commands are explicit operator configuration, never generated from ticket text.

Each job's initial event is followed by its chronological `events`. An event can supply an updated/different `ticket`, override public `request` / `observe`, specify a repeated `id` for idempotency testing, and check submission `receipt_expect`. `skip_observe` allows receipt-only negative tests and cannot be combined with an observation request or observation assertions. Changed event tickets refresh ticket/source/project template variables; captures absent from a new observation are removed. Inserted template values are treated as data and are not recursively expanded. Assertions use JSON pointers and `eq`, `ne`, `exists`, `absent`, `contains` or `not_contains`. Observation assertions see canonical properties and the full response under `/raw`; receipt assertions see `/status` and `/body`.

`--pipeline` enables multiple jobs. Numeric version order is the default. A managed target session must receive events in historical order; incompatible version ordering is rejected rather than leaking later case knowledge. Use `--order time` when chronological job ordering is necessary. Job event streams must not overlap; related cross-ticket operations should be expressed as one chronological stream, as in the lifecycle example.

## Runtime and fix assertions

`runtime` defines a separate driver command. It speaks JSON lines over stdin/stdout:

- `prepare`: immutable commit, exported source path, file hashes and profile; returns `ready`, pinned runtime identity and source/input delta counts.
- `observe`: the same registered command and deadline; returns outcome fields such as `exit_code`, `stdout`, `stderr`, `log`.
- `reset`: clears task processes and mutable state while retaining valid inputs/builds; its `reset: true` acknowledgement is required.
- `close`: cleans owned resources and acknowledges cleanup.

The bundled `runtime_driver.py` supports `metadata-disconnect` and an operator `command` profile in a pinned Java 8 Docker runtime. The metadata fixture compiles unmodified historical clients and injects a disconnected response with the historical shipped `MockClient`. It is a real client-code replay, not a cluster/topology replay. It exposes observations without coding the expected repaired exception into the fixture. Future assertions remain in the evaluator, not the initial system input.

There are no host mounts, host credentials, Docker socket mounts or container network access in the bundled runtime. Source, dependencies and baseline classes are read-only; diagnostic state resets between observations. Different products/JDKs/topologies need another driver/profile. Arbitrary historical Kafka server Git revisions are not verified by substituting a released server binary.

## Independent semantic evaluation

An optional `evaluator` config supplies `url`, optional `token_env`, and a public `request`, typically:

```json
{"url":"http://127.0.0.1:19000","request":{"method":"POST","path":"/evaluate","body":"{{evaluation_packet}}"}}
```

The service returns `findings` with each zero-based `index`, `localization`, `problem`, `solution` (`correct`, `incorrect`, `unknown`) and explanation, plus evidence (`source`, exact `reference`, exact line substring `quote`). Supplied documents cover original source, PR message/patch and runtime logs. Full finding coverage and citations are validated. Without an evaluator, semantic accuracy remains `unassessed`, even if a behavior change and patch overlap were verified.

The tester does not choose or instantiate a model; engineering invocation is owned by the system, semantic invocation by the separately configured evaluator. This keeps the tester independent of ESA's provider implementation.

## Isolation, cleanup and limits

The included ESA examples bind port 18090 and write a new state file below each run's output. They disable live Redmine sync. They make no model calls by default: the lifecycle example tests actual business transitions, while the Kafka example expects actual `awaiting_configuration` after the asynchronous worker runs. To test an engineering Agent, configure the target's own provider and environment-preparation capability, and change assertions to expect its actual completed report. The tester does not bypass this by calling a provider itself.

Managed process isolation is process-group ownership and an independent state path, not an OS filesystem sandbox. Target configuration determines stronger isolation. SIGTERM/Ctrl-C stop owned process groups and close the owned runtime; a failed cleanup is reported. A crash/SIGKILL can prevent cleanup. Recover bundled runtime sessions using `SYSTEM_REPLAY_SESSIONS=<run>/sessions python3 runtime_driver.py --cleanup`; live sessions and foreign ownership are protected. Attached systems are never stopped or reset.

Credentials are passed through environment/header configuration and omitted from recorded request headers; the service token is redacted from responses and process logs. Reports and verified caches are retained. Source repositories and input files are read-only inputs.

Token thresholds are advisory and configured in the test manifest, independently of application rules. Measured usage is aggregated per job and deduplicated by observed run ID when available; missing usage remains unknown. Thresholds print warnings and do not terminate the target or an Agent. An explicit target timeout is a test deadline, not a token limit. Aggregate counts are not a price estimate.

## Tests

```sh
go test -race ./...
go vet ./...
go list -deps ./...
```

Tests cover public submission/polling, withholding assertion labels, refusing existing targets, historical order, unchanged failure/pass scenarios, malformed evidence, source/history boundaries, mainline merges/renames and owned-process cleanup. HTTP tests need permission to bind local test ports. The examples additionally verify real ESA business transitions and real Kafka client behavior; their lack of model calls must not be interpreted as an Agent accuracy result.
