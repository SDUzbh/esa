# Historical replay pipeline

ESA business logic stays in Go. Python standard-library scripts orchestrate local Git exports, verified artifact downloads and Docker lifecycle; no system packages are installed. Product files in `kafka/` and preserved Jira files in `data/` are never modified. Diagnosis returns recommendations and verification steps, with engineer review required.

## Commands

Build project-local commands when implementation changes, then resolve inputs without Docker or model calls:

```sh
python3 scripts/replay_pipeline.py --build-tools --pipeline --plan KAFKA-3258 KAFKA-2880 KAFKA-2875
```

The planner reconstructs the ticket at its creation time, requires complete count-checked history and selects exactly one historical affected version. It resolves the matching local Git tag and rejects source commits authored or committed after the cutoff. Missing, ambiguous or future versions become `needs_clarification`; it does not guess the oldest tag. Supply `--ref`, `--at` and, for a development commit, a numeric `--version` ordering label when the actual environment is independently known. Version release metadata from the current Jira export is not treated as historical evidence.

Run one registered bug fixture with Codex:

```sh
python3 scripts/replay_pipeline.py --profile metadata-disconnect --agent KAFKA-2880
```

Run a batch in numeric version order with one environment:

```sh
python3 scripts/replay_pipeline.py --pipeline --profile source KAFKA-3258 KAFKA-2880 KAFKA-2875
```

Use `--manifest jobs.json` for a JSON list of jobs. Each job needs `raw`; optional fields include `profile`, `ref`, `at`, `version`, `environment`, `project`, `scala_version`, `command` and `command_timeout`. `command` is a trusted operator override inside the owned container, not ticket text. Relative raw paths resolve against the project root. Multiple jobs require `--pipeline`. `--output` must name a new directory. `--agent` is explicit; planning and baseline environment tests make no model calls.

## Profiles and interpretation

| Profile | Automated preparation | What it establishes |
| --- | --- | --- |
| `source` | Frozen full source plus pinned Java 8 JDK | Environment readiness, without a customer bug reproduction claim |
| `metadata-disconnect` | Historical client source, dependency versions from historical `build.gradle`, unchanged Java harness, baseline compilation | Registered metadata request replay for Kafka 0.9.0.0 / 0.9.0.1; KAFKA-2880 is the matched issue |
| `kafka-server` | Apache historical binary, official checksum, full historical source, local ZooKeeper and broker startup, control topic creation | Server readiness; source compilation and reproduction of a customer bug are not claimed |

The server profile defaults to Scala 2.11. These are explicit Java 8 profiles, verified with Kafka 0.9.0.0; they do not infer a customer's OS, JVM, topology or configuration, and do not promise compatibility across every historical version. Add a reviewed profile for other runtime requirements. The network is disabled inside replay containers; operator-controlled downloads happen before input synchronization. Server ports remain inside the container. Product binaries are installed without agent compilation. The metadata profile compiles once because it exercises historical clients and the deterministic harness.

## Reuse and cleanup

Source exports are cached by commit and scope. Downloads are validated against official checksums and cached content hashes. The initial runtime image is pinned to its repository digest. One container stays alive across a pipeline; source and runtime inputs synchronize by SHA-256 differences. An identical version requires no transfers or recompilation. Changing source or dependencies invalidates baseline bytecode. Successful baseline classes are published in the read-only input volume.

Between tickets, the runner stops ticket-created processes and clears mutable scripts, logs and temporary state. Product inputs and valid baseline bytecode remain available; customer state is not inherited between tickets. A version transition reuses the runtime image and applies file differences, while incompatible profiles must be explicitly registered.

Normal exit, exceptions, Ctrl-C and SIGTERM clean the two owned containers, input volume and temporary source directory. Only resources with the exact session ownership label can be removed. Verified caches and reports remain for reuse and audit. SIGKILL, a power loss or a Docker outage can prevent immediate cleanup; recover stale sessions with:

```sh
python3 scripts/replay_pipeline.py --cleanup
```

Live sessions are skipped. This command does not prune unrelated containers, volumes or images. A failed cleanup retains its session journal for recovery.

## Agent capabilities

Native host shell, file editing, browsing, plugins and other user MCPs are disabled. The installed CLI's Code Mode dispatcher is enabled only to invoke registered MCP tools; it does not add a native host command tool. Agent input has no `.git` directory, future code, resolution fields or later discussion.

| Tool | Allowed operation |
| --- | --- |
| `read_code` | Read 1–200 lines from a registered historical file, validate its SHA-256 and return a commit/path/line citation |
| `install_environment` | Run the operator-supplied installer inside the owned container |
| `run` | Run the system, JDK debug tools or auxiliary scripts in the owned container; writable state is confined to `/workspace` and `/tmp` |
| `request_environment_change` | Record reason, proposed change and validation as `requested` for engineer review; apply nothing |

Product source, dependencies, product binaries and baseline classes are mounted read-only. The container has no host credentials, Docker socket or host filesystem mounts. Commands have bounded time, output, process count, memory and CPU. The runtime still permits diagnostic scripts and helper compilation; instructions prohibit copying/patching or shadowing product implementations with helpers. The mount restriction protects authoritative product files; it is not a semantic proof that every arbitrary auxiliary program is harmless to a diagnosis. Engineers review the tool audit and evidence before accepting conclusions. Environment change requests appear on the ESA report page; automatic approval or application is not implemented.

## Advisory token alerts

`config/rules.json` version 3 defines input 16,000, output 6,000 and total 22,000 token warning thresholds. Existing `max_input_tokens` and `max_output_tokens` field names are retained for contract compatibility but now mean warning thresholds. `warn_total_tokens` adds a combined warning. Adjust these fields and restart the services to tune the policy.

Crossing a threshold emits a warning and continues analysis. Alerts are saved in `alerts.jsonl`, `audit.json`, the report and batch result; the batch prints them to stderr and ESA displays them on ticket reports. `provider.log` is written while the provider runs. The CLI may report aggregate usage only at turn completion, so a threshold alert can arrive after diagnosis has finished. Cached input is included in the CLI input count; these counts are not a price estimate. Missing usage is unknown rather than zero. Invocation limits, explicit execution deadlines and invalid-evidence rejection remain separate from token alerts.

## Validation

```sh
GOCACHE="$PWD/.tools/go-cache" GOTOOLCHAIN=local .tools/go/bin/go test -race ./...
GOCACHE="$PWD/.tools/go-cache" GOTOOLCHAIN=local .tools/go/bin/go vet ./...
python3 scripts/verify_replay_runtime.py
```

The runtime check uses real containers to verify all four MCP tools, denied source/bytecode writes, allowed helpers, JDK debugger availability, process/state reset, no-op version reuse, normal cleanup, SIGTERM cleanup and stale recovery after SIGKILL. It makes no model calls. Reports retain the limitations and baseline scope for each job. The later upstream fix remains outside the diagnostic environment and is evaluated only after freezing the report.

The CLI integration uses the [official MCP configuration](https://learn.chatgpt.com/docs/extend/mcp?surface=cli) and [non-interactive execution](https://learn.chatgpt.com/docs/non-interactive-mode) contracts. Deployment still registers one immutable packet for live Redmine diagnosis; batch mode creates a separate packet and audit per ticket. It does not automatically register every uploaded issue with the live adapter.
