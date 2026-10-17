# ESA operations

ESA uses one workflow: forwarded snapshots, Routing Agent retrieval from local Issue directories, and engineer-reviewed solutions. `config/rules.json` contains analysis budgets, worker limits and the optional classifier. Editing this repository does not deploy the service or migrate its database.

## Workflow

1. Capture an immutable manually forwarded snapshot with effective, available, recorded and cutoff times. Reject source updates/comments beyond the cutoff; strict replay also rejects unknown comment timestamps.
2. Every open snapshot enters an exact diagnostic/environment/version signature group. Identical inputs share a task; otherwise create an unassigned group and frozen Task Contract. Intake creates no Issue.
3. Claim the task with a lease, heartbeat and fencing epoch. Rotate projects and limit active workflows per project (default four); initial routing has one durable gate per project and engineering has a separate global hard limit of three. Work runs outside transactions.
4. Generate a project/cutoff-scoped local Issue directory, separating `agent_analyzed` proposals from `engineer_resolved` solutions and `investigating` coordination records. Send only the view path/hash; existing Issue/case content is retrieved on demand, without inlining or a top-N archive cutoff.
5. Routing Agent owns local Issue search/read tools and reads engineering requests with `issue_context` after analysis. Applicable records from either directory kind are reused without invoking engineering, resolving source or setting up a sandbox. An engineer solution has priority when applicable; an Agent proposal can also be reused. A matching `investigating` record joins the existing task without engineering. With no reusable solution or pending task, ESA registers the claim, waits for a free engineering slot, and engineering chooses an approved Profile and analyzes read-only. Engineering has only the original diagnostic task and source/runtime tools; it submits `issue_request` without searching or selecting Issues. Routing Agent then decides assignment or creation. Only its accepted supported decision assigns/creates an Issue. Insufficient or superseded results create none; explicit human membership decisions take precedence.
6. Engineer review uses an expected solution ID and publishes another immutable revision. A reviewed case from any linked member can supersede the shared family's proposals within the same scope. All case, solution review and withdrawal paths publish through one revision writer. Reopening/changing source scope withdraws trusted advice. A membership review changes links only: snapshot ID and membership ID guard against stale edits; confirmation, split and reassignment never forward input or enqueue analysis. Reanalysis requires an explicit forwarding request. Closing a source with the already reviewed resolution collects a case with the same canonical review state, without a second approval.

Rejected/failed/incomplete investigations remain linked and never become repairs. Available usage, alerts and partial evidence are retained even when output validation fails. Investigation state owns execution status; run and family statuses are projections. New analyses publish a solution referencing the immutable run report. Reuse stores a `solution_application` reference, never a copied knowledge revision. Its expected-revision applicability review does not promote an Agent proposal or change source trust. Source revision changes or withdrawal invalidate dependent applications. Uncertain invocations require explicit retry. New versions/environments and explicit force requests create separate tasks. Exact text matching is conservative: differently worded incidents are not claimed to share a cause. Initially one primary membership is exposed. The directory preserves separate records per Issue/scope; paginated retrieval can inspect the complete eligible archive. Reuse requires an exact audited read and a final authoritative-source check. The backend reconstructs the reused solution rather than trusting model-generated replacement text. See [Issue directory operations](ISSUE_DIRECTORY.md) for layout, root configuration, mounts and retention. Historical Issues remain readable; old queued contracts require fresh forwarding. Starting a replacement investigation detaches the old active membership while retaining history.

A later case approval does not match open tickets, replace unrelated investigations or enqueue analysis. It becomes context for subsequent tasks; review of a case already linked to an Issue may still revise that shared solution within the same scope. Original snapshots and diagnosis cutoffs remain frozen. An unstarted investigation with no active members becomes superseded; in-flight work retains its usage audit.

## Issue retention

ESA retains at most **100 Engineering Issues globally across projects**. Startup and every successful business transaction first remove empty Issues, then evict the least recently accessed until the catalogue is within the limit. Empty means no current public ticket membership, eligible closed case, or usable analysis source still associated with that Issue. A pending unassigned replacement task does not erase its usable prior analysis; moving the source to another Issue releases that association.

Creation, assignment, reuse, review, authorized ticket/investigation detail reads, and successful hash-verified Agent record reads update access order. Listing tickets/Issues, exporting an index, and search results alone do not. Agent reads are reported on successful stage completion and constrained to that task's frozen directory. A persisted sequence preserves operation order even when replay timestamps are equal; legacy Issues fall back to creation time and ID until accessed. Historical cutoffs use the replay clock rather than wall time.

Eviction removes the current Issue entry, detaches its active memberships, clears human overrides and invalidates dependent solution applications. Snapshots, runs, solution revisions, cases and membership history remain as audit records. Removed Issue IDs cannot re-enter the directory through those retained records, be reviewed as current knowledge, or be automatically restored by stale Agent output. Explicit forwarding can start a new analysis. The 100 limit applies to Engineering Issues, not Redmine tickets, standalone cases or audit storage size. This policy needs no scheduler or manual cleanup command.

## Run and persist

```sh
export ESA_API_TOKEN='<private service token of at least 24 random characters>'
.tools/go/bin/go run ./cmd/esa -listen 127.0.0.1:8090 \
  -store runtime/shared-investigations/state.json -issues-directory runtime/esa/issues -rules config/rules.json
```

JSON remains a single-process adapter with an exclusive writer lock. Stored events reference their snapshot (`snapshot_id`, `ticket_hash`) instead of copying the ticket; an ignored stale or self event, which captures no snapshot, keeps its `ticket`. ESA converts events in the older shape when it loads a state and writes the new shape on the next change; no forwarded content is dropped. Back up the state before upgrading: an older ESA binary cannot read the new event shape correctly, so downgrading requires that backup. Existing report findings remain readable; queued runs without a current manually forwarded snapshot require a new forwarding. Migration does not fabricate historical provenance. Report assessments use `report.assessment`; older sidecar fields require explicit data migration before reuse. Existing stored data is not rewritten by this naming change.

## Postgres state store

ESA can keep its state in its own Postgres database instead of the JSON file: one row per entity, written only where an update changed something, in one transaction per update. See the [storage design](STORAGE_DESIGN.md). Set `-database-url` or `ESA_DATABASE_URL`; the JSON `-store` is then not used, and `-state-url` cannot be combined with it.

One-time setup with Compose (the private `.env` needs `ESA_DB_PASSWORD` and `ESA_DATABASE_URL`, see `deploy/redmine/.env.example`):

```sh
# Recreate db so it sees ESA_DB_PASSWORD, then create ESA's role and database (safe to rerun).
DOCKER_CONTEXT=colima-redmine docker-compose -f deploy/redmine/compose.yaml up -d db
DOCKER_CONTEXT=colima-redmine docker-compose -f deploy/redmine/compose.yaml exec -T db sh -s < deploy/redmine/postgres-init/10-esa-database.sh
# Stop ESA, back up the esa_state volume, and import the stopped JSON store once.
DOCKER_CONTEXT=colima-redmine docker-compose -f deploy/redmine/compose.yaml stop esa
DOCKER_CONTEXT=colima-redmine docker-compose -f deploy/redmine/compose.yaml build esa
DOCKER_CONTEXT=colima-redmine docker-compose -f deploy/redmine/compose.yaml run --rm esa -import-state /var/lib/esa/state.json
DOCKER_CONTEXT=colima-redmine docker-compose -f deploy/redmine/compose.yaml up -d esa
```

The script is piped into the database container, so it needs no bind mount (Colima shares only the Issue directory). To import from the deprecated Redmine state API instead, use `-import-state-url http://redmine:3000/esa/api/state`.

- **Import** runs in one transaction: it refuses a database that already has state, writes everything, reads it back and commits only if the stored state reproduces the source byte for byte, completely and as the working set ESA loads. It never modifies the source. It holds the JSON file lock and refuses to start while another ESA process holds the writer role.
- **One writer.** A process waits as a **standby** until it holds the writer lock: `/healthz` answers 200 with `"role":"standby"`, `/healthz?role=active` and every `/v1/*` route answer 503, and no workers run. The writer bumps a term before loading; every commit is fenced on that term, so a process that lost the role can never commit. A standby takes over within about 5 seconds of a clean exit, or about 25 seconds after its connection dies.
- **Losing the role** (the lock session drops, or a commit is fenced) switches the API to 503, stops the workers and exits with an error; Compose restarts the process, which then waits as a standby. An outbox event the old writer could not commit is not acknowledged, and the new writer receives it.
- **Connections:** connect directly or through session pooling. PgBouncer *transaction* pooling breaks the session advisory lock and is not supported. The lock key is derived from the schema name (`esa` by default). Run one ESA instance per database by default; for a standby, never put it behind the same round-robin name as the writer, and route by `/healthz?role=active`.
- **Not loaded into memory:** snapshots that no run, investigation contract or live membership references, membership history, and raw event payloads. They stay in the database. Until cold reads are added, `GET /v1/snapshots/{id}` returns 404 for such a snapshot; the Redmine plugin does not use that route.
- **Rollback:** the imported JSON file is unchanged, so unsetting `ESA_DATABASE_URL` returns to the state as of the import; changes made in Postgres afterwards are not exported back.

## Redmine state API (deprecated)

Redmine migration `003_create_esa_state_records.rb` adds a locked revision/frame row and separate shared investigation entity rows. `/esa/api/state` provides authenticated transactional CAS; snapshots and solution revisions cannot be modified/deleted. Set `ESA_STATE_API_ENABLED=1` in Redmine and `ESA_STATE_URL=http://redmine:3000/esa/api/state` in Go. Go then writes only to this authority, with no JSON fallback or dual writing. Browser users never receive the service token.

For an existing installation: back up JSON/database, stop the ESA writer, rebuild images, start the migrated plugin, import the stopped JSON once into a pristine backend, then start ESA. Set `ESA_STATE_URL=http://redmine:3000/esa/api/state` in the private Compose `.env` when choosing PostgreSQL; leave it unset for the local JSON store:

```sh
DOCKER_CONTEXT=colima-redmine docker-compose -f deploy/redmine/compose.yaml stop esa
DOCKER_CONTEXT=colima-redmine docker-compose -f deploy/redmine/compose.yaml build redmine esa
DOCKER_CONTEXT=colima-redmine docker-compose -f deploy/redmine/compose.yaml up -d redmine
DOCKER_CONTEXT=colima-redmine docker-compose -f deploy/redmine/compose.yaml run --rm esa -import-state /var/lib/esa/state.json
DOCKER_CONTEXT=colima-redmine docker-compose -f deploy/redmine/compose.yaml up -d esa
```

Import preserves the original JSON and rejects an existing/nonzero backend. After remote writes begin, the old file is a backup, not a current rollback target. Returning to JSON requires a consistent export while writers are stopped.

The Redmine state API (`-state-url`) is **deprecated** in favor of the Postgres state store above. It is a **transactional adapter prototype**: HTTP CAS writes whole state (64 MiB bound); unchanged reads use revision ETags to avoid retransmitting the archive; shared investigation entities are stored separately, while per-ticket data remains in the core state frame. Row-level claims, scalable queries and production migration/load validation remain staged work. Graphs, bytecode and profiling arrays do not belong in business state.

## Shutdown and cleanup

Run from the repository root (Python 3.9+, no packages):

```sh
python3 -B scripts/shutdown.py --dry-run
python3 -B scripts/shutdown.py
```

The script sends SIGTERM to this checkout's service/provider, replay and verification
entry points. Owners have 30 seconds to cancel work and clean their helpers; remaining
owned descendants receive SIGTERM and then SIGKILL if necessary. Process arguments
and credentials are not printed. Do not start new work during shutdown.

In the selected Docker context, it stops the `esa-redmine` Compose services, removes
containers and scratch volumes with `esa.replay.session` or `system.replay.session`
labels, and removes disposable `esa-deploy-<12 hex digits>` deployment-test resources
and their service image tags. It deletes stale generated session directories in this
checkout only after Docker cleanup succeeds. Production volumes, attachments, ESA
state, Issue projections, Jira data, reports and tool/build caches are preserved.
Downloads and unrelated host applications are not stopped.

The default context is `colima-redmine`. When no foreign containers remain running,
the script stops its `redmine` Colima profile. Use `--keep-vm` for cleanup without VM
shutdown, or `--context NAME` for another context; non-Colima contexts have no VM
shutdown step. Foreign running containers keep Colima running even without
`--keep-vm`. Missing ownership information or cleanup failures return nonzero rather
than claiming success. If Colima is already stopped, it is not restarted; orphan
Docker resources can only be cleaned after the daemon is available again. It does
not delete VM disks, prune Docker globally, or remove custom session directories
outside the checkout. Shutdown does not acknowledge or discard queued tickets;
persisted workflow recovery still applies on restart.

## Source registry, Profiles and tools

Copy `config/releases.example.json` to a private runtime file; provide actual local repositories, release refs and verified availability times. The sample date is illustrative, not release evidence. After Routing chooses `analyze`, a deterministic handoff check requires one nonblank, unambiguous affected release and a matching project/release registration available by the cutoff. Missing or ambiguous releases return **Provide one unambiguous affected release**; unmatched registrations return **Invalid or nonexistent version**. The effective route becomes `request_information`, and ESA enters **Awaiting information** without invoking Engineering, exporting source, claiming engineering capacity or creating a sandbox. Reuse, join and customer-support returns bypass this check. A precompiled environment or reproducer is not required. Once admitted, source resolution also checks author/committer timestamps against the cutoff. It downloads no repository.

```sh
bin/esa-codex -registry runtime/releases.json -profiles profiles \
  -issues-directory runtime/esa/issues -runs runtime/codex -listen 127.0.0.1:8091
```

Run the provider from the repository root. Its `-token-env-file` defaults to the private `deploy/redmine/.env` and must contain the same service token as ESA; an `ESA_API_TOKEN` environment variable alone does not configure this adapter. Keep the provider terminal open and use Ctrl-C for owned cleanup. `scripts/manage_codex_provider.py` only manages its hardcoded historical `-evidence` invocation; it does not supervise or stop a registry-based provider.

Configure the matching provider URL/token on ESA. In Compose, use a reachable provider address rather than assuming container loopback is host loopback. The service dispatches `route`, `engineer`, and `assign` stages. Engineering concurrency defaults to three and can be lowered with `-concurrency 1..3`; it cannot exceed three. The shipped `worker.max_active_per_project: 4` permits three engineering tasks plus another routing/assignment workflow; tasks parked for an engineering slot do not count. A lower project limit reduces throughput. Each stage has its own `budget.timeout_seconds` deadline (900 s shipped), matching the provider's per-invocation timeout, so a supported diagnosis can take up to three stage deadlines of active time. Waiting for an engineering slot does not use any stage deadline; `worker.max_queue_wait_seconds` (1..86400, default and shipped 1800) bounds it separately. A task that exceeds it fails with a "no engineering call was made" error, keeps its exact routing usage, and needs an explicit re-forward.

The provider is stateful across stages. For each `run_id` it must durably keep the routing request and audit (the Codex adapter uses `<runs>/<run_id>/input.json` and `routing-report.json`), and accept `engineer` and `assign` only for a request identical apart from `stage`. A task parked for an engineering slot can send `engineer` much later, from another ESA worker or process, possibly after either side restarted. ESA resends the frozen request with the same `run_id` and Issue-directory view. Keep the provider runs directory on persistent storage. If the provider has lost the run, `engineer` fails, spend is treated as uncertain, and the ticket needs an explicit re-forward. See [pipeline coordination](ISSUE_DIRECTORY.md#pipeline-and-in-progress-deduplication).

For a host ESA process, set `ESA_ANALYZER_URL=http://127.0.0.1:8091/analyze` and `ESA_ANALYZER_TOKEN` to the same private token before starting it. `ESA_API_TOKEN` protects ESA's own API; it does not automatically populate `ESA_ANALYZER_TOKEN`. Compose supplies the latter explicitly. Starting the provider alone does not connect an already running ESA service; update its configuration and restart it.

The provider resolves a task-local packet and caches source by project, commit, scope and policy. The model sees no `.git` history. Reads/searches verify source hashes. With managed tools enabled (the default), a known release can provision a generic workspace on an approved runtime capability request, independently of `packet_path`. The workspace uses the verified preset image and read-only frozen source, with writable scratch. It supplies no product build, reproduction program or prior runtime observations; the Agent must compile unchanged source and write bounded diagnostic helpers. The current preset supplies target JDK 8, not a universal historical runtime. Missing or incompatible runtimes/dependencies produce an environment request. `-managed-tools=false` retains explicit source-only operation.

An optional `packet_path` remains an operator-registered scenario: commit, cutoff and diagnostic fingerprint must match before its prior evidence and specialized reproducer are used. Generic workspaces never inherit that fixture. Both modes use the same owned container lifecycle, capability gate and cleanup helper.

Product dependency archives can be supplied through the existing `-tool-catalog /private/catalog.json` option. Entries require an approved ID, version, local file or URL, and SHA256. After requesting `temporary_tools`, the Agent installs only the needed entries with `tool_install`. A raw JAR may be copied unchanged from its returned scratch path into `/workspace/lib/name.jar`; no host network, unrestricted package manager or product-class replacement is granted. Unknown dependencies require an environment request.

`read_code` and `run` return line objects `{line, id, text}` and actual `first_line`/`last_line` bounds. IDs bind source, reference and exact line bytes and remain stable across overlapping reads. Engineering output selects `location`, `end_location`, `problem`, `solution` and supporting `evidence_ids`; the provider derives canonical commit/file/line ranges and exact evidence quotes. Unknown IDs, non-Git locations, cross-file ranges, stale source and invented coordinates are rejected. Empty `findings` remain valid when localization is unavailable. Persisted/API reports keep their existing concrete finding/evidence shape, and retrospective review keeps its separate schema. Citation correctness still does not establish causality.

## Ticket runner and runtime logs

Build host binaries with `scripts/build_esa.sh` and start the registry-based provider above. Then explicitly forward a ticket from a file:

```sh
python3 scripts/analyze_ticket.py examples/agent-ticket.json \
  --routing-provider http://127.0.0.1:8091 \
  --issues-directory runtime/esa/issues --output runtime/ticket-example
```

The positional argument accepts an ESA Ticket JSON object or an Event JSON containing `ticket`. Supply nonempty string `id`, `project_id` and `subject`, plus the original description, versions, components, environment and comments. Original source dates are preserved; missing creation/update dates use the explicit intake time. `examples/agent-ticket.json` is a support-only fixture; replace its content/project/version to exercise a registered engineering scenario. Raw Jira exports require conversion to this Ticket contract.

`--routing-provider` selects the provider base URL. Its Routing Agent decides reuse, join, clarification, customer support or engineering; this is not an engineering bypass or keyword classifier. The default starts `bin/esa-host` with a fresh private state, the configured rules and the normal service/worker pipeline. It shares only the generated Issue projection root with the provider, which must use the same root. This isolated state starts with no prior Issues. It disables Redmine synchronization and stops the local service on normal exit, interruption or failure; managed engineering environments retain their existing cleanup policy. It retains the private state/results/logs for inspection. An abrupt OS kill cannot run process cleanup.

To use existing Issues and the existing service's worker coordination, append `--esa-url http://127.0.0.1:8090`. That service must already use the selected provider, have a host-accessible API and use the same service credential. The script forwards through its authenticated `/v1/events` API and polls `/v1/tickets/{id}`; it does not change an existing service's provider configuration or stop that service. Project scope comes from the ticket. `--forwarded-by` defaults to the invoking OS user prefixed with `cli:`. The token is read from `ESA_API_TOKEN` or `--token-env-file` (default `deploy/redmine/.env`); it is never put in arguments or logs. The output directory must be new and is private. There is no automatic retry of analysis or forced reanalysis of a cached ticket.

The provider exposes operator-only `GET /runs/{id}/logs?stage=route|engineer|assign&offset=0`, authenticated with its service token. Pages contain the original Codex CLI JSON event payloads, `available`, `next_offset` (bytes in the source log) and `has_more`. Each page contains at most 200 complete events and is size bounded. Partial trailing events are retried from the same cursor. Missing/uninvoked stages return `available: false` with no events. The endpoint confines paths to the provider's run directory, including symlinks. These private logs are not exposed through the Redmine browser API.

The script streams event/tool summaries to the terminal and saves each stage under `logs/<run_id>/routing.events.jsonl`, `engineering.events.jsonl` and `assignment.events.jsonl`. Events include model messages, tool arguments/results, errors and completion usage when emitted by the CLI. There is no synthesized reasoning trace. A support return or Issue reuse has no engineering log. Joined investigations retain separate run directories. `event.json`, `receipt.json`, `result.json` and `logs.json` preserve the submitted snapshot, receipt, final/latest API result and log cursors. Logs are also downloaded on failed analysis; failures return nonzero. The default client wait is 1,200 seconds, independently configurable through `--wait-seconds`; token warnings never stop analysis.

### Weighted token alerts

`budget.token_weights` defaults to `{"input":1,"cached_input":0.1,"output":1}`. Omitted fields inherit these defaults; weights must be finite and nonnegative, and explicit zero is allowed. The calculation is:

```text
weighted_input = (input_tokens - cached_input_tokens) * input_weight
                 + cached_input_tokens * cached_input_weight
weighted_output = output_tokens * output_weight
weighted_total = weighted_input + weighted_output
```

The existing thresholds (`max_input_tokens: 16000`, `max_output_tokens: 6000`, `warn_total_tokens: 22000`) now compare weighted units. These are advisory credit-equivalent estimates, not actual billed credits. Raw counts remain unchanged; cache hits are parsed from reported CLI usage and summed across routing, engineering and assignment. Cached input is not added twice. Reasoning output is already included in output and is not counted again. Older providers without cache statistics receive no discount; unknown usage stays unknown. Final report warnings reflect current aggregate usage; per-stage raw events/audits retain stage counts and warnings. Existing historical reports are preserved. Restart the provider and ESA service after rebuilding to activate these changes.

## Profile environments

Five versioned Profiles (`general`, `java-debug`, `performance`, `compatibility`, `configuration`) provide `SKILL.md`, environment and tool manifests. Profiles share one preinstalled toolkit image, retain `toolset.txt` declarations and use the common `deploy/tools/toolset.release.txt` inspection. Approved runtime requests create the sandbox lazily after Profile selection. Run `python3 scripts/update_tool_images.py` before managed runtime diagnosis; see [tool image operations](TOOL_IMAGES.md). Skill text does not grant authority. Catalog hashes are frozen and checked; actual readiness and runtime permissions determine usable capabilities. A separate tool JVM does not automatically provide a registered customer-runtime compatibility variant.

Extended MCP tools use underscore names: `source_search`, `capability_request`, `tool_install`. Source search is bounded literal matching, not parser-verified symbols or a static analysis service. Planned tool-enabled Codex reports require an approved plan in the tool audit.

An operator-owned task configuration can register `tools: {"diagnostic": {"version":"1", "file":"/absolute/approved/tool", "sha256":"<verified digest>"}}`. Alternatively pass `-tool-catalog runtime/tools.json` to the provider, with that file containing the tool-ID map directly. Capability responses list approved IDs/versions/hashes, without host paths or download URLs. A pinned HTTPS `url` may replace `file`; redirects are denied and downloads are bounded at 16 MiB. Installation requires a granted `temporary_tools` capability and hypothesis/purpose/expected observation. Go validates the package, writes as the sandbox user to `/workspace/tools`, then checks the installed hash. Matching bytes are reused. Catalog data never comes from ticket text. Installed shell scripts use `sh <path>`; installation does not attest that an arbitrary native binary can execute on the runtime's scratch mount.

Runtime/tool installation verifies ownership, nonroot user, offline network, read-only root, dropped capabilities and resource limits. Frozen `/input` source, dependencies and any registered baseline classes stay read-only. Generic workspaces have no supplied product classes: the Agent builds unchanged source into writable scratch, with no product patching or class shadowing permitted. Diagnostic scripts and tools use scratch/separate classpaths. Runtime creation uses the selected Profile and a registered helper; source-only work does not launch a runtime. Arbitrary diagnostic commands remain available after a runtime grant; avoiding behavior-changing product shadowing also remains a contract requirement. Cleanup is recorded in `cleanup.json`; failures add alerts. No tool is installed on the host.

The Java toolkit includes `javac` and `java`. In the optional registered Kafka scenario, the helper compiles the historical product baseline and publishes its classes read-only. A generic workspace instead leaves compilation and diagnostic-helper creation to the Agent, using `/workspace/classes` and any approved JARs in `/workspace/lib`. In the registered scenario, after a runtime grant the Agent can use `run` to compile auxiliary Java programs under `/workspace/diagnostics` and execute them against `/input/classes` and `/input/deps/*`, keeping the registered product classpath first. `verify_profile_runtime.py` exercises this exact tool-server path by compiling a helper that invokes an existing Kafka class. This verifies executable compilation and runtime access for the registered Java scenario; other languages and product builds need their own registered toolchains and build inputs.

Soot and SootUp are preinstalled in the shared image. SootUp provides Jimple/CFG inspection and an offline callgraph SDK through the same runtime gateway; generic scratch builds require an explicit analysis classpath. Through the same `run` tool, the Agent can inspect frozen bytecode and emit Jimple under `/workspace/diagnostics/soot`. The launcher uses the separate analysis JVM with registered target classes, dependencies and JDK 8 libraries. The runtime verification script checks analysis of Kafka `NetworkClient` and unchanged product bytecode. See [tool environment](TOOL_IMAGES.md) for the command. No additional worker or workflow is introduced.

## Time, reports and UI

Initial retrieval/diagnosis uses the frozen forwarding cutoff. Later reviews and responses carry their own availability; they never rewrite initial input or make future knowledge available retroactively. Historical replay starts the same service with `-replay-at <RFC3339 time> -replay-projects <comma-separated IDs>`; `POST /v1/replay/clock` accepts `{"time":"2020-01-01T00:00:01Z"}`. Never enable this clock in live operation. This evaluates present ESA on historical product input, not historical model/Skill deployment reconstruction.

The report assessment sidecar records snapshot/input/provider-input hashes, times, issue/task IDs, elapsed time and explicit verification limitations. Approved plan hashes, findings and evidence live in the original report without duplicate assessment arrays. Retired classification, matching, generation and inline-report fields survive storage round trips as inert raw JSON for audit compatibility. They have no active dispatch or matching behavior; public run/solution projections omit these fields. Historical cross-ticket evidence without verifiable source provenance stays in the retained storage audit and is withheld from public reports. New records use run references and `report.plan` exclusively. The provider derives current finding coordinates from inspected evidence IDs and verifies exact source references/quotes. This validates citation provenance, not causal correctness; legacy coordinate claims are not retroactively repaired. Hypotheses, exact quotation checks, symptom reproduction, review and causal verification are separate. Engineer approval does not mark causality verified. Codex still checks exact inspected source ranges/quotes. Legacy reports remain unassessed.

The local directory is a generated projection with paginated lexical search. Future/other-project records are excluded before directory publication. Token alerts are advisory; missing usage stays unknown. Deadlines and permissions are separate enforced limits.

| Public route | Purpose |
| --- | --- |
| `GET /v1/engineering-issues` | Authorized families and visible member counts |
| `GET /v1/investigations/{id}` | Task and eligible result |
| `GET /v1/investigations/{id}/solutions` | Immutable, time-visible solution history |
| `POST /v1/investigations/{id}/review` | Expected-revision review/amendment |
| `POST /v1/tickets/{id}/membership/review` | Snapshot/membership-guarded links only; no analysis |
| `GET /v1/snapshots/{id}` | Authorized frozen input |
| `POST /v1/replay/clock` | Test-only observation clock |

Engineering Issue titles retain canonical provenance; representative ticket subjects are separate. A title is hidden when its source is outside viewer scope. Ticket rows expose their own snapshot ID and shared task/solution. Follower input is never replaced with the lead's raw ticket. Source reports are withheld if the viewer cannot see the source. Rejected/withdrawn advice is withheld from the current view; prior evidence remains in authorized history. Redmine keeps live state and editable metadata on the ticket page. Full pending/reviewed solutions, review forms, membership assignment and history are on the separate report page; the Engineering issues list remains available. **Fill metadata from ESA** offers eligible resolution values without saving, forwarding or promoting trust. See [frontend behavior](ESA_PLUGIN_FRONTEND.md). Reviews do not close customer issues automatically.

## Validation and current limits

```sh
.tools/go/bin/go test ./...
.tools/go/bin/go build -o bin/esa-host ./cmd/esa
.tools/go/bin/go build -o bin/esa-codex ./cmd/esa-codex
ruby scripts/test_esa_presentation.rb
ruby scripts/test_esa_acknowledgement.rb
node --test scripts/test_esa_ticket_ui.cjs
python3 scripts/verify_label_frontend.py
python3 scripts/verify_transactional_store.py
python3 scripts/verify_shared_investigations.py
python3 scripts/verify_sandbox_tools.py
python3 scripts/verify_engineering_workspace.py
# Optional registered-runtime check (operator-supplied historical packet):
python3 scripts/verify_profile_runtime.py --evidence /absolute/registered/evidence.json
cd system_replay
../.tools/go/bin/go test ./...
../.tools/go/bin/go run ./cmd/system-replay --pipeline \
  --manifest examples/esa-shared-investigations.json --output ../runtime/shared-investigation-replay
```

Host UI unit tests require Ruby and Node.js; the frontend compilation check uses the installed Redmine image. Database/tool tests use isolated owned resources and existing images. API/replay tests use a synthetic HTTP provider with no model calls. Tests cover 100-ticket concurrent deduplication, ACLs, pending/trusted distinction, immutable revisions, reviewed split, expired-worker fencing, future-corpus isolation and approved tool installation. They validate contracts, not real-ticket localization accuracy. `system_replay` stays a separate module with no ESA business imports.

Implemented tools include generic preset workspaces, unchanged-source compilation, Soot/SootUp Jimple/CFG inspection and the SootUp callgraph SDK. They do not constitute universal customer-environment reconstruction, automatic scenario generation or verified causal diagnosis. Automated slicing/profiling services, secondary Issue links, filesystem projection garbage collection and scalable row-level state/authorization remain outside the current implementation. Differently worded duplicate detection depends on Routing Agent judgment; no deterministic semantic deduplication is claimed. Production rollout and any learned routing require load/migration checks and independent time/group-disjoint judged data.
