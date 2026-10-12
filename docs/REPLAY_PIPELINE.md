# System replay entry point

The current pipeline is the independent [external system replay tester](TICKET_BACKTEST.md). Its [standalone module](../system_replay/README.md) defines public target adapters, historical input isolation, runtime protocols, scenarios, assertions, version tracking and cleanup.

```sh
sh scripts/build_system_replay.sh
python3 scripts/replay_pipeline.py --pipeline --manifest system_replay/examples/esa-api-lifecycle.json --output reports/new-system-run
```

The tester needs an opaque target executable or explicit attached HTTP service. Build the ESA host executable separately for the provided examples. It does not import application modules or call its internal classification, issue assignment, business engine or Agent adapter.

The old positional-ticket, `--agent`, imported-prediction and local issue-history shortcuts are removed. `--plan` performs no system/model calls; `--track-only` is explicitly not a system test. Product scenario registration is required for fault reproduction and repair validation.

Application-owned historical packet preparation and native replay MCP tools remain available for configuring the system's engineering provider. They are part of the application under test, not the external tester's business logic. Their historical reports remain unchanged.
