# Deterministic workflow checks

`.github/workflows/ci.yml` runs on pull requests, pushes to `main`, and manual
dispatch. It invokes one local entry point instead of implementing a second
ticket-processing workflow:

```sh
sh scripts/check_workflow.sh
```

Requirements are Go matching `go.mod`, a C compiler for the race detector,
Python 3.9+, Ruby with Minitest, Node.js 18+, and Git. The Go toolchain and standard
language libraries are sufficient; Docker, Redmine, Jira data, Kafka source,
Agent authentication and API credentials are not required.

The entry point reuses:

| Existing implementation | Checks |
| --- | --- |
| Root Go tests and `go vet` | Manual forwarding, native Routing/Engineering/assignment orchestration through fake CLIs, Issue reuse and membership, version handoff gates, permissions, usage, concurrency and cleanup contracts |
| `scripts/build_esa.sh` | Root tests and service/provider/classifier builds |
| `scripts/build_system_replay.sh` | Separate tester module tests and build |
| `scripts/test_*.py` | Dataset/downloader and tool-image contracts with mocked network/Docker responses |
| Existing Ruby and Node.js tests | Metadata snapshots, presentation, live status and metadata filling |
| `scripts/verify_shared_investigations.py` | Concurrent public API requests and a local deterministic HTTP provider |
| Three existing replay manifests | Lifecycle, manual forwarding and shared investigations against owned ESA processes |

Go tests run with race detection through the existing build scripts' optional
`ESA_TEST_RACE=1` switch. Both modules are formatted and vetted. Local checks use
a new `.tools/workflow-checks-*` directory unless `ESA_WORKFLOW_OUTPUT` names a
new output directory. `ESA_GO_BIN`, `ESA_GO_ARCH` and `ESA_NODE_BIN` select the
existing toolchain and deployment architecture; normal build-script defaults
are unchanged. The replay cases run sequentially because they share port 18090;
they do not attach to a running deployment.

## No real Agent calls

The check entry point clears inherited analyzer, Redmine and state service URLs
and API key variables. It shadows the default `codex` command with a failing
guard and fails if that guard was invoked. Native adapter tests use explicitly
supplied fake CLI executables. The shared-investigation target installs its own
localhost fixture provider; other selected manifests disable analysis.
GitHub-hosted jobs receive no application or Agent secrets and install no Codex
CLI. Synthetic `usage.calls` and token values describe fixture interactions,
not model usage or credit consumption.

These checks establish workflow behavior, not model reasoning accuracy, semantic
diagnosis correctness, actual sandbox isolation, or live Redmine browser behavior.
The pipeline does not run the Kafka runtime manifest, live `analyze_ticket.py`,
real provider acceptance, Docker tool verification, or software release updates.
Doxygen remains available separately through `scripts/build_docs.sh`.

## Build candidates and deployment

Every run retains synthetic replay reports for seven days. Successful `main`
runs also retain binaries identified by the checked commit SHA. Hosted CI builds
the service for Linux AMD64; provider and CLI binaries match the Linux runner.
GitHub artifact downloads may lose executable bits; restore them before use.
The artifact is a build candidate, not a deployed application.

There is no registry publishing, automatic deployment, local provider restart,
database migration, or real Agent smoke test. Deployment continues through the
existing documented Compose and provider operations. Configure the `workflow`
job as a required branch check in GitHub when enabling merge protection.
