package esa

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// runWorkers runs four workers as cmd/esa does, waiting on the work signal and
// on a fallback poll, until stop closes.
func runWorkers(t *testing.T, e *Engine, provider Analyzer, fallback time.Duration, stop <-chan struct{}) *sync.WaitGroup {
	t.Helper()
	var wg sync.WaitGroup
	for i := 0; i < EngineeringConcurrency+1; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				wake := e.WorkSignal()
				worked, err := e.ProcessOne(context.Background(), provider)
				if err != nil {
					t.Error(err)
					return
				}
				if worked {
					continue
				}
				select {
				case <-stop:
					return
				case <-wake:
				case <-time.After(fallback):
				}
			}
		}()
	}
	return &wg
}

func taskOf(s State, runID string) Investigation {
	return s.Investigations.Get(s.Runs.Get(runID).InvestigationID)
}

func TestParkedTaskFreesWorkerForRouting(t *testing.T) {
	e := queueEngine(t, EngineeringConcurrency+1)
	// Parked tasks hold no worker, so the shipped project limit still admits routing.
	e.Config.Worker.MaxActivePerProject = 4
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var mu sync.Mutex
	inputs := map[string]map[string]string{}
	var engineering atomic.Int32
	var staleEpochAccepted atomic.Bool
	provider := pipelineFunc(func(ctx context.Context, in AnalyzeRequest, stage string) (Report, error) {
		raw, _ := json.Marshal(in)
		mu.Lock()
		if inputs[in.RunID] == nil {
			inputs[in.RunID] = map[string]string{}
		}
		inputs[in.RunID][stage] = string(raw)
		mu.Unlock()
		switch stage {
		case "route":
			if in.Ticket.ID == "cheap" {
				return intakeReport("return_to_support"), nil
			}
			return intakeReport("analyze"), nil
		case "assign":
			return pipelineDiagnosticReport(), nil
		}
		if engineering.Add(1) > EngineeringConcurrency {
			// The resumed owner has a new epoch; the worker that parked it is fenced.
			task := taskOf(e.Store.Snapshot(), in.RunID)
			stale := task
			stale.Epoch--
			staleEpochAccepted.Store(task.Epoch != 2 || ownsInvestigation(task, stale))
		}
		select {
		case <-release:
			return pipelineEngineeringReport(), nil
		case <-ctx.Done():
			return Report{}, ctx.Err()
		}
	})
	stop := make(chan struct{})
	wg := runWorkers(t, e, provider, 20*time.Millisecond, stop)
	defer func() { close(stop); wg.Wait() }()
	var parkedRun string
	waitPipeline(t, func() bool {
		s := e.Store.Snapshot()
		for _, task := range s.Investigations.All() {
			if parked(task) {
				parkedRun = task.RunID
			}
		}
		return engineering.Load() == EngineeringConcurrency && parkedRun != ""
	})
	// Three engineering runs and one parked task: a fourth routable ticket is
	// still routed, because the parked task returned its worker.
	cheap := fixture(e, "cheap")
	cheap.Subject, cheap.Description = "Invoice address change", "Please update the billing address on my invoice."
	receipt := receive(t, e, cheap.ID, cheap)
	waitPipeline(t, func() bool { return taskOf(e.Store.Snapshot(), receipt.RunID).State == "returned_to_support" })
	s := e.Store.Snapshot()
	if !parked(taskOf(s, parkedRun)) || engineering.Load() != EngineeringConcurrency || s.Runs.Get(parkedRun).PipelineInput == nil {
		t.Fatal("routing the ticket disturbed the parked task or the engineering slots")
	}
	once.Do(func() { close(release) })
	waitPipeline(t, func() bool {
		for _, task := range e.Store.Snapshot().Investigations.All() {
			if task.State != "agent_proposed" && task.State != "returned_to_support" {
				return false
			}
		}
		return true
	})
	s = e.Store.Snapshot()
	for _, task := range s.Investigations.All() {
		run := s.Runs.Get(task.RunID)
		if run.Error != "" || run.PipelineInput != nil || run.Report.Envelope == nil {
			t.Fatal("finished task kept resume state or lost its audit", task.State, run.Error)
		}
	}
	if engineering.Load() != EngineeringConcurrency+1 || staleEpochAccepted.Load() {
		t.Fatal("parked task was not resumed under a new epoch", engineering.Load())
	}
	if task := taskOf(s, parkedRun); task.Epoch != 2 || s.Runs.Get(parkedRun).Report.Usage.Calls != 3 {
		t.Fatal("parked task did not complete all stages", task.Epoch, s.Runs.Get(parkedRun).Report.Usage)
	}
	mu.Lock()
	defer mu.Unlock()
	stages := inputs[parkedRun]
	for _, stage := range []string{"engineer", "assign"} {
		if stages[stage] == "" || stages[stage] != stages["route"] {
			t.Fatal("resumed stage input differs from the routed input", stage)
		}
	}
}

func TestFreedSlotWakesIdleWorkersWithoutPolling(t *testing.T) {
	e := queueEngine(t, EngineeringConcurrency+2)
	release := make(chan struct{})
	provider := pipelineFunc(func(ctx context.Context, in AnalyzeRequest, stage string) (Report, error) {
		switch stage {
		case "route":
			return intakeReport("analyze"), nil
		case "assign":
			return pipelineDiagnosticReport(), nil
		}
		select {
		case <-release:
			return pipelineEngineeringReport(), nil
		case <-ctx.Done():
			return Report{}, ctx.Err()
		}
	})
	stop := make(chan struct{})
	// No fallback poll fires during the test: only work signals wake workers.
	wg := runWorkers(t, e, provider, time.Hour, stop)
	defer func() { close(stop); wg.Wait() }()
	waitPipeline(t, func() bool {
		waiting := 0
		for _, task := range e.Store.Snapshot().Investigations.All() {
			if parked(task) {
				waiting++
			}
		}
		return waiting == 2
	})
	close(release)
	waitPipeline(t, func() bool {
		for _, task := range e.Store.Snapshot().Investigations.All() {
			if task.State != "agent_proposed" {
				return false
			}
		}
		return true
	})
}

func TestParkedTasksResumeOldestFirstAcrossProjects(t *testing.T) {
	at := time.Date(2026, 10, 11, 1, 0, 0, 0, time.UTC)
	s := State{LastClaimedProject: "1"}
	for i, project := range []string{"1", "1", "2", "3", "2"} {
		id := fmt.Sprint("task-", i)
		s.Investigations.Put(id, Investigation{ID: id, ProjectID: project, State: "investigating", Phase: "waiting_engineering", WaitingSince: at.Add(time.Duration(i) * time.Second)})
	}
	s.Investigations.Put("owned", Investigation{ID: "owned", ProjectID: "2", State: "investigating", Phase: "waiting_engineering", Owner: "worker", WaitingSince: at.Add(-time.Hour)})
	var order []string
	for _, task := range fairParked(s) {
		order = append(order, task.ID)
	}
	if fmt.Sprint(order) != "[task-2 task-4 task-3 task-0 task-1]" {
		t.Fatal("parked tasks are not oldest first within a project, rotating across projects", order)
	}
}

func TestQueueWaitIsMeasuredFromParking(t *testing.T) {
	e := queueEngine(t, 1)
	e.Config.Worker.MaxQueueWaitSeconds = 60
	var id string
	if err := e.update(func(s *State) error {
		for _, task := range s.Investigations.All() {
			// Queued long ago, but parked only now.
			task.CreatedAt = e.Now().Add(-24 * time.Hour)
			task.State, task.Phase, task.WaitingSince = "investigating", "waiting_engineering", time.Now().UTC()
			s.Investigations.Put(task.ID, task)
			id = task.ID
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	settle := func() Investigation {
		if err := e.update(func(s *State) error { e.settleParked(s); return nil }); err != nil {
			t.Fatal(err)
		}
		return e.Store.Snapshot().Investigations.Get(id)
	}
	if task := settle(); !parked(task) {
		t.Fatal("queue wait counted time before parking", task.State)
	}
	if err := e.update(func(s *State) error {
		task := s.Investigations.Get(id)
		task.WaitingSince = time.Now().UTC().Add(-61 * time.Second)
		s.Investigations.Put(id, task)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if task := settle(); task.State != "failed" || e.Store.Snapshot().Runs.Get(task.RunID).PipelineInput != nil {
		t.Fatal("expired queue wait did not fail the parked task", task.State)
	}
}

// statefulProvider imitates the codex adapter's per-run state between stages:
// the request and routing audit live on its "disk", keyed by run ID, and a
// continuation is accepted only for the identical request.
type statefulProvider struct {
	mu    *sync.Mutex
	disk  map[string][]byte
	calls map[string]int
}

func (p statefulProvider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var in AnalyzeRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	stage := in.Stage
	in.Stage = ""
	raw, _ := json.Marshal(in)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls[in.RunID+"/"+stage]++
	saved, ok := p.disk[in.RunID]
	var report Report
	switch {
	case stage == "route" && !ok:
		p.disk[in.RunID] = raw
		report = intakeReport("analyze")
	case stage == "route":
		http.Error(w, "previous routing attempt exists", http.StatusConflict)
		return
	case !ok:
		http.Error(w, "no intake routing audit exists", http.StatusConflict)
		return
	case !bytes.Equal(saved, raw):
		http.Error(w, "run ID reused with different input", http.StatusConflict)
		return
	case stage == "engineer":
		report = pipelineEngineeringReport()
	default:
		report = pipelineDiagnosticReport()
	}
	_ = json.NewEncoder(w).Encode(report)
}

// occupyEngineering fills every slot with another process's live work.
func occupyEngineering(t *testing.T, e *Engine) []string {
	t.Helper()
	var ids []string
	if err := e.update(func(s *State) error {
		for i := 0; i < EngineeringConcurrency; i++ {
			id := fmt.Sprint("investigation_busy_", i)
			s.Investigations.Put(id, Investigation{ID: id, ProjectID: "busy", State: "investigating", Phase: "investigating", Owner: "other-process", Epoch: 1, LeaseUntil: e.Now().Add(time.Hour)})
			ids = append(ids, id)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return ids
}

func freeEngineering(t *testing.T, e *Engine, ids []string) {
	t.Helper()
	if err := e.update(func(s *State) error {
		for _, id := range ids {
			task := s.Investigations.Get(id)
			task.State = "failed"
			s.Investigations.Put(id, task)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestParkedTaskResumesAfterProviderRestart(t *testing.T) {
	for _, keepsState := range []bool{true, false} {
		t.Run(fmt.Sprint("keeps_state_", keepsState), func(t *testing.T) {
			e := testEngine(t)
			lead := receive(t, e, "lead", fixture(e, "lead"))
			busy := occupyEngineering(t, e)
			var mu sync.Mutex
			start := func(disk map[string][]byte) statefulProvider {
				return statefulProvider{mu: &mu, disk: disk, calls: map[string]int{}}
			}
			var current atomic.Value
			current.Store(start(map[string][]byte{}))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				current.Load().(statefulProvider).ServeHTTP(w, r)
			}))
			defer server.Close()
			provider := HTTPPipelineAnalyzer{HTTPAnalyzer{URL: server.URL, Client: server.Client()}}
			ctx := context.Background()
			if worked, err := e.ProcessOne(ctx, provider); err != nil || !worked {
				t.Fatal("lead was not routed", worked, err)
			}
			s := e.Store.Snapshot()
			task, run := taskOf(s, lead.RunID), s.Runs.Get(lead.RunID)
			if !parked(task) || run.PipelineInput == nil || run.Report.Usage.Calls != 1 || !task.LeaseUntil.IsZero() {
				t.Fatal("full capacity did not park the routed task", task.State, task.Phase, task.Owner)
			}
			// A sibling routed meanwhile changes what a fresh export would contain,
			// so only the frozen request matches the provider's stored run.
			sibling := fixture(e, "sibling")
			sibling.Description += " Independent defect"
			next := receive(t, e, sibling.ID, sibling)
			if worked, err := e.ProcessOne(ctx, provider); err != nil || !worked || !parked(taskOf(e.Store.Snapshot(), next.RunID)) {
				t.Fatal("sibling did not park behind the lead", worked, err)
			}
			s = e.Store.Snapshot()
			_, fresh, err := ExportIssueDirectory(e.IssueDirectory, task.ProjectID, task.Contract.ObservationCutoff, issueRecords(s, task, run.Input))
			if err != nil || fresh == run.PipelineInput.IssueDirectoryHash {
				t.Fatal("test needs Issue-directory state that changed while parked", err)
			}
			// Restart the provider while both tasks are parked.
			disk := current.Load().(statefulProvider).disk
			if !keepsState {
				disk = map[string][]byte{}
			}
			restarted := start(disk)
			current.Store(restarted)
			freeEngineering(t, e, busy)
			for i := 0; i < 2; i++ {
				if worked, err := e.ProcessOne(ctx, provider); err != nil || !worked {
					t.Fatal("parked task was not resumed", worked, err)
				}
			}
			s = e.Store.Snapshot()
			task, run = taskOf(s, lead.RunID), s.Runs.Get(lead.RunID)
			if keepsState {
				if task.State != "agent_proposed" || run.Error != "" || run.Report.Usage.Calls != 3 || task.Epoch != 2 || taskOf(s, next.RunID).State != "agent_proposed" {
					t.Fatal("restart lost the resumed run", task.State, run.Error, run.Report.Usage.Calls, task.Epoch)
				}
				return
			}
			// The provider no longer knows the run. Spend is uncertain, and the
			// task is not retried automatically.
			if task.State != "failed" || run.Report.Usage.Calls != 2 || run.Report.Usage.TokensReported {
				t.Fatal("lost provider state must fail with uncertain spend", task.State, run.Error, run.Report.Usage)
			}
			worked, err := e.ProcessOne(ctx, provider)
			mu.Lock()
			defer mu.Unlock()
			if err != nil || worked || restarted.calls[lead.RunID+"/engineer"] != 1 {
				t.Fatal("failed resume was retried", worked, err, restarted.calls)
			}
		})
	}
}
