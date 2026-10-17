package esa

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// runQueueWorkers runs four workers, as cmd/esa does, until every task stops.
func runQueueWorkers(t *testing.T, e *Engine, provider PipelineAnalyzer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	settled := func() bool {
		s := e.Store.Snapshot()
		if s.Investigations.Len() == 0 {
			return false
		}
		for _, task := range s.Investigations.All() {
			if task.State == "queued" || activeInvestigation(task) {
				return false
			}
		}
		return true
	}
	var wg sync.WaitGroup
	for i := 0; i < EngineeringConcurrency+1; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil && !settled() {
				if worked, err := e.ProcessOne(ctx, provider); err != nil {
					t.Error(err)
					return
				} else if !worked {
					time.Sleep(10 * time.Millisecond)
				}
			}
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		t.Fatal("workers did not settle")
	}
}

// watchState calls found once when any task matches, until stop closes.
func watchState(e *Engine, stop <-chan struct{}, match func(Investigation) bool, found func()) {
	for {
		for _, task := range e.Store.Snapshot().Investigations.All() {
			if match(task) {
				found()
				return
			}
		}
		select {
		case <-stop:
			return
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func queueEngine(t *testing.T, tickets int) *Engine {
	t.Helper()
	e := testEngine(t)
	e.Config.Worker.MaxActivePerProject = 8
	for i := 0; i < tickets; i++ {
		ticket := fixture(e, fmt.Sprintf("queue-%d", i))
		ticket.Description += fmt.Sprintf(" Independent defect %d", i)
		receive(t, e, ticket.ID, ticket)
	}
	return e
}

func TestQueueWaitDoesNotConsumeEngineeringDeadline(t *testing.T) {
	e := queueEngine(t, EngineeringConcurrency+1)
	e.Config.Budget.TimeoutSeconds = 2
	var mu sync.Mutex
	engineering := 0
	var lastRemaining time.Duration
	provider := pipelineFunc(func(ctx context.Context, in AnalyzeRequest, stage string) (Report, error) {
		switch stage {
		case "route":
			return intakeReport("analyze"), nil
		case "assign":
			return pipelineDiagnosticReport(), nil
		}
		deadline, ok := ctx.Deadline()
		mu.Lock()
		engineering++
		// The first engineers finish within their own budget; the last one,
		// which waited for a slot, needs only part of its budget.
		hold := 1500 * time.Millisecond
		if engineering > EngineeringConcurrency {
			hold = time.Second
			lastRemaining = time.Until(deadline)
		}
		mu.Unlock()
		if !ok {
			return Report{}, fmt.Errorf("engineering stage has no deadline")
		}
		select {
		case <-time.After(hold):
			return pipelineEngineeringReport(), nil
		case <-ctx.Done():
			return Report{}, ctx.Err()
		}
	})
	sawWaiting, stop := make(chan struct{}), make(chan struct{})
	defer close(stop)
	go watchState(e, stop, func(task Investigation) bool { return task.Phase == "waiting_engineering" }, func() { close(sawWaiting) })
	runQueueWorkers(t, e, provider)
	select {
	case <-sawWaiting:
	default:
		t.Fatal("no task waited for engineering capacity")
	}
	s := e.Store.Snapshot()
	for _, task := range s.Investigations.All() {
		run := s.Runs.Get(task.RunID)
		if task.State != "agent_proposed" || run.Error != "" || run.Report.Usage.Calls != 3 {
			t.Fatal("waiting for a slot exhausted the task deadline", task.State, run.Error, run.Report.Usage.Calls)
		}
	}
	if lastRemaining < 1800*time.Millisecond {
		t.Fatal("engineering deadline started before the slot was reserved", lastRemaining)
	}
}

func TestQueueWaitLimitFailsWithoutEngineeringCall(t *testing.T) {
	e := queueEngine(t, EngineeringConcurrency+1)
	e.Config.Worker.MaxQueueWaitSeconds = 1
	release := make(chan struct{})
	var engineering atomic.Int32
	provider := pipelineFunc(func(ctx context.Context, in AnalyzeRequest, stage string) (Report, error) {
		switch stage {
		case "route":
			return intakeReport("analyze"), nil
		case "assign":
			return pipelineDiagnosticReport(), nil
		}
		engineering.Add(1)
		select {
		case <-release:
			return pipelineEngineeringReport(), nil
		case <-ctx.Done():
			return Report{}, ctx.Err()
		}
	})
	// Hold every slot until the waiting task has given up.
	stop := make(chan struct{})
	defer close(stop)
	go watchState(e, stop, func(task Investigation) bool { return task.State == "failed" }, func() { close(release) })
	runQueueWorkers(t, e, provider)
	s := e.Store.Snapshot()
	failed, proposed := 0, 0
	for _, task := range s.Investigations.All() {
		run := s.Runs.Get(task.RunID)
		switch task.State {
		case "failed":
			failed++
			if !strings.Contains(run.Error, "no engineering call was made") || run.Report.Usage.Calls != 1 || !run.Report.Usage.TokensReported || run.Report.Routing == nil {
				t.Fatal("queue timeout must keep exact routing-only usage", run.Error, run.Report.Usage)
			}
			for _, limit := range run.Report.Limitations {
				if strings.Contains(limit, "usage is unknown") {
					t.Fatal("queue timeout reported uncertain spend")
				}
			}
		case "agent_proposed":
			proposed++
		}
	}
	if failed != 1 || proposed != EngineeringConcurrency || engineering.Load() != EngineeringConcurrency {
		t.Fatal("only the waiting task may time out, before any engineering call", failed, proposed, engineering.Load())
	}
}

func TestEachStageGetsItsOwnDeadline(t *testing.T) {
	e := queueEngine(t, 1)
	e.Config.Budget.TimeoutSeconds = 2
	remaining := map[string]time.Duration{}
	provider := pipelineFunc(func(ctx context.Context, in AnalyzeRequest, stage string) (Report, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			return Report{}, fmt.Errorf("stage %s has no deadline", stage)
		}
		remaining[stage] = time.Until(deadline)
		time.Sleep(1200 * time.Millisecond) // Each stage alone fits; together they exceed 2 s.
		switch stage {
		case "route":
			return intakeReport("analyze"), nil
		case "engineer":
			return pipelineEngineeringReport(), nil
		}
		return pipelineDiagnosticReport(), nil
	})
	runQueueWorkers(t, e, provider)
	s := e.Store.Snapshot()
	for _, task := range s.Investigations.All() {
		if task.State != "agent_proposed" || s.Runs.Get(task.RunID).Error != "" {
			t.Fatal("stages within their own deadline failed", task.State, s.Runs.Get(task.RunID).Error)
		}
	}
	for _, stage := range []string{"route", "engineer", "assign"} {
		if remaining[stage] < 1800*time.Millisecond || remaining[stage] > 2*time.Second {
			t.Fatal("stage deadline is not its own timeout_seconds", stage, remaining[stage])
		}
	}
}

func TestStageDeadlineStillStopsSlowStage(t *testing.T) {
	e := queueEngine(t, 1)
	e.Config.Budget.TimeoutSeconds = 1
	provider := pipelineFunc(func(ctx context.Context, in AnalyzeRequest, stage string) (Report, error) {
		if stage == "route" {
			return intakeReport("analyze"), nil
		}
		<-ctx.Done()
		return Report{}, ctx.Err()
	})
	runQueueWorkers(t, e, provider)
	s := e.Store.Snapshot()
	for _, task := range s.Investigations.All() {
		run := s.Runs.Get(task.RunID)
		if task.State != "failed" || !strings.Contains(run.Error, "deadline exceeded") || run.Report.Usage.Calls != 2 || run.Report.Usage.TokensReported {
			t.Fatal("a stage exceeding timeout_seconds must fail with uncertain spend", task.State, run.Error, run.Report.Usage)
		}
	}
}

func TestQueueWaitConfiguration(t *testing.T) {
	e := testEngine(t)
	if e.Config.Worker.MaxQueueWaitSeconds != 1800 {
		t.Fatal("shipped rules must bound queue wait", e.Config.Worker.MaxQueueWaitSeconds)
	}
	config := e.Config
	config.Worker.MaxQueueWaitSeconds = 0
	if defaulted, err := NewEngine(e.Store, config); err != nil || defaulted.Config.Worker.MaxQueueWaitSeconds != 1800 {
		t.Fatal("missing max_queue_wait_seconds must default", err)
	}
	for _, invalid := range []int{-1, 86401} {
		config.Worker.MaxQueueWaitSeconds = invalid
		if _, err := NewEngine(e.Store, config); err == nil {
			t.Fatal("invalid max_queue_wait_seconds accepted", invalid)
		}
	}
}
