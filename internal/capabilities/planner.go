// Package capabilities authorizes plans against operator-owned manifests. It
// contains no model invocation and never treats a Skill as a permission grant.
package capabilities

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Profile binds operator-owned environment, tool policy and Skill content hashes.
// Skill text describes procedures but cannot independently grant capabilities.
type Profile struct {
	ID           string            `json:"id"`
	Version      string            `json:"version"`
	Capabilities []string          `json:"capabilities"`
	Environment  map[string]string `json:"environment"`
	Skill        string            `json:"skill"`
	SkillHash    string            `json:"skill_hash"`
	ManifestHash string            `json:"manifest_hash"`
	Toolset      string            `json:"toolset,omitempty"`
	ToolsetHash  string            `json:"toolset_hash,omitempty"`
}
type Request struct {
	Profile             string   `json:"profile"`
	Version             string   `json:"version"`
	Capabilities        []string `json:"requested_capabilities"`
	HypothesisID        string   `json:"hypothesis_id"`
	Reason              string   `json:"reason"`
	ExpectedObservation string   `json:"expected_observation"`
}
type Decision struct {
	Status  string   `json:"status"`
	Reason  string   `json:"reason"`
	Profile *Profile `json:"profile,omitempty"`
	Granted []string `json:"granted"`
}
// Planner intersects requested capabilities with both Profile and task policy.
type Planner struct {
	Root                                   string
	AllowedProfiles, AvailableCapabilities []string
}

func Digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func member(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
func (p Planner) Catalog() ([]Profile, error) {
	profiles := []Profile{}
	for _, id := range p.AllowedProfiles {
		profile, err := p.Load(id)
		if err != nil {
			return nil, err
		}
		profiles = append(profiles, profile)
	}
	return profiles, nil
}
// Load confines manifest reads to the approved root and hashes the complete Profile.
func (p Planner) Load(id string) (Profile, error) {
	var profile Profile
	if !member(p.AllowedProfiles, id) || id == "" || filepath.Base(id) != id || strings.ContainsAny(id, "/\\.") {
		return profile, fmt.Errorf("profile outside task policy")
	}
	root, err := os.OpenRoot(p.Root)
	if err != nil {
		return profile, err
	}
	defer root.Close()
	environment, err := root.ReadFile(filepath.Join(id, "environment.json"))
	if err != nil {
		return profile, err
	}
	if err = json.Unmarshal(environment, &profile); err != nil {
		return profile, err
	}
	var tools struct {
		Capabilities []string `json:"capabilities"`
	}
	toolData, err := root.ReadFile(filepath.Join(id, "tools.json"))
	if err != nil {
		return profile, err
	}
	if err = json.Unmarshal(toolData, &tools); err != nil {
		return profile, err
	}
	skill, err := root.ReadFile(filepath.Join(id, "SKILL.md"))
	if err != nil {
		return profile, err
	}
	if profile.ID != id || profile.Version == "" || len(skill) > 32768 {
		return profile, fmt.Errorf("invalid approved profile")
	}
	profile.Capabilities = tools.Capabilities
	var preset []byte
	if name := profile.Environment["toolset"]; name != "" {
		if filepath.Base(name) != name {
			return profile, fmt.Errorf("invalid toolset filename")
		}
		preset, err = root.ReadFile(filepath.Join(id, name))
		if err != nil {
			return profile, err
		}
		if len(preset) > 32768 {
			return profile, fmt.Errorf("toolset exceeds approved documentation limit")
		}
		profile.Toolset = string(preset)
		profile.ToolsetHash = Digest(preset)
	}
	profile.Skill = string(skill)
	profile.SkillHash = Digest(skill)
	profile.ManifestHash = Digest(append(append(append(environment, toolData...), skill...), preset...))
	return profile, nil
}
// Resolve requires a hypothesis, purpose and expected observation before any grant.
// Unknown Profiles, version drift or unavailable capabilities return a denied decision.
func (p Planner) Resolve(request Request) Decision {
	deny := func(reason string) Decision { return Decision{Status: "denied", Reason: reason, Granted: []string{}} }
	if strings.TrimSpace(request.HypothesisID) == "" || strings.TrimSpace(request.Reason) == "" || strings.TrimSpace(request.ExpectedObservation) == "" {
		return deny("A hypothesis, purpose and expected observation are required")
	}
	profile, err := p.Load(request.Profile)
	if err != nil {
		return deny("Profile is not registered for this task")
	}
	if request.Version != profile.Version {
		return deny("Requested Profile version is not approved")
	}
	for _, capability := range request.Capabilities {
		if !member(profile.Capabilities, capability) || !member(p.AvailableCapabilities, capability) {
			return deny("Capability is outside the Profile or registered environment policy: " + capability)
		}
	}
	return Decision{Status: "granted", Reason: "Authorized by task policy and registered environment", Profile: &profile, Granted: request.Capabilities}
}
