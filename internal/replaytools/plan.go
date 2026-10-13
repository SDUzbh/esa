package replaytools

import (
	"bufio"
	"encoding/json"
	"os"

	"esa/internal/capabilities"
	"esa/internal/esa"
)

func ReadPlan(path string) (*esa.AnalysisPlan, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 4096), 2<<20)
	var plan *esa.AnalysisPlan
	for scan.Scan() {
		var record Record
		if err = json.Unmarshal(scan.Bytes(), &record); err != nil {
			return nil, err
		}
		if record.Tool != "capability_request" {
			continue
		}
		encoded, _ := json.Marshal(record.Result["decision"])
		var decision capabilities.Decision
		if err = json.Unmarshal(encoded, &decision); err != nil {
			return nil, err
		}
		if decision.Status != "granted" || decision.Profile == nil {
			continue
		}
		var request capabilities.Request
		if err = json.Unmarshal(record.Arguments, &request); err != nil {
			return nil, err
		}
		profile := decision.Profile
		plan = &esa.AnalysisPlan{Profile: profile.ID, ProfileVersion: profile.Version, SkillHash: profile.SkillHash, ManifestHash: profile.ManifestHash, Capabilities: decision.Granted, HypothesisID: request.HypothesisID}
	}
	return plan, scan.Err()
}
