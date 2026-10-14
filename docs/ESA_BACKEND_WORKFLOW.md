# ESA backend workflow

ESA uses one workflow with immutable snapshots, shared engineering investigations, reviewed solutions and approved Profile planning. See [ESA operations](ESA_OPERATIONS.md) for deployment, persistence, tools and validation.

ESA receives manually forwarded Redmine issues through a Ruby plugin and an independent Go service. Classification is removed from the active workflow. New code and interface text remain English; engineering diagnosis is read-only.

1. An authorized user forwards the current issue snapshot. The plugin records the actor/time/force choice in its durable outbox.
2. Go validates explicit forwarding, commits the immutable event, handles duplicate/stale input and acknowledges only after persistence.
3. Private snapshots are restricted; forwarded closed snapshots become case drafts.
4. Every open snapshot queues or joins an exact-input shared task. Go exports a project/cutoff-scoped local Issue directory with `agent_analyzed` and `engineer_resolved` records; only its relative path and manifest hash are sent to the provider.
5. A Routing Agent searches and reads that directory on demand. It reuses an applicable Agent analysis or engineer solution without invoking engineering or preparing a runtime; otherwise it requests information or calls the engineering Agent. Engineer solutions are preferred when applicable. No Issue list or case body is inlined in either initial prompt.
6. New engineering analysis chooses approved tools, analyzes read-only and proposes an existing Issue or a new title. Intake creates no Issue. An accepted supported result assigns or creates one; insufficient results create none. An absent provider produces `awaiting_configuration` without a model call.
7. Engineers review solution revisions and approve complete cases. Subsequent directory views reflect these changes; original task cutoffs remain frozen. Reuse preserves source provenance and does not turn Agent proposals into trusted repairs. Closing/editing Redmine does not itself submit to ESA.

The store preserves snapshots, investigations, runs, solutions and case revisions. Local development uses atomic JSON with a single-writer lock; Redmine PostgreSQL is available through the transactional state API. Network analysis runs outside transactions. Interrupted executions are marked failed and require explicit retry; old workflow queues wait for a new forwarding.

## Public interface

All `/v1` routes require a service token and explicit project scope. The plugin additionally derives ticket visibility from the logged-in user. Credentials stay on the backend.

| Method | Endpoint | Purpose |
| --- | --- | --- |
| GET | `/healthz` | Health |
| POST | `/v1/events` | Explicit manual forwarding; actor and time required |
| GET | `/v1/tickets` | Forwarded tickets, filters, sort and pagination |
| GET | `/v1/tickets/{id}` | Forwarded ticket and history |
| POST | `/v1/tickets/{id}/analyze` | Explicit retry with `forwarded_by` and optional `force` |
| GET | `/v1/runs/{id}` | Authorized run |
| POST | `/v1/investigations/{id}/review` | Review a solution with an expected revision and explicit cause, solution and validation |
| GET | `/v1/engineering-issues` | Visible engineering issues and members |
| GET | `/v1/investigations/{id}` | Investigation status and solution |
| POST | `/v1/tickets/{id}/membership/review` | Split or reassign tentative membership |
| GET | `/v1/cases` | Authorized cases |
| POST | `/v1/cases/{id}/review` | Case review |
| POST | `/v1/import/preview` | Import validation; no intake or analysis |

The provider receives `run_id`, frozen `ticket`, `task_contract`, `issue_directory`, `issue_directory_hash`, `budget` and `read_only: true`. It receives no inline `known_issues` or `cases`. See [local Issue directory and routing](ISSUE_DIRECTORY.md) for the directory layout, shared mounts, on-demand tools and reuse validation. New engineering findings still require inspected exact source/log evidence and an approved tool plan.

Each stage permits one provider invocation within the overall 900-second deadline. Reuse or clarification calls routing only; a new diagnosis normally calls routing plus engineering. `report.routing.usage` records routing spend and `report.usage` aggregates both. Input 16,000, output 6,000 and total 22,000 tokens generate advisory per-ticket warnings; missing stage usage makes aggregate consumption unknown. The optional TF-IDF + Logistic Regression classifier remains disabled by default and never controls intake or routing. See [classifier operations](CLASSIFIER.md).

See [manual forwarding](MANUAL_FORWARDING.md) for snapshots, cached/forced requests and upgrade behavior. The independent [system replay tester](TICKET_BACKTEST.md) exercises this public boundary; it never implements a substitute ESA workflow. External fixtures declare explicit human forwarding as test input.
