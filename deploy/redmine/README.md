# Local Redmine and ESA

Redmine core is an external dependency: the Dockerfile references `redmine:7.0.2-bookworm` and copies only `plugins/redmine_esa`. ESA does not upload or patch Redmine core. See [dependencies and fresh-checkout setup](../../docs/DEPENDENCIES.md).

## Configure and start

Run commands from the repository root. The local deployment uses Docker context `colima-redmine`; select your actual context on another machine. Copy `deploy/redmine/.env.example` to the private `deploy/redmine/.env` and generate unique database, session and ESA secrets before invoking Compose. Local `credentials.md`, when created during bootstrap, contains administrator credentials; it is not shipped. Never commit or print either private file. `bootstrap.rb` initializes administrator credentials and is not a routine migration script.

Pre-create `runtime/esa/issues`. For a host provider, configure `ESA_UID`/`ESA_GID` to the host account's numeric identity and ensure the Issue directory and existing state volume belong to that service identity. Colima must share the host directory. Follow [directory mounts and ownership](../../docs/ISSUE_DIRECTORY.md#local-service-and-compose) before starting; do not erase existing volumes to repair permissions.

```sh
./scripts/build_esa.sh
mkdir -p runtime/esa/issues
DOCKER_CONTEXT=colima-redmine docker-compose -f deploy/redmine/compose.yaml build redmine esa
DOCKER_CONTEXT=colima-redmine docker-compose -f deploy/redmine/compose.yaml up -d
```

Redmine is published only at http://127.0.0.1:8080/. PostgreSQL 17 and the Go service on port 8090 remain inside the Compose network; host requests to port 8090 require a separate host service or an explicit local port override. Linux ARM64 is the default ESA build; set `ESA_GO_ARCH=amd64` for an AMD64 deployment.

Plugin migrations run at Redmine startup. Database and attachments use separate persistent named volumes. ESA keeps its state in its own Postgres database next to Redmine's: set `ESA_DB_PASSWORD` in the private `.env`, and the one-shot `esa-db-init` service creates the role and database before ESA starts. See [Postgres state store](../../docs/ESA_OPERATIONS.md#postgres-state-store). The older `ESA_STATE_URL` adapter through the plugin's state API has been removed; ESA refuses to start while it is set. There is no dual writing or automatic fallback.

`enable_esa.rb` sets English defaults, translates exact legacy default enum labels and creates a private **ESA Demo** project when explicitly run. It does not change passwords or import data. Configure the ESA project module and View ESA, Submit ESA and Review ESA role permissions; importing also requires Add issues.

## Stop, resume and update

```sh
DOCKER_CONTEXT=colima-redmine docker-compose -f deploy/redmine/compose.yaml stop
DOCKER_CONTEXT=colima-redmine docker-compose -f deploy/redmine/compose.yaml start
```

These commands preserve data. To deploy edits, rebuild the relevant images and run `up -d` again; `start` alone does not recreate containers. Plugin changes require the Redmine image, and Go changes require the built binary and ESA image. Rules in `config/rules.json` are copied into the ESA image; rebuild it or mount an explicit configuration override. Stopping Compose does not stop a separately running host provider. Avoid `down -v`, which deletes persistent volumes.

## Routing and engineering provider

Set `ESA_ANALYZER_URL` in `.env` to a reachable provider `/analyze` URL; the local Colima host adapter uses `http://host.lima.internal:8091/analyze`. Compose uses the private ESA token for provider authentication. Open forwarded issues await configuration when this URL is empty, regardless of Tracker.

The normal provider runs from the repository root with a private project/release registry:

```sh
bin/esa-codex -registry runtime/releases.json -profiles profiles \
  -issues-directory runtime/esa/issues -runs runtime/codex \
  -listen 127.0.0.1:8091 -concurrency 3
```

Prepare the registry and verified tool image using [ESA operations](../../docs/ESA_OPERATIONS.md#source-registry-profiles-and-tools). Known releases can use a generic sandbox without a prior evidence packet. Missing source registration permits context-only analysis; it does not grant arbitrary repository downloads. Use Ctrl-C in the provider terminal to stop it and clean owned task resources.

`scripts/manage_codex_provider.py` manages only its hardcoded historical fixed-packet experiment. It neither starts nor stops the registry provider above; see [historical trial](../../docs/CODEX_REPLAY_TRIAL.md) for that separate utility.
