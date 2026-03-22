package orb

// ORBAnalysisResult is the structured LLM response for ORB catalyst scoring.
type ORBAnalysisResult struct {
	Action          string   `json:"action"`           // "trade" | "skip"
	Conviction      float64  `json:"conviction"`       // 0.0–1.0
	CatalystType    string   `json:"catalyst_type"`    // earnings | upgrade | filing | news | unknown
	CatalystQuality string   `json:"catalyst_quality"` // substantive | speculative | none
	Reasoning       string   `json:"reasoning"`
	Risks           []string `json:"risks"`
}
