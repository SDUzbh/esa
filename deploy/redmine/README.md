# Local Redmine and ESA

Redmine core is an external dependency: the Dockerfile references the official `redmine:7.0.2-bookworm` image and copies only `plugins/redmine_esa`. No Redmine core source is uploaded with ESA. See [external dependencies](../../docs/DEPENDENCIES.md).

For a fresh checkout, copy `.env.example` to `.env` and generate unique values for its three required secret fields before invoking Compose. Local credentials and generated installation/verification records are ignored by Git.

Redmine 7.0.2 runs at http://127.0.0.1:8080/ using PostgreSQL 17 and the existing Colima `redmine` profile. The service binds only to localhost. ESA 0.1.0 is a separate Go container on the internal Compose network.

Administrator credentials are stored in `credentials.md`. Database, session, and ESA service credentials are in `.env`; both files are private and must never be committed or printed. `bootstrap.rb` initializes administrator credentials and is not a routine migration script.

Build ESA from the project root:

```sh
./scripts/build_esa.sh
DOCKER_CONTEXT=colima-redmine docker-compose -f deploy/redmine/compose.yaml build redmine esa
DOCKER_CONTEXT=colima-redmine docker-compose -f deploy/redmine/compose.yaml up -d
```

Redmine loads `plugins/redmine_esa` from the project-specific image and runs plugin migrations on startup. PostgreSQL, attachments, and ESA state have separate persistent named volumes. Do not delete volumes when stopping services.

`enable_esa.rb` sets the default and administrator language to English, translates exact legacy default enum labels, and creates a private **ESA Demo** project. It does not change passwords or import customer data. Existing projects require explicit ESA module and role configuration.

For development, edit the plugin and rebuild/recreate Redmine to load controller/model changes. Changes to Go require rebuilding the binary and ESA image. Rules reside in `config/rules.json` and are copied into the image; rebuild or mount a separate rules file when updating them.

The engineering provider is configured with `ESA_ANALYZER_URL` in `.env`; Compose uses the private ESA service token for provider authentication. The local Codex replay trial points to the host adapter at `http://host.lima.internal:8091/analyze`. Start/stop that adapter with `scripts/manage_codex_provider.py`. It currently accepts only the explicitly registered KAFKA-2880 evidence packet. See the root README and `docs/CODEX_REPLAY_TRIAL.md` for supported behavior and remaining work.
