package esa

import "time"

// The service builds this sidecar. A source citation and a human approval are
// recorded separately from a reproduced causal result.
type ReportEnvelope struct {
	ProviderInputHash   string               `json:"provider_input_hash"`
	Findings            []FindingAssessment  `json:"findings"`
	Evidence            []EvidenceAssessment `json:"evidence"`
	Plan                *AnalysisPlan        `json:"plan,omitempty"`
	Version             string               `json:"version"`
	SnapshotID          string               `json:"snapshot_id"`
	InputHash           string               `json:"input_hash"`
	ObservationCutoff   time.Time            `json:"observation_cutoff"`
	ResponseAvailableAt time.Time            `json:"response_available_at"`
	IssueID             string               `json:"issue_id"`
	InvestigationID     string               `json:"investigation_id"`
	Provenance          string               `json:"provenance"`
	SourceMapping       string               `json:"source_mapping"`
	CausalVerification  string               `json:"causal_verification"`
	ElapsedMS           int64                `json:"elapsed_ms"`
}
type FindingAssessment struct {
	ID                 string   `json:"id"`
	State              string   `json:"state"`
	LocationRole       string   `json:"location_role"`
	SourceMapping      string   `json:"source_mapping_status"`
	EvidenceIDs        []string `json:"evidence_ids"`
	VerificationStatus string   `json:"verification_status"`
}
type EvidenceAssessment struct {
	ID                 string `json:"id"`
	Source             string `json:"source"`
	Reference          string `json:"reference"`
	ContentHash        string `json:"content_hash"`
	ArtifactHashStatus string `json:"artifact_hash_status"`
}

func assessReport(report *Report, input AnalyzeRequest) {
	envelope := report.Envelope
	envelope.ProviderInputHash = hash(input)
	allEvidence := append([]Evidence(nil), report.Evidence...)
	for _, finding := range report.Findings {
		allEvidence = append(allEvidence, finding.Evidence...)
	}
	seen := map[string]bool{}
	for _, ev := range allEvidence {
		id := hash(ev)
		if seen[id] {
			continue
		}
		seen[id] = true
		envelope.Evidence = append(envelope.Evidence, EvidenceAssessment{ID: "evidence_" + hash(ev), Source: ev.Source, Reference: ev.Reference, ContentHash: hash(ev.Quote), ArtifactHashStatus: "not_supplied"})
	}
	for _, finding := range report.Findings {
		ids := []string{}
		for _, ev := range finding.Evidence {
			ids = append(ids, "evidence_"+hash(ev))
		}
		envelope.Findings = append(envelope.Findings, FindingAssessment{ID: "finding_" + hash(finding), State: "hypothesis", LocationRole: "suspected_fault_site", SourceMapping: "provider_reported_not_independently_verified", EvidenceIDs: ids, VerificationStatus: "unverified"})
	}
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
	return &ReportEnvelope{Version: "2", SnapshotID: task.Contract.SnapshotID, InputHash: task.Contract.InputHash, ObservationCutoff: task.Contract.ObservationCutoff, ResponseAvailableAt: available, IssueID: task.IssueID, InvestigationID: task.ID, Provenance: "frozen_input_provider_evidence", SourceMapping: "provider_reported_not_independently_verified", CausalVerification: "unverified", ElapsedMS: elapsed}
}
