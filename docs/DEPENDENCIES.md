# External dependencies

This repository contains ESA application code, its Redmine plugin, replay orchestration, tests, synthetic examples and deployment definitions. Third-party product source and binaries are obtained from their upstream projects; they are not vendored into the repository.

| Dependency | Reference | Use |
| --- | --- | --- |
| Redmine | Official `redmine:7.0.2-bookworm` Docker image | Base application; the Dockerfile adds only the ESA plugin |
| PostgreSQL | Official `postgres:17-alpine` Docker image | Redmine database |
| Eclipse Temurin | Official `eclipse-temurin:8-jdk` Docker image | Replay JVM; the runner records and uses its repository digest |
| Apache Kafka | `https://github.com/apache/kafka.git` | External historical source checkout for replay |
| Historical Kafka binaries | Apache archive release URLs selected by the runner | Optional server profile; validated against official checksums |
| Historical client dependencies | Maven Central coordinates from historical Kafka `build.gradle` | Client replay compilation; downloaded with checksum validation |
| Go | Go 1.27+ | ESA backend and local commands; standard library only |
| Python | Python 3.9+ | Replay/download/verification scripts; standard library only |
| Codex CLI | Separately installed and authenticated CLI | Optional diagnosis provider; credentials are never included |

Redmine core source is referenced through its upstream Docker image instead of copied or tracked as a Git submodule. Its source is available through the [Redmine download page](https://www.redmine.org/projects/redmine/wiki/Download) and the linked [GitHub mirror](https://github.com/redmine/redmine); the [official Docker image](https://hub.docker.com/_/redmine) supplies the deployed application. ESA does not patch Redmine core. Only `plugins/redmine_esa` is maintained here.

Fetch Kafka separately when running historical replay:

```sh
git clone https://github.com/apache/kafka.git kafka
```

The `kafka/` directory is ignored by Git. Jira exports in `data/`, runtime evidence in `.esa/`, reports, local tools, compiled binaries, credentials, environment secrets and generated deployment records are also ignored. Synthetic examples under `examples/` remain tracked.

## Fresh checkout

1. Install Go 1.27+, Python 3.9+, Docker and Docker Compose independently.
2. Build with `./scripts/build_esa.sh`; it uses the project-local toolchain when present and otherwise uses `go` from PATH.
3. Copy `deploy/redmine/.env.example` to `deploy/redmine/.env`. Fill its database password, session secret and ESA token with unique random values; keep the generated file private.
4. Build and start Compose services using the root README. Outside this Mac, replace `colima-redmine` with the appropriate Docker context and set `ESA_GO_ARCH` as needed.
5. Configure Redmine users, project modules and ESA roles. Local administrator bootstrap scripts read the password from an environment variable; no credentials are shipped.

Replay additionally requires the external Kafka checkout and preserved Jira exports. They are separate inputs, not repository contents. Live Codex replay also requires a registered evidence bundle and local CLI authentication. Third-party images and archives retain their upstream licensing and notices.
