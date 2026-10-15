# Historical fixed-packet Codex trial

This document describes the specialized KAFKA-2880 fixture and its retained experiments. It is not the normal provider setup or the external tester. Use [ESA operations](ESA_OPERATIONS.md#source-registry-profiles-and-tools) for registry-based diagnosis and [external system replay](TICKET_BACKTEST.md) for independent evaluation.

## Selected issue and scope

The selected public Bug is **KAFKA-2880**, reported on **2015-11-24T00:13:27.387Z**: a consumer metadata lookup throws a NullPointerException when the broker cannot be reached. The affected version is **0.9.0.0**. The snapshot uses commit `fc7243c2af4b2b4affcdf14d80826986fe6d4ce8`, authored and committed before the cutoff.

`cmd/esa-replay` reconstructs available historical diagnostic fields, excludes later comments and unavailable earlier edited bodies, clears resolution evidence, and exports source with Git archive. The export has no `.git`, future commits, fix versions, issue links, attachments or post-cutoff discussion. It retains raw/source hashes. Without an independently captured initial description, reconstruction depends on complete changelog metadata and available old values; ambiguous histories fail instead of inventing values.

`scripts/replay_trial.py prepare` compiles historical clients plus the shipped MockClient in Java 8. A successful metadata response is the control; a disconnected ClientResponse with an absent body is the failure scenario. It captures runtime logs and selected source windows in `evidence.json`. Those windows and the reproducer are supplied by the operator, not discovered by the Agent. This is a deterministic client request replay, not a complete broker cluster or Kafka Connect deployment.

## Current provider versus this fixture

The normal `-registry` provider resolves time-eligible source per project/release and can provision a generic workspace without prior evidence. It does not require this ticket. An optional registry `packet_path` selects the specialized fixture only when commit, cutoff and diagnostic fingerprint match; otherwise a known release uses the generic source path. Unknown/ambiguous releases receive context-only analysis.

The alternative `-evidence` mode registers one immutable packet and checks its ticket fingerprint. `scripts/manage_codex_provider.py` manages only its hardcoded invocation for `.esa/replays/KAFKA-2880/trial-1/evidence.json`. It is not a general provider supervisor and does not control registry-based processes. Do not start it on a port already occupied by another provider.

Current workflow constraints also apply to this mode: manual forwarding, initial Routing Agent retrieval, engineering diagnosis when needed, and Routing Agent grouping after supported findings. Engineering chooses inspected evidence IDs; the provider derives concrete source coordinates and quotes. The validator establishes citation provenance, not root cause or repair correctness. Historical reports retain the schema, tool availability and conditions of their original runs.

The adapter uses the authenticated local Codex CLI with native host tools and inherited user MCPs/plugins/hooks disabled. Routing receives only Issue-directory tools; engineering receives approved source, capability, runtime, pinned-tool installation and environment-request tools. The managed runtime is offline, nonroot and resource bounded, with frozen inputs and writable scratch. It can rerun the registered fixture; that is distinct from constructing an independent reproducer. See [tool environments](TOOL_IMAGES.md).

## Prepare and run the fixed fixture

Run from the repository root with the external Kafka checkout, preserved Jira export, Docker context `colima-redmine` and authenticated CLI available. Build using `./scripts/build_esa.sh`. A private `deploy/redmine/.env` must contain the matching ESA service token. The bundle output directory must not already exist:

```sh
bin/esa-replay -raw data/jira/raw/2015/11/24/KAFKA-2880.json -repo kafka -ref 0.9.0.0 \
  -at 2015-11-24T00:13:27.387Z -out .esa/replays/KAFKA-2880/trial-1 \
  -environment '{"reported_scenario":"Kafka Connect distributed startup with no broker running, as stated in the issue description"}'
docker --context colima-redmine pull eclipse-temurin:8-jdk
python3 scripts/replay_trial.py prepare
python3 scripts/update_tool_images.py --context colima-redmine
python3 scripts/manage_codex_provider.py start
```

Configure ESA's `ESA_ANALYZER_URL` to this adapter's `/analyze` endpoint, using an address reachable from the service (for the local Colima setup, `http://host.lima.internal:8091/analyze`). Both processes must share the [Issue projection directory](ISSUE_DIRECTORY.md#local-service-and-compose). Preparing a bundle or starting a provider does not forward a Redmine issue. Stop this specific fixed-packet adapter with `python3 scripts/manage_codex_provider.py stop`; its process is not installed as a system service.

Private runs default to `.esa/codex-runs/<run-id>` and preserve input, prompt, schema, CLI events, usage, tool audit, errors and reports. Completed stage requests can return cached reports; failed/uncertain attempts require explicit new forwarding rather than an automatic model retry. Usage may arrive only at CLI completion; missing usage stays unknown. [Weighted token warnings](ESA_OPERATIONS.md#weighted-token-alerts) are advisory; invocation limits and the overall deadline are separate.

## Independent evaluation

The former imported-report evaluator is removed. `scripts/replay_trial.py evaluate` is unsupported. The command below starts a **new** public-API system replay; it does not evaluate a diagnosis stored by this fixed-packet trial:

```sh
sh scripts/build_system_replay.sh
python3 scripts/replay_pipeline.py --pipeline --manifest system_replay/examples/esa-api-kafka.json --output reports/new-system-run
```

Its default manifest disables the provider, expects `awaiting_configuration` after manual forwarding, and independently checks baseline/parent/fix behavior with a neutral fixture. It proves the registered behavior change, not Agent accuracy. Testing model judgments requires a configured target provider and independent assertions/evaluation.

A stack-rich ticket with selected code/log evidence cannot establish archive-wide localization accuracy. A generic workspace trial must separately demonstrate its own compilation and observations. Historical input isolation cannot remove knowledge a model learned during training. Universal historical builds, attachment replay, unavailable comment-body recovery and external release monitoring remain outside current coverage.
