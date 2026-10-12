# External system replay

The current tester is the standalone [`system_replay`](../system_replay/README.md) Go module. It can build and test outside the ESA project and has no application imports. The former direct-provider pipeline, local issue assignment engine and imported-prediction evaluation entry point were removed.

Replay enters the system through configured public requests. The system itself performs classification, issue/case handling, routing, queuing and engineering-provider invocation. The tester observes those results, freezes them, advances product Git snapshots and applies unchanged runtime assertions. It does not reproduce business logic inside the test harness.

Build the application artifact and tester independently, then run:

```sh
GOCACHE="$PWD/.tools/go-cache" GOTOOLCHAIN=local .tools/go/bin/go build -o bin/esa-host ./cmd/esa
sh scripts/build_system_replay.sh
python3 scripts/replay_pipeline.py --pipeline --manifest system_replay/examples/esa-api-lifecycle.json --output reports/new-system-lifecycle
python3 scripts/replay_pipeline.py --pipeline --manifest system_replay/examples/esa-api-kafka.json --output reports/new-system-kafka
```

The first example verifies actual case collection, approval, zero-provider-call case reuse, withdrawal after reopening and customer-support routing through the ESA HTTP API. The second submits a real historical ticket through the same API and verifies baseline / PR parent / fixed Kafka behavior in an independent runtime. Both use a fresh managed application process and independent state; the defaults do not call a model. Neither modifies the running Redmine or live ESA store.

A repair is `behavior_verified` only if the same scenario reproduces the baseline and parent failure, all controls pass, and repaired code passes healthy assertions without reproducing the failure. PR/file hits alone cannot pass this gate. A separate configured HTTP evaluator can assess the actual initial findings against PR/source evidence after this runtime gate. Without it, root-cause/solution semantics remain unassessed.

`--plan` validates without executing systems; `--track-only` inspects Git without submitting tickets. `--attach` explicitly opts into an existing target and does not reset or stop it. Numeric version ordering must not deliver future events before older ones; use `--order time` when needed. Related, overlapping event histories should be expressed as a single chronological job stream.

The initial adapter tests the backend HTTP boundary. Browser actions, Redmine outbox behavior, universal customer-environment reconstruction and virtual business time require corresponding external adapters/capabilities. Missing capabilities are recorded as limitations instead of supplied by substitute business implementations. Full protocol, safety boundaries, runtime profiles, cleanup and assertion details are in the [standalone module documentation](../system_replay/README.md).
