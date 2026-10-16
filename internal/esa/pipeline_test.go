package esa

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type pipelineFunc func(context.Context, AnalyzeRequest, string) (Report, error)

func (f pipelineFunc) Analyze(context.Context, AnalyzeRequest) (Report, error) {
	return Report{}, errors.New("pipeline must dispatch named stages")
}
func (f pipelineFunc) AnalyzeStage(ctx context.Context, in AnalyzeRequest, stage string) (Report, error) {
	return f(ctx, in, stage)
}
func intakeReport(action string) Report {
	u := Usage{Calls: 1, InputTokens: 12, OutputTokens: 8, TokensReported: true}
	return Report{Summary: "Intake routing decision", Limitations: []string{"No engineering result yet"}, Routing: &RoutingResult{Action: action, Reason: "Compare the original symptom and scope", Usage: u}, Usage: u}
}
func pipelineDiagnosticReport() Report {
	r := diagnosticReport()
	r.Routing = intakeReport("analyze").Routing
	r.IssueRequest = &IssueRequest{Title: "Engineering grouping suggestion", Reason: "Inspected source"}
	r.AssignmentRouting = &RoutingResult{Action: "create", Title: r.Issue.Title, Reason: r.Issue.Reason, Usage: Usage{Calls: 1, TokensReported: true}}
	r.Usage.Calls = 3
	return r
}

func pipelineEngineeringReport() Report {
	r := pipelineDiagnosticReport()
	r.Issue, r.AssignmentRouting = nil, nil
	r.Usage.Calls = 2
	return r
}

func waitPipeline(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("pipeline condition timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPipelineThreeEngineersAndFourthWaitsAcrossWorkers(t *testing.T) {
	a := testEngine(t)
	a.Config.Worker.MaxActivePerProject = 8
	b, err := NewEngine(a.Store, a.Config)
	if err != nil {
		t.Fatal(err)
	}
	b.Now, b.IssueDirectory = a.Now, a.IssueDirectory
	for i := 0; i < 4; i++ {
		ticket := fixture(a, fmt.Sprintf("parallel-%d", i))
		ticket.Description += fmt.Sprintf(" Independent defect %d", i)
		if i >= 2 {
			ticket.ProjectID = "2"
		}
		receive(t, a, ticket.ID, ticket)
	}
	var active, peak, routes atomic.Int32
	started := make(chan string, 4)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	provider := pipelineFunc(func(ctx context.Context, in AnalyzeRequest, stage string) (Report, error) {
		if stage == "route" {
			routes.Add(1)
			return intakeReport("analyze"), nil
		}
		if stage == "assign" {
			return pipelineDiagnosticReport(), nil
		}
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		started <- in.RunID
		select {
		case <-release:
			return pipelineEngineeringReport(), nil
		case <-ctx.Done():
			return Report{}, ctx.Err()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			engine := a
			if i%2 == 1 {
				engine = b
			}
			for ctx.Err() == nil {
				worked, err := engine.ProcessOne(ctx, provider)
				if err != nil {
					t.Error(err)
					return
				}
				if !worked {
					time.Sleep(5 * time.Millisecond)
				}
				if len(engine.Store.Snapshot().Solutions) == 4 {
					return
				}
			}
		}(i)
	}
	waitPipeline(t, func() bool {
		if len(started) != 3 || routes.Load() != 4 {
			return false
		}
		for _, task := range a.Store.Snapshot().Investigations {
			if task.Phase == "waiting_engineering" {
				return true
			}
		}
		return false
	})
	s := a.Store.Snapshot()
	waiting, investigating := 0, 0
	for _, task := range s.Investigations {
		if task.Phase == "waiting_engineering" {
			waiting++
		}
		if task.Phase == "investigating" {
			investigating++
		}
		if s.Runs[task.RunID].State != "investigating" || s.Runs[task.RunID].Report.Usage.Calls != 1 {
			t.Fatal("missing durable claim/routing audit", task)
		}
	}
	if waiting != 1 || investigating != 3 || len(s.Issues) != 0 || peak.Load() != 3 {
		t.Fatal("invalid pipeline capacity", waiting, investigating, peak.Load(), len(s.Issues))
	}
	once.Do(func() { close(release) })
	wg.Wait()
	if len(a.Store.Snapshot().Solutions) != 4 || peak.Load() != 3 || len(started) != 4 {
		t.Fatal("queued task did not drain")
	}
}

func TestPipelineRoutingJoinsPendingWithoutEarlyIssueOrSecondEngineer(t *testing.T) {
	e := testEngine(t)
	first := receive(t, e, "lead", fixture(e, "lead"))
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var engineers atomic.Int32
	provider := pipelineFunc(func(ctx context.Context, in AnalyzeRequest, stage string) (Report, error) {
		if stage == "route" {
			for _, record := range inputIssueRecords(t, e, in) {
				if record.Kind == "investigating" {
					if record.IssueID != "" || record.Summary != "" || len(record.Findings) != 0 {
						t.Error("pending work was presented as a diagnosis")
					}
					r := intakeReport("join")
					r.Routing.Reason = "LEAD_ONLY_GROUPING_BASIS: same original symptom and scope"
					r.Routing.RecordPath, r.Routing.RecordHash = recordPath(record), issueRecordHash(record)
					return r, nil
				}
			}
			return intakeReport("analyze"), nil
		}
		if stage == "assign" {
			return pipelineDiagnosticReport(), nil
		}
		engineers.Add(1)
		close(started)
		select {
		case <-release:
			return pipelineEngineeringReport(), nil
		case <-ctx.Done():
			return Report{}, ctx.Err()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := e.ProcessOne(ctx, provider); done <- err }()
	<-started
	variant := fixture(e, "variant")
	variant.Subject = "Empty partitions cause my consumer to crash"
	second := receive(t, e, variant.ID, variant)
	if _, err := e.ProcessOne(ctx, provider); err != nil {
		t.Fatal(err)
	}
	s := e.Store.Snapshot()
	joined := s.Investigations[s.Runs[second.RunID].InvestigationID]
	if joined.State != "joined" || joined.JoinedInvestigationID != s.Runs[first.RunID].InvestigationID || s.Latest[variant.ID] != first.RunID || s.Runs[second.RunID].Report.Usage.Calls != 1 || len(s.Issues) != 0 || engineers.Load() != 1 {
		t.Fatal("duplicate engineering or missing routing audit", joined)
	}
	response := classifierRequest(e, "GET", "/v1/tickets/"+variant.ID, "", "1", variant.ID)
	if response.Code != 200 || strings.Contains(response.Body.String(), "LEAD_ONLY_GROUPING_BASIS") {
		t.Fatal("joined routing history exposed another ticket's grouping evidence", response.Code, response.Body)
	}
	third := variant
	third.ID, third.SourceKey = "third", "third"
	if receipt := receive(t, e, third.ID, third); receipt.RunID != first.RunID {
		t.Fatal("alias group lost the shared investigation", receipt)
	}
	once.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	s = e.Store.Snapshot()
	if len(s.Issues) != 1 || len(s.Solutions) != 1 || engineers.Load() != 1 {
		t.Fatal("duplicate issue/solution")
	}
	for _, ticket := range []string{"lead", "variant", "third"} {
		link, ok := activeMembership(s, ticket)
		if !ok || link.IssueID != s.Runs[first.RunID].IssueID || link.InvestigationID != joined.JoinedInvestigationID {
			t.Fatal("joined member lost final assignment", ticket, link)
		}
	}
}

func TestPipelineRoutingLeaseBlocksIntakeAndExpiredClaimIsReleased(t *testing.T) {
	e := testEngine(t)
	for _, id := range []string{"one", "two"} {
		ticket := fixture(e, id)
		ticket.Subject += id
		receive(t, e, id, ticket)
	}
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	provider := pipelineFunc(func(ctx context.Context, in AnalyzeRequest, stage string) (Report, error) {
		close(started)
		select {
		case <-release:
			return intakeReport("request_information"), nil
		case <-ctx.Done():
			return Report{}, ctx.Err()
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := e.ProcessOne(ctx, provider); done <- err }()
	<-started
	if worked, err := e.ProcessOne(ctx, provider); err != nil || worked {
		t.Fatal("concurrent initial router bypassed durable project claim", worked, err)
	}
	// Expire the owner while it is still returning a result: the result is fenced.
	if err := e.update(func(s *State) error {
		for id, task := range s.Investigations {
			if task.State == "running" {
				task.LeaseUntil = e.Now().Add(-time.Second)
				s.Investigations[id] = task
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.Recover(); err != nil {
		t.Fatal(err)
	}
	if routingBusy(e.Store.Snapshot(), "1") {
		t.Fatal("expired routing claim remained locked")
	}
	once.Do(func() { close(release) })
	if err := <-done; !errors.Is(err, ErrConflict) {
		t.Fatal("stale result was accepted", err)
	}
}

func TestPipelineQueuedTicketJoinsCompletionBeyondItsCutoff(t *testing.T) {
	e := testEngine(t)
	var clock atomic.Int64
	clock.Store(e.Now().UnixNano())
	e.Now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	lead := receive(t, e, "lead", fixture(e, "lead"))
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	provider := pipelineFunc(func(ctx context.Context, in AnalyzeRequest, stage string) (Report, error) {
		if stage == "route" {
			if in.Ticket.ID == "lead" {
				return intakeReport("analyze"), nil
			}
			records := inputIssueRecords(t, e, in)
			if len(records) != 1 || records[0].Kind != "investigating" || records[0].Summary != "" || len(records[0].Findings) != 0 {
				return Report{}, fmt.Errorf("later knowledge leaked or pending claim was lost: %+v", records)
			}
			r := intakeReport("join")
			r.Routing.RecordPath, r.Routing.RecordHash = recordPath(records[0]), issueRecordHash(records[0])
			return r, nil
		}
		if stage == "assign" {
			return pipelineDiagnosticReport(), nil
		}
		close(started)
		select {
		case <-release:
			return pipelineEngineeringReport(), nil
		case <-ctx.Done():
			return Report{}, ctx.Err()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := e.ProcessOne(ctx, provider); done <- err }()
	<-started
	ticket := fixture(e, "queued")
	ticket.Subject = "Differently worded duplicate waits in the queue"
	queued := receive(t, e, ticket.ID, ticket)
	clock.Add(int64(time.Second))
	once.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := e.ProcessOne(ctx, provider); err != nil {
		t.Fatal(err)
	}
	s := e.Store.Snapshot()
	if s.Latest[ticket.ID] != lead.RunID || s.Runs[queued.RunID].State != "joined" || len(s.Solutions) != 1 {
		t.Fatal("queue delay caused duplicate analysis")
	}
}

func TestPipelineJoinRechecksLeaseAndSourceVisibility(t *testing.T) {
	for _, change := range []string{"expired", "private"} {
		t.Run(change, func(t *testing.T) {
			e := testEngine(t)
			lead := receive(t, e, "lead", fixture(e, "lead"))
			started, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			defer once.Do(func() { close(release) })
			provider := pipelineFunc(func(ctx context.Context, in AnalyzeRequest, stage string) (Report, error) {
				if stage == "route" {
					if in.Ticket.ID == "lead" {
						return intakeReport("analyze"), nil
					}
					records := inputIssueRecords(t, e, in)
					if len(records) != 1 {
						return Report{}, fmt.Errorf("missing pending record")
					}
					r := intakeReport("join")
					r.Routing.RecordPath, r.Routing.RecordHash = recordPath(records[0]), issueRecordHash(records[0])
					err := e.update(func(s *State) error {
						if change == "expired" {
							task := s.Investigations[s.Runs[lead.RunID].InvestigationID]
							task.LeaseUntil = e.Now().Add(-time.Second)
							s.Investigations[task.ID] = task
						} else {
							ticket := s.Tickets["lead"]
							ticket.Private = true
							s.Tickets["lead"] = ticket
						}
						return nil
					})
					return r, err
				}
				if stage == "assign" {
					return pipelineDiagnosticReport(), nil
				}
				close(started)
				select {
				case <-release:
					return pipelineEngineeringReport(), nil
				case <-ctx.Done():
					return Report{}, ctx.Err()
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := e.ProcessOne(ctx, provider); done <- err }()
			<-started
			ticket := fixture(e, "variant")
			ticket.Subject += " variant"
			next := receive(t, e, ticket.ID, ticket)
			if _, err := e.ProcessOne(ctx, provider); err != nil {
				t.Fatal(err)
			}
			s := e.Store.Snapshot()
			if s.Runs[next.RunID].State != "failed" || s.Latest[ticket.ID] != next.RunID || s.Runs[next.RunID].Report.Usage.Calls != 1 {
				t.Fatal("invalid source/lease was joined or audit lost", s.Runs[next.RunID])
			}
			once.Do(func() { close(release) })
			<-done
		})
	}
}

func TestPipelineReleasesEngineeringSlotBeforeAssignment(t *testing.T) {
	e := testEngine(t)
	for i := 0; i < 4; i++ {
		ticket := fixture(e, fmt.Sprint(i))
		ticket.Subject += fmt.Sprint(i)
		receive(t, e, ticket.ID, ticket)
	}
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var assignments atomic.Int32
	provider := pipelineFunc(func(ctx context.Context, in AnalyzeRequest, stage string) (Report, error) {
		switch stage {
		case "route":
			return intakeReport("analyze"), nil
		case "engineer":
			return pipelineEngineeringReport(), nil
		default:
			assignments.Add(1)
			select {
			case <-release:
				return pipelineDiagnosticReport(), nil
			case <-ctx.Done():
				return Report{}, ctx.Err()
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A task that found every slot busy parks, and a free worker resumes it.
			for ctx.Err() == nil && assignments.Load() < 4 {
				worked, err := e.ProcessOne(ctx, provider)
				if err != nil {
					t.Error(err)
					return
				}
				if !worked {
					time.Sleep(5 * time.Millisecond)
				}
			}
		}()
	}
	waitPipeline(t, func() bool { return assignments.Load() == 4 })
	for _, task := range e.Store.Snapshot().Investigations {
		if task.Phase != "assignment" || task.State != "investigating" {
			t.Fatal("engineering capacity remained occupied during assignment", task)
		}
	}
	once.Do(func() { close(release) })
	wg.Wait()
}

func TestPipelineExpiredInvestigatingPhasesDoNotRetainClaims(t *testing.T) {
	for _, phase := range []string{"waiting_engineering", "investigating", "assignment"} {
		t.Run(phase, func(t *testing.T) {
			e := testEngine(t)
			receipt := receive(t, e, "abandoned", fixture(e, "abandoned"))
			id := e.Store.Snapshot().Runs[receipt.RunID].InvestigationID
			if err := e.update(func(s *State) error {
				task := s.Investigations[id]
				task.State, task.Phase, task.Owner, task.Epoch = "investigating", phase, "abandoned-worker", 1
				task.InvocationStarted = true
				task.LeaseUntil = e.Now().Add(-time.Second)
				s.Investigations[id] = task
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if !e.Store.needsWork(e.Now(), true, time.Hour) {
				t.Fatal("expired investigating lease was invisible to recovery")
			}
			if err := e.Recover(); err != nil {
				t.Fatal(err)
			}
			if task := e.Store.Snapshot().Investigations[id]; task.State != "failed" || task.Epoch != 2 {
				t.Fatal("investigating lease remained active", task)
			}
			ticket := fixture(e, "next")
			ticket.Subject += " variant"
			receive(t, e, ticket.ID, ticket)
			calls := 0
			provider := pipelineFunc(func(ctx context.Context, in AnalyzeRequest, stage string) (Report, error) {
				calls++
				if stage != "route" || len(inputIssueRecords(t, e, in)) != 0 {
					t.Fatal("failed work was offered for reuse or automatically retried")
				}
				return intakeReport("request_information"), nil
			})
			if worked, err := e.ProcessOne(context.Background(), provider); err != nil || !worked || calls != 1 {
				t.Fatal("expired claim blocked the next ticket", worked, err, calls)
			}
		})
	}
}

func TestPipelineAssignmentCannotRewriteEngineeringFindings(t *testing.T) {
	e := testEngine(t)
	r := receive(t, e, "ticket", fixture(e, "ticket"))
	provider := pipelineFunc(func(ctx context.Context, in AnalyzeRequest, stage string) (Report, error) {
		switch stage {
		case "route":
			return intakeReport("analyze"), nil
		case "engineer":
			return pipelineEngineeringReport(), nil
		default:
			report := pipelineDiagnosticReport()
			report.Findings[0].Solution = "A replacement invented by Routing Agent"
			return report, nil
		}
	})
	if _, err := e.ProcessOne(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	s := e.Store.Snapshot()
	if s.Runs[r.RunID].State != "failed" || len(s.Solutions) != 0 || len(s.Issues) != 0 || s.Runs[r.RunID].Report.Usage.Calls != 3 {
		t.Fatal("assignment rewrote the diagnosis or lost spend")
	}
}

func TestPipelineReturnToSupportCreatesNoIssueAndIsIdempotent(t *testing.T) {
	e := testEngine(t)
	ticket := fixture(e, "invoice")
	ticket.Tracker, ticket.Subject = "Support", "Invoice copy request"
	receipt := receive(t, e, "invoice", ticket)
	calls := 0
	provider := pipelineFunc(func(ctx context.Context, in AnalyzeRequest, stage string) (Report, error) {
		calls++
		if stage != "route" {
			t.Fatal("customer support reached engineering", stage)
		}
		r := intakeReport("return_to_support")
		r.Recommendations = []string{"Contact Billing for the invoice download process"}
		return r, nil
	})
	if _, err := e.ProcessOne(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	s := e.Store.Snapshot()
	run := s.Runs[receipt.RunID]
	if run.State != "returned_to_support" || run.Route != "customer_support" || len(s.Issues) != 0 || len(s.Solutions) != 0 || calls != 1 || len(run.Report.Recommendations) != 1 {
		t.Fatal("incorrect customer support outcome", run)
	}
	if again := receive(t, e, "invoice-again", ticket); again.RunID != receipt.RunID {
		t.Fatal("identical forwarding restarted returned support work", again)
	}
	if worked, err := e.ProcessOne(context.Background(), provider); worked || err != nil || calls != 1 {
		t.Fatal("customer support consumed another call", worked, err, calls)
	}
}
