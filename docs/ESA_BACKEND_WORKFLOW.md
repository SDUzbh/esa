# ESA backend workflow

ESA uses one workflow with immutable snapshots, shared engineering investigations, reviewed solutions and approved Profile planning. See [ESA operations](ESA_OPERATIONS.md) for deployment, persistence, tools and validation.

ESA receives manually forwarded Redmine issues through a Ruby plugin and an independent Go service. Classification is removed from the active workflow. New code and interface text remain English; engineering diagnosis is read-only.

1. An authorized user forwards the current issue snapshot. The plugin records the actor/time/force choice in its durable outbox.
2. Go validates explicit forwarding, commits the immutable event, handles duplicate/stale input and acknowledges only after persistence.
3. Private snapshots are restricted; forwarded closed snapshots become case drafts.
4. Every open snapshot queues or joins an exact-input shared task. Go exports a project/cutoff-scoped local Issue directory with `agent_analyzed`, `engineer_resolved`, `investigating` and `unresolved` records; only its relative path and manifest hash are sent to the provider.
5. A Routing Agent searches and reads that directory on demand. It reuses an applicable Agent analysis or engineer solution without invoking engineering or preparing a runtime; a matching in-progress record joins its investigation, and a matching `unresolved` record (an earlier investigation engineering could not answer) is handed to the new analysis as its starting point. An accidental non-engineering request returns to customer support without invoking engineering or creating an Issue. Otherwise routing requests information or registers a durable `investigating` claim before engineering. Initial routing is serialized per project; up to three engineering tasks run concurrently across projects/workers. Engineer solutions are preferred when applicable. No Issue list or case body is inlined in either initial prompt.
6. New engineering analysis first checks the ticket's failing stack frame against the frozen release and stops with empty findings when it is missing; otherwise it chooses approved tools and diagnoses the original task read-only. It submits `issue_request` (suggested title and evidence-based reason), with no catalogue access or authoritative Issue ID. After engineering cleanup frees its capacity, Routing Agent reads the request/findings and decides existing/new grouping or requests more information. Intake creates no Issue. An accepted supported result assigns or creates one; insufficient results create none. An absent provider produces `awaiting_configuration` without a model call.
7. Engineers review solution revisions and approve complete cases. Subsequent directory views reflect these changes; original task cutoffs remain frozen. Reuse records a solution application referencing existing knowledge; applicability review does not duplicate knowledge or turn Agent proposals into trusted repairs. Linked cases share canonical solution review and withdrawal. Membership edits change association only; reanalysis is explicit. Closing/editing Redmine does not itself submit to ESA.

ESA removes empty Engineering Issues automatically in the same persistence transaction. Issues with engineer-reviewed knowledge are never evicted; once solved and unmatched for `issue_retirement_days` they retire to a searchable archive, and others are capped at 100 by least recent access ([retention](ESA_OPERATIONS.md#issue-retention)). Retained reports do not automatically restore evicted Issues. The store preserves snapshots, investigations, runs, solutions and case revisions. ESA keeps its state in its own Postgres database, with one writer and row-level commits (see [Postgres state store](ESA_OPERATIONS.md#postgres-state-store)). Network analysis runs outside transactions. Interrupted executions are marked failed and require explicit retry; old workflow queues wait for a new forwarding.

## State machines

Each entity's state is a type with named constants in `internal/esa/states.go`: `InvestigationState`, `RunState`, `CaseState`, `SolutionState`, `MembershipState` and `ApplicationState`. The same file lists, per entity, the transitions ESA commits. In tests the published-state guard checks every committed change against those tables and fails on any other, naming the entity and the from/to states. Adding a path through a state machine therefore means adding its row; production does not check the tables. Where one entity's state feeds another (a case mirrors its solution, a run projects its investigation), the code converts explicitly.

## Public interface

All `/v1` routes require a token and explicit project scope. The plugin's token must also send the ticket visibility it derives from the logged-in user, and cannot inject events; the optional operator token can ([service tokens](ESA_OPERATIONS.md#service-tokens)). Credentials stay on the backend.

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
| POST | `/v1/tickets/{id}/membership/review` | Confirm/split/reassign membership without analysis |
| GET | `/v1/cases` | Authorized cases |
| POST | `/v1/cases/{id}/review` | Case review |
| POST | `/v1/import/preview` | Import validation; no intake or analysis |

The provider receives `run_id`, frozen `ticket`, `task_contract`, `issue_directory`, `issue_directory_hash`, `budget`, `read_only: true`, and the pipeline `stage` (`route`, `engineer`, `assign`). It receives no inline `known_issues` or `cases`. See [local Issue directory and routing](ISSUE_DIRECTORY.md) for the directory layout, shared mounts, on-demand tools and reuse validation. New engineering findings still require inspected exact source/log evidence and an approved tool plan. In the Codex adapter, the model returns `issue_request`, `findings` and `limitations`; each finding selects inspected `location`/`end_location` and supporting `evidence_ids`, plus `problem`/`solution`. The adapter derives commit/file/line ranges and exact quotes for the public report. Unknown evidence IDs or invalid ranges fail validation; valid citations alone do not prove causality.

Each stage permits one provider invocation within its own `timeout_seconds` deadline (900 s shipped). Waiting for an engineering slot is bounded separately by `worker.max_queue_wait_seconds`. Reuse, joining an existing investigation, or initial clarification calls routing only. Supported new diagnosis calls initial routing, engineering and assignment routing once each; an unlocated result stops after engineering. `report.assignment_routing.usage` records the second routing turn. `report.routing.usage` records routing spend and `report.usage` aggregates all invoked turns. Advisory thresholds are 16,000 weighted input units, 6,000 weighted output units and 22,000 weighted total units. Uncached input and output default to weight 1; cached input, a subset of input, defaults to 0.1. `budget.token_weights` can override all three weights. Raw counts and cache hits remain in stage and aggregate usage; reasoning is already included in output. Final warnings are refreshed from aggregate usage, while stage audits preserve their own warnings. Missing stage usage makes aggregate consumption unknown; missing cache statistics receive no discount. The optional TF-IDF + Logistic Regression classifier remains disabled by default and never controls intake or routing. See [classifier operations](CLASSIFIER.md).

See [manual forwarding](MANUAL_FORWARDING.md) for snapshots, cached/forced requests and upgrade behavior. The independent [system replay tester](TICKET_BACKTEST.md) exercises this public boundary; it never implements a substitute ESA workflow. External fixtures declare explicit human forwarding as test input.
