# External dependencies

This repository contains ESA application code, its Redmine plugin, replay orchestration, tests, synthetic examples and deployment definitions. Third-party product source and binaries are obtained from their upstream projects; they are not vendored into the repository.

| Dependency | Reference | Use |
| --- | --- | --- |
| Redmine | Official `redmine:7.0.2-bookworm` Docker image | Base application; the Dockerfile adds only the ESA plugin |
| PostgreSQL | Official `postgres:17-alpine` Docker image | Redmine database and ESA's own state database ([storage design](STORAGE_DESIGN.md)); tests use a disposable server from `scripts/test_postgres.sh` |
| Eclipse Temurin | Official JDK 8 replay image; digest-pinned JDK 8/21 bases for the preset toolkit | Product JVM and separate analysis JVM; see [tool image operations](TOOL_IMAGES.md) |
| Soot, SootUp, async-profiler and OS tools | Approved versions/checksums in `deploy/tools/toolset.release.txt` | Shared engineering toolkit, built by `scripts/update_tool_images.py` |
| Apache Kafka | `https://github.com/apache/kafka.git` | External historical source checkout for replay |
| Historical Kafka binaries | Apache archive URLs used by legacy application runtime utilities | Optional application-side setup; not used by the standalone client replay |
| Historical client dependencies | Maven Central coordinates from historical Kafka `build.gradle` | Client replay compilation; downloaded with checksum validation |
| Go | Go 1.27+ | ESA backend and local commands; standard library plus the module below |
| pgx | `github.com/jackc/pgx/v5` v5.11.0, pinned by `go.sum` (with its small runtime modules `jackc/pgpassfile`, `jackc/pgservicefile`, `golang.org/x/text`) | Postgres driver for ESA's state store; pure Go, so the static `CGO_ENABLED=0` build is unchanged. Not vendored; `check_workflow.sh` runs `go mod verify` |
| Python | Python 3.9+ | Replay/download/verification scripts; standard library only |
| Codex CLI | Separately installed and authenticated CLI | Optional diagnosis provider; credentials are never included |
| Doxygen | Separately installed; validated with 1.18.0 | Optional code-reference generation; see [documentation build](DOXYGEN.md) |

Redmine core source is referenced through its upstream Docker image instead of copied or tracked as a Git submodule. Its source is available through the [Redmine download page](https://www.redmine.org/projects/redmine/wiki/Download) and the linked [GitHub mirror](https://github.com/redmine/redmine); the [official Docker image](https://hub.docker.com/_/redmine) supplies the deployed application. ESA does not patch Redmine core. Only `plugins/redmine_esa` is maintained here.

Fetch Kafka separately when running historical replay:

```sh
git clone https://github.com/apache/kafka.git kafka
```

The `kafka/` directory is ignored by Git. Jira exports in `data/`, runtime evidence in `.esa/`, reports, local tools, compiled binaries, credentials, environment secrets and generated deployment records are also ignored. Synthetic examples under `examples/` remain tracked.

## Fresh checkout

1. Install Go 1.27+, Python 3.9+, Docker and Docker Compose independently.
2. Build with `./scripts/build_esa.sh`; it uses the project-local toolchain when present and otherwise uses `go` from PATH.
3. Copy `deploy/redmine/.env.example` to `deploy/redmine/.env`. Fill its database passwords (`REDMINE_DB_PASSWORD`, and `ESA_DB_PASSWORD` in hex), session secret and ESA token with unique random values; keep the generated file private.
4. Pre-create the shared Issue directory, and configure its ownership; follow [local deployment](../deploy/redmine/README.md). Outside this Mac, replace `colima-redmine` with the appropriate Docker context and set `ESA_GO_ARCH` as needed.
5. Configure Redmine users, project modules and ESA roles. Local administrator bootstrap scripts read the password from an environment variable; no credentials are shipped.

The standalone tester is built separately with `sh scripts/build_system_replay.sh`; its module has no ESA imports or third-party Go/Python packages. The lifecycle manifest requires a separately built host ESA binary but no Jira archive or Kafka checkout. The Kafka manifest additionally requires the external source checkout, preserved Jira export and Docker runtime. These are separate inputs, not repository contents. Live Codex analysis requires local CLI authentication, a private provider token file and the shared Issue directory. Source analysis requires a time-eligible local release registry; managed runtime analysis also requires a verified preset tool image. Prior evidence bundles are optional registered scenarios. A fresh checkout does not include the local release registry, product dependencies or generated image registry. Ruby and Node.js are needed only for the host UI unit tests; container template checks use Ruby from the Redmine image. Third-party images and archives retain their upstream licensing and notices.
