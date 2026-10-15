package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"esa/internal/esa"
)

func TestEngineeringHandoffChecksReleaseBeforeSourceOrModel(t *testing.T) {
	for _, stage := range []string{"", "route"} {
		for _, test := range []struct {
			name     string
			versions []string
			project  string
			future   bool
			want     string
		}{
			{name: "missing", want: "Provide one unambiguous affected release."},
			{name: "multiple", versions: []string{"1", "2"}, want: "Provide one unambiguous affected release."},
			{name: "blank", versions: []string{" "}, want: "Provide one unambiguous affected release."},
			{name: "unknown", versions: []string{"unknown"}, want: "Invalid or nonexistent version."},
			{name: "wrong_project", versions: []string{"1"}, project: "other", want: "Invalid or nonexistent version."},
			{name: "future", versions: []string{"1"}, future: true, want: "Invalid or nonexistent version."},
		} {
			t.Run(stage+"/"+test.name, func(t *testing.T) {
				p, in, sourceCalls := routingFixture(t, "new")
				in.Stage, in.Ticket.Versions = stage, test.versions
				release := ReleaseRegistration{ProjectID: in.Ticket.ProjectID, Version: "1", AvailableAt: in.Contract.ObservationCutoff}
				if test.project != "" {
					release.ProjectID = test.project
				}
				if test.future {
					release.AvailableAt = release.AvailableAt.Add(time.Second)
				}
				p.CheckRelease = (&Registry{Releases: []ReleaseRegistration{release}}).CheckRelease
				for attempt := 0; attempt < 2; attempt++ {
					report, err := p.Analyze(context.Background(), in)
					if err != nil || report.Routing == nil || report.Routing.Action != "request_information" || report.Routing.Reason != test.want || len(report.Limitations) != 1 || report.Limitations[0] != test.want || report.Usage.Calls != 1 || report.Usage.InputTokens != 12 || report.Usage.CachedInputTokens != 8 || *sourceCalls != 0 || report.Issue != nil || report.IssueRequest != nil || report.AssignmentRouting != nil || len(report.Findings) != 0 || len(report.Evidence) != 0 {
						t.Fatal("blocked handoff spent on engineering or lost its reason/usage", report, err, *sourceCalls)
					}
				}
				for _, path := range []string{"engineering", "engineering-attempt", "assignment-attempt"} {
					if _, err := os.Stat(filepath.Join(p.RunsDir, in.RunID, path)); !os.IsNotExist(err) {
						t.Fatal("blocked handoff prepared engineering or assignment", path, err)
					}
				}
				data, err := os.ReadFile(filepath.Join(p.RunsDir, in.RunID, "routing", "answer.json"))
				var original esa.RoutingResult
				if err != nil || json.Unmarshal(data, &original) != nil || original.Action != "analyze" {
					t.Fatal("handoff gate overwrote the raw model decision", string(data), err)
				}
				in.Stage = "engineer"
				if _, err := p.Analyze(context.Background(), in); err == nil || *sourceCalls != 0 {
					t.Fatal("explicit engineering stage bypassed the gate", err, *sourceCalls)
				}
			})
		}
	}
}

func TestNativePipelineReturnsNonEngineeringRequestWithoutSourceOrEngineer(t *testing.T) {
	p, in, sourceCalls := routingFixture(t, "return_to_support")
	in.Stage = "route"
	report, err := p.Analyze(context.Background(), in)
	if err != nil || report.Routing == nil || report.Routing.Action != "return_to_support" || report.Usage.Calls != 1 || *sourceCalls != 0 || report.Issue != nil || len(report.Findings) != 0 || len(report.Recommendations) != 1 {
		t.Fatal("non-engineering request entered engineering or lost advice", report, err, *sourceCalls)
	}
	if _, err := os.Stat(filepath.Join(p.RunsDir, in.RunID, "engineering")); !os.IsNotExist(err) {
		t.Fatal("customer support prepared an engineering environment", err)
	}
	if _, err := os.Stat(filepath.Join(p.RunsDir, in.RunID, "routing", "tools.jsonl")); !os.IsNotExist(err) {
		t.Fatal("ordinary customer support required unnecessary Issue retrieval", err)
	}
}

func TestNonAnalysisRoutesSkipReleaseCheck(t *testing.T) {
	for _, decision := range []string{"return_to_support", "request_information", "reuse_agent", "reuse_engineer"} {
		t.Run(decision, func(t *testing.T) {
			p, in, sourceCalls := routingFixture(t, decision)
			in.Stage = "route"
			p.CheckRelease = func(esa.AnalyzeRequest) string {
				t.Fatal("non-analysis route checked engineering admission")
				return ""
			}
			if report, err := p.Analyze(context.Background(), in); err != nil || report.Usage.Calls != 1 || *sourceCalls != 0 {
				t.Fatal("non-analysis route entered engineering", report, err, *sourceCalls)
			}
		})
	}
}

func TestNativePipelineRoutesAndProcessesGroupingRequest(t *testing.T) {
	for _, decision := range []string{"new", "assign_existing", "assignment_info", "assignment_unread", "assignment_no_request"} {
		t.Run(decision, func(t *testing.T) {
			p, in, sourceCalls := routingFixture(t, decision)
			p.CheckRelease = (&Registry{Releases: []ReleaseRegistration{{ProjectID: in.Ticket.ProjectID, Version: "1", AvailableAt: in.Contract.ObservationCutoff}}}).CheckRelease
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
			p.CheckRelease = func(esa.AnalyzeRequest) string {
				t.Fatal("joining checked engineering admission")
				return ""
			}
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
