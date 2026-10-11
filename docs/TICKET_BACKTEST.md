# Ticket-centered mainline backtesting

This implements the original ESA design: the engineering Agent classifies a ticket, creates or joins an issue and freezes specific code locations, problem descriptions and proposed solutions. The pipeline then follows subsequent versions on the main branch. Changes in issue scope trigger a separate backtest Agent, which uses the PR and patch to confirm a repair and assess the original prediction. This is retrospective PR-based assessment, not an independent ground-truth construction workflow.

## Run

Build all Go commands:

```sh
sh scripts/build_esa.sh
```

Plan a batch without Docker or model calls:

```sh
python3 scripts/replay_pipeline.py --pipeline --plan KAFKA-3258 KAFKA-2880
```

Inspect later Git versions and issue-key candidates without invoking either Agent:

```sh
python3 scripts/replay_pipeline.py --pipeline --track-only --versions 3 --mainline origin/trunk KAFKA-2880
```

Diagnose the ticket and review all candidate fixes in the next release observation point:

```sh
python3 scripts/replay_pipeline.py --pipeline --agent --review-fixes --profile metadata-disconnect --versions 1 KAFKA-2880
```

`--agent` enables initial diagnosis. `--review-fixes` separately enables runtime replay and retrospective model calls. Neither is implicit. `--versions N` defaults to five subsequent release observation points. `--until RFC3339` adds an upper timestamp bound. `--mainline` selects the main branch and defaults to `origin/trunk` for Kafka. Multiple tickets require `--pipeline`; initial environments remain sorted by numeric affected version. Version development refs still require explicit historical provenance and an ordering label.

A JSON job manifest supports the existing `raw`, `profile`, `ref`, `at`, `version`, `environment`, `project`, `command`, `command_timeout` and `scala_version` fields. Operator-supplied commands execute in the owned runtime. Ticket text is never executed as a command.

Use `--issue-history <prior-run>/issue-history.json` to supply previously established issues. The runner exposes only revisions visible at each ticket cutoff and writes the extended history to the new run.

Use `--predictions predictions.json` to import existing structured reports. The file maps ticket keys to absolute report paths. Reports are copied into the new run, checked against historical source, made read-only and SHA-256 pinned. External report provenance and runtime evidence remain operator supplied. Old prose-only reports cannot be converted into scored line locations automatically.

## Engineering output

The model returns only these structured diagnostic fields:

```json
{
  "classification": "code_defect",
  "issue": {"existing_id": "", "title": "Metadata completion fails on disconnect", "reason": "New issue supported by the ticket"},
  "findings": [{
    "commit": "<historical SHA>", "file": "<repository path>",
    "start_line": 200, "end_line": 205,
    "problem": "Specific faulty behavior and its cause",
    "solution": "Proposed correction, without applying a patch",
    "evidence": [{"source": "git", "reference": "<SHA>:<path>:200", "quote": "Exact source substring"}]
  }],
  "limitations": []
}
```

A finding's commit must equal the registered historical revision. Both endpoint lines must have been inspected; each finding needs an exact quote from its own file and range. Empty findings explicitly mean that no concrete location was established. Support categories cannot contain engineering findings. The adapter derives the existing ESA report fields for display compatibility; usage, alerts and environment requests come from the runner and audited tools.

Classification belongs to the Agent; deterministic rules supply a hint. Categories are `code_defect`, `compatibility`, `performance`, `configuration`, `usage_question`, `feature_request` and `unknown`. Only the first three receive engineering fix assessment. Existing issue assignments must refer to a supplied issue in the same project and category. The pipeline records issue revisions and exposes only revisions observed at or before the next ticket's cutoff. It does not merge issues merely because their keywords match.

## Pipeline stages

1. **Diagnosis:** reconstruct historical ticket fields, prepare the affected version, run the registered baseline and invoke the engineering Agent when requested. Every report is frozen before future Git history is read. Known issue membership contains no subsequent fix data.
2. **Version tracking:** resolve an immutable mainline head, build the first-parent graph and label subsequent observation points with numeric release tags. Branch release tags identify publication observation points; the source being evaluated is always the mainline snapshot at that time. Branch-only code is not substituted for mainline code. Duplicate tags observing the same mainline snapshot count once.
3. **Scope matching:** inspect every mainline change in the window. An exact ticket-key reference or a change to an initially localized file creates a candidate. Line offsets and renames are followed between commits. File matching is deliberately broad so nearby fixes can be reviewed. Unmerged branches and commits before ticket receipt are excluded. Matching does not itself establish correctness.
4. **Retrospective replay:** combine all jobs' version observation points and candidate before/after revisions in mainline order. One owned environment is reused; only source and runtime differences are synchronized. Customer state and helper state reset between observations. The same neutral observation harness runs before and after a candidate. It contains no expected future exception type.
5. **PR assessment:** start a fresh, tool-free backtest Agent session with the frozen prediction, issue, local PR reference/message, actual patch, numbered before/after source and runtime observations. This Agent intentionally sees the initial prediction for reverse assessment. It never shares state with the engineering Agent.
6. **Aggregation:** record repair verdicts and assess each original finding's specific location, problem description and solution independently. A confirmed later regression reopens the issue. All candidates remain in the trace, including unsuccessful and uncertain ones; tracking continues after the first confirmed repair.

A version tag's commit time is a local Git observation timestamp, not proof of a public release date. If an affected snapshot differs from the mainline state at ticket receipt, its line locations are mapped to that state and the divergence is retained as a limitation. No Git checkout or reset modifies the product repository.

## Review and interpretation

The review verdict is `resolved`, `not_resolved`, `regressed` or `uncertain`. A resolved verdict requires a PR reference in the local Git record and exact evidence citations. Each initial finding gets `correct`, `incorrect` or `unknown` for localization, problem and solution, with an explanation. Without a confirmed repair, accuracy remains unknown. A proposed solution can be causally correct without matching the PR's exact implementation. Inconsistent confirmed assessments remain unknown for engineer review.

`evaluation.json` records the issue state, confirmed repair commits, every completed review, counts of fully correct / incorrect / unknown findings, and pending candidates. A missing prediction, execution failure, unknown localization or unfinished review is not a successful diagnosis. A file hit or keyword hit is not an accuracy score. Static PR assessment is not runtime verification; the output retains the exact runtime scope.

PR records currently come from local commit messages, linked PR identifiers and actual committed changes. Review discussions and external PR metadata are not automatically downloaded; their absence must be disclosed. The `source` profile establishes runtime readiness only. The registered metadata profile reproduces a specific client scenario; `kafka-server` release binaries cannot verify an arbitrary mainline source commit and are explicitly marked unsupported for that part of evaluation. Additional product/runtime profiles require operator registration.

## Artifacts and operational controls

A new output directory contains `pipeline.json`, `issues.json`, `issue-history.json` and per-ticket plans, frozen reports with hashes, `trace.json`, runtime observations, review packets, CLI events/audits and `evaluation.json`. Failed attempts retain private logs. Reports and verified source/artifact caches persist across cleanup.

All existing engineering permissions remain: only registered installation, isolated system/debugger execution, source reads and auxiliary scripts are available; product files remain read-only. Environment defects are reported for engineer review. Token thresholds emit warnings without terminating Agent work. The pipeline also warns on per-ticket aggregate measured usage across initial diagnosis and retrospective attempts. Missing usage is unknown; failed calls may have consumed unreported tokens. Runtime profiles and explicit execution deadlines remain separate from token warnings.

Normal exit, exceptions, Ctrl-C and SIGTERM clean owned containers, volume and temporary state. Recovery after SIGKILL remains available through `--cleanup`. A failed environment setup can leave no runnable diagnosis; that condition must not become a product defect claim.

## Verification

```sh
GOCACHE="$PWD/.tools/go-cache" GOTOOLCHAIN=local .tools/go/bin/go test -race ./...
GOCACHE="$PWD/.tools/go-cache" GOTOOLCHAIN=local .tools/go/bin/go vet ./...
python3 -m unittest discover -s scripts -p 'test_backtest_pipeline.py' -v
python3 scripts/verify_mainline_backtest.py
python3 scripts/verify_mainline_backtest.py --runtime
```

Synthetic Git tests verify version windows, temporal boundaries, line changes, renames, unmerged exclusion, explicit grouping, evidence validation, unknown judgments and reopened issues. Pipeline tests verify phase isolation, temporal issue visibility, advisory aggregate usage and cleanup on failures. The real Kafka check tracks KAFKA-2880 / PR #581, optionally observes historical baseline, PR parent and repaired source, and checks cleanup without model calls. It does not assign Agent accuracy labels.
