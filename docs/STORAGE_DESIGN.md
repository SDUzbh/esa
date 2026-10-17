# ESA storage: Phase 3 design

**Status:** proposed, for review. Nothing here is implemented yet.
**Scope:** REVIEW_ISSUES.md Issue 1 (store scaling), Phases 2 and 3 combined.

## Decisions already made (2026-10-17)

| Question | Decision |
|---|---|
| Retention | **Dedupe + cold audit.** Nothing is deleted. Duplicate ticket copies become references; audit-only data moves out of memory. |
| Storage | **ESA owns a Postgres schema**, accessed through `github.com/jackc/pgx/v5`. This is the project's first external Go dependency. The JSON file stays as the dev/test adapter. |
| Writers | **One active writer**, guarded by a Postgres advisory lock. A second process may wait as a standby. |
| Order | **Phase 2 is folded into Phase 3.** The JSON state is migrated once, straight into the normalized schema. |

## Goals and acceptance criteria

The targets come from REVIEW_ISSUES.md:

- A single-ticket GET takes under a few ms at 10k tickets, **including tickets that have an Issue**. A GET of a ticket with an Issue writes its access order.
- Ingesting 20k synthetic tickets finishes in minutes.
- A write costs time proportional to **what changed**, not to the total state size.
- Every existing workflow, CAS, lease/epoch and replay test passes, with no change to `system_replay` assertions.
- Idempotency, fencing and crash safety are kept, each with a test (see [Guarantees](#guarantees)).

Non-goals:

- Several active writers.
- Moving domain logic into SQL.
- Changing the plugin–ESA HTTP API.
- Making Case history or run reports cold. Both stay in memory; see [Open questions](#open-questions).

## Where we are after Phase 1

Phase 1 is merged as PRs #6–#10:

- `Update` is copy-on-write: `State.detach()` shallow-copies the top-level maps.
- `Snapshot()` shares the published state, read-only.
- A test-only guard proves that no `Update` callback or `Snapshot` reader edits shared values in place.

Benchmarks (`go test ./internal/esa -run '^$' -bench 'Store|TicketGET|ReceiveClosed'`):

| | 1k tickets | 10k tickets |
|---|---|---|
| `Snapshot` | ~2 µs | ~0.2 µs |
| One-ticket `Update` | 36 ms | 366 ms |
| Ticket GET, ticket without an Issue | 0.05 ms | 0.30 ms |

Every write still encodes and persists the **whole** state, so write cost grows linearly. A 1,000-ticket sample shows where the bytes go: `snapshots` 21%, `events` 20%, `tickets` 19%, `runs` 12%, `cases` 9%, `case_history` 9%. Most of that is the same ticket stored three or four times.

## Design overview

```
            ┌──────────── ESA process (one active writer) ────────────┐
 HTTP ──►   │ handlers ── Snapshot() (shared, read-only, O(1))        │
 workers ─► │ Update(fn): detach → fn → project → diff ─► changes     │
            │                                          │               │
            │                     Persister.Apply(changes, rev, term)  │
            └──────────────────────────────────────────┼──────────────┘
                                                       ▼
                     Postgres database "esa": one row per entity, history tables,
                     meta row (revision, writer term), cold audit tables
```

- The **in-memory working state and all domain code stay the same**. Callbacks keep editing `*State` as they do now.
- The store **diffs** the detached state against the published one and hands only the changes to a `Persister`.
- **Postgres is the persistence layer, not a query engine.** Reads stay in memory.

## Change detection

`detach()` copies the top-level maps, so they share every entry with the published state. The Phase 1 guard proves callbacks only **replace** entries (`s.Tickets[id] = t`) and never edit shared values. Therefore:

- An entry the callback did not touch is **bit-for-bit identical** to the published one.
- An entry the callback replaced has different field values, so its bytes differ.

The diff for each map is then:

```go
// sameBits compares the raw memory of two values of the same type.
func sameBits[V any](a, b *V) bool {
	n := unsafe.Sizeof(*a)
	return bytes.Equal(unsafe.Slice((*byte)(unsafe.Pointer(a)), n),
		unsafe.Slice((*byte)(unsafe.Pointer(b)), n))
}
// changed = keys in next that are missing from published or fail sameBits; deleted = keys only in published
```

- **History slices** (`CaseHistory`, `MembershipHistory`) are append-only, so their change is the tail past the published length.
- **Scalar fields** (`IssueAccessSequence`, `ReplayClock`, `LastClaimedProject`, `SchemaVersion`, `StoredFields`) are compared directly and stored in the meta row.
- **False positives are harmless.** A replaced entry with equal content, or different padding bytes, just rewrites one row.
- **False negatives would lose data.** They can only happen if something edits a shared value in place, which the guard forbids. In tests, the guard also **cross-checks the diff on every `Update`** against a full-encoding comparison and panics if they disagree. This runs across the whole suite (about 80% coverage).
- `projectInvestigationStates` re-assigns every run on every `Update`. With equal values the bytes stay equal, so it does not mark runs as changed. A probe with 6 open investigations found 0 changed runs; rollout step 1 adds a unit test that pins this.

**Measured cost** of detach, a one-ticket change, projection and the diff over the seven large maps: **8.7 ms at 10k tickets and 24 ms at 20k**, with exactly one changed entity found. Today's one-ticket `Update` takes 366 ms at 10k.

**Considered and rejected for now:** wrapping each map in a change-tracking `Table[V]` type. Detecting changes would then cost time proportional to the changes, but it would rewrite about 600 call sites (65 writes, 262 index reads and 50 loops in production code, plus 238 accesses in tests). We can do it later if the diff ever shows up in profiles.

## Persisted model

### Normalization (the folded-in Phase 2)

| Data | In memory | Persisted |
|---|---|---|
| **Events** | **Changes** to `EventRecord`: ID, origin, force, forwarder, forwarding/occurred/received times, `ticket_id`, `ticket_hash`, `snapshot_id`. No `Ticket`, no `Raw`. | `esa.events` row. A non-empty `Raw` goes to cold `esa.event_raw`. |
| **Run input** | `Run.Input` is unchanged in memory and is filled from `Snapshots[run.SnapshotID].Ticket` at load. | The payload **omits `input`** when `SnapshotID != ""` and `hash(Input) == snapshot.Hash`. Otherwise `input` is kept (legacy or divergent runs). |
| **Snapshots** | The **hot set** (see below) | All snapshots are stored in `esa.snapshots`. Only the hot set is loaded. |
| **Membership history** | **Not loaded.** It is append-only and no code reads it. | `esa.membership_history` |
| **Case history** | Loaded, because `eligibleCaseHistory`/`CasesAt` reads it on every Issue-directory export | `esa.case_history` |
| Everything else | Unchanged | One row per entity |

Why the event change is safe:

- The only reader of `Event.Ticket` is the duplicate-event check, `hash(old.Ticket) != hash(t)` in `Receive` (`engine.go:261`). The stored event's ticket is the same value `captureSnapshot` saves with `Hash: hash(t)`, so comparing `ticket_hash` is equivalent.
- Nothing in `internal/esa` reads `Raw`, so it is kept, but cold.

**The hot snapshot set** is loaded at startup:

- every snapshot referenced by a run (`Run.SnapshotID`),
- every snapshot referenced by an investigation contract,
- every snapshot referenced by a membership link that is not detached. `detachChangedMemberships` and `taskHasActiveMembers` read `Snapshots[link.SnapshotID]` for tentative links.

The `latest_snapshot` *map* (ticket → snapshot ID) is always loaded, but the latest snapshot's *body* does not need to be:
- `rowFor` and `ReviewMembership` read only the ID.
- `enqueue` reads the body only right after `captureSnapshot` creates it in the same callback, and snapshots created since startup stay hot.

This matters for an imported archive, which is mostly closed tickets: their snapshots have no run or link. In the real Kafka archive, 5,284 of 19,869 snapshots are hot.

All other snapshots are audit data, read only by `GET /v1/snapshots/{id}`. That handler falls back to the database outside any `Update`.

### Memory

These measurements are from a temporary probe, since deleted. It ran all **19,869 real Kafka issues** from `data/jira/raw` through ESA's Jira parser (`parseJira`) and `Receive`:
- 14,585 closed and 5,284 open tickets.
- Half the open tickets carry the largest real Agent report from `runtime/ui-acceptance-20261014` (18 KB).
- Real ticket text averages 4.6 KB (median 1.3 KB, p99 49 KB), 92 MB in total.

The figure is live Go heap after loading state, the way a restart does:

| Tickets | Whole-state JSON today | Heap today | Heap after Phase 3 | Snapshots in memory |
|---|---|---|---|---|
| 5,000 | 120 MB | 128 MB | **65 MB** | 572 / 5,000 |
| 19,869 (full archive) | 492 MB | 509 MB | **280 MB** | 5,284 / 19,869 |

- With the default `GOGC=100`, process memory can reach about twice the live heap between collections, so plan for **about 0.6 GB** for the full archive. `GOMEMLIMIT` can cap it.
- The rest of the 280 MB is mostly ticket text plus `Case.Summary` and `CaseHistory` copies of the descriptions. Sharing those strings at load time is a possible later reduction, out of scope here.
- The one-time import holds the 492 MB JSON source and its decoded state together. Expect a peak of 2–3 GB, offline, once.
- Today every write re-encodes the 492 MB state. After Phase 3, a write encodes only the changed rows.

### Schema

ESA gets a dedicated database `esa` and role `esa`, in the existing `postgres:17` service.

```sql
CREATE TABLE esa.meta (
  singleton     boolean PRIMARY KEY DEFAULT true CHECK (singleton),
  schema_version int    NOT NULL,          -- State.SchemaVersion
  revision      bigint  NOT NULL,          -- +1 per committed Update
  writer_term   bigint  NOT NULL,          -- +1 each time a process becomes the writer
  frame         jsonb   NOT NULL           -- scalar State fields and StoredFields
);

-- One table per State map, all with the same shape. The Go struct stays the
-- schema of record; JSONB keeps unknown-field preservation (StoredFields).
CREATE TABLE esa.tickets (
  id         text   PRIMARY KEY,
  project_id text,
  payload    jsonb  NOT NULL,
  revision   bigint NOT NULL               -- meta.revision that last wrote it
);
-- Same shape: events, receipts, rejections, runs, latest, cases, snapshots,
-- latest_snapshot, engineering_issues, issue_ticket_links, issue_overrides,
-- investigations, investigation_groups, solution_revisions,
-- classifier_predictions, classifier_feedback.

CREATE TABLE esa.case_history       (seq bigint PRIMARY KEY, case_id text NOT NULL, payload jsonb NOT NULL);
CREATE TABLE esa.membership_history (seq bigint PRIMARY KEY, link_id text NOT NULL, payload jsonb NOT NULL);  -- cold
CREATE TABLE esa.event_raw          (event_id text PRIMARY KEY, raw jsonb NOT NULL);                           -- cold
CREATE TABLE esa.schema_migrations  (version int PRIMARY KEY, applied_at timestamptz NOT NULL);
```

- `snapshots` and `solution_revisions` get a `BEFORE UPDATE OR DELETE` trigger that raises an error. This matches the existing immutability rule (`IMMUTABLE_KEYS` in the plugin's `EsaBackendState`).
- `seq` is the slice position, so the next row is always `seq = published length`.
- Schema migrations are embedded SQL files, applied at startup by the writer while it holds the lock. A small runner does this; no extra dependency is needed.

## Write path

`Store.updateLocked`, Postgres adapter:

1. `next := published.detach()`; run the callback; `projectInvestigationStates(&next)`. This is unchanged.
2. `changes := diff(published, next)`. If it is empty, **return without encoding or touching the database**.
3. Encode only the changed rows, applying the normalization above.
4. `Persister.Apply(ctx, changes, expectedRevision, term)`, in one transaction:
   ```sql
   BEGIN;
   UPDATE esa.meta SET revision = revision + 1, frame = $frame
    WHERE revision = $expected AND writer_term = $term;      -- 0 rows → fenced, ROLLBACK
   INSERT ... ON CONFLICT (id) DO UPDATE SET payload = EXCLUDED.payload, revision = ...;  -- upserts (pgx.Batch)
   DELETE FROM ... WHERE id = ANY($deleted);
   INSERT INTO esa.case_history ...;                          -- history tails
   COMMIT;
   ```
5. Only after `COMMIT` succeeds, publish `next` in memory. "A failed write never changes memory" still holds.

**When the commit outcome is unknown** (the connection drops during `COMMIT`), there is a single writer, so `meta.revision` settles it after reconnecting:

- `revision = expected + 1` means it committed: publish `next`.
- `revision = expected` means it did not: memory stays unchanged and the `Update` returns an error.
- Anything else means another writer exists: stop (see below).

The JSON adapter keeps its whole-file atomic write. It uses the same diff only to skip writes when nothing changed.

## Single writer and standby

The guarantee comes from two layers:
- **Safety** (no two processes ever both commit) relies **only on term fencing inside the commit transaction**.
- **The advisory lock is for liveness and efficiency.** It decides who *should* write and lets a standby wait without racing. It is never trusted on its own.

### Mechanism

**Taking over.** The order of these steps is part of the correctness argument:
1. On a dedicated `pgx.Conn` (not a pool), loop on `SELECT pg_try_advisory_lock(<esa key>)` every 5 s.
   - A session-level lock dies with its connection, and a single `pgx.Conn` never reconnects behind our back.
   - On this connection, `SET idle_session_timeout = 0`, and enable TCP keepalives (`tcp_keepalives_idle=10`, `interval=5`, `count=3`) so the server detects a dead peer.
2. Once the lock is held, **bump the term in its own committed transaction**: `UPDATE esa.meta SET writer_term = writer_term + 1 RETURNING writer_term`.
3. **Only after that commit**, apply schema migrations, load the in-memory tables, validate them, and record the loaded `meta.revision` as the base.
4. Start the HTTP API, the outbox poller and the workers. `Engine.Recover` and `recoverInvestigations` handle leases left by the previous writer, exactly as after a restart today.

**While writing.** Every commit starts with `UPDATE esa.meta ... WHERE revision = $expected AND writer_term = $term`. Zero rows means `ErrLostWriter`, which is handled as described under Losing the lock.

**Watching the lock** every 5 s on the lock connection: `SELECT pg_backend_pid()` must equal the PID recorded at acquisition, and `pg_locks` must show our advisory key `granted` for that PID.

**Losing the lock** (a check fails, the lock connection errors, or an `ErrLostWriter`):
1. **At once**, switch `/v1/*` to 503 and stop starting new `Update`s.
2. Cancel the workers and the outbox poller.
3. **Exit.** Compose's `restart: unless-stopped` restarts the process as a standby.

**Standby behavior:**
- `/healthz` returns 200 with `{"role":"standby"}`, so a supervisor doesn't kill it.
- `/healthz?role=active` returns 503, so a health-aware proxy can route only to the writer.
- Every `/v1/*` route returns 503 `"ESA standby: not the active writer"`. The plugin client already turns a non-2xx response into an "ESA unavailable" error.
- No workers or outbox poller run.
- **Deployment rule:** never put an active writer and a standby behind one round-robin service name. Compose runs a single instance by default. For high availability, run the standby as a separate service and route by `role=active`.

### Why it is correct

- **One row serializes everything.** Every commit and every takeover updates the single `esa.meta` row, so Postgres row locking orders them. Under `READ COMMITTED`, an `UPDATE` that waited for a concurrent update of the same row **re-checks its `WHERE` clause against the newly committed row**. There are only two orderings:
  - **The old writer's commit gets the row first.** It commits, and then the takeover's term bump proceeds. The new writer loads *after* its bump commits (step 3), so it sees the old commit.
  - **The bump gets the row first.** The old writer's `UPDATE ... WHERE writer_term = $old` is re-checked, matches nothing, and rolls back with `ErrLostWriter`.
- **So after any term bump, only the new term can commit, and the new writer's base includes every commit from earlier terms.** Loading *before* the bump would allow a stale base. The `revision` check would then fail the first write, but step order rules it out anyway.
- **The lock's role.** It only stops two processes from both *trying* to write and fencing each other. If the lock is lost without the writer noticing (for example a network partition while the process lives on), fencing still prevents any commit. The worst case is a few seconds of stale **reads** from the old process's memory, bounded by the 5 s check interval.
- **How long takeover waits.** A crashed writer's session is dropped when its TCP connection dies: immediately on a clean exit, or after about 25 s (10 + 3×5) of keepalive failure. The standby then gets the lock on its next 5 s poll.

**Effects outside the database during a takeover:**
- **Outbox acknowledgements:** `SyncRedmine` acknowledges an event only after `Receive` commits. A fenced `Receive` fails, so the event is not acknowledged and is redelivered to the new writer. Event idempotency covers an acknowledgement that was lost after a successful commit.
- **Provider calls in flight:** the old writer cannot record their results (fenced). The new writer finds the investigation's lease still owned by the old process. When the lease expires, `recoverInvestigations` marks it failed with "spend is uncertain", which is today's semantics after a crash. Provider work already started on the host may finish unrecorded, as with a crash today.
- **Investigation leases** (`Owner`/`Epoch`) are unchanged. They are state persisted like any other entity.

### Requirements

- Connect directly to Postgres, or through **session** pooling. PgBouncer *transaction* pooling breaks session advisory locks and is not supported.
- The advisory key is a fixed 64-bit constant derived from `"esa-writer"`, documented in `ESA_OPERATIONS.md`.

## Migration

Done once, with ESA stopped:

```sh
esa -database-url "$ESA_DATABASE_URL" -import-state /var/lib/esa/state.json        # from the JSON file
esa -database-url "$ESA_DATABASE_URL" -import-state-url "$ESA_STATE_URL"           # from the Redmine state API
```

- **Refuses a non-pristine database.** `meta` must be absent or at revision 0, and all entity tables must be empty. This keeps today's `-import-state` rule.
- Loads the source with the existing loaders, applies the normalization, and writes everything with `COPY` in **one transaction**.
- **Verifies before committing**, inside the same transaction:
  - Load every row back, including cold snapshots, and require `json.Marshal(loaded) == json.Marshal(source)` **byte for byte**, after restoring event tickets from their snapshots.
  - Load the hot set alone and require every hot entity to match.
  - Any mismatch rolls back.
- **Never modifies the source.** The JSON file and the Redmine state rows stay as they are. To roll back, deploy the previous image with `-store`.

After migration:

- Compose sets `ESA_DATABASE_URL` and drops `ESA_STATE_URL`.
- `ESA_STATE_API_ENABLED` is turned off.
- The plugin's `/esa/api/state` endpoints stay for one release, then are removed.

## Configuration and deployment

- **Flags:** a new `-database-url` (env `ESA_DATABASE_URL`). It cannot be combined with `-store`, and `-state-url` becomes deprecated.
- **Database:** an init script creates database `esa` and role `esa` with privileges only on `esa`.
  - `docker-entrypoint-initdb.d` runs only on a new volume, so existing installs need a documented one-time SQL step.
  - The password `ESA_DB_PASSWORD` is a new, separate secret. This is a first step for Issue 5.
- **Dependency:** `github.com/jackc/pgx/v5` is a Go **library** (a module of packages) compiled into the `esa` binary. It is not a separate program or service; ESA talks to the existing Postgres server through it. It is pure Go, so the static `CGO_ENABLED=0` build into the `scratch` image is unaffected.
  - It is added as a **normal `go.mod` requirement, not vendored** (decided 2026-10-17). `go.sum` pins the exact content of pgx and of each small module it pulls in, and builds verify downloads against those checksums and against `sum.golang.org`. Local builds and CI already download through `proxy.golang.org`, and the Docker image copies a prebuilt binary, so no build needs to run offline.
  - CI adds `go mod verify`. Dependency changes are reviewed through the `go.mod`/`go.sum` diff; `go mod graph` lists the exact modules when pgx is added.
  - `docs/DEPENDENCIES.md` and README's "No external Go modules" line need updating.
- **CI:** add a pinned `postgres:17` service container (by digest) to `.github/workflows/ci.yml`. Tests use `ESA_TEST_DATABASE_URL` and skip locally when it is unset; in CI it is required.

## Guarantees

| Guarantee | Today | Phase 3 mechanism | Test |
|---|---|---|---|
| Event idempotency (same ID and content means duplicate; same ID with different content is rejected) | Compare `hash(old.Ticket)` | Compare `EventRecord.TicketHash` | Existing idempotency and outbox tests, plus a reload variant |
| A failed callback leaves memory unchanged | `detach` | Unchanged | Existing `TestUpdateFailureLeaves…` |
| A failed write leaves memory unchanged | Write before publish | `Apply` before publish; unknown outcome settled by `meta.revision` | Fault injection: fail before, during and after `COMMIT` |
| Multi-entity changes are atomic | Whole-file rename | One Postgres transaction | Fail after N of M rows; database and memory unchanged |
| Single writer | `flock` / Redmine CAS | Term fencing in every commit (safety) plus an advisory lock (liveness); see [Why it is correct](#why-it-is-correct) | The fencing tests in test plan item 5 |
| Stale investigation owners are rejected | Lease/epoch in state | Unchanged | Existing pipeline and lease tests |
| Snapshots and solution revisions are immutable | Plugin check | Database trigger plus a Go check | Trigger test |
| Nothing after the forwarding cutoff is used | State data | Unchanged; `CaseHistory` stays hot | Existing temporal tests, plus the hot-only reload |
| Crash safety | fsync plus directory sync | Postgres durability (`synchronous_commit = on`) | Integration test: `kill -9` during ingest, restart, verify |

## Test plan

1. **Diff unit tests:** add, replace, delete, identical replace, history tail, and scalar fields. Also confirm `projectInvestigationStates` marks nothing changed when values are equal.
2. **Diff cross-check in the guard:** in tests, every `Update` compares the bitwise diff against a full-encoding diff, across the whole `internal/esa` suite.
3. **Store contract suite for both adapters:** the same tests run against the JSON and Postgres persisters. They cover idempotency, failed callbacks, failed writes, unknown commit outcomes and no-op `Update`s.
4. **Reload-after-every-Update mode** (Postgres, tests only): after each commit, throw memory away, reload from the database (hot set only), and require the same encoding. Running the whole suite in this mode catches anything not persisted, and any read that the hot set misses.
5. **Single writer and fencing.** These run against real Postgres with two connections, and use explicit `BEGIN` and pauses to force each interleaving:
   - *Old commit first:* the old writer locks `meta` and commits; the bump waits, then commits. The new writer's load contains the old commit.
   - *Bump first:* the bump commits, then the old writer's `UPDATE ... WHERE writer_term = $old` matches no row and returns `ErrLostWriter`. The database and the new writer are unchanged.
   - *Load order:* a writer that loaded before the bump cannot commit (revision mismatch). This pins the takeover step order.
   - *Lock loss:* `pg_terminate_backend` on the lock connection. Within one check interval the writer returns 503 on `/v1/*` and exits, and the standby takes the lock on its next poll.
   - *Outbox across a takeover:* an event whose `Receive` was fenced is not acknowledged, and the new writer receives it exactly once.
   - *Standby endpoints:* `/healthz` returns `role=standby`, `/healthz?role=active` returns 503, and `/v1/*` returns 503.
6. **Migration:** fixtures that include legacy runs without `SnapshotID`, unknown `StoredFields`, private tickets, rejections and `Raw` events. Import, verify byte for byte, refuse a non-pristine database, and leave the source untouched.
7. **Cold reads:** `GET /v1/snapshots/{id}` for a cold snapshot, and Issue-directory export with only the hot set loaded.
8. **Benchmarks:** the existing store benchmarks gain a Postgres variant at 1k, 10k and 20k. A 20k ingest script checks the "minutes" target. The ticket GET for a ticket *with* an Issue is measured too.
9. **End to end:** `system_replay` fixtures unchanged, and `verify_deployment.py` (restart persistence) run in Postgres mode.

## Rollout (one PR each)

1. **Diff-based change detection** in `Store`, in memory only. The JSON adapter skips no-op writes without encoding, and the guard cross-checks the diff. No format change.
2. **`Persister` interface** (`Load`, `Apply(changes)`), with the JSON adapter behind it.
3. **`EventRecord`** in memory, plus a JSON loader that converts old events. This is the first persisted-format change, and the loader handles it.
4. **Postgres adapter:** schema and migrations, `Apply`, hot-set loading, advisory lock and term fencing, the `pgx` requirement with `go mod verify` in CI, and the CI Postgres service.
5. **Import and verification tool,** compose and init SQL, `.env.example`, and docs.
6. **Cold snapshot reads** for `GET /v1/snapshots/{id}`.
7. **Retire ESA's use of the Redmine state API:** deprecate `-state-url` and keep the plugin endpoint for one release.

Each step leaves `main` releasable. Steps 1–3 also speed up the JSON adapter.

## Risks

- **`unsafe` comparison.** It is correct only while the immutability contract holds. The test guard enforces the contract and cross-checks the diff. Comparing values of the same Go type is stable across Go versions.
- **Memory footprint.** The working set stays in RAM: 280 MB of live heap for the full 19,869-issue Kafka archive (see [Memory](#memory)), about 0.6 GB of process memory with the default `GOGC`. Benchmark step 8 tracks it as the data grows. Run reports and `Case.Summary` copies are the next candidates if it ever needs to shrink.
- **Postgres availability.** ESA fails closed when the database is unavailable, as it does today with the Redmine backend: `/v1/*` returns 503 and no writes are accepted.
- **Diff cost stays O(entries)** (24 ms at 20k). That is acceptable for the targets. The `Table[V]` refactor is the fallback.

## Review answers and open questions

1. **Standby returning 503: accepted** (2026-10-17), on condition that the mechanism is correct. See [Single writer and standby](#single-writer-and-standby) for the mechanism, the correctness argument and the interleaving tests.
2. **`pgx` as a dependency:** a normal `go.mod` requirement pinned by `go.sum`, **not vendored** (2026-10-17). See [Configuration and deployment](#configuration-and-deployment).
3. **Memory:** measured at 280 MB of live heap for the full archive (see [Memory](#memory)). Run reports and `Case.Summary` stay in memory.
4. **Plugin `/esa/api/state` endpoints: remove** them one release after migration, once no installation still uses them (2026-10-17).
