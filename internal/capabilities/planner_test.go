package capabilities

import (
	"path/filepath"
	"testing"
)

func TestPlannerRequiresRegisteredAuthority(t *testing.T) {
	root, err := filepath.Abs("../../profiles")
	if err != nil {
		t.Fatal(err)
	}
	planner := Planner{Root: root, AllowedProfiles: []string{"general", "java-debug"}, AvailableCapabilities: []string{"source"}}
	request := Request{Profile: "java-debug", Version: "2", Capabilities: []string{"source"}, HypothesisID: "H1", Reason: "Inspect dereference", ExpectedObservation: "Read guarded assignment"}
	decision := planner.Resolve(request)
	if decision.Status != "granted" || decision.Profile.SkillHash == "" || decision.Profile.ManifestHash == "" {
		t.Fatalf("invalid grant: %+v", decision)
	}
	for _, change := range []func(*Request){func(r *Request) { r.Capabilities = []string{"runtime"} }, func(r *Request) { r.Profile = "../../host" }, func(r *Request) { r.Version = "future" }, func(r *Request) { r.HypothesisID = "" }} {
		copy := request
		change(&copy)
		if planner.Resolve(copy).Status != "denied" {
			t.Fatal("request expanded operator policy")
		}
	}
}
