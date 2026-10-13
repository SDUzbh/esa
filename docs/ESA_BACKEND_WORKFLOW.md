# ESA backend workflow

ESA uses one workflow with immutable snapshots, shared engineering investigations, reviewed solutions and approved Profile planning. See [ESA operations](ESA_OPERATIONS.md) for deployment, persistence, tools and validation.

ESA receives manually forwarded Redmine issues through a Ruby plugin and an independent Go service. Classification is removed from the active workflow. New code and interface text remain English; engineering diagnosis is read-only.

1. An authorized user forwards the current issue snapshot. The plugin records the actor/time/force choice in its durable outbox.
2. Go validates explicit forwarding, commits the immutable event, handles duplicate/stale input and acknowledges only after persistence.
3. Private snapshots are restricted; forwarded closed snapshots become case drafts.
4. Open snapshots retrieve trusted historical cases within the project, checking component, version, environment and specific symptoms.
5. Applicable cases provide suggestions without model calls. Otherwise missing context enters `awaiting_information`; complete context links to a matching investigation or queues one shared engineering task.
6. A configured provider returns evidence, code findings, solutions and verification steps for human review. An absent provider produces `awaiting_configuration` with no call. The Agent does not classify tickets.
7. Engineers review solution revisions and approve complete case evidence. Recommendations refresh only for forwarded tickets. Closing/editing a Redmine issue does not itself submit it to ESA.

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

The provider receives `run_id`, frozen `ticket`, candidate `cases`, `task_contract`, eligible `known_issues`, `budget` and `read_only: true`. It plans tools within approved Profiles; labels do not control intake. The Codex output schema requires issue assignment, findings and limitations; exact source/log evidence remains validated.

The analysis budget permits one provider invocation and a separate 900-second deadline. Input 16,000, output 6,000 and total 22,000 tokens generate advisory warnings; missing usage is unknown. The former `/v1/labels` classification endpoints are removed. An independent optional TF-IDF + Logistic Regression module adds `/v1/tickets/{id}/classifier` suggestions and human feedback with a separate training CLI; it is disabled by default and never changes intake, routing or Agent contracts. See [classifier operations](CLASSIFIER.md). Historical classification fields are retained solely for audit.

See [manual forwarding](MANUAL_FORWARDING.md) for snapshots, cached/forced requests and upgrade behavior. The independent [system replay tester](TICKET_BACKTEST.md) exercises this public boundary; it never implements a substitute ESA workflow. External fixtures declare explicit human forwarding as test input.
