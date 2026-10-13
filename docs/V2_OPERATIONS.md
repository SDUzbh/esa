# ESA V2 operations

V2 implements the shared-investigation core behind an explicit switch. `config/rules.json` retains workflow 4; `config/rules.v2.json` enables workflow 5. The classifier stays disabled. Editing this repository does not activate a deployment or migrate its database.

## Workflow

1. Capture an immutable manually forwarded snapshot with effective, available, recorded and cutoff times. Reject source updates/comments beyond the cutoff; strict replay also rejects unknown comment timestamps.
2. Retrieve project/time-eligible trusted cases using deterministic BM25/RRF and an eligible-corpus hash. Validate repair applicability separately, including contextual terms, component/version coverage and environment. Similarity never confers trust.
3. When no trusted case applies, check context and compute an exact diagnostic/environment/version signature. Create a tentative Engineering Issue, membership, group and frozen Task Contract. Matching tickets share one investigation and solution reference.
4. Claim the task atomically with a lease, heartbeat and fencing epoch. Rotate projects and limit active tasks per project (default one). Provider work runs outside transactions; outbox polling runs separately.
5. Within one primary Codex session, the Agent chooses an approved Profile through `capability_request`. Go validates manifests and grants; further capability requests use the same session. Publish an immutable Agent solution in `proposed` state.
6. Engineer review uses an expected solution ID and publishes another immutable revision. Reviewed cases can supersede their source task's proposal within exact scope. Reopening/changing source scope withdraws trusted advice. A membership review can split or reassign a ticket, retaining old snapshots and membership history.

Rejected/failed/incomplete investigations remain linked and never become repairs. Uncertain invocations require explicit retry. New versions/environments and explicit force requests create separate tasks. Exact text matching is conservative: differently worded incidents are not claimed to share a cause. Initially one primary membership is exposed, and the Agent receives the task's allocated tentative family; wider issue-candidate assignment is a later extension.

## Enable and persist

```sh
export ESA_API_TOKEN='<private service token of at least 24 random characters>'
.tools/go/bin/go run ./cmd/esa -listen 127.0.0.1:8090 \
  -store runtime/v2/state.json -rules config/rules.v2.json
```

JSON remains a single-process adapter with an exclusive writer lock. Old reports remain readable; V1 queued runs require a new forwarding for V2. Migration does not fabricate historical provenance.

Redmine migration `003_create_esa_state_records.rb` adds a locked revision/frame row and separate V2 entity rows. `/esa/api/state` provides authenticated transactional CAS; snapshots and solution revisions cannot be modified/deleted. Set `ESA_STATE_API_ENABLED=1` in Redmine and `ESA_STATE_URL=http://redmine:3000/esa/api/state` in Go. Go then writes only to this authority, with no JSON fallback or dual writing. Browser users never receive the service token.

For an existing installation: back up JSON/database, stop the ESA writer, rebuild images, start the migrated plugin, import the stopped JSON once into a pristine backend, then start ESA:

```sh
DOCKER_CONTEXT=colima-redmine docker-compose -f deploy/redmine/compose.yaml stop esa
DOCKER_CONTEXT=colima-redmine docker-compose -f deploy/redmine/compose.yaml \
  -f deploy/redmine/compose.v2.yaml build redmine esa
DOCKER_CONTEXT=colima-redmine docker-compose -f deploy/redmine/compose.yaml \
  -f deploy/redmine/compose.v2.yaml up -d redmine
DOCKER_CONTEXT=colima-redmine docker-compose -f deploy/redmine/compose.yaml \
  -f deploy/redmine/compose.v2.yaml run --rm esa -import-state /var/lib/esa/state.json
DOCKER_CONTEXT=colima-redmine docker-compose -f deploy/redmine/compose.yaml \
  -f deploy/redmine/compose.v2.yaml up -d esa
```

Import preserves the original JSON and rejects an existing/nonzero backend. After remote writes begin, the old file is a backup, not a current rollback target. Returning to JSON requires a consistent export while writers are stopped.

This is a **transactional adapter prototype**: HTTP CAS still transfers whole state (64 MiB bound); V2 entities are stored separately, while V1 compatibility data remains in a frame. Row-level claims, scalable queries and production migration/load validation remain staged work. Graphs, bytecode and profiling arrays do not belong in business state.

## Source registry, Profiles and tools

Copy `config/releases.example.json` to a private runtime file; provide actual local repositories, release refs and verified availability times. The sample date is illustrative, not release evidence. Source resolution requires one affected release and checks registration availability plus author/committer timestamps against the cutoff. It downloads no repository.

```sh
bin/esa-codex -registry runtime/releases.json -profiles profiles \
  -runs runtime/codex-v2 -listen 127.0.0.1:8091
```

Configure the matching provider URL/token on ESA. In Compose, use a reachable provider address rather than assuming container loopback is host loopback. Registry concurrency defaults to one, configurable up to eight.

The provider resolves a task-local packet and caches source by project, commit, scope and policy. The model sees no `.git` history. Reads/searches verify source hashes. Source-only registrations deny runtime work; an optional `packet_path` can supply a registered runtime only when commit, cutoff and diagnostic fingerprint match. The managed helper currently supports the Kafka metadata-disconnect fixture, not a universal installer.

Five versioned Profiles (`general`, `java-debug`, `performance`, `compatibility`, `configuration`) provide `SKILL.md`, environment and tool manifests. Version 2 adds preinstalled container images, `toolset.txt` declarations and `toolset.release.txt` inspections. Run `python3 scripts/update_tool_images.py` before managed runtime diagnosis; see [tool image operations](TOOL_IMAGES.md). Skill text does not grant authority. Catalog hashes are frozen and checked; actual readiness and runtime permissions determine usable capabilities. A separate tool JVM does not automatically provide a registered customer-runtime compatibility variant.

Extended MCP tools use underscore names: `source_search`, `capability_request`, `tool_install`. Source search is bounded literal matching, not parser-verified symbols or a static analysis service. V2 tool-enabled Codex reports require an approved plan in the tool audit.

An operator-owned task configuration can register `tools: {"diagnostic": {"version":"1", "file":"/absolute/approved/tool", "sha256":"<verified digest>"}}`. Alternatively pass `-tool-catalog runtime/tools.json` to the provider, with that file containing the tool-ID map directly. Capability responses list approved IDs/versions/hashes, without host paths or download URLs. A pinned HTTPS `url` may replace `file`; redirects are denied and downloads are bounded at 16 MiB. Installation requires a granted `temporary_tools` capability and hypothesis/purpose/expected observation. Go validates the package, writes as the sandbox user to `/workspace/tools`, then checks the installed hash. Matching bytes are reused. Catalog data never comes from ticket text. Installed shell scripts use `sh <path>`; installation does not attest that an arbitrary native binary can execute on the runtime's scratch mount.

Runtime/tool installation verifies ownership, nonroot user, offline network, read-only root, dropped capabilities and resource limits. Product source/classes/dependencies stay read-only. Diagnostic scripts and tools use scratch/separate classpaths. Arbitrary diagnostic commands remain available after a runtime grant; avoiding behavior-changing product shadowing also remains a contract requirement. Cleanup is recorded in `cleanup.json`; failures add alerts. No tool is installed on the host.

## Time, reports and UI

Initial retrieval/diagnosis uses the frozen forwarding cutoff. Later reviews and responses carry their own availability; they never rewrite initial input or make future knowledge available retroactively. Historical replay enables the scoped monotonic clock in `config/rules.v2-replay.json`; `POST /v1/replay/clock` accepts `{"time":"2020-01-01T00:00:01Z"}`. Never enable this clock in live operation. This evaluates present ESA on historical product input, not historical model/Skill deployment reconstruction.

`report.envelope_v2` records snapshot/input/provider-input hashes, times, issue/task IDs, approved plan hashes, finding/evidence identities, location roles and elapsed time. Provider-reported mapping remains independently unverified. Hypotheses, exact quotation checks, symptom reproduction, review and causal verification are separate. Engineer approval does not mark causality verified. Codex still checks exact inspected source ranges/quotes. Legacy reports remain unassessed.

BM25/RRF uses an in-memory eligible-corpus projection, not a persistent FTS service. Future/other-project documents do not contribute scoring statistics. Token alerts are advisory; missing usage stays unknown. Deadlines and permissions are separate enforced limits.

| Public V2 route | Purpose |
| --- | --- |
| `GET /v1/engineering-issues` | Authorized families and visible member counts |
| `GET /v1/investigations/{id}` | Task and eligible result |
| `GET /v1/investigations/{id}/solutions` | Immutable, time-visible solution history |
| `POST /v1/investigations/{id}/review` | Expected-revision review/amendment |
| `POST /v1/tickets/{id}/membership/review` | Snapshot-guarded split/assignment |
| `GET /v1/snapshots/{id}` | Authorized frozen input |
| `POST /v1/replay/clock` | Test-only observation clock |

Ticket rows expose their own snapshot ID and shared task/solution. Follower input is never replaced with the lead's raw ticket. Source reports are withheld if the viewer cannot see the source. Rejected/withdrawn advice is withheld from the current view; prior evidence remains in authorized history. Redmine shows shared status, pending/reviewed solutions, review forms, membership assignment and an Engineering issues list. Reviews do not close customer issues automatically.

## Validation and staged scope

```sh
.tools/go/bin/go test ./...
.tools/go/bin/go build -o bin/esa-host ./cmd/esa
.tools/go/bin/go build -o bin/esa-codex ./cmd/esa-codex
python3 scripts/verify_label_frontend.py
python3 scripts/verify_transactional_store.py
python3 scripts/verify_v2_http.py
python3 scripts/verify_v2_tools.py
cd system_replay
../.tools/go/bin/go test ./...
../.tools/go/bin/go run ./cmd/system-replay --pipeline \
  --manifest examples/esa-api-v2.json --output ../runtime/new-v2-replay
```

Database/tool tests use isolated owned resources and existing images. API/replay tests use a synthetic HTTP provider with no model calls. Tests cover 100-ticket concurrent deduplication, ACLs, pending/trusted distinction, immutable revisions, reviewed split, expired-worker fencing, future-corpus isolation and approved tool installation. They validate contracts, not real-ticket localization accuracy. `system_replay` stays a separate module with no ESA business imports.

Later phases remain explicit: SootUp/CFG/call-graph/slicing workers, generic runtime provisioning, profiling adapters, persistent search projections, approximate incident grouping, wider issue-candidate assignment, secondary links, richer artifact attestations and stage timings. Production rollout and learned routing/active learning require load/migration checks and independent time/group-disjoint judged data. These optional staged components are not prerequisites for shared-investigation controls.
