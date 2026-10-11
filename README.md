# ESA

ESA connects customer support and engineers through a Redmine plugin and an independent Go service. Development language is English. Engineering diagnosis is read-only: deliver root-cause candidates, evidence, repair recommendations, and verification steps.

This repository contains ESA code and deployment definitions. Redmine is referenced through the official `redmine:7.0.2-bookworm` Docker image; only the ESA plugin is maintained here. See [external dependencies and fresh-checkout setup](docs/DEPENDENCIES.md). Preserved Jira data, Kafka source, credentials, local tools, compiled artifacts and runtime reports are excluded from Git.

## Current implementation

- A native Redmine **ESA** menu, ticket list, filters, sorting, pagination, ticket details, and review controls.
- UTF-8 Jira JSON, ESA JSON, and Jira CSV upload with preview, validation, preserved original input, and source-key deduplication.
- Durable Redmine event outbox, acknowledgement after backend commit, idempotent ingestion, and stale-event handling.
- Deterministic classification and routing. Only configured engineering categories may enter engineering analysis.
- Project-scoped case retrieval with component, affected-version, environment, and symptom checks.
- Collection of all closed tickets as case drafts; duplicates and rejected resolutions have separate states. Engineer approval requires cause, solution, validation, component, version, and category.
- Automatic applicable-case recommendations with zero model calls. Reopening, changing, rejecting, or making a source ticket private withdraws its trusted evidence.
- A bounded asynchronous HTTP engineering-provider interface, explicit unconfigured/failed states, evidence validation, and human report review.
- Tests in `replay_test` establish temporal visibility of case revisions and reconstruct conservative Jira snapshots without future comments or diagnostic field values.
- A Go Codex CLI adapter accepts an explicitly registered historical evidence packet, validates exact source quotes, records actual usage, and returns engineering reports for human review.
- A deterministic KAFKA-2880 client replay compiles historical source in an isolated Java 8 container; its later upstream fix is evaluated separately after diagnosis.
- A CLI replay pipeline resolves historical affected versions, sorts batches, reuses one isolated environment, synchronizes file differences and cleans owned runtime resources.
- Registered MCP tools support installation, system/debugger execution, frozen source inspection and environment change requests. Source, dependencies and baseline bytecode remain read-only.
- Per-ticket token warnings appear in provider logs, audit files, batch results and ESA reports without cancelling analysis.

## Run

The existing local Redmine deployment is at http://127.0.0.1:8080/. The **ESA Demo** project contains synthetic validation data and one frozen real KAFKA-2880 replay ticket. Enable the **ESA** project module for other projects and grant View ESA, Submit ESA, and Review ESA permissions to the appropriate roles. Uploading also requires Add issues permission.

Build using Go 1.27 or the verified project-local toolchain:

```sh
./scripts/build_esa.sh
DOCKER_CONTEXT=colima-redmine docker-compose -f deploy/redmine/compose.yaml build redmine esa
DOCKER_CONTEXT=colima-redmine docker-compose -f deploy/redmine/compose.yaml up -d
```

The build defaults to Linux ARM64 for this machine. Set `ESA_GO_ARCH=amd64` for an AMD64 deployment. No external Go modules or Python packages are required.

For direct development, configure a service token and use a separate store:

```sh
export ESA_API_TOKEN='<at least 24 random characters>'
.tools/go/bin/go run ./cmd/esa -listen 127.0.0.1:8090 -store runtime/dev/state.json
```

The service token in `deploy/redmine/.env` is private. The browser never receives it. The plugin derives project and issue visibility from the current Redmine user. The Go service is reachable only inside the Compose network.

## Tests

```sh
GOCACHE="$PWD/.tools/go-cache" GOTOOLCHAIN=local .tools/go/bin/go test -race ./...
GOCACHE="$PWD/.tools/go-cache" GOTOOLCHAIN=local .tools/go/bin/go vet ./...
```

Use `examples/resolved-case.json` to import a resolved synthetic issue. Open its ESA detail and approve the case after reviewing its evidence. Upload `examples/new-issues.csv`: the matching engineering issue receives the trusted recommendation, while the usage question follows customer support. Reopen the source issue to withdraw the recommendation.

## Engineering provider

Without `ESA_ANALYZER_URL`, engineering issues become **Awaiting configuration**. The local trial connects the Go Codex adapter for one explicitly registered ticket/evidence packet. Other tickets are rejected before invoking a model. See [the Codex replay trial](docs/CODEX_REPLAY_TRIAL.md) for startup, isolation, audits and evaluation. The general HTTP contract remains in [the workflow document](docs/ESA_BACKEND_WORKFLOW.md).

The Codex adapter disables native host tools and exposes only four registered replay tools. It checks exact evidence quotes against frozen source or audited runtime output. Token thresholds are advisory; the separate invocation limit and execution deadline still apply. CLI usage may arrive only when the turn finishes; missing usage is unknown. The service cannot independently restrict tools inside an arbitrary remote HTTP provider. See [replay pipeline operations](docs/REPLAY_PIPELINE.md) for profiles, batch commands, permissions and cleanup.

## Boundaries of this first version

This is a working development foundation with one focused historical replay trial. Automatic customer environment inference, universal historical build profiles, document retrieval, recovery of unavailable historical comment bodies, archive-wide ground-truth scoring, release listening, generalized fixed-version verification and a rules administration page remain to be implemented. Replay profiles explicitly separate environment readiness from reproduction of a customer issue. Importing a current Jira export does not make it a valid historical snapshot.

State is stored as an atomic JSON snapshot with a single-writer process lock. It preserves immutable events, run inputs, and case revisions; it is intended for this local prototype. The plugin limits access to 2,000 visible ESA issues per request. Move state to a transactional database and implement scalable authorization before importing the full Jira archive. Cross-project issue moves, deletions, bulk-edit hooks, REST-created issues, and retroactive ingestion need a dedicated reconciliation mechanism; use the manual submit action for existing visible issues. Private issues are excluded from shared analysis and case recommendations.

Original `data/` and `kafka/` remain untouched. Old audit reports retain their original language.
