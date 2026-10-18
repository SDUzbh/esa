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

Semantic evaluation is optional and cannot override the behavior gate. It requires full finding coverage, explanations and exact citations from supplied documents. Different repair wording or code from the upstream PR can still be semantically valid. No configured evaluator means semantics remain `unassessed`; the tester has no built-in model. [Agent evaluation](#agent-evaluation) scores location, routing and cost, not semantic correctness.

## Agent evaluation

`scripts/build_eval_manifest.py` draws a seeded evaluation set from the preserved Kafka export, and `system-replay -score` scores a finished run of it. Building, the fixture run and scoring make no model calls.

| Set | Selection | Expected route |
| --- | --- | --- |
| Fixed bugs (`--bugs`, default 60) | `Bug`/`Fixed`, complete history, one affected version never edited in Jira, whose release tag predates the ticket and holds only regular files; a first-parent trunk commit names the ticket within the next `--versions` (default 3) releases and changes an existing non-test file. Stratified by primary component (components under 2% of the pool share `other`), proportional allocation | `analyze` |
| Duplicates (`--duplicates`, default 10) | Resolved `Duplicate`, created after an eligible original and linked to it, with a usable version of their own. Chosen first; their originals join the bug sample within their stratum's quota and are labelled `selected: duplicate_original` | `reuse` |
| Support requests (`--support`, default 10) | Synthetic questions and administrative requests, each an hour after a sampled bug and on its release | `return_to_support` |

The builder writes `manifest.json`, `labels.json` (seed, labels, pool size per stratum) and `releases.json` (the provider registry for every release in the set) into a new directory. The same seed and inputs produce identical files. Of 5,044 fixed Kafka bugs, 675 are eligible; most of the rest have no single affected version.

Ground truth is only the trace candidates found by an exact ticket reference, never those found because they touch a file the Agent named, so the truth does not depend on the prediction. Tests, documentation, license files and newly added files are not part of a fix; build files are, since a build bug is fixed in one. Each finding is mapped from the commit the Agent read to the fix's parent, as the runner does. Per ticket the scorer reports whether any finding names a fix file (file hit) and overlaps its changed lines (line hit), the share of findings naming a fix file (precision), and the share of fix files named (recall). Only delivered findings count: a failed run's findings were never published. A ticket that delivers none is a miss and is also counted as an abstention, so the report gives the file-hit rate both overall and when the Agent answered. A duplicate must join its original's issue (`issue_id`) to count as reused; when the original produced no issue, there is nothing to reuse and the duplicate is expected to be analyzed instead. A miss whose finding names a file that no longer exists at the fix's parent, as after a rewrite, is flagged for review by hand. The report gives routing accuracy overall and per expected route, the routing confusion matrix, file/line hit rates with 95% Wilson intervals, mean precision/recall with seeded 95% bootstrap intervals, and token, call and wall-time totals, means, medians and 90th percentiles. Observed routes come from the Routing Agent's `routing.action`; a ticket without one is `none`.

```sh
python3 scripts/build_eval_manifest.py --output reports/eval-set           # fixture mode
ESA_DATABASE_URL=... bin/system-replay -pipeline -order time -manifest reports/eval-set/manifest.json -output reports/eval-run
bin/system-replay -score reports/eval-run/pipeline.json -labels reports/eval-set/labels.json -repo kafka -output reports/eval-score
```

Duplicates must replay after their originals, so `-order time` is required. The run keeps a read-only source export per release in `snapshots/`, about 1.3 GB for the default set. The fixture provider always answers `analyze` with an invented file, so a fixture run checks the plumbing, not the Agent: it scores 0% localization and misroutes every duplicate and support request.

To evaluate the real Agent, build the set with `--mode provider --issues-directory DIR` and run it with `scripts/run_agent_eval.py`. It starts `esa-codex` with the set's `releases.json`, the tool catalog and that Issue directory, gives the provider and the managed ESA one fresh analyzer token kept only in their environments, runs the replay and stops the provider. Choose `--tool-catalog PATH` or `--no-tool-catalog` explicitly: without a catalog of pinned dependencies (for Kafka, the client JARs such as `slf4j-api`, `snappy-java` and `lz4`) every dependency request is denied and the Agent can only read source, never build or run it. **This makes real model calls for every ticket.** Run a small pilot (`--bugs 10 --duplicates 2 --support 2`) to measure cost per ticket before a full set. Per-ticket wait defaults to 1,800 s in this mode. Start a long run with `nohup` so it outlives the terminal; SIGTERM stops the replay, then the provider.

```sh
export ESA_DATABASE_URL=...
python3 scripts/build_eval_manifest.py --mode provider --issues-directory reports/eval/issues --output reports/eval/set
nohup python3 scripts/run_agent_eval.py --set reports/eval/set --tool-catalog runtime/esa/tool-catalog.json --codex /path/to/codex --output reports/eval/run-1 &
bin/system-replay -score reports/eval/run-1/run/pipeline.json -labels reports/eval/set/labels.json -repo kafka -output reports/eval/score-1
```

## Current examples and coverage

| Manifest | Actual coverage | Model calls by default |
| --- | --- | --- |
| `esa-api-lifecycle.json` | Case collection/approval, duplicate and stale events, reopening and authorization; open tickets await provider configuration, without automatic case reuse | 0 |
| `esa-api-forwarding.json` | Manual actor/time requirements, rejection of automatic intake, missing-context admission and removed legacy label endpoint | 0 |
| `esa-api-kafka.json` | Historical ticket intake with an unconfigured provider, plus independent baseline/parent/fix runtime checks | 0 |
| `esa-shared-investigations.json` | Deterministic HTTP provider: shared investigation identity, pending proposals and immutable engineer solution revisions | 0 |

These examples start a separate backend on port 18090 with a new state file and disable Redmine synchronization. The Kafka fixture compiles unmodified historical clients and uses the shipped MockClient to inject a disconnected response. It is a client-code replay, not a full broker cluster or Kafka Connect deployment.

For the registered Kafka scenario, the baseline `fc7243c2af4b2b4affcdf14d80826986fe6d4ce8` and upstream parent `8c3c9548b636cdf760d2537afe115942d13bc003` exhibit NullPointerException. Upstream fix `9fb1e25738f89b75a36ef69f730b0e138ccd55b1` (PR #581) instead exhibits TimeoutException with the successful-response control passing. This establishes the registered behavioral change, not an engineering Agent accuracy result.

The backend HTTP adapter does not cover Redmine browser actions or its outbox. Universal scenario generation, customer environment reconstruction, external release listening and generic registration of arbitrary target-owned environments through this tester remain outside current coverage. The application provider can provision its own registered-source workspace; this backend-API fixture does not test that integration. See [operations](REPLAY_PIPELINE.md) for commands and artifacts, and [the standalone contract](../system_replay/README.md) for manifest details.

Current ESA intake requires explicit `forwarded_by` and `forwarded_at`. External fixtures declare the manual action; the tester does not infer it. ESA has no category-based intake gate. Its optional supervised label classifier is disabled by default and does not control routing. See [manual forwarding](MANUAL_FORWARDING.md).

The shared-investigation fixture verifies orchestration with synthetic provider responses; it does not test model judgment. No default example proves Agent case matching or localization accuracy; that needs a provider-mode [Agent evaluation](#agent-evaluation) run.
