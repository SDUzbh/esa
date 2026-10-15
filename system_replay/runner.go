package systemreplay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Options distinguishes real system replay from planning/tracking and explicit attach mode.
type Options struct {
	PlanOnly, Pipeline, Attach, TrackOnly bool
	Order                                 string
}

func versionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 4; i++ {
		x, y := 0, 0
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			return x < y
		}
	}
	return a < b
}

// Run observes an opaque public target, freezes predictions, then follows later Git repairs.
// Historical ordering overrides version-based environment reuse. Owned resources are cleaned
// on success, failure or cancellation; attached target processes are never stopped.
// The returned error includes failed assertions, persistence and cleanup failures.
func Run(ctx context.Context, path, output string, options Options) (summary Summary, err error) {
	m, e := load(path)
	if e != nil {
		return summary, e
	}
	root, e := filepath.Abs(output)
	if e != nil {
		return summary, e
	}
	if _, e = os.Stat(root); !os.IsNotExist(e) {
		return summary, fmt.Errorf("output must be a new directory")
	}
	if e = os.MkdirAll(root, 0700); e != nil {
		return summary, e
	}
	summary = Summary{Schema: 1, Phase: "planning", Target: m.Target.URL, Cleanup: "not_started", Limitations: []string{"Public HTTP adapter tests the backend entry point; Redmine UI/outbox are outside this adapter.", "Startup/readiness alone does not establish a replay or an Agent accuracy result."}}
	var reportErr error
	persist := func(path string, value any) {
		if e := write(path, value); e != nil {
			reportErr = errors.Join(reportErr, fmt.Errorf("write report %s: %w", path, e))
		}
	}
	save := func() { persist(filepath.Join(root, "pipeline.json"), summary) }
	defer func() {
		for _, result := range summary.Results {
			persist(filepath.Join(root, "jobs", safe(result.ID), "result.json"), result)
		}
		err = errors.Join(err, reportErr)
		if err != nil {
			summary.Phase = "failed"
			summary.Error = err.Error()
		}
		err = errors.Join(err, write(filepath.Join(root, "pipeline.json"), summary))
	}()
	plans := []Plan{}
	for _, job := range m.Jobs {
		p, e := preparePlan(ctx, m, job)
		result := Result{ID: job.ID, State: "planned", Plan: p}
		if e != nil {
			result.State = "needs_clarification"
			result.Error = e.Error()
			summary.Results = append(summary.Results, result)
			continue
		}
		plans = append(plans, p)
	}
	if len(summary.Results) > 0 {
		for _, p := range plans {
			summary.Results = append(summary.Results, Result{ID: p.Job.ID, State: "planned", Plan: p})
		}
		return summary, fmt.Errorf("one or more replay plans are invalid")
	}
	if !options.Pipeline && len(plans) > 1 {
		return summary, fmt.Errorf("multiple jobs require --pipeline")
	}
	if options.Order == "" {
		options.Order = "version"
	}
	if options.Order != "version" && options.Order != "time" {
		return summary, fmt.Errorf("order must be version or time")
	}
	sort.SliceStable(plans, func(i, j int) bool {
		if options.Order == "version" && plans[i].Version != plans[j].Version {
			return versionLess(plans[i].Version, plans[j].Version)
		}
		a, _ := timestamp(plans[i].At)
		b, _ := timestamp(plans[j].At)
		return a.Before(b)
	})
	if options.PlanOnly {
		for _, p := range plans {
			summary.Results = append(summary.Results, Result{ID: p.Job.ID, State: "planned", Plan: p})
		}
		summary.Phase = "planned"
		summary.Cleanup = "not_required"
		return summary, nil
	}
	if !options.TrackOnly {
		if m.Target.URL == "" || len(m.Target.TerminalStates) == 0 || m.Target.Submit.Path == "" || m.Target.Observe.Path == "" {
			return summary, fmt.Errorf("target URL, public submit/observe requests and terminal states required")
		}
		// One target session must never receive future events before older ones merely
		// to save environment transfers. Version order remains the default when valid.
		var last time.Time
		for _, p := range plans {
			at, _ := timestamp(p.At)
			if !last.IsZero() && at.Before(last) {
				return summary, fmt.Errorf("version order would send future events first; use --order time")
			}
			last = at
			for _, event := range p.Job.Events {
				at, _ := timestamp(event.At)
				if at.Before(last) {
					return summary, fmt.Errorf("historical event stream is out of order")
				}
				last = at
			}
		}
	}
	token := ""
	if m.Target.TokenEnv != "" {
		token = os.Getenv(m.Target.TokenEnv)
	}
	if token == "" && !options.Attach {
		token = randomID() + randomID()
	}
	vars := map[string]any{"run_dir": root, "manifest_dir": m.BaseDir, "token": token, "target_url": m.Target.URL, "session_dir": filepath.Join(root, "sessions")}
	var target *Process
	var client *Client
	var runtime *Runtime
	defer func() {
		cleanup := []string{}
		if target != nil {
			if e := target.close(); e != nil {
				cleanup = append(cleanup, e.Error())
			}
			redactLog(filepath.Join(root, "target.log"), token)
		}
		if runtime != nil {
			if e := runtime.close(); e != nil {
				cleanup = append(cleanup, e.Error())
			}
		}
		if len(cleanup) > 0 {
			summary.Cleanup = "failed"
			summary.Limitations = append(summary.Limitations, cleanup...)
			if err == nil {
				err = fmt.Errorf("owned resource cleanup failed")
			}
		} else if options.TrackOnly {
			summary.Cleanup = "not_required"
		} else if options.Attach {
			summary.Cleanup = "runtime_cleaned_target_external"
		} else {
			summary.Cleanup = "completed"
		}
	}()
	if !options.TrackOnly {
		client, e = newClient(m.Target, token)
		if e != nil {
			return summary, e
		}
		if options.Attach {
			if !health(ctx, client) {
				return summary, fmt.Errorf("attached target is not healthy")
			}
			summary.Limitations = append(summary.Limitations, "Attached target state/isolation is operator supplied; it is not cleared by the tester.")
		} else {
			if health(ctx, client) {
				return summary, fmt.Errorf("target already responds at this URL; refusing writes without --attach")
			}
			target, e = start(m.Target.Start, vars, m.BaseDir, filepath.Join(root, "target.log"))
			if e != nil {
				return summary, e
			}
			if e = waitHealth(ctx, client, target); e != nil {
				return summary, e
			}
		}
		if m.Target.AdvanceClock == nil {
			summary.Limitations = append(summary.Limitations, "Events are delivered in historical order, but the target business clock is wall time; virtual-time behavior is untested.")
		}
	}
	ensureRuntime := func() error {
		if runtime != nil {
			return nil
		}
		var e error
		runtime, e = runtimeStart(m.Runtime, vars, m.BaseDir, root)
		return e
	}
	summary.Phase = "system_replay"
	save()
	// The runner never classifies, assigns issues, retrieves cases or invokes an
	// engineering provider. Those outcomes are observed from the actual target.
	for _, p := range plans {
		e = nil
		result := Result{ID: p.Job.ID, State: "running", Plan: p}
		dir := filepath.Join(root, "jobs", safe(p.Job.ID))
		persist(filepath.Join(dir, "plan.json"), p)
		if reportErr != nil {
			return summary, reportErr
		}
		v := varsFor(p, root)
		for k, x := range vars {
			v[k] = x
		}
		v["event_id"] = safe(filepath.Base(root)) + "_" + safe(p.Job.ID) + "_initial"
		if p.Commit != "" {
			source, _, e := export(ctx, m.Repo, p.Commit, root)
			if e != nil {
				result.Error = e.Error()
			} else {
				v["product_source"] = source
			}
		}
		if result.Error == "" && p.Job.Scenario != nil && !options.TrackOnly {
			if e = ensureRuntime(); e == nil {
				var record RuntimeRecord
				record, e = observeRuntime(ctx, runtime, m, p, p.Commit, root)
				if e == nil {
					result.Baseline = &record
					persist(filepath.Join(dir, "baseline.json"), record)
					v["runtime_observation"] = record.Outcome
				}
			}
			if e != nil {
				result.Error = e.Error()
			}
		}
		if result.Error == "" && !options.TrackOnly {
			// Only the historical context is sent to the target's own environment adapter.
			// Scenario assertions, future commits and evaluation labels are withheld.
			v["historical_context"] = map[string]any{"ticket": p.Ticket, "source_commit": p.Commit, "cutoff": p.At, "source_dir": v["product_source"], "runtime": v["runtime_observation"]}
			historicalPath := filepath.Join(dir, "historical-context.json")
			persist(historicalPath, v["historical_context"])
			v["historical_context_path"] = historicalPath
			if m.Target.PrepareCommand != nil {
				e = runCommand(ctx, *m.Target.PrepareCommand, v, m.BaseDir, filepath.Join(dir, "target-prepare.log"))
				redactLog(filepath.Join(dir, "target-prepare.log"), token)
			}
			if e == nil && m.Target.Prepare != nil {
				_, e = client.accept(ctx, *m.Target.Prepare, v)
			}
			if e != nil {
				result.Error = e.Error()
			} else {
				events := append([]Event{{At: p.At, Expect: p.Job.Expect}}, p.Job.Events...)
				for i, event := range events {
					v["event_id"] = safe(filepath.Base(root)) + "_" + safe(p.Job.ID) + "_" + fmt.Sprint(i)
					record := client.event(ctx, event, v)
					if !event.SkipObserve && record.Observation.Raw != nil {
						tokenWarnings(&result, record.Observation.Usage, m, record.Observation.RunID)
					}
					result.Events = append(result.Events, record)
					persist(filepath.Join(dir, fmt.Sprintf("event-%03d.json", i)), record)
					if record.Error != "" || reportErr != nil {
						result.Error = record.Error
						break
					}
				}
			}
		}
		if result.Error != "" {
			result.State = "failed"
		} else if options.TrackOnly {
			result.State = "tracking_only"
		} else {
			result.State = "observed"
		}
		if len(result.Events) > 0 {
			predictionPath := filepath.Join(dir, "system-output.json")
			bytes, _ := json.Marshal(result.Events)
			result.PredictionHash = sha(bytes)
			if e = os.WriteFile(predictionPath, bytes, 0400); e != nil {
				reportErr = errors.Join(reportErr, e)
			}
		}
		persist(filepath.Join(dir, "result.json"), result)
		summary.Results = append(summary.Results, result)
		save()
		if e := errors.Join(ctx.Err(), reportErr); e != nil {
			return summary, e
		}
	}
	// Future evidence is read only after every initial public-system output freezes.
	history := &historyReader{Git{m.Repo, ctx}, map[string]string{}}
	summary.Phase = "version_tracking"
	save()
	type task struct {
		index int
		c     Candidate
	}
	tasks := []task{}
	for i := range summary.Results {
		result := &summary.Results[i]
		if result.Error != "" || result.Plan.Commit == "" {
			continue
		}
		if _, _, e := export(ctx, m.Repo, result.Plan.Commit, root); e != nil {
			result.Error = e.Error()
			result.State = "failed"
			continue
		}
		if result.PredictionHash != "" {
			bytes, e := os.ReadFile(filepath.Join(root, "jobs", safe(result.ID), "system-output.json"))
			if e != nil || sha(bytes) != result.PredictionHash {
				result.Error = "frozen public-system output changed"
				result.State = "failed"
				continue
			}
		}
		observation := Observation{}
		if len(result.Events) > 0 {
			observation = initialObservation(*result)
		}
		issue := Issue{ID: observation.IssueID, TicketKeys: []string{text(result.Plan.Ticket["source_key"])}, Findings: observation.Findings}
		if issue.TicketKeys[0] == "" {
			issue.TicketKeys[0] = result.ID
		}
		trace, e := history.Trace(issue, result.Plan.Commit, m.Mainline, result.Plan.At, m.Until, m.Versions)
		if e != nil {
			result.Error = e.Error()
			result.State = "failed"
			continue
		}
		result.Trace = &trace
		persist(filepath.Join(root, "jobs", safe(result.ID), "trace.json"), trace)
		if options.TrackOnly || result.Plan.Job.Scenario == nil {
			continue
		}
		for _, c := range trace.Candidates {
			tasks = append(tasks, task{i, c})
		}
	}
	summary.Phase = "behavior_verification"
	save()
	if len(tasks) > 0 {
		if e = ensureRuntime(); e != nil {
			return summary, e
		}
		chain, e := history.run("rev-list", "--first-parent", "--reverse", summary.Results[tasks[0].index].Trace.MainlineHead)
		if e != nil {
			return summary, e
		}
		order := map[string]int{}
		for i, c := range strings.Fields(chain) {
			order[c] = i
		}
		// All parent and repaired snapshots are prepared globally in mainline order.
		type point struct {
			index  int
			commit string
		}
		points := []point{}
		seen := map[string]bool{}
		for _, t := range tasks {
			for _, commit := range []string{t.c.Parent, t.c.Commit} {
				key := fmt.Sprint(t.index) + ":" + commit
				if !seen[key] {
					seen[key] = true
					points = append(points, point{t.index, commit})
				}
			}
		}
		sort.SliceStable(points, func(i, j int) bool { return order[points[i].commit] < order[points[j].commit] })
		observations := map[string]RuntimeRecord{}
		failures := map[string]string{}
		for _, point := range points {
			result := &summary.Results[point.index]
			key := fmt.Sprint(point.index) + ":" + point.commit
			record, e := observeRuntime(ctx, runtime, m, result.Plan, point.commit, root)
			if e != nil {
				failures[key] = e.Error()
			} else {
				observations[key] = record
			}
			persist(filepath.Join(root, "jobs", safe(result.ID), "runtime", point.commit+".json"), record)
			save()
			if e := errors.Join(ctx.Err(), reportErr); e != nil {
				return summary, e
			}
		}
		for _, t := range tasks {
			result := &summary.Results[t.index]
			repair := Repair{Candidate: t.c, Before: observations[fmt.Sprint(t.index)+":"+t.c.Parent], After: observations[fmt.Sprint(t.index)+":"+t.c.Commit], State: "unverified"}
			for _, commit := range []string{t.c.Parent, t.c.Commit} {
				if failure := failures[fmt.Sprint(t.index)+":"+commit]; failure != "" {
					repair.Error = failure
				}
			}
			if repair.Error == "" && result.Baseline != nil {
				repair.State = repairState(*result.Baseline, repair.Before, repair.After)
			}
			// Patch overlap is a transparent metric, not a causal accuracy verdict.
			if repair.State == "behavior_verified" && len(result.Events) > 0 {
				for n, f := range initialObservation(*result).Findings {
					hit := false
					mapped := f
					if f.Commit != t.c.Parent {
						patch, e := history.run("diff", "--no-ext-diff", "--no-textconv", "--find-renames", "--unified=0", f.Commit, t.c.Parent)
						if e == nil {
							mapped = mapFindings([]Finding{f}, changes(patch))[0]
						}
					}
					for _, change := range t.c.Changes {
						if change.OldFile == mapped.File && overlap(mapped.StartLine, mapped.EndLine, change.OldStart, change.OldCount) {
							hit = true
						}
					}
					repair.Localization = append(repair.Localization, map[string]any{"finding_index": n, "patch_overlap": hit, "problem_correctness": "unassessed", "solution_correctness": "unassessed"})
				}
			}
			if repair.Error != "" || repair.State == "environment_failure" || repair.State == "scenario_changed" {
				result.Error = "Runtime verification failed: " + repair.Error + " (" + repair.State + ")"
			}
			if repair.State == "behavior_verified" && m.Evaluator != nil {
				assessment, e := evaluateRepair(ctx, m, *result, repair)
				if e != nil {
					repair.Error = "semantic assessment: " + e.Error()
					result.Error = repair.Error
				} else {
					repair.SemanticAssessment = assessment
					if obj, ok := assessment.(map[string]any); ok {
						usage, _ := obj["usage"].(map[string]any)
						tokenWarnings(result, usage, m, "evaluation:"+repair.Candidate.Commit)
					}
				}
			}
			result.Repairs = append(result.Repairs, repair)
		}
	}
	summary.Phase = "completed"
	for i := range summary.Results {
		r := &summary.Results[i]
		if r.Error != "" {
			r.State = "failed"
			continue
		}
		if options.TrackOnly {
			r.State = "tracking_only"
		} else if r.Plan.Job.Scenario == nil {
			r.State = "system_observed_replay_unregistered"
		} else if r.Baseline == nil || !passed(r.Baseline.Controls) {
			r.State = "environment_failure"
		} else if !passed(r.Baseline.Bug) {
			r.State = "baseline_not_reproduced"
		} else {
			r.State = "reproduced"
			for _, repair := range r.Repairs {
				if repair.State == "behavior_verified" {
					r.State = "repair_behavior_verified"
				}
			}
		}
	}
	for _, r := range summary.Results {
		if r.Error != "" || r.State == "environment_failure" || r.State == "baseline_not_reproduced" {
			return summary, fmt.Errorf("one or more system replay tests failed; inspect pipeline.json")
		}
	}
	return summary, nil
}
func tokenWarnings(result *Result, usage map[string]any, m Manifest, key string) {
	if result.UsageKeys == nil {
		result.UsageKeys = map[string]bool{}
	}
	if key != "" && result.UsageKeys[key] {
		return
	}
	if key != "" {
		result.UsageKeys[key] = true
	}

	if usage == nil {
		result.Alerts = append(result.Alerts, "Target token usage is unavailable; usage is unknown.")
		return
	}
	reported, _ := usage["tokens_reported"].(bool)
	if !reported {
		result.Alerts = append(result.Alerts, "Target token usage is unreported; it is not zero.")
		return
	}
	in, _ := usage["input_tokens"].(float64)
	out, _ := usage["output_tokens"].(float64)
	if result.MeasuredUsage == nil {
		result.MeasuredUsage = map[string]int{}
	}
	result.MeasuredUsage["input_tokens"] += int(in)
	result.MeasuredUsage["output_tokens"] += int(out)
	in = float64(result.MeasuredUsage["input_tokens"])
	out = float64(result.MeasuredUsage["output_tokens"])

	for _, limit := range []struct {
		name      string
		actual    float64
		threshold int
	}{{"input", in, m.WarnInputTokens}, {"output", out, m.WarnOutputTokens}, {"total", in + out, m.WarnTotalTokens}} {
		if limit.threshold > 0 && limit.actual > float64(limit.threshold) {
			message := fmt.Sprintf("Ticket aggregate token warning: %s measured usage %.0f exceeds %d; target execution is not stopped.", limit.name, limit.actual, limit.threshold)
			found := false
			for _, old := range result.Alerts {
				if old == message {
					found = true
				}
			}
			if !found {
				result.Alerts = append(result.Alerts, message)
			}
			fmt.Fprintln(os.Stderr, "WARNING "+result.ID+": "+message)
		}
	}
}
