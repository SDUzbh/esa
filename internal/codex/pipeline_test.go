package codex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"esa/internal/esa"
)

func TestNativePipelineRoutesAndProcessesGroupingRequest(t *testing.T) {
	for _, decision := range []string{"new", "assign_existing", "assignment_info", "assignment_unread", "assignment_no_request"} {
		t.Run(decision, func(t *testing.T) {
			p, in, sourceCalls := routingFixture(t, decision)
			in.Stage = "route"
			routed, err := p.Analyze(context.Background(), in)
			if err != nil || routed.Routing == nil || routed.Routing.Action != "analyze" || routed.Usage.Calls != 1 || *sourceCalls != 0 {
				t.Fatal("intake did more than route", routed, err)
			}
			if _, err := os.Stat(filepath.Join(p.RunsDir, in.RunID, "engineering")); !os.IsNotExist(err) {
				t.Fatal("routing prepared an engineering environment", err)
			}
			in.Stage = "engineer"
			engineered, err := p.Analyze(context.Background(), in)
			if err != nil || engineered.Issue != nil || engineered.IssueRequest == nil || engineered.Usage.Calls != 2 {
				t.Fatal("engineering failed or selected membership", engineered, err)
			}
			in.Stage = "assign"
			report, err := p.Analyze(context.Background(), in)
			shouldFail := decision == "assignment_unread" || decision == "assignment_no_request"
			if (err != nil) != shouldFail || report.Usage.Calls != 3 || report.Usage.InputTokens != 36 || *sourceCalls != 1 || report.AssignmentRouting == nil || len(report.Findings) != 1 {
				t.Fatal("grouping request or spend lost", report, err, *sourceCalls)
			}
			if shouldFail && report.Issue != nil {
				t.Fatal("unverified grouping was accepted")
			}
			if decision == "assign_existing" && (report.Issue == nil || report.Issue.ExistingID == "") {
				t.Fatal("routing failed to assign inspected existing Issue")
			}
			if decision == "assignment_info" && report.Issue != nil {
				t.Fatal("clarification assigned an Issue")
			}
			if !shouldFail {
				if _, err := p.Analyze(context.Background(), in); err != nil || *sourceCalls != 1 {
					t.Fatal("completed continuation spent twice", err)
				}
			}
		})
	}
}

func TestNativePipelineJoinRequiresReadAndCannotReusePendingAsSolution(t *testing.T) {
	for _, decision := range []string{"join", "join_unread", "reuse_pending"} {
		t.Run(decision, func(t *testing.T) {
			p, in, sourceCalls := routingFixture(t, decision)
			record := esa.IssueRecord{ID: "investigation_lead", InvestigationID: "investigation_lead", ProjectID: in.Ticket.ProjectID, Kind: "investigating", ScopeHash: "scope", Title: "Pending assignment crash", SourceTicketID: "lead", AvailableAt: in.Contract.ObservationCutoff}
			var err error
			in.IssueDirectory, in.IssueDirectoryHash, err = esa.ExportIssueDirectory(p.IssuesRoot, in.Ticket.ProjectID, in.Contract.ObservationCutoff, []esa.IssueRecord{record})
			if err != nil {
				t.Fatal(err)
			}
			in.Stage = "route"
			report, err := p.Analyze(context.Background(), in)
			if (err == nil) != (decision == "join") || report.Usage.Calls != 1 || *sourceCalls != 0 || len(report.Findings) != 0 || report.Issue != nil {
				t.Fatal("pending routing violated role/solution boundary", report, err)
			}
		})
	}
}

func TestNativePipelineEngineeringSlotsDoNotBlockIntakeRouting(t *testing.T) {
	p, in, _ := routingFixture(t, "new")
	var registrations atomic.Int32
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	p.Resolve = func(ctx context.Context, request esa.AnalyzeRequest) (Registration, error) {
		registrations.Add(1)
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return Registration{}, ctx.Err()
		}
		return Registration{Packet: Packet{Ticket: request.Ticket, Commit: "old", Cutoff: in.Contract.ObservationCutoff, Documents: []Document{{Source: "git", Prefix: "old:Example.java:", First: 1, Lines: []string{"bug();"}}}}}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		request := in
		request.RunID, request.Stage = fmt.Sprintf("run_parallel%d", i), "route"
		if _, err := p.Analyze(ctx, request); err != nil {
			t.Fatal(err)
		}
		request.Stage = "engineer"
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.Analyze(ctx, request); err != nil {
				t.Error(err)
				return
			}
			request.Stage = "assign"
			if _, err := p.Analyze(ctx, request); err != nil {
				t.Error(err)
			}
		}()
	}
	for i := 0; i < 3; i++ {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("engineering did not start")
		}
	}
	if registrations.Load() != 3 {
		t.Fatal("fourth engineering environment started early")
	}
	// A fifth ticket still reaches Routing Agent while all engineering slots wait.
	in.RunID, in.Stage = "run_routerfree", "route"
	if report, err := p.Analyze(ctx, in); err != nil || report.Routing == nil || report.Usage.Calls != 1 {
		t.Fatal("engineering slots blocked Routing Agent", err)
	}
	if registrations.Load() != 3 {
		t.Fatal("routing consumed an engineering slot")
	}
	once.Do(func() { close(release) })
	wg.Wait()
	if registrations.Load() != 4 {
		t.Fatal("fourth task did not drain")
	}
}
