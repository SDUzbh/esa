# External system replay architecture

The current tester is the standalone [`system_replay`](../system_replay/README.md) Go module. It builds outside ESA, has no application imports, and communicates with an opaque system through public HTTP requests. The former direct-provider pipeline, local issue assignment engine and imported-prediction entry point have been removed.

## Responsibilities

| Component | Responsibility |
| --- | --- |
| System under test | Receive manually forwarded tickets and group issues; retrieve cases; route work; invoke engineering providers; expose results and usage |
| External tester | Reconstruct historical inputs; submit/poll public interfaces; freeze outputs; follow Git history; run unchanged scenarios; record assertions and cleanup |
| Product runtime driver | Prepare isolated historical code and dependencies; run observations; reset mutable state; clean owned resources |
| Optional semantic evaluator | Judge frozen initial findings using supplied PR, code and runtime evidence after a repair passes the behavior gate |

The tester does not create substitute business issues or call application functions. Engineering diagnosis remains read-only. The target-owned provider orchestrates engineering requests and Routing Agent membership decisions, then returns final issue assignment and findings with immutable commit, file, start/end lines, problem description and solution recommendation. Restrictions on source writes and debugging tools belong to that provider's registered runtime; an arbitrary remote provider is not sandboxed by the tester.

## Flow and historical boundaries

1. Validate the manifest and reconstruct each ticket at its observation time. Jira history must be complete and count-checked. Later comments, unavailable earlier edited comment bodies, fix versions, links and resolution evidence are excluded from initial inputs. Explicit prepared tickets have operator-supplied provenance.
2. Resolve an explicit Git ref or one affected-version tag to an immutable commit. Author and committer timestamps must not follow the ticket cutoff. Export read-only source and retain file hashes without changing the product checkout.
3. Start a fresh target process, or explicitly attach to an operator-managed service. Prepare any registered product scenario and optional target-owned environment adapter. Only historical ticket/code and observed baseline logs are passed to the target; failure/healthy assertions and future repairs are withheld.
4. Deliver chronological public events and poll actual results. Classification, routing, issue IDs, findings and usage are observed properties. The first event's public observation is the job's initial prediction; later updates, related tickets and receipt-only tests do not replace it.
5. Freeze all public event results, hash the output and verify historical source before reading future Git evidence. After this boundary, resolve the mainline once per run and share successful Git queries across tickets.
6. Walk first-parent mainline history through the next 1 to N release observation points. Exact ticket references, including references in merged branch commits, and changes to finding files create candidates. Track line shifts and renames. Unmerged branch changes and pre-receipt updates are excluded. A release tag outside first-parent history labels a mainline observation point; its binary is not substituted for the evaluated Git code.
7. Observe each candidate's parent and repaired commit in global mainline order. Reuse one runtime, transfer source/dependency differences, and reset mutable state after every observation. Apply the same registered control, bug and healthy assertions at all revisions.
8. Record runtime gates, patch overlap and optional semantic judgments. Cleanup owned resources and write the final summary. Planning, assertions, report persistence and unacknowledged/failed runtime resets cannot produce a successful test exit.

Numeric version ordering is the default in `--pipeline`, but historical ordering takes priority over environment savings. If version order would deliver future events first, the runner rejects it; `--order time` can resolve that conflict. Job event streams cannot overlap and are not automatically interleaved. Related operations belong in one chronological stream. Target business time advances only if a public `advance_clock` capability is configured; otherwise the target still uses wall time.

## What is evaluated

| Evaluation | Evidence | Meaning |
| --- | --- | --- |
| Business assertions | Public receipts and observations | The actual system returned the configured intake, route, state or other behavior |
| Baseline reproduction | Historical code, neutral scenario and control/bug assertions | The registered failure occurs in that historical environment |
| Repair behavior | Baseline + PR parent + repaired observations | The same scenario fails before the change and is healthy after it |
| Patch overlap | Initial finding lines mapped to the candidate parent and diff | A location metric; it does not establish root cause |
| Semantic correctness | Frozen initial finding, original code, PR evidence, runtime logs and independent evaluator | Whether localization, explanation and solution are supported; each can be `correct`, `incorrect` or `unknown` |

A candidate is `behavior_verified` only when scenario hashes match, all three controls pass, baseline and parent bug assertions pass, and repaired healthy assertions pass while repaired bug assertions fail. An environment failure is not a product defect. A parent that no longer reproduces the bug does not itself prove that the initial Agent diagnosis was wrong.

Semantic evaluation is optional and cannot override the behavior gate. It requires full finding coverage, explanations and exact citations from supplied documents. Different repair wording or code from the upstream PR can still be semantically valid. No configured evaluator means semantics remain `unassessed`; there is no built-in model or archive-wide accuracy score.

## Current examples and coverage

| Manifest | Actual coverage | Model calls by default |
| --- | --- | --- |
| `esa-api-lifecycle.json` | Manually forwarded lifecycle events: collect/approve cases without bypassing the Agent path, duplicate and stale events, reopening, manual support requests, denied project submission | 0 |

Both examples start a separate backend on port 18090 with a new state file and disable Redmine synchronization. The Kafka fixture compiles unmodified historical clients and uses the shipped MockClient to inject a disconnected response. It is a client-code replay, not a full broker cluster or Kafka Connect deployment.

For the registered Kafka scenario, the baseline `fc7243c2af4b2b4affcdf14d80826986fe6d4ce8` and upstream parent `8c3c9548b636cdf760d2537afe115942d13bc003` exhibit NullPointerException. Upstream fix `9fb1e25738f89b75a36ef69f730b0e138ccd55b1` (PR #581) instead exhibits TimeoutException with the successful-response control passing. This establishes the registered behavioral change, not an engineering Agent accuracy result.

The backend HTTP adapter does not cover Redmine browser actions or its outbox. Universal scenario generation, customer environment reconstruction, external release listening and generalized per-ticket provider environment registration remain outside current coverage. See [operations](REPLAY_PIPELINE.md) for commands and artifacts, and [the standalone contract](../system_replay/README.md) for manifest details.

Current ESA intake requires explicit `forwarded_by` and `forwarded_at`. External fixtures declare the manual action; the tester does not infer it. Classification and label learning are removed from the active application. See [manual forwarding](MANUAL_FORWARDING.md).

`esa-api-forwarding.json` verifies explicit actor/time requirements, rejection of automatic intake, Agent admission with missing context, category-independent diagnosis and removal of the label endpoint. `esa-api-kafka.json` explicitly forwards the historical ticket and expects an unconfigured provider while independently verifying product repair behavior.
