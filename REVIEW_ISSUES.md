# ESA — Review Findings for Implementation

Source: architecture review of this repository at commit `7897f1b` (2026-10-16).
Audience: Claude Code (or any engineer) fixing these issues.

## How to work through this file

1. **Verify before fixing.** Each issue lists evidence. Re-check it against the current code first; line numbers may have drifted. If a finding turns out to be wrong, say so and skip it.
2. **Test first.** For every bug, add a failing test that reproduces it, then fix it. Tests that already exist must keep passing.
3. **One issue per commit/branch.** Do not combine the storage redesign (Issue 1) with the smaller fixes.
4. **Always run before finishing:**
   ```sh
   GOCACHE="$PWD/.tools/go-cache" GOTOOLCHAIN=local .tools/go/bin/go test -race ./...
   GOCACHE="$PWD/.tools/go-cache" GOTOOLCHAIN=local .tools/go/bin/go vet ./...
   GOCACHE="$PWD/.tools/go-cache" GOTOOLCHAIN=local .tools/go/bin/go -C system_replay test -race ./...
   sh scripts/check_workflow.sh   # full CI equivalent (gofmt, vet, tests, replay fixtures)
   ```
   The module requires Go 1.27 (`internal/capabilities/planner.go` uses `os.Root.ReadFile`).
5. **Never invoke a real Agent** (Codex CLI / model calls) while testing. Use the existing fake providers in the tests.
6. **Ask the user before** any of these:
   - adding an external Go dependency (the project currently has none)
   - changing the public HTTP API contract between the plugin and the service
   - changing persisted state formats without a migration

## Baseline at review time

- `go test -race ./...` passes.
- Coverage:

  | Package | Coverage |
  |---|---|
  | `internal/esa` | 79.6% |
  | `internal/codex` | 75.5% |
  | `internal/classifier` | 84.6% |
  | `internal/retrieval` | 88.7% |
  | `internal/replay` | 78.1% |
  | `internal/replaytools` | 49.5% |
  | `internal/codexreview` | 0% |

- `system_replay`: 64.9%.
- Python tests: 21/21 pass.

## Priority overview

| # | Issue | Severity | Type | Verified how |
|---|---|---|---|---|
| 1 | Whole-state JSON store: full deep copy on every read and write, and it grows without limit | Critical | Architecture / performance | Measured with a probe test |
| 2 | One permanently invalid outbox event blocks all later forwarding | High | Bug | Reproduced with a probe test |
| 3 | Waiting for an engineering slot uses up the task's execution deadline | High | Bug | Code reading (no test yet) |
| 4 | 100-Issue global LRU cap evicts engineer-approved knowledge from retrieval | Medium–High | Design | Code reading |
| 5 | One shared bearer token across all trust boundaries; scope comes from caller-supplied headers | Medium | Security | Code / config reading |
| 6 | State machines as untyped strings, very long conditions, prompts inside Go source | Medium | Maintainability | Code reading |
| 7 | No measurement of whether the Agents diagnose correctly | Medium (product risk) | Evaluation | Inspected reports |
| 8 | Small items (healthcheck port, test coverage gaps, tool enforcement) | Low | Misc | Code reading |
| 9 | One mistyped evidence ID throws away the whole engineering report; IDs are long random strings | Medium–High | Bug / robustness | Real-provider evaluation, code reading |
| 10 | Provider quota exhaustion fails tickets and discards finished stages | Medium–High | Robustness | Real-provider evaluation, code reading |
| 12 | Engineering finds a release/evidence mismatch only after a full analysis | Low–Medium | Design / cost | Real-provider evaluation |
| 13 | (Archived) The same ticket gets different outcomes between runs; single-run numbers include that noise | Low–Medium | Measurement | Real-provider evaluation, code reading |
| 14 | (Finished) About 300k input tokens per analyzed ticket; about half of the reading tools' output is bookkeeping; no enforced budget | Medium | Cost | Real-provider evaluation, run logs |
| 15 | (Finished) Routing search is untested at realistic directory sizes: it browses, searches identifiers that never match, and matches any word | Medium | Design / evaluation risk | Run logs, code reading |
| 17 | A provider CLI call can block forever after its timeout, holding an engineering slot | High | Robustness / availability | Code audit, reproduced with a fake CLI |
| 18 | One event line over 2 MB stalls a provider stage until its 900 s timeout | Medium | Robustness | Code audit, reproduced with a fake CLI |
| 19 | Import preview turns a 12 MB upload into a ~500 MB heap spike; very wide CSV headers are accepted | Medium | Memory / DoS by authenticated users | Code audit, measured |
| 20 | Cold reads (snapshots, closed-ticket text) share one Postgres connection behind a mutex | Low | Performance | Code audit |
| 21 | `read_code` has no bound on total output size | Low | Memory | Code audit |
| 22 | A few request paths scan whole tables (`GET /v1/cases`, classifier view and feedback, legacy title source) | Low | Performance | Code audit |
| 23 | A regular expression is compiled on every Maven install | Low | Performance | Code audit |

Suggested order: **2 → 3 → 1 (plan first, then increments) → 4 → 5 → 6 → 8**. Issue 7 is separate evaluation work. Issues 9, 10 and 12–14 came out of the Issue 7 evaluation runs (2026-10-19); suggested order **9 → 10 → 14 → 13 → 12**. Issue 15 came from reading the routing runs afterwards; do its offline measurement before changing routing search. Issue 13 is archived (2026-10-21). Issues 17–23 came from the code audit (2026-10-21); suggested order **17 → 18 → 19 → 21 → 22 → 20 → 23** (17 and 18 touch the same function, `invoke`, and fit one change). Issue 11 was removed: its one example (KAFKA-4562) turned out to be a valid re-analysis of a ticket whose original had a wrong version field.

---

## Issue 1 — The whole-state JSON store does not scale (Critical)

### Where

| Location | What it does |
|---|---|
| `internal/esa/store.go` `Store.Update` (~L142) | Wraps every mutation |
| `internal/esa/store.go` `Store.updateLocked` (~L159) | Copies the whole state with `json.Marshal` and `json.Unmarshal`, runs the callback, marshals again, compares bytes, and writes the whole file |
| `internal/esa/store.go` `Store.Snapshot` (~L224) | Copies the whole state on every read |
| `internal/esa/http.go`, `investigation_http.go`, `classifier.go` | About 20 HTTP handlers call `Store.Snapshot()`, even to read one ticket |
| `internal/esa/issue_retention.go` `Engine.update` (~L13) | Runs `maintainIssues` (a full scan) on every mutation |
| `internal/esa/model.go` `State` | Append-only maps and slices that are never pruned (see below) |
| `plugins/redmine_esa/app/models/esa_backend_state.rb` `commit!` | The Postgres adapter receives the whole state on every commit and re-parses each entity record |
| `plugins/redmine_esa/app/controllers/esa_state_controller.rb` (~L19) | 64 MiB request limit |

The `State` fields that only ever grow are:

- `Events`: each event holds a full `Ticket` copy plus `Raw`
- `Receipts`
- `Snapshots`
- `Runs`: each holds a full `Report`
- `CaseHistory`
- `MembershipHistory`

### Background load that multiplies the cost

- `cmd/esa/main.go` starts `EngineeringConcurrency+1` (4) worker loops. Each calls `engine.Refresh()` and `ProcessOne` every second.
- `pipeline.go` (~L83) polls every 250 ms while waiting for engineering capacity. Each attempt runs a store `Update`.
- `ProcessOne` sends a heartbeat every `LeaseSeconds/3` (about 20 s). Each heartbeat is a full-state commit.
- Every authenticated HTTP request calls `Engine.Refresh()` (`http.go` ~L146). With the Postgres backend, that is a round trip to Redmine.

### Evidence (probe test, 2-vCPU Linux container)

Setup: closed tickets with a description of about 2.2 KB each.

| Tickets | State size | Ratio to raw ticket bytes | `Snapshot()` | One `Receive()` |
|---|---|---|---|---|
| 250 | 3.7 MB | 5.3× | 0.20 s | 0.30 s |
| 1,000 | 14.7 MB | 5.3× | 0.74 s | 0.96 s |

- Cost is linear per operation, so ingesting N tickets is O(N²) in total. Ingesting 2,000 tickets did not finish within 10 minutes.
- The local Jira archive (`data/jira/raw`) holds **19,869** Kafka issues. Extrapolated, that is about 290 MB of state and roughly 15 s per read or write.
- The Postgres backend's 64 MiB frame limit would be hit at about 4,000–5,000 closed tickets. Open tickets with runs and reports are larger.

Benchmark to add (as `internal/esa/store_bench_test.go`, so the improvement can be measured):

```go
func BenchmarkSnapshotAndReceive(b *testing.B) {
	for _, n := range []int{250, 1000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			e := testEngineB(b) // same as testEngine in workflow_test.go but taking testing.TB
			desc := strings.Repeat("stack trace line at org.apache.kafka.Foo(Foo.java:123)\n", 40)
			for i := 0; i < n; i++ {
				tk := fixture(e, fmt.Sprintf("K-%d", i))
				tk.Description, tk.Closed = desc, true
				tk.Resolution = Resolution{Reason: "fixed", Cause: "c", Solution: "s", Validation: "v"}
				if _, err := e.Receive(Event{ID: fmt.Sprintf("ev-%d", i), Origin: "t", ForwardedBy: "u", ForwardedAt: e.Now(), Ticket: tk}); err != nil {
					b.Fatal(err)
				}
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = e.Store.Snapshot()
			}
		})
	}
}
```

(`testEngine` and `fixture` take `*testing.T`. Generalize them to `testing.TB`, or add a helper for benchmarks.)

### Required approach

**Write a design plan and get the user's approval before implementing.** This is a redesign, not tuning. A suggested sequence:

1. **Quick wins that keep the format** (each one a separate commit, each verified with the benchmark):
   - Read paths stop deep-copying the whole `State`. Add narrow accessors under `RLock`, such as `Store.Ticket(id)`, `Store.Run(id)` and `Store.TicketRows(filter)`. They copy only what they return.
   - `updateLocked` stops round-tripping the whole state through JSON to clone it and to detect changes. Options: copy-on-write per top-level map, or have callbacks report whether they changed anything.
   - Run `maintainIssues` only when Issues, memberships, cases or solutions changed, not on every mutation.
   - Replace the 250 ms capacity polling with a notification (`sync.Cond`/channel within the process) plus a slow fallback poll.
2. **Retention policy for append-only data.** Agree on a policy with the user first.
   - Candidates: store `Event.Ticket` and `Raw` once (link events to snapshot IDs instead of copying); cap `Receipts`; archive old `Runs` reports to separate files.
   - Immutable audit data may go to append-only files or tables. It does not need to sit in the hot state.
3. **Real storage.**
   - Use row-level tables: tickets, snapshots, events, runs, investigations, memberships, solutions, cases, plus history tables.
   - Each operation runs in its own transaction and touches only the rows it needs.
   - Claim work with a proper queue (`SELECT … FOR UPDATE SKIP LOCKED`, or SQLite with a single writer).
   - Keep the JSON store only as a test or dev adapter behind the same interface.
   - Decide with the user whether ESA owns its database directly, rather than going through Redmine's HTTP state API. Today Redmine depends on ESA for the UI and ESA depends on Redmine for state, which is a circular dependency.
   - Adding a DB driver means adding an external dependency, so ask first.

### Acceptance criteria

- The benchmark shows reads that do not grow with the total number of tickets (single-ticket GET under a few ms at 10k tickets).
- Ingesting 20k synthetic tickets finishes in minutes, not hours.
- All existing workflow, CAS, lease/epoch and replay tests pass. No behaviour change in `system_replay` assertions.
- The idempotency, CAS fencing and crash-safety guarantees are kept, with a test for each.

---

## Issue 2 — A poison event blocks the Redmine outbox forever (High)

### Where

- `internal/esa/redmine.go` `SyncRedmine` (~L16–L64). At ~L44, if `e.Receive(event)` returns any error, the function returns immediately without acknowledging the event.
- `plugins/redmine_esa/app/controllers/esa_events_controller.rb` `index` (~L6) always returns the oldest 20 unacknowledged events, ordered by id.

### Failure scenario

`Receive` returns permanent errors in several cases, for example:

- `"ticket belongs to a different project"` (`ErrConflict`), when an issue is moved to another Redmine project and forwarded again
- `"event ID was reused with different content"`
- `"snapshot source time exceeds forwarding cutoff"` (`ErrInvalid`)

The failing event is never acknowledged. Every 5 s the poll fetches it again and fails again. **No later event from any user or project is ever processed.** The only symptom is a `slog.Warn` every 5 seconds.

### Reproduction (probe — confirmed failing behaviour)

```go
func TestSyncRedminePoisonEventDoesNotBlockLaterEvents(t *testing.T) {
	e := testEngine(t)
	receive(t, e, "seed", fixture(e, "T1"))
	moved := fixture(e, "T1")
	moved.ProjectID = "2" // moved to another project, forwarded again
	good := fixture(e, "T2")
	var mu sync.Mutex
	acked := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/ack") {
			var b map[string]string
			_ = json.NewDecoder(r.Body).Decode(&b)
			mu.Lock(); acked = append(acked, b["event_id"]); mu.Unlock()
			_, _ = w.Write([]byte(`{}`))
			return
		}
		// A real outbox would drop acked events; this fake always returns both.
		_ = json.NewEncoder(w).Encode(map[string]any{"events": []Event{
			{ID: "redmine_event_1", Origin: "manual_forward", ForwardedBy: "u", ForwardedAt: e.Now(), Ticket: moved},
			{ID: "redmine_event_2", Origin: "manual_forward", ForwardedBy: "u", ForwardedAt: e.Now(), Ticket: good},
		}})
	}))
	defer srv.Close()
	for i := 0; i < 3; i++ {
		_ = e.SyncRedmine(context.Background(), srv.URL, "tok", srv.Client())
	}
	if _, ok := e.Store.Snapshot().Tickets["T2"]; !ok {
		t.Fatal("valid event behind a permanently rejected event was never received")
	}
}
```

Current behaviour: `acked=[]` and T2 is never received.

### Suggested fix

- Separate **permanent** rejections (`ErrInvalid`, `ErrConflict` from validation) from **transient** failures (store unavailable, CAS contention, network). Use `errors.Is` together with a dedicated sentinel; do not match error strings.
- For permanent rejections, record a rejection receipt in ESA state (event ID, reason, time) so retries stay idempotent, then send the ack with a rejection status.
- Extend the plugin:
  - Add a migration with columns `rejected_at` and `rejection_reason` on `esa_events`.
  - The ack endpoint accepts `{event_id, status: "accepted"|"rejected", reason}`.
  - Show rejected forwards to the forwarding user in the Redmine UI, so they know to fix the issue and forward again.
- For transient errors, keep the current retry behaviour, but only for that event. Consider continuing with the rest of the batch only if ordering per ticket is preserved: stale-event detection relies on `UpdatedAt`, so skipping ahead across tickets is safe, but within one ticket it must stay ordered.
- Check whether a clock-skew rejection (`ForwardedAt.After(e.Now())` in `Receive`) should be transient. It currently returns `ErrInvalid`. Either allow a small skew tolerance or classify it as transient.

### Acceptance criteria

- The test above passes.
- A rejected event is acknowledged exactly once and visible in the plugin.
- The existing idempotency tests (duplicate event ID with the same content) still pass.

---

## Issue 3 — Waiting for engineering capacity burns the task deadline (High)

### Where

- `internal/esa/shared_worker.go` `ProcessOne`: ~L200 creates `callCtx` with `Budget.TimeoutSeconds` (900 s in `config/rules.json`) **before** calling `e.pipeline(callCtx, …)` (~L252).
- `internal/esa/pipeline.go` `pipeline`: the capacity wait loop (~L83–L127) waits on `ctx.Done()`, which is that same `callCtx`.

### Failure scenario

1. Three engineering runs occupy all `EngineeringConcurrency = 3` slots, each running for several minutes.
2. A fourth task is routed with action `analyze` and moves to `waiting_engineering`.
3. Its 900 s deadline keeps running while it waits. If the slots stay busy long enough, `callCtx` expires.
4. The task is marked `failed` with spend "uncertain", even though no engineering call was made. A human then has to retry it explicitly.

The waiting task also occupies one of the 4 worker goroutines, and its 250 ms polling loop adds load to the store (see Issue 1).

### Suggested fix

- Give each stage its own deadline. The routing stage and the engineering stage each get their own timeout, and the engineering clock starts when the slot is reserved.
- Bound queue waiting separately with its own configurable `max_queue_wait_seconds`. When it is exceeded, mark the task as something like `queued_timeout`, not as a failed invocation with uncertain spend.
- Keep the lease heartbeat running while waiting, as it does today.
- Better (and ties into Issue 1): when capacity is full, stop holding a worker goroutine. Persist the routing result, mark the task `waiting_engineering`, and return. Have `ProcessOne` pick up tasks waiting for engineering when a slot frees up.

### Test to add

Use a fake `PipelineAnalyzer` (see the existing fakes in `internal/esa/pipeline_test.go`):

- The `engineer` stage blocks on a channel for 3 tasks.
- Set `Budget.TimeoutSeconds` small (for example 2 s).
- Forward a 4th ticket that routes to `analyze`, hold the slots longer than the timeout, then release them.
- Assert that the 4th task is not `failed` because of the timeout and reaches `agent_proposed` after a slot frees up. A fake clock or short real timeouts are both fine; the `-race` run must stay under the CI 20-minute limit.

---

## Issue 4 — The global 100-Issue LRU cap evicts reviewed knowledge (Medium–High)

### Where

- `internal/esa/issue_retention.go`: `const IssueLimit = 100` (~L9), and `maintainIssues` (~L52) evicts least-recently-accessed Issues across all projects (~L119).
- `internal/esa/issue_directory.go` `issueRecords` (~L76–L82): any record whose `IssueID` no longer exists in `s.Issues` is skipped. So once an Issue is evicted, its solutions and cases (including engineer-reviewed `trusted` ones) are no longer offered to the Routing Agent.
- Side effect: `Engine.ReviewCase` returns `ErrConflict "engineering issue was removed"` for cases linked to an evicted Issue.

### Why it matters

The main value of the system is reusing reviewed solutions. A cap of 100 shared by every project means that, in a busy project, approved knowledge silently disappears from routing.

### Suggested direction (confirm with the user; this is a product decision)

- Never evict Issues that hold a `trusted` solution or an approved case. Or make the cap per project and configurable in `rules.json`.
- If the cap exists to keep the per-run exported directory small, solve that in retrieval instead (BM25 top-k, pagination already exists) rather than by deleting from the knowledge base.
- When an Issue is evicted, keep its trusted solutions retrievable, or archive them in a way `issueRecords` can still read.
- Add a test: approve a case, create more than `IssueLimit` newer Issues, and assert the trusted record is still in `issueRecords` (or that whatever the agreed behaviour is happens).

---

## Issue 5 — One shared token; scope is asserted by the caller (Medium)

### Where

- `deploy/redmine/compose.yaml`: `ESA_ANALYZER_TOKEN: ${ESA_API_TOKEN}` (~L64). The same secret is used for all of the following:
  - the ESA service API (`internal/esa/http.go` ~L138)
  - the Redmine outbox API (`esa_events_controller.rb`)
  - the Redmine **state write** API (`esa_state_controller.rb`)
  - the analyzer / Codex provider (`internal/codex/http.go`)
- `internal/esa/http.go`:
  - `projectScope` (~L53) trusts the `X-ESA-Project-IDs` header.
  - `ticketAllowed` (~L71) returns **true when the `X-ESA-Ticket-IDs` header is absent**, meaning all tickets are allowed.
- Visibility is passed as a list of IDs in a header (`plugins/redmine_esa/lib/esa/client.rb`). The plugin therefore caps requests at 2,000 visible issues (`esa_controller.rb` ~L191), and the service has `MaxHeaderBytes: 32 KiB`.

### Impact

Anyone holding the token, including the analyzer host that runs Codex, can:

- read every project
- write arbitrary authoritative state through the Redmine state API
- inject forwarding events

### Suggested fix

- Use separate secrets per role:
  - plugin → ESA
  - ESA → Redmine outbox
  - ESA → Redmine state API
  - ESA → analyzer
- Update the compose file, `.env.example`, `docs/DEPENDENCIES.md` and `scripts/verify_deployment.py` to match.
- Make `ticketAllowed` deny by default, or make the header mandatory on routes that return evidence from other tickets. Check that the plugin always sends it where needed. Add tests.
- Longer term (with Issue 1): let ESA evaluate visibility from stored project/ACL data, or let the plugin pass a signed, short-lived scope token, instead of long ID lists in headers.

---

## Issue 6 — Maintainability (Medium)

### Findings

- About 22 distinct state strings are used across investigations, runs, cases, solutions and memberships, for example `"queued"`, `"running"`, `"investigating"`, `"agent_proposed"`, `"awaiting_configuration"` and `"reuse_proposed"`. There are no typed constants and no transition table, so a typo compiles fine.
- 138 non-test Go lines are longer than 160 characters, 66 longer than 250, and 29 longer than 400. Most are long `if` conditions. Examples:
  - `validateReport` and `ProcessOne` in `shared_worker.go`
  - `joinInvestigation` in `pipeline.go`
  - `NewEngine` in `engine.go`
- Agent prompts and instructions are inlined as Go string literals. `internal/codex/routing.go` ~L121 is a single 2,123-character line. Prompts are not versioned or reviewable as separate files.
- `WorkflowVersion = "10"` (`model.go`) after about 6 days of history. The contract churns a lot.

### Suggested fix (one change at a time, no behaviour change)

- Introduce typed states, for example `type InvestigationState string` with constants, plus a `validTransition(from, to)` table per entity. Add a test that walks all allowed transitions.
- Pull long conditions out into named predicate functions, such as `isRoutingStageValid(report)` and `canJoin(target, task, …)`.
- Move prompts and instructions into versioned files under `internal/codex/prompts/` using `embed`. Include a prompt version in the run audit.
- Keep `gofmt` and `go vet` clean. These are pure refactors: no API or state-format changes.

---

## Issue 7 — Agent accuracy has not been evaluated (Medium, product risk)

### Findings

- `runtime/ui-acceptance-20261014/codex-runs` holds 7 real Agent runs. Their output looks plausible, but nobody has scored it.
- `reports/replay-validation-*/SUMMARY.md` states that no model calls were made. The Kafka `repair_behavior_verified` result proves the harness works, not that an Agent diagnosis was correct.
- The README's "Current limits" section says this honestly.

### Suggested work (do not run real Agents without the user's explicit go-ahead; it costs money)

- Build an evaluation manifest for `system_replay` covering 50–100 historical Kafka tickets that have known fix commits.
- Score each diagnosis on:
  - whether its file/line localization overlaps the actual fix diff
  - whether the routing decision was correct (reuse / analyze / return to support)
  - tokens and time per ticket
- Report precision and recall-style metrics with confidence intervals. Keep predictions frozen before looking at future history, as the harness already does.

---

## Issue 8 — Small items (Low)

1. **The healthcheck ignores `-listen`.** `cmd/esa/main.go` ~L41 always calls `http://127.0.0.1:8090/healthz`. Derive the URL from the `-listen` flag, or add a `-healthcheck-url` flag.
2. **Tool enforcement is after the fact.** `internal/codex/invoke.go` ~L115 cancels the run only after it sees a disallowed item in the event stream. Real prevention depends on the Codex CLI feature flags in `CLIArgs` (`internal/codex/provider.go` ~L107), and those are tied to the CLI version.
   - Add a startup self-check: run the CLI's config or feature dump, and refuse to start if any disabled feature is unknown or still enabled.
   - Pin the CLI version in `deploy/tools/toolset.release.txt`, or wherever it is managed.
3. **The container isolation check is conditional.** In `internal/replaytools/server.go` `owned` (~L157), the read-only filesystem, no network, dropped capabilities, non-root and resource limit checks only run when `ProfilesRoot != ""`. Otherwise only the ownership label is checked. Consider always applying the isolation checks before `run` / `install_environment`.
4. **Coverage gaps.**
   - `internal/codexreview` has 0%; add tests for `validate.go`.
   - `internal/replaytools` is at 49.5%; focus on `capabilities.go` and the `Call` error paths.
   - `cmd/esa` flag handling is untested.
5. **The provider is stateful across stage calls.** The HTTP provider receives `route`, `engineer` and `assign` as separate `POST /analyze` calls, and `Usage.Calls` must equal 1, 2 and 3 in turn. The provider has to keep intermediate state per `RunID`, but this is not written down anywhere. Document the contract in `docs/ESA_OPERATIONS.md`, and add a test for a provider restart between stages.

---

## Issues 9, 10, 12–14 — found by the real-provider evaluation (Issue 7)

Source: the 14-ticket pilot (`reports/eval-pilot-20261019`) and the 80-ticket runs without and with a tool catalog (`reports/eval-full-20261019`, `reports/eval-catalog-20261019`), model `gpt-6.1-sol` through the Codex CLI. The missing-dependency problem those runs also showed is handled by `maven_install` (PR #29) and is not listed here.

## Issue 9 — One bad evidence citation throws away the whole engineering report (Medium–High)

**Status:** fixed, merged in PR #30 (2026-10-20): readable, self-checking IDs; a bad citation drops only itself or its finding.

### Where

- `internal/replaytools/evidence.go` `EvidenceID` (~L12): every line that `read_code` or `run` returns is tagged `e_` + 20 random-looking hex characters (a hash of source, reference prefix, line and text).
- `internal/codex/diagnosis.go` ~L77 rebuilds the IDs and ~L101–110 rejects the **whole** report if any cited ID is unknown or selects an empty line.
- `internal/codex/http.go` ~L44–48 turns that into HTTP 422, and ESA ends the run as `failed`.
- The engineering output schema accepts any string for `location`, `end_location` and `evidence_ids`, and the Agent has no way to check a citation before the final answer.

### Evidence

- Pilot, KAFKA-3459: 18 IDs cited across 2 findings; one, `e_523b7d0a0b8b338f0ca824`, was never returned by any tool. It has 22 hex characters, not 20, and its tail looks like the frozen commit hash (`…c4db8b338f0ca824…`) blended in. The ticket failed. In the later full runs the same analysis hit the fix file.
- Catalog run, KAFKA-5512: 30 IDs cited; one, `e_6664e21d57609cae0f6a3`, has 21 hex characters and appears only in the Agent's answer. The ticket failed after 383k input tokens.
- In both, every other citation was valid and the other finding was fully supported.
- Cause: the Agent must reproduce long random strings from memory. It picks 18–30 of 577–706 near-identical IDs in a 300–400k-token context, most last seen 14–22 tool calls before the answer. A model regenerates such strings rather than copying them, so one length slip or blend with another hex string is enough.

### Suggested fix (chosen by the user)

Make IDs easy to copy and self-checking, instead of opaque hashes:

- Format: a readable location plus a short checksum, for example `KafkaConfigStorage.java:315#a3f9` for source, or `run3:42#9c1e` for line 42 of the third `run` output.
- The checksum stays a hash of source, reference prefix, line and exact text, so the ID still binds those bytes. A mistyped name, line or checksum no longer matches and is rejected; it is never resolved to a neighbour.
- The Agent can rebuild the readable part from what it is citing (file and line), so it no longer has to recall a random string.
- Two files with the same base name stay distinct through the checksum. Keep rejecting any ID that matches more than one line.
- Change only `EvidenceID`, the tool descriptions and the evidence prompt. Reports store resolved commit/file/line and quotes, not IDs, so persisted reports and the API do not change.

Secondary safeguard: if an ID still fails validation, drop only that citation, or that finding if its location is invalid, and add a limitation. Do not discard the whole report.

### Acceptance criteria

- `read_code` and `run` return IDs in the new format, and the same line always gets the same ID.
- A one-character change to the name, line or checksum of a valid ID is rejected.
- A report with one invalid citation is delivered with its valid findings and a limitation, not as a failed run.
- Existing evidence and validator tests keep passing; persisted reports keep their shape.

---

## Issue 10 — Provider quota exhaustion fails tickets and discards finished stages (Medium–High)

**Status:** fixed, merged in PR #31 (2026-10-20): the provider answers 429 with the reset time; ESA pauses provider work and resumes each ticket at the refused stage.

### Where

- `internal/codex/invoke.go` ~L146–148: any Codex CLI failure becomes the generic "Codex failed; inspect private run logs". A usage limit isn't told apart from a real error.
- ESA then ends the run as `failed`; per `docs/ESA_OPERATIONS.md`, failed or uncertain attempts need an explicit re-forward.

### Evidence

- Catalog run, 2026-10-19 20:05: the CLI reported "You've hit your usage limit … try again at 11:19 PM" (`codex-runs/run_adf4e877…/assignment/events.jsonl`).
- KAFKA-7763's `engineering-report.json` was complete, but the assignment stage was refused, so the whole ticket failed and the finished engineering result was not delivered.
- Every later ticket would have failed the same way; the run had to be stopped by hand and resumed after the reset.

### Suggested fix

- Detect usage-limit and rate-limit failures in the CLI event stream (`turn.failed` / `error` messages) and return a distinct, retryable error carrying the reset time when the CLI gives one.
- ESA should park the task (as it already does while waiting for engineering capacity) until the reset, instead of failing it. Raise one operator alert, not one per ticket.
- On retry, reuse the completed stage reports for the same `run_id`; the provider already keeps them. Resume at the failed stage instead of re-running engineering.

### Acceptance criteria

- A fake CLI that emits a usage-limit failure during `assign` leaves the ticket parked, not failed, and keeps the engineering report.
- After the reset, the task completes without a new engineering call.
- Other CLI failures still fail as today.

---

## Issue 12 — Engineering finds a release/evidence mismatch only after a full analysis (Low–Medium)

**Status:** done: PRs #34 and #36 merged, measured 2026-10-21. Prompt change merged in PR #34 (2026-10-20): the engineering Agent checks the ticket's frames first. Measured 2026-10-21 (`reports/eval-issue12-20261021`, six tickets, current main, compared with the catalog run):
- KAFKA-3994 stopped at the version check: 143k input tokens and 10 engineering tool calls (was 351k, 22), naming the mismatching `putCacheCallback` frame.
- Unaffected: KAFKA-6167 (same `Segments.java:72` finding), KAFKA-9883 (same method, lines 308–315 vs 302–309), KAFKA-3409 (same `MirrorMaker.scala:416-424` finding, but 499k vs 283k tokens).
- Regression: KAFKA-6075 stopped (130k) because `LogManager.loadLog` and `LogDirFailureChannel` are not in 0.11.0.1, although the faulty `Log.loadSegmentFiles` is there; the catalog run found `Log.scala:315-321` (a line hit). The rule is too strict when only surrounding frames differ. PR #36 (merged 2026-10-20) narrows it: stop only when the failing frame (where the error is raised or each thread waits), or the code quoted there, is missing; note other differences and continue. Rerun 2026-10-21 (`reports/eval-issue12b-20261021`): KAFKA-3994 still stops at the version check (160k tokens, 9 calls; names the `putCacheCallback` mismatch at line 288); KAFKA-6075 recovers its `Log.scala:315-321` finding (357k, 28 calls) and lists the missing `loadLog`/`LogDirFailureChannel` frames in limitations. Issue 12's acceptance criteria are met on these tickets; one run each, so run-to-run variation (Issue 13) still applies.
- KAFKA-4562 started from KAFKA-3994's unresolved record (PR #35; the hint reached engineering), then failed: the provider ended the turn with a "possible cybersecurity risk" content flag after 91 tool calls.

### Evidence

- KAFKA-3994 says 0.10.0.0, but its deadlock dump came from unreleased trunk: every broker frame matches trunk on the ticket date and none matches 0.10.0.0 (for example `GroupCoordinator.scala:701` is `delayedStore.foreach(groupManager.store)` on trunk but `def onCompleteHeartbeat() {` at the tag). The Agent noticed the mismatch in all three runs, but only after 250–350k input tokens each time.
- Milder drift elsewhere: KAFKA-6075 ("the supplied source differs from the ticket's LogManager stack") and KAFKA-9883 ("ticket stack line numbers differ from this snapshot"); the Agent continued and produced findings.

### Decision (user)

Keep the check in the engineering Agent, with a small prompt change rather than a separate pre-check: before deeper analysis, read two or three of the product frames the ticket quotes; if the named methods or quoted code are not there, stop with empty findings and a limitation naming the mismatching frames, so the reporter can give the exact version; shifted line numbers within the same methods are not a mismatch.

### Acceptance criteria

- `engineering-forwarded.txt`, used in every engineering mode, carries the check.
- In a provider run, a ticket like KAFKA-3994 ends in `awaiting_information` after a few reads, far below a full analysis's tokens, and tickets whose frames match are unaffected.

---

## Issue 13 — The same ticket gets different outcomes between runs; single-run numbers include that noise (Low–Medium)

**Status:** archived 2026-10-21 by the user's decision: not planned for now. The text is kept for reference; bring it back before relying on small before/after differences between single runs.

### Where

- The model is already resolved and audited. `cmd/esa-codex/main.go` fills an empty `-model` / `-effort` at start-up from `~/.codex/config.toml` (currently `gpt-6.1-sol`, effort `high`) and logs the model. Each engineering run's `audit.json` records `model` and `prompt_version`.
- The audit does not record the reasoning effort or the Codex CLI version. `internal/codex/cli_check.go` reads `codex --version` only to explain a start-up failure.
- `scripts/run_agent_eval.py` passes no `-model` / `-effort`, so an evaluation silently runs on whatever the operator's personal Codex config says at that moment.
- The prompts leave some borderline judgments open, for example whether to report the confirmed half of a mechanism or abstain. The model resolves those differently from run to run.

### Evidence

All runs below used the same model (`gpt-6.1-sol`), so pinning alone would not have prevented them.

- KAFKA-3994 (pilot, full and catalog runs, 2026-10-19): byte-identical input, prompt, schema and tools (the catalog run also had three dependency JARs). All three runs saw the same version mismatch. The pilot reported the confirmed watcher-lock half as a finding (`awaiting_review`); the other two abstained (`awaiting_information`). Its duplicate KAFKA-4562 therefore cost 58k tokens (reuse) or 535k / 741k (fresh analysis). PR #35 (merged) now exports abstentions as `unresolved` records, which limits that knock-on cost but not the flip itself.
- KAFKA-3459: `failed` in the pilot (an evidence-ID typo, fixed by #30), `awaiting_review` with a fix hit in the full runs.
- Unchanged tickets, Issue 12 runs (2026-10-21) vs the catalog run: KAFKA-3409 took 499k tokens against 283k with the same `MirrorMaker.scala:416-424` finding; KAFKA-9883 moved from lines 302–309 to 308–315 in the same method; KAFKA-3994 took 143k, then 160k on the rerun.
- An example of tightening a vague rule: PR #36 made the version check say exactly when to stop. KAFKA-3994 then stopped and KAFKA-6075 continued, where #34's looser wording had stopped both.
- Single-run evaluation numbers therefore include run-to-run noise, not only real differences.

### Suggested fix

1. **Audit (no Agent calls).** Record the reasoning effort and the Codex CLI version in every run's `audit.json` next to `model` and `prompt_version`, and carry all four into the evaluation report.
2. **Launcher (no Agent calls).** Give `scripts/run_agent_eval.py` explicit `--model` / `--effort` options, passed to `esa-codex`, and write them to `launcher.json`. An evaluation then states what it runs on instead of reading the operator's personal config. Production configs set them explicitly too.
3. **Agreement rates (paid).** Rerun a fixed subset, for example 10 tickets × 3 runs (about 30 analyses, roughly 9M input tokens at today's average). Report per ticket: agreement on route, end state, file hit and line hit, plus the token spread. Pick the subset to include known borderline tickets (KAFKA-3994, KAFKA-3459, KAFKA-5154) as well as stable ones. Later comparisons count a change as real only when it exceeds that noise.
4. **Tighten vague prompt rules one at a time.** Where the repeated runs show a ticket flipping on a judgment the prompt leaves open, write the rule down, as #36 did for the version check. Then rerun just the affected tickets to confirm, one rule per change so each effect can be attributed.

### Acceptance criteria

- Every run's audit and the evaluation report record model, effort, Codex CLI version and prompt version.
- The evaluation launcher takes model and effort explicitly and records them.
- The evaluation report includes a per-ticket agreement rate and token spread for the repeated subset.
- Each prompt rule added under item 4 comes with a measured rerun of the tickets it targets.

---

## Issue 14 — About 300k input tokens per analyzed ticket; half of the reading tools' output is bookkeeping; no enforced budget (Medium)

**Status:** finished 2026-10-21 by the user's decision. Merged:
- PR #32: compact `read_code` / `source_search` output (48% smaller on the recorded outputs).
- PR #37: `issue_search` takes up to five queries in one call, and the routing prompt asks for one batched first search.
- PR #38: `read_code` takes up to five ranges in one call; the catalog-mode engineering instructions drop from 8,231 to 4,017 characters, with the runtime procedure returned once at the first runtime grant.

Estimated on KAFKA-6075's logged run, not measured: 357k input tokens fall to about 215–230k if the Agents batch as asked (routing about 76k → 25k, engineering about 256k → 164–181k); about 16k of that is certain. Left undone by decision: symbol index, per-tool token reporting, enforced budget, ticket stack-trace compaction and a one-line profile catalog.

### Where

- `internal/codex/invoke.go` audit `token_policy`: "Advisory warnings only; never cancel or reject because of token consumption." The thresholds in `config/rules.json` only warn.

### Evidence

- Analyzed tickets used a mean of about 300k input tokens (about 79% cached) and 3k output tokens; the most expensive used 741k.
- 43 tickets (13.1M input tokens) used up a ChatGPT plan's usage window in under 2 hours.
- Routing-only tickets (support returns, reuse) cost 6–60k, so almost all of the cost is engineering.

### Where engineering spends it (catalog run, 65 engineering runs)

- **Building is the Agent's job, and it's cheap.** The provider supplies only frozen source, a JDK and installable dependencies ("No product build … is supplied", `internal/codex/registry.go` ~L195). 44 of 65 runs tried to build: 61 build commands, mostly `javac` of the classes involved (39) and `scalac` (11), plus helper programs run with `java`. 21 of the 61 failed (missing dependencies, no Scala compiler, mismatched library versions). The 22 `gradle` attempts cannot work offline. All `run` output together is only about 4% of tool output; the median run makes one `run` call.
- **Reading source is the cost.** Share of tool output by tool:

  | Tool | Calls | Share of output |
  | --- | --- | --- |
  | `read_code` | 697 | 61% |
  | `source_search` | 711 | 24% |
  | `capability_request` | 67 | 9% |
  | `run` (builds included) | 85 | 4% |
  | everything else | 156 | ~2% |

  The median run makes 26 tool calls. Each output stays in context for the rest of the turn, so every read is paid again on each later step. That is how roughly 1,400 reads and searches become about 300k input tokens per ticket.
- **About half of that output is bookkeeping, not code.**
  - `read_code`: only 41% of the output is source text; 53% is the per-line JSON wrapper `{"line":511,"id":"e_…","text":"…"}`.
  - `source_search`: only 21% is the matching line; 69% is the 40-character commit hash, 64-character file hash and full path repeated on every hit (6.8 hits per search on average).
- **Read size and re-reads are minor.** Reads are already modest (median 48 lines, 90th percentile 115; only 1% at the 200-line cap), and only 3% of reads overlap a range already read in the same run.
- Runs that built had a higher median input (262k vs 154k), but mostly because they were harder tickets that also read more code, not because of build output.

### Suggested direction (confirm with the user; this is a product decision)

The main lever is the output format of the reading tools, not how much the Agent reads:

- **Compact `read_code` output.** Return the file, commit and range once in a header, then plain numbered lines such as `511 a3f9 | static class StreamsMetricsThreadImpl …`. Each line's full evidence ID is derived from the header's file plus the line and checksum, so the Agent can still cite it exactly. The per-line overhead drops from about 40 characters to about 10. The provider must accept the same IDs as now, and evidence binding (exact line bytes) must not weaken.
- **Compact `source_search` output.** State the commit once per response, group hits by file, and drop the per-hit file hash, which the Agent never needs because `read_code` already verifies the source.
- Together these could remove roughly 40–50% of what the two tools add to context, without the Agent reading any less code.

Smaller, secondary levers:

- **Cheaper navigation.** A symbol index (definitions, callers, class hierarchy) for the frozen release would answer "where is X defined / who calls Y" in a few lines, instead of searches whose hits the Agent then reads.
- **Measure it.** Report tokens per stage and per tool in the run audit, so each change can be checked against this baseline.
- Smaller default read windows or re-read suppression would save little (see above); do them only if measurement shows otherwise.

Then, as a safety net:

- An optional enforced budget per ticket and per project per day. When it's reached, park new engineering work and alert, instead of spending until the provider refuses (Issue 10).

Not a cost lever: an automatic per-release build would barely change tokens. It would make reproduction more reliable (a third of the Agent's builds failed), so track it separately if wanted. `maven_install` (PR #29) already removes the missing-dependency failures.

### Acceptance criteria

- `read_code` and `source_search` output shrinks substantially with evidence IDs still resolving exactly. PR #32 measured 48% on the recorded outputs; the remaining overhead is about 29% for reads and higher for searches (file paths, line numbers), so the earlier "under 20%" target is not met.
- The run audit reports input tokens per stage and per tool.
- On a fixed replay subset, median input tokens per analyzed ticket drop measurably against this baseline (about 300k), with no loss in fix-file hits.
- With a budget configured, new engineering work stops at the budget and resumes when it resets; without one, behavior is unchanged.

---

## Issue 15 — Routing search is untested at realistic directory sizes (Medium)

**Status:** finished 2026-10-21 by the user's decision: PR #39 merged (identifier parts + light stemming, at least a third of the query words, offline evaluation tools). The paid Agent pilot (test plan step 4) was not run. Earlier: PR #37 (batched `issue_search`, merged for Issue 14) cuts the number of routing searches but changes nothing about what a search matches. Test plan steps 1–3 done 2026-10-21, offline (`reports/routing-search-eval-20261021/report.md`; tooling `scripts/build_routing_corpus.py` and `tools/routing-eval`, not yet committed):
- 252 duplicates and 252 controls; each directory holds every Fixed ticket created before the case (median 4,754 records, up to 11,320). The offline ranking is checked against an exported ESA directory.
- Original in the top 10: browse 13%, one identifier 29%, component + version 8%, ticket title 56%, stack-frame classes 52%, title + exception names 62%; best of all styles 67%. Title misses beyond the top 10 have median rank 74.
- Any-word matching: a title query matches a median of 3,175 records for duplicates and 2,830 for controls; every control gets candidates from title, browse and component + version queries.
- Scores do not separate a true original from an unrelated best hit: the original's title-query score has median 15.8; a control's best candidate has median 19.4. Telling them apart rests entirely on the Agent reading candidates.
- Correction to the evidence below: at realistic sizes identifier queries usually match something (154 of 172 did); the problem is that they rarely rank the original (29% top 10), not that they return nothing.
- Measures 1–7 tested one at a time on the same cases (report section "Measures 1–7"): identifier parts + light stemming is the only clear recall gain (best of the Agent's styles 67% → 73%, identifier queries 29% → 37%); requiring a third of the query words keeps recall and cuts matches about tenfold (3,175 → 330); keeping versions and dropping bare numbers fixes component + version flooding (2,543 → 548 matches); field weighting lowers recall at both weights tried; stack signatures are precise but quoted by only 21% of duplicates (+3 top-10 originals); an ESA-built query from the ticket reaches 59% against the Agent's 29% / 8% habits; snippets could not be judged offline (a word-overlap proxy got no help from them). Combination 1 + 2 + 5 (a third of the words): best of the Agent's styles 67% → 71%, title 56% → 59% top 10 (63% → 67% top 20), with title-query lists about eight times shorter (3,175 → 388 matches; controls 2,830 → 276); measure 2 alone reaches higher recall (73%) but keeps lists of thousands. The combination drops 26 originals, 3 of them from the top 10: measure 1 discards bare numbers such as CVE IDs and splits nothing on versions like `2.25.4`, and measure 2's identifier parts raise the third-of-words bar. Low-ranked originals mostly describe the problem in other words (symptom vs cause). Measures 1 + 2 without the rule: best 72%, title 60% (2 alone: 73%, 63%), lists of thousands; measure 1 loses 9 top-10 originals that 2 alone found, through ticket keys (`KAFKA-8562`), KIP and CVE numbers and whole versions (`2.13` vs `2.13.0`), so its number handling needs fixing before use. Measures 2 + 5 (a third of the words): best 73% and title 62% top 10 / 69% top 20, the same recall as 2 alone, with title-query lists of 412 instead of 3,175 (controls 285 instead of 2,830); the rule drops only one top-10 original that 2 alone found. The best combination measured. PR #39 ships it (`esa.IssueSearch`) and commits the evaluation tools (merged 2026-10-21). Weighting the share by word rarity (to keep originals like KAFKA-10353, which shares 3 of 18 terms) was tried and rejected: it is stricter, since the query's own rare words are usually absent from the original (best 70%, title 59%, 10 top-10 originals lost).

### Where

- `internal/replaytools/issues.go`: the Routing Agent's `issue_search` (query, kind, offset, limit 1–20, `archive`), `issue_read` (required before reuse or join) and `issue_context`.
- `internal/esa/issue_directory.go` ~L324: each index entry's search text is the source ticket key, the solution summary, the ticket description and environment, plus title, components and versions. No code identifiers, file names or stack frames from the diagnosis are indexed.
- `internal/retrieval/bm25.go`: BM25 over lowercase `[a-z0-9_]+` tokens, no stemming or synonyms; a record matches if it contains **any** query word. An empty query lists everything.

### Evidence (71 routing runs, catalog evaluation)

- **Browsing habit:** a median of 8 searches per run (up to 16), 169 of them with an empty query listing up to 20 records, but only 25 `issue_read` calls in total.
- **Identifier queries that never match:** the median query is one word, often a code identifier (`RecordCollector`, `makeLeader`, `LogOffsetMetadata`). These usually return 0, because the index holds ticket prose rather than code.
- **Any-word matching:** queries that do match are mostly component and version words (`core 0.11.0.0`, 19 matches), which match nearly every record in that component.
- **Not stressed:** routing was correct on every ticket, but each directory held only Issues created earlier in the same run, at most a few dozen. The Agent can list all of them. With hundreds or thousands of real Issues, listing shows 20 at a time, identifier queries still find nothing, version words match everything, and the same symptom in different words ("deadlock" vs "hangs") is missed.

### Suggested direction (confirm with the user; measure first)

- Index what the Agent searches with: the finding's files, classes, methods and stack-frame names from each Issue's diagnosis.
- Rank by how many query words match, or offer an all-words mode, so version and component words stop matching everything.
- Discourage blind browsing in the tool description and prompt once the directory is large: search with symptom words, error names and the classes in the stack trace, and read the top candidates.
- Optionally add a semantic, embedding-based candidate list next to BM25. That would be a new dependency, so ask first.

### Test plan

Use the preserved Kafka export: 11,362 fixed tickets and 437 duplicates linked to their original.

1. **Realistic directory (no model calls).** For a set of cutoff dates, build Issue directories from all fixed Kafka tickets available by then, as `engineer_resolved` records with their summaries and descriptions. That gives several hundred to several thousand records, with the same project and time scoping as production.
2. **Retrieval quality without a model.** For each duplicate whose original is in the directory, issue the kinds of query the Agent actually sent (from the 71 runs' logs: single identifiers, component/version words, empty browse) and the kinds it could send (ticket title, error names, stack-frame classes). Record whether the original appears in the top 10 and top 20, and the total match count. Repeat for each candidate index or ranking change.
3. **Negative controls.** Take tickets with no duplicate in the directory and record how many candidates each query returns, to measure false-reuse risk.
4. **Agent pilot (paid; only with the user's explicit approval and budget).** Route about 30 duplicates and 30 non-duplicates against a large directory with the real Routing Agent. Measure reuse recall (joined the right Issue), false reuse, searches and reads per ticket, and tokens.

### Acceptance criteria

- An offline report gives top-10 and top-20 recall of the original for each query style at realistic directory sizes, for the current search and for each proposed change.
- Any change to indexing or ranking improves that recall without raising the candidate count for the negative controls.
- If the Agent pilot is approved, it reports reuse recall and false reuse at that directory size.

---

## Issues 17–23 — found by the code audit (memory safety and performance)

Static review of the Go code on `main` at `a1fe713`, with dynamic checks where needed (2026-10-21). Full report: `reports/code-audit-20261021/report.md` (local; `reports/` is git-ignored). The audit also confirmed what is sound: request body limits and server timeouts, bounded reads, constant-time token checks, `os.OpenRoot` path confinement, parameterized SQL, no secrets in logs, `go test -race ./...` clean for all 12 packages, and no panics in 1.66 million fuzzed import inputs. Not covered: the Ruby plugin, Python scripts, dependency vulnerabilities and third-party linters (`govulncheck`, `staticcheck`, `gosec` are not installed).

## Issue 17 — A provider CLI call can block forever after its timeout (High)

**Status:** merged in PR #42 (by the user's choice, the process-group method): the CLI runs in its own process group, and the whole group is killed on timeout or cancellation; a fake-CLI test shows the call returning 1 s after a 1 s timeout with the child gone (without the fix it was still blocked at 15 s). `cmd.WaitDelay` was not added, so a child that leaves the group, or one still holding the pipes after a normal CLI exit, is not covered. Not checked against the real Codex CLI.

### Where

- `internal/codex/invoke.go`: the Codex CLI runs under `exec.CommandContext`; stderr goes to a `bytes.Buffer`, so `os/exec` copies it in a goroutine. On timeout the context kills only the CLI process, and `cmd.Wait()` (line 124) then waits for the stderr copy, which never finishes while a child of the CLI still holds the pipe. No `cmd.WaitDelay` is set.

### Evidence

- Reproduced with a fake CLI that starts a background `sleep`: `invoke` had not returned 25 s after its 5 s timeout. The first attempt, a CLI whose child kept writing, stayed stuck in `cmd.Wait()` until the test was killed.
- The real Codex CLI starts children (the ESA tool server, which runs `docker`). Whether they inherit the CLI's stderr was not checked against the real CLI.
- Effect: ESA's own HTTP call gives up at the stage timeout (`timeout_seconds: 900`) and marks the task failed, but the provider keeps the blocked call's engineering slot (`Provider.Slots`, three in total) until it restarts. After three such hangs every later engineering request waits for a slot until ESA's timeout and fails.

### Suggested fix

- Set `cmd.WaitDelay` (for example 10 s) so `Wait` returns after the process exits even if its pipes stay open.
- Start the CLI in its own process group (`SysProcAttr.Setpgid`) and kill the whole group on timeout or cancellation.
- Drain stdout before calling `Wait`.

### Acceptance criteria

- A test with a fake CLI whose child keeps stdout and stderr open: `invoke` returns within the timeout plus `WaitDelay`, the slot is released, and no child process is left running.
- Normal and quota-refused CLI runs behave as before.

---

## Issue 18 — One event line over 2 MB stalls a provider stage until its timeout (Medium)

**Status:** fix in PR #43: CLI events are read with a bounded reader, so an over-long line is discarded as it is read, never buffered. Because the tool policy cannot check a discarded event, the run is stopped at once (the process group is killed, PR #42) and fails with an error naming the event's size; `events.jsonl` records the event by size only. A fake-CLI test emitting a 3 MB line stops in 0.7 s against a 60 s timeout (without the fix: 60 s, then `token too long`). A legitimate run that emits a 2 MB line now fails fast rather than at the timeout; it never succeeded before either.

### Where

- `internal/codex/invoke.go`: CLI events are read with a `bufio.Scanner` limited to 2 MB per line. A longer line ends the loop; the scan error is checked only after `cmd.Wait()`, so the CLI blocks writing to the unread pipe until the stage timeout kills it. The tool-permission check in the loop also sees no events after that point.

### Evidence

- Reproduced with a fake CLI printing a 3 MB event line followed by more output: the call returned only at its timeout, with `bufio.Scanner: token too long`. With the shipped 900 s timeout that is 15 minutes of a held engineering slot for a stage that has already failed.
- A line that long needs a tool result or Agent message over 2 MB; see Issue 21 for how `read_code` can produce one. Not reachable on Kafka (its longest source line is 5.7 KB).

### Suggested fix

- On an over-long line, skip or truncate it and keep reading (for example `bufio.Reader` with an explicit size check), so the pipe keeps draining and permission checks keep running.
- Fail the stage promptly with a clear error instead of waiting for the timeout, recording the skipped line's size in the audit.

### Acceptance criteria

- A test with a fake CLI emitting a 3 MB line: the stage ends promptly (not at the timeout), the error names the over-long event, and later events are still checked for unregistered tools.

---

## Issue 19 — Import preview turns a 12 MB upload into a ~500 MB heap spike (Medium)

**Status:** fix in PR #44 (the user chose suggestions 1 and 2: stream JSON with per-record bounds, cap CSV columns and field size; no concurrency limit, body limits unchanged). JSON arrays (top-level or `issues`) are read one record at a time and the 2,000-record cap applies while reading; each record is limited to 8 MB and 200,000 JSON values, checked before decoding (real Jira records reach 4.3 MB and 38,000 values). CSV is pre-scanned without allocating and rejected above 1,024 columns in any row; a field over 8 MB is that row's problem. Peak heap for the crafted uploads: +463 → +16 MB, +383 → +14 MB, +499 → +13 MB; 2,000 realistic records +106 → +70 MB. Previews for the examples, 300 real Jira records and edge cases are byte-identical to before.

### Where

- `internal/esa/http.go` `preview` and `internal/esa/import.go` `ParseImport`: the 12 MB body is decoded in full into `any` before the 2,000-record cap is applied, and the CSV branch accepts any number of header columns.

### Evidence

Measured with crafted ≈12 MB uploads:

| Upload | Allocated | Peak heap growth | Result |
|---|---|---|---|
| JSON array of empty objects | 753 MB | +463 MB | rejected (cap) |
| JSON array of small numbers | 776 MB | +383 MB | rejected (cap) |
| CSV header with ~6 million columns | 999 MB | +499 MB | accepted |
| 2,000 realistic records | 133 MB | +106 MB | accepted |

- Reachable by any Redmine user who may upload issues; the plugin forwards the file. A few concurrent uploads add up on top of the working set (230 MB for the Kafka archive).

### Suggested fix

- Stream the JSON array with `json.Decoder.Token` and stop after 2,000 records; reject a single record over a size bound.
- Cap the CSV column count (for example 256) and field size.
- Run at most one or two previews at a time.
- Lowering the 12 MB body limit changes what the plugin may send, so agree on it before changing it.

### Acceptance criteria

- The four uploads above stay under a stated peak (for example +50 MB each); oversized or over-wide input is rejected with a clear error before full decoding.
- Valid uploads, including the 2,000-record case, produce the same preview as today.

---

## Issue 20 — Cold reads share one Postgres connection (Low)

### Where

- `internal/esa/pg_persister.go` `ReadSnapshot` and `ReadTickets` use one `pgx.Conn` behind `readMu`, so concurrent cold reads (snapshot GETs, closed-ticket text since PR #41) queue behind each other.

### Evidence

- A closed ticket's GET takes 0.34 ms (0.10 ms for an open one); the plugin polls an open ticket page every 15 s. At that rate the queue is never noticeable; one slow query would still delay all other cold reads.

### Suggested fix

- If cold-read load grows, use a small `pgxpool` for reads instead of one connection and a mutex.

### Acceptance criteria

- Concurrent cold reads proceed in parallel up to the pool size; a slow read does not delay others.

---

## Issue 21 — `read_code` has no bound on total output size (Low)

### Where

- `internal/replaytools/server.go` `readSource`: up to 200 lines per range, five ranges per call (PR #38), and lines up to 2 MB each.

### Evidence

- Worst case about 400 MB per range held in the tool server, plus JSON copies; the result can also exceed the provider's 2 MB event line (Issue 18). Normal source never comes near this; minified or generated files in another product could.

### Suggested fix

- Cap the total output per call (for example 256 KB) and truncate long lines with a marker, keeping evidence IDs exact for the lines returned.

### Acceptance criteria

- A read of a file with multi-megabyte lines returns a bounded, marked result, and citations of the returned lines still validate.

---

## Issue 22 — A few request paths scan whole tables (Low)

### Where

- `GET /v1/cases` (`internal/esa/http.go` `cases`) walks every case and returns all matches unpaginated, each with its summary. The plugin does not call it.
- With the classifier enabled, `classifierView` scans all predictions and feedback on every ticket GET, and feedback submission scans all feedback inside the write transaction (`internal/esa/classifier.go`).
- `issueTitleSource` (`internal/esa/investigation_http.go`) scans all runs for Issues without `TitleSourceTicketID`, which only Issues created before that field have.

### Evidence

- Code reading. For scale: building the Issue directory (`issueRecords`) over the 19,869-ticket archive takes 16–26 ms per analysis, so whole-table work is cheap at Kafka's size; these paths matter only with much more history or heavy classifier use.

### Suggested fix

- Paginate `GET /v1/cases` (or require a ticket scope).
- Index classifier predictions and feedback by ticket.
- Backfill `TitleSourceTicketID` for old Issues once, then drop the fallback scan.

### Acceptance criteria

- Each path's cost depends on the requested scope, not on the archive size, and responses are unchanged for existing callers.

---

## Issue 23 — A regular expression is compiled on every Maven install (Low)

**Status:** fixed on branch `fix/maven-regexp`, pushed without a PR (commit `052b8d5`, by the user's choice): the pattern is the package variable `pomProperty`; `internal/replaytools` tests pass. Not yet merged into main.

### Where

- `internal/replaytools/maven.go:196` `pomDependencies` compiles `\$\{([^}]+)\}` on each call.

### Suggested fix

- Move it to a package-level variable.

### Acceptance criteria

- Compiled once; `maven_install` tests pass unchanged.

---

## Things that are good and must be preserved

Any fix must keep these properties. Each has existing tests; extend them, do not weaken them.

- Agents only advise and are read-only. Go validates every stage and rejects any rewrite of the earlier stage's output (the hash comparisons in `pipeline.go` and `validateReport`).
- Findings must point at pinned evidence (commit, file, line range of at most 200 lines), and `read_code` checks a SHA-256 of the source file.
- Nothing after the forwarding cutoff is used: comment provenance, `CasesAt`, the classifier's time split, and frozen replay predictions.
- The sandbox is checked before running: read-only filesystem, `NetworkMode none`, capabilities dropped, uid 1000, resource limits, no docker.sock.
- Events are idempotent, writes use CAS, and leases use epoch fencing so stale owners' results are rejected. Files are written atomically with fsync and the directory is synced. One process-level lock prevents two writers.
- CI never calls real Agents (the `codex` guard in `scripts/check_workflow.sh`) and pins GitHub Actions to exact versions.
