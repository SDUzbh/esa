package esa

import "time"

// The service builds this sidecar. A source citation and a human approval are
// recorded separately from a reproduced causal result.
type ReportEnvelope struct {
	ProviderInputHash   string        `json:"provider_input_hash"`
	Plan                *AnalysisPlan `json:"plan,omitempty"` // Historical sidecars only; new plans live in Report.Plan.
	Version             string        `json:"version"`
	SnapshotID          string        `json:"snapshot_id"`
	InputHash           string        `json:"input_hash"`
	ObservationCutoff   time.Time     `json:"observation_cutoff"`
	ResponseAvailableAt time.Time     `json:"response_available_at"`
	IssueID             string        `json:"issue_id"`
	InvestigationID     string        `json:"investigation_id"`
	Provenance          string        `json:"provenance"`
	SourceMapping       string        `json:"source_mapping"`
	CausalVerification  string        `json:"causal_verification"`
	ElapsedMS           int64         `json:"elapsed_ms"`
}
type AnalysisPlan struct {
	Profile        string   `json:"profile"`
	ProfileVersion string   `json:"profile_version"`
	SkillHash      string   `json:"skill_hash"`
	ManifestHash   string   `json:"manifest_hash"`
	Capabilities   []string `json:"capabilities"`
	HypothesisID   string   `json:"hypothesis_id"`
}

func reportEnvelope(task Investigation, available time.Time, elapsed int64) *ReportEnvelope {
	return &ReportEnvelope{
		Version:             "2",
		SnapshotID:          task.Contract.SnapshotID,
		InputHash:           task.Contract.InputHash,
		ObservationCutoff:   task.Contract.ObservationCutoff,
		ResponseAvailableAt: available,
		IssueID:             task.IssueID,
		InvestigationID:     task.ID,
		Provenance:          "frozen_input_provider_evidence",
		SourceMapping:       "provider_reported_not_independently_verified",
		CausalVerification:  "unverified",
		ElapsedMS:           elapsed,
	}
}
