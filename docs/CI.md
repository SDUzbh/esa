# Deterministic workflow checks

`.github/workflows/ci.yml` runs on pull requests, pushes to `main`, and manual
dispatch. It invokes one local entry point instead of implementing a second
ticket-processing workflow:

```sh
sh scripts/check_workflow.sh
```

Requirements are Go matching `go.mod`, a C compiler for the race detector,
Python 3.9+, Ruby with Minitest, Node.js 18+, and Git. Go modules are
downloaded through the module proxy and checked with `go mod verify`. Docker,
Redmine, Jira data, Kafka source, Agent authentication and API credentials are
not required; a disposable Postgres is (below).

ESA keeps its state only in Postgres, so the store-backed tests, the replay
fixtures and `scripts/check_workflow.sh` need `ESA_TEST_DATABASE_URL`; without
it `internal/esa` and the workflow check stop before running anything. CI
provides it through a digest-pinned `postgres:17-alpine` service container.
Every `testEngine`-based test runs on its own schema and reloads and compares
the state after every commit; replay targets run ESA with `-ephemeral-schema`.
Locally, `sh scripts/test_postgres.sh && . .tools/test-postgres/env` starts the
same image in Docker (data in tmpfs, `127.0.0.1:55432`) and loads the URL from
a private file; `sh scripts/test_postgres.sh stop` removes it. Any disposable
Postgres 17 works.

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

## Manual deployment acceptance

`Deployment acceptance` is a separate, manually dispatched workflow. It reuses
the real deployment Dockerfiles and Compose file, with `compose.test.yaml` as a
test-only overlay. No real Agent is invoked and no existing deployment is updated.
The existing fixture provider supports `--provider-only` so both the API replay
and container acceptance share the same deterministic responses.

After building for the Docker daemon's architecture, run locally:

```sh
sh scripts/build_esa.sh
python3 -B scripts/verify_deployment.py --context colima-redmine \
  --output .tools/deployment-checks-local
```

The default build targets Linux ARM64. For an AMD64 Docker daemon use
`ESA_GO_ARCH=amd64 sh scripts/build_esa.sh`. Docker, Compose 2.24.4+ (or a newer
standalone `docker-compose`) and the official image downloads are required.
There are no Python package dependencies. Omit `--context` to use the active
Docker context. The output directory must be new.

The check creates a unique Compose project, loopback port, image tags and named
volumes. Database, ESA and the fixture provider use an internal network without
external connectivity; Redmine also has a frontend bridge for its loopback port.
Containers mount no
host directories, credentials or Docker socket. A private generated env file
replaces the normal deployment `.env`; no production tokens are loaded. The seed
script refuses to initialize an existing project database or run without the
test-only environment marker.

Acceptance uses HTTP cookie sessions and real Rails CSRF tokens for three fixture
users. It creates an issue through Redmine, saves metadata twice, forwards through
the plugin's durable outbox, polls status, retrieves suggestions and the report,
checks role/project boundaries, and publishes a synthetic engineer review.
It then recreates Redmine and ESA with the same volumes and checks retained data,
solution identity, acknowledgement and absence of repeated fixture analysis.
This verifies service integration and rendered HTTP endpoints; JavaScript browser
behavior remains covered by the existing Node.js tests. It is not a historical
schema-upgrade or automatic rollback test.

`artifacts/result.json` lists assertions and cleanup status; `artifacts/compose.log`
contains sanitized logs. Only `artifacts/` is uploaded. Generated passwords and
service tokens remain in the private output directory, which is ignored by Git.
Normal exit, test failure and SIGTERM remove only this project's containers,
networks, volumes and image tags. Official base images remain cached. SIGKILL or a
host crash may prevent cleanup; `private/compose.env` retains the unique project
name even if no final result was written. A cleanup failure fails acceptance.
