# ESA Backend Workflow

Status: Go service and Redmine integration implemented as version 0.1.0. All new code, comments, documentation, errors, and plugin UI use English.

## Architecture

Redmine owns users, permissions, projects, issues, and issue statuses. Its Ruby plugin owns the ESA pages, immutable upload records, supplemental metadata, and durable event outbox. The independent Go service owns events, classification, routing, analysis runs, reports, case revisions, and recommendations. No Redmine core source is patched.

The outbox is polled every five seconds. Go commits an event before acknowledging it. A lost acknowledgement can safely be retried because event IDs are idempotent. Incoming source timestamps reject stale updates. Processing preserves each diagnostic input rather than reading a mutable issue during a running analysis.

The first version uses the Go standard library, explicit state transitions, versioned JSON contracts, and synchronous durable commits. A worker performs network analysis outside the store lock. A single-process lock prevents multiple writers to one state file. Interrupted provider executions fail visibly and require manual retry after checking provider activity.

## Decision flow

1. Preserve the incoming event and normalize its explicit fields.
2. Classify through human overrides and ordered keyword/tracker rules. Multiple categories remain unknown; a Bug tracker alone does not establish a code defect.
3. Retrieve trusted case revisions visible at the observation time and within the same project.
4. Require overlapping components and affected versions, compatible environment requirements, and a specific diagnostic signal or sufficient keyword evidence for applicability. Generic timeout/OOM words cannot establish a shared root cause.
5. Reuse an applicable case without a model call. Otherwise request missing information, route non-engineering categories to support, or queue an allowed engineering category.
6. A configured provider returns candidate causes, evidence, recommendations, and verification steps. Reports await engineer review; ESA never automatically closes the Redmine issue.
7. Closed issues are collected regardless of whether engineering analysis ran. Copy explicit conclusions without inventing facts. Duplicate and rejected resolutions do not become trusted fixes.
8. Engineers approve complete case evidence. New applicable cases refresh recommendations without initiating extra model calls. Reopening or changing a trusted source withdraws stale recommendations.

Initial engineering categories are `code_defect`, `compatibility`, and `performance`. Edit `config/rules.json`, increment its version, and restart the service to change this configuration. Per-project rules and a rule administration screen are future work.

## Cost controls

Classification, case retrieval, routing, and template summaries use zero model calls. Cache keys include diagnostic input, workflow version, rule version, and trusted-case generation. Assignee-only updates reuse the previous run. Self-originated ESA events do not trigger analysis.

Rule version 3 permits one provider invocation and a separate 900-second execution deadline. Input 16,000, output 6,000 and total 22,000 tokens are advisory warning thresholds, not rejection limits. Large prompts or measured usage above these values do not terminate or invalidate the diagnosis. Warnings are preserved in reports and displayed in ESA. The Codex adapter also emits structured audit alerts and stderr warnings, including for an invalid final report. CLI usage may only become available at turn completion; missing usage is unknown. A forced manual retry creates a new invocation and can spend again.

## HTTP contract

All `/v1` routes require `Authorization: Bearer <service token>` and an explicit `X-ESA-Project-IDs` scope. The plugin additionally supplies `X-ESA-Ticket-IDs` derived from `Issue.visible(User.current)`. The service token is a backend credential, not an end-user API key. Never expose the Go port publicly or let a browser supply these authorization headers.

| Method | Endpoint | Purpose |
| --- | --- | --- |
| GET | `/healthz` | Process health |
| POST | `/v1/events` | Commit a source event |
| GET | `/v1/tickets` | Authorized list, filters, sort, pagination |
| GET | `/v1/tickets/{id}` | Current ticket, report, and run history |
| POST | `/v1/tickets/{id}/analyze` | Cached or forced manual analysis |
| GET | `/v1/runs/{id}` | Analysis run |
| POST | `/v1/runs/{id}/review` | Approve or reject an engineering report |
| GET | `/v1/cases` | Authorized current cases; prototype endpoint |
| POST | `/v1/cases/{id}/review` | Approve or reject a case |
| POST | `/v1/import/preview` | Parse and validate UTF-8 JSON/CSV |

See exported JSON fields in `internal/esa/model.go`. Redmine ticket IDs are `redmine:<issue ID>`. An event has `id`, `origin`, `occurred_at`, and `ticket`. Preview accepts `filename`, `project_id`, and `content`. Reviews require `action` (`approve` or `reject`), `reviewer`, and optional `note` and structured `resolution`.

The provider receives a POST containing `run_id`, `ticket`, `classification`, historical candidate `cases`, `budget`, and `read_only: true`. Return HTTP 200 and a Report object:

```json
{
  "summary": "A root-cause candidate requiring engineer verification.",
  "root_cause_candidates": ["Candidate with explicit uncertainty"],
  "evidence": [{"source": "git", "reference": "<commit>:<path>:<line>", "quote": "Observed source behavior"}],
  "recommendations": ["Proposed repair or workaround"],
  "verification_steps": ["Reproduction and regression checks"],
  "limitations": ["Missing or unverified evidence"],
  "usage": {"calls": 1, "input_tokens": 1200, "output_tokens": 500, "tokens_reported": true}
}
```

Evidence completeness checks establish a reviewable structure, not factual correctness. Human review and later ground-truth evaluation are essential.

## Replay

`replay_test` uses the same `Engine.Decide` function as live processing. `CasesAt` selects the latest case revision known at time T, including its approval or withdrawal time. Tests demonstrate that future approvals cannot leak into earlier recommendations.

The independent [`system_replay`](../system_replay/README.md) module tests the system externally. It submits events and polls public APIs; it neither imports `Engine.Decide` nor reproduces ESA's classification, issue assignment or case retrieval. It freezes the first observed diagnosis for each job before examining future mainline Git evidence. Subsequent lifecycle events remain audit/test evidence and cannot replace that initial prediction.

Registered product scenarios run at the historical baseline, candidate parent and candidate repaired revision. A repair requires unchanged assertions, reproduced baseline/parent failure, passing controls and healthy repaired behavior. This runtime gate and optional external semantic evaluation are described in [system replay architecture](TICKET_BACKTEST.md); commands, ordering and cleanup are in [pipeline operations](REPLAY_PIPELINE.md).

The focused [Codex trial](CODEX_REPLAY_TRIAL.md) remains an application-provider configuration example. Current external test manifests do not invoke Codex by default. Universal build profiles, recovery of unavailable comment revisions and archive-wide scoring remain future work. Current live imports must not be passed directly to replay as historical truth.

## Operations and limits

The current JSON store rewrites the complete snapshot on mutations and is appropriate for small local trials. Use a database-backed repository for the full Jira archive. The outbox retains acknowledged events for audit and currently needs manual intervention for a permanently invalid event. No automatic retention or external notification is configured. Recommendations appear inside ESA; they do not send email or customer messages.

Private issues are excluded from shared case handling. Permission-scoped responses withhold a report if a case it references is outside the user's issue visibility. Original customer text is not rewritten. Supplemental metadata and reviews are separate records.
