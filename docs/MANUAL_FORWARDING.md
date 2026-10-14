# Manual forwarding to ESA

ESA accepts only explicitly, manually forwarded ticket snapshots. Classification does not control intake or routing: there are no category rules, human label gates, preset catalogs or Agent classification outputs. The [optional classifier](CLASSIFIER.md) is disabled by default and offers human-confirmed suggestions and independent supervised training only.

## Intake

An authorized Redmine user opens an issue and selects **Forward to ESA**. The plugin writes the current snapshot, the logged-in user's identity, forwarding time and optional force flag to its durable outbox. Go commits the event before acknowledging it. Duplicate event IDs are idempotent; an ID reused with different content or force semantics is rejected. Old source updates cannot overwrite a newer accepted snapshot.

New/edit hooks, import and metadata saves do not forward issues. Imports create ordinary Redmine issues; users forward selected ones separately. Metadata remains local until forwarding. The issue detail can open before the first forwarding even though the backend has no record yet.

`POST /v1/events` requires the usual service token and authorized project scope, plus `forwarded_by` and a nonfuture `forwarded_at` timestamp. This is a trusted integration assertion of a human action; the backend does not independently authenticate a Redmine person from that string. The plugin enforces issue visibility and Submit ESA permission and supplies the logged-in identity. Automated producers must not fabricate forwarding metadata.

```json
{
  "id": "unique-forward-event",
  "origin": "manual_forward",
  "forwarded_by": "engineer-login",
  "forwarded_at": "2026-10-12T01:00:00Z",
  "occurred_at": "2026-10-12T01:00:00Z",
  "force": false,
  "ticket": {"id":"redmine:123","project_id":"1","subject":"Reported failure","updated_at":"2026-10-12T00:30:00Z"}
}
```

A forwarding with incomplete environment/context is accepted and sent to the Agent, which decides whether clarification is needed. An unchanged re-forward uses the cached run. A force flag creates one new run in the same event transaction, not an initial run followed by a second forced run. Replaying that event never spends again.

## Processing

A private snapshot is restricted. A forwarded closed issue is collected as a case draft. Every open forwarded issue enters the Agent path, independent of Tracker or metadata completeness. The service supplies only a project/time-scoped local Issue directory path/hash. Routing Agent searches and reads records on demand; applicable Agent analyses and engineer solutions both skip engineering. A new problem reaches the engineering Agent for diagnosis only; its grouping request returns to Routing Agent for an existing/new Issue decision. Essential missing information can be requested without engineering. See [directory operations](ISSUE_DIRECTORY.md). Exact duplicate diagnostic inputs share one investigation.

Engineering output contains a grouping request, concrete code findings (commit/file/lines/problem/solution/evidence) and limitations. Routing Agent alone supplies the final Issue assignment. It does not request categories or tags. Reports await human review; missing localization can await more information. Approval does not close the Redmine issue. Token warnings remain advisory.

Trusted case approvals become context for subsequent Agent tasks. They can revise an already linked solution within the same scope; they do not automatically match other tickets or replace their investigations. These records describe the last manually forwarded source snapshot. Reopening, editing, closing or changing metadata is not automatically synchronized; forward the updated snapshot to withdraw obsolete case evidence or collect a new case. Normal Redmine visibility continues to restrict page/report access.

## Upgrade and compatibility

The new nullable outbox forwarding columns leave old events intact. The delivery endpoint excludes legacy automatic events without forwarding provenance. Existing stored ticket/events/reports are retained, including historical classification fields for audit, but unforwarded legacy tickets are excluded from the active list. Old queued runs stop at `awaiting_forward` and cannot invoke a provider until a new manual forwarding creates a current run. No historical state is automatically erased or reclassified.

`config/rules.json` is retained as a compatible path and `-rules` remains a CLI option, but the file contains the analysis budget, worker settings and an explicitly disabled optional classifier configuration. There is no rules engine. `/v1/labels`, `/v1/labels/review` and `/v1/labels/examples` have been removed. Legacy classification input fields are discarded from newly accepted snapshots; historical stored fields are archive-only.

Deployment requires rebuilding the ESA binary/image and plugin image and running the plugin migration adding forwarding columns. The code change does not restart an existing deployment automatically. Validate with application tests and the external `esa-api-forwarding.json`, lifecycle and Kafka fixtures before deployment.
