# System replay operations

The pipeline runs the independent [external system replay tester](TICKET_BACKTEST.md). The system under test remains responsible for ticket business logic and engineering invocation. The tester requires a public-interface manifest and an opaque target executable or explicitly attached service.

## Build and run

From the repository root, build the provided example target separately:

```sh
GOCACHE="$PWD/.tools/go-cache" GOTOOLCHAIN=local .tools/go/bin/go build -o bin/esa-host ./cmd/esa
sh scripts/build_system_replay.sh
python3 scripts/replay_pipeline.py --pipeline --manifest system_replay/examples/esa-api-lifecycle.json --output reports/new-lifecycle-run
python3 scripts/replay_pipeline.py --pipeline --manifest system_replay/examples/esa-api-kafka.json --output reports/new-kafka-run
python3 scripts/replay_pipeline.py --pipeline --manifest system_replay/examples/esa-api-forwarding.json --output reports/new-forwarding-run
```

Use a new output directory for every invocation. The lifecycle example needs the host target artifact. The Kafka example also needs the ignored external `kafka/` checkout, `data/jira/raw/2015/11/24/KAFKA-2880.json`, Docker and verified client dependencies. Manifests resolve paths relative to their own directory. The bundled Docker context defaults to `colima-redmine`; configure the runtime command for another context or verified cache.

The Python compatibility wrapper builds only the tester if its binary is absent or `--build-tools` is given, then replaces itself with the executable. It does not build the target or call an engineering provider. After tester source changes, use `--build-tools` or rebuild explicitly to avoid running an older binary. After building, `bin/system-replay --help` shows native options; the Python wrapper requires `--manifest` even for help and handles `--build-tools` itself.

To use the module independently:

```sh
cd system_replay
go test -race ./...
go vet ./...
go build -o /tmp/system-replay ./cmd/system-replay
/tmp/system-replay --manifest /absolute/path/test.json --output /absolute/path/new-report --pipeline
```

Local HTTP tests need permission to bind temporary ports. The module uses the Go standard library and Python standard library; root `go test ./...` does not include this separate module.

## Command-line options

| Option | Behavior |
| --- | --- |
| `--manifest PATH` | Required public target, jobs, assertions and optional runtime/evaluator configuration |
| `--output PATH` | Required new report directory; existing directories are rejected |
| `--pipeline` | Enable multiple jobs and shared runtime/history reads |
| `--order version` | Default numeric version ordering; reject historical event inversion |
| `--order time` | Sort initial jobs by historical observation time; overlapping streams are still rejected |
| `--plan` | Validate inputs/source selection without starting target, runtime or model |
| `--track-only` | Track local Git without target or model calls; explicitly not a system replay test |
| `--attach` | Use an existing healthy target; never clear its state or stop it |
| `--score PATH --labels PATH --repo PATH --output PATH` | Score a finished run's `pipeline.json` against evaluation labels, using the product checkout; writes `report.json` and `report.md` into the new output directory. No target, runtime or model calls ([Agent evaluation](TICKET_BACKTEST.md#agent-evaluation)) |
| `--build-tools` | Python wrapper only: rebuild the standalone tester before execution |

The former positional-ticket, `--agent`, `--predictions` and `--issue-history` shortcuts are removed. `scripts/replay_trial.py prepare` and registered replay MCP tools remain application-provider setup utilities. Their retained reports are historical evidence, not input to the external tester's prediction evaluation.

## Pipeline ordering and reuse

Initial jobs receive version ordering only when all their public events remain chronological. Related cross-ticket operations can share one job stream, but future repair evaluation always uses that job's first public observation. All initial outputs freeze before future Git evidence is read. Successful history queries are cached within the run and the resolved mainline head is reused across jobs.

The product runtime remains alive between observations. Source/dependency transfers are differences; unchanged inputs and valid compiled artifacts are retained. Changes invalidate affected build inputs, mutable diagnostic state resets after every observation, and candidate parent/fix snapshots are prepared in global mainline order. A failed or unacknowledged reset fails verification.

The bundled driver exposes `prepare`, `observe`, `reset` and `close` over JSON lines. Its `metadata-disconnect` profile is a registered Kafka client scenario. An operator `command` profile requires its own executable scenario and controls. Readiness alone is not accepted as a defect replay. Other JDKs, products or cluster topologies require another driver/profile; see [runtime protocol](../system_replay/README.md).

## Reports and exit status

| Artifact below the output directory | Content |
| --- | --- |
| `pipeline.json` | Final phase, per-job results, cleanup, limitations and any run error |
| `jobs/<id>/plan.json` | Historical ticket/source selection and provenance limits |
| `jobs/<id>/historical-context.json` | Historical-only context available to the target's environment adapter |
| `jobs/<id>/event-NNN.json` | Public receipt, observed response, assertion checks and event error |
| `jobs/<id>/system-output.json` | Read-only frozen event results, pinned by `prediction_sha256` |
| `jobs/<id>/baseline.json` | Successful registered historical runtime observation |
| `jobs/<id>/trace.json` | Mainline/version graph and repair candidates |
| `jobs/<id>/runtime/<commit>.json` | Candidate runtime observations |
| `jobs/<id>/result.json` | Final job state, repair gates, metrics, assessments and advisory alerts |
| `snapshots/<commit>/` | Read-only Git exports and file hash manifests |
| `target.log`, `runtime-driver.log` | Owned-process diagnostics |

Successful job states distinguish `tracking_only`, `system_observed_replay_unregistered`, `reproduced` and `repair_behavior_verified`. Missing scenarios do not imply fault reproduction. Invalid inputs, public assertion failures, runtime/control failures, baseline nonreproduction, required evaluator failures, report-write failures and cleanup failures return nonzero. Candidate states such as `parent_not_reproduced` or `not_fixed` are retained without automatically declaring the initial diagnosis incorrect.

Reports, caches and exports remain local and ignored by Git. The tester checks frozen public output and source hashes before reading future evidence. It does not modify the original source checkout or Jira inputs.

## Cleanup and usage alerts

Normal exit, Ctrl-C and SIGTERM close the owned runtime and stop owned target process groups. Attached targets are never stopped. Cleanup failure is recorded and fails the run. A host crash or SIGKILL may leave resources; recover the bundled driver's stale, owned sessions with:

```sh
SYSTEM_REPLAY_SESSIONS=/absolute/path/run/sessions python3 system_replay/runtime_driver.py --cleanup
```

The cleanup utility protects live sessions and resources with different ownership labels. Verified caches and reports are retained. The Docker runtime has disabled networking, read-only inputs and no host, credential or Docker socket mounts. The managed target process has independent state and process-group ownership; stronger filesystem isolation requires target configuration.

`warn_input_tokens`, `warn_output_tokens` and `warn_total_tokens` are advisory per-job thresholds, independent of application rules. The tester sums reported raw input/output counts and does not apply ESA's cached-input weight of 0.1; its totals are not billing estimates. Public run IDs deduplicate measured usage; absent usage is unknown. Warnings appear on stderr and in result alerts and never terminate the target or Agent. `timeout_seconds` is a separate test deadline. Token usage and semantic evaluation require actual configured providers; the default examples make no model calls.


Current ESA intake requires explicit `forwarded_by` and `forwarded_at`. External fixtures declare the manual action; the tester does not infer it. ESA has no category-based intake gate. Its optional supervised label classifier is disabled by default and does not control routing. See [manual forwarding](MANUAL_FORWARDING.md).
