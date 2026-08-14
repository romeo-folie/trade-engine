// Package tradeprofile owns the versioned contract separating scanner
// provenance from the execution lenses an analyst may consider. It depends
// only on the standard library so store, scanner, and analysis can share it.
package tradeprofile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const (
	DiscoveryContextVersion = 1
	ExecutionProfileVersion = 1
)

// DiscoveryContext records why a candidate was discovered. Sources are
// attribution, not execution strategies.
type DiscoveryContext struct {
	Version          int                        `json:"version"`
	Sources          []string                   `json:"sources"`
	Thesis           string                     `json:"thesis"`
	EvidenceBySource map[string]json.RawMessage `json:"evidence_by_source"`
}

// ExecutionProfile is the scanner-owned, bounded menu for later analysis.
type ExecutionProfile struct {
	Version              int      `json:"version"`
	RecommendedLens      string   `json:"recommended_lens"`
	AdmissibleLenses     []string `json:"admissible_lenses"`
	PrimaryTimeframe     string   `json:"primary_timeframe"`
	EntryTimingTimeframe string   `json:"entry_timing_timeframe,omitempty"`
	ScorerRationale      string   `json:"scorer_rationale"`
}

var executionLenses = map[string]struct{}{
	"trend_following":   {},
	"mean_reversion":    {},
	"momentum":          {},
	"news_sentiment":    {},
	"pullback_strength": {},
}

var executionTimeframes = map[string]struct{}{"1D": {}, "4H": {}, "1H": {}}

var sourceIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// IsExecutionLens excludes source labels and entry tags from the contract.
func IsExecutionLens(value string) bool {
	_, ok := executionLenses[strings.ToLower(strings.TrimSpace(value))]
	return ok
}

func (c DiscoveryContext) Validate() error {
	_, err := c.Canonicalize()
	return err
}

// Canonicalize returns a validated copy suitable for durable storage. Source
// identifiers are lower snake_case; evidence keys are canonicalized using the
// same rule, so attribution cannot diverge from its evidence by whitespace or
// casing alone.
func (c DiscoveryContext) Canonicalize() (DiscoveryContext, error) {
	c.Thesis = strings.TrimSpace(c.Thesis)
	if c.Version != DiscoveryContextVersion {
		return DiscoveryContext{}, fmt.Errorf("discovery context version %d is unsupported", c.Version)
	}
	if len(c.Sources) == 0 || strings.TrimSpace(c.Thesis) == "" || len(c.EvidenceBySource) == 0 {
		return DiscoveryContext{}, fmt.Errorf("discovery context requires sources, thesis, and evidence_by_source")
	}
	canonicalSources, err := CanonicalizeSources(c.Sources)
	if err != nil {
		return DiscoveryContext{}, err
	}
	sources := make(map[string]struct{}, len(canonicalSources))
	for _, source := range canonicalSources {
		sources[source] = struct{}{}
	}
	canonicalEvidence := make(map[string]json.RawMessage, len(c.EvidenceBySource))
	for sourceValue, evidence := range c.EvidenceBySource {
		source, err := canonicalSource(sourceValue)
		if err != nil {
			return DiscoveryContext{}, fmt.Errorf("discovery context evidence key: %w", err)
		}
		if _, exists := canonicalEvidence[source]; exists {
			return DiscoveryContext{}, fmt.Errorf("discovery context evidence source %q is duplicated", source)
		}
		if _, ok := sources[source]; !ok {
			return DiscoveryContext{}, fmt.Errorf("discovery context evidence source %q is not attributed", source)
		}
		if !isJSONObject(evidence) {
			return DiscoveryContext{}, fmt.Errorf("discovery context evidence for %q must be a JSON object", source)
		}
		canonicalEvidence[source] = append(json.RawMessage(nil), evidence...)
	}
	for source := range sources {
		if _, ok := canonicalEvidence[source]; !ok {
			return DiscoveryContext{}, fmt.Errorf("discovery context source %q is missing evidence", source)
		}
	}
	c.Sources = canonicalSources
	c.EvidenceBySource = canonicalEvidence
	return c, nil
}

// CanonicalizeSources validates and canonicalizes discovery source identifiers
// while preserving their order. Callers that compare source sets should ignore
// that preserved order after this function returns.
func CanonicalizeSources(values []string) ([]string, error) {
	sources := make(map[string]struct{}, len(values))
	canonicalSources := make([]string, 0, len(values))
	for _, value := range values {
		source, err := canonicalSource(value)
		if err != nil {
			return nil, err
		}
		if _, exists := sources[source]; exists {
			return nil, fmt.Errorf("discovery context source %q is duplicated", source)
		}
		sources[source] = struct{}{}
		canonicalSources = append(canonicalSources, source)
	}
	return canonicalSources, nil
}

func (p ExecutionProfile) Validate() error {
	_, err := p.Canonicalize()
	return err
}

// Canonicalize returns a validated copy suitable for durable storage. The
// order of admissible lenses is preserved because it represents scanner-owned
// preference order for later analysis.
func (p ExecutionProfile) Canonicalize() (ExecutionProfile, error) {
	p.RecommendedLens = canonicalLens(p.RecommendedLens)
	p.PrimaryTimeframe = canonicalTimeframe(p.PrimaryTimeframe)
	p.EntryTimingTimeframe = canonicalTimeframe(p.EntryTimingTimeframe)
	p.ScorerRationale = strings.TrimSpace(p.ScorerRationale)
	if p.Version != ExecutionProfileVersion {
		return ExecutionProfile{}, fmt.Errorf("execution profile version %d is unsupported", p.Version)
	}
	if !IsExecutionLens(p.RecommendedLens) || len(p.AdmissibleLenses) == 0 {
		return ExecutionProfile{}, fmt.Errorf("execution profile requires a recognised recommended lens and admissible lenses")
	}
	admissible := make(map[string]struct{}, len(p.AdmissibleLenses))
	canonicalLenses := make([]string, 0, len(p.AdmissibleLenses))
	for _, value := range p.AdmissibleLenses {
		lens := canonicalLens(value)
		if !IsExecutionLens(lens) {
			return ExecutionProfile{}, fmt.Errorf("execution profile admissible lens %q is not recognised", lens)
		}
		if _, exists := admissible[lens]; exists {
			return ExecutionProfile{}, fmt.Errorf("execution profile admissible lens %q is duplicated", lens)
		}
		admissible[lens] = struct{}{}
		canonicalLenses = append(canonicalLenses, lens)
	}
	if _, ok := admissible[p.RecommendedLens]; !ok {
		return ExecutionProfile{}, fmt.Errorf("execution profile recommended lens %q is not admissible", p.RecommendedLens)
	}
	if !isExecutionTimeframe(p.PrimaryTimeframe) ||
		(p.EntryTimingTimeframe != "" && !isExecutionTimeframe(p.EntryTimingTimeframe)) ||
		strings.TrimSpace(p.ScorerRationale) == "" {
		return ExecutionProfile{}, fmt.Errorf("execution profile requires recognised timeframes and scorer rationale")
	}
	p.AdmissibleLenses = canonicalLenses
	return p, nil
}

func MarshalDiscoveryContext(value *DiscoveryContext) ([]byte, error) {
	if value == nil {
		return nil, nil
	}
	canonical, err := value.Canonicalize()
	if err != nil {
		return nil, err
	}
	return json.Marshal(canonical)
}

func DecodeDiscoveryContext(raw []byte) (*DiscoveryContext, error) {
	if isNull(raw) {
		return nil, nil
	}
	if !isJSONObject(raw) {
		return nil, fmt.Errorf("discovery context must be a JSON object")
	}
	var value DiscoveryContext
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("decode discovery context: %w", err)
	}
	canonical, err := value.Canonicalize()
	if err != nil {
		return nil, err
	}
	return &canonical, nil
}

func MarshalExecutionProfile(value *ExecutionProfile) ([]byte, error) {
	if value == nil {
		return nil, nil
	}
	canonical, err := value.Canonicalize()
	if err != nil {
		return nil, err
	}
	return json.Marshal(canonical)
}

func DecodeExecutionProfile(raw []byte) (*ExecutionProfile, error) {
	if isNull(raw) {
		return nil, nil
	}
	if !isJSONObject(raw) {
		return nil, fmt.Errorf("execution profile must be a JSON object")
	}
	var value ExecutionProfile
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, fmt.Errorf("decode execution profile: %w", err)
	}
	canonical, err := value.Canonicalize()
	if err != nil {
		return nil, err
	}
	return &canonical, nil
}

func isNull(raw []byte) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func isJSONObject(raw []byte) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && raw[0] == '{' && json.Valid(raw)
}

func isExecutionTimeframe(value string) bool {
	_, ok := executionTimeframes[strings.ToUpper(strings.TrimSpace(value))]
	return ok
}

func canonicalLens(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func canonicalTimeframe(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}

func canonicalSource(value string) (string, error) {
	source := strings.ToLower(strings.TrimSpace(value))
	if source == "" {
		return "", fmt.Errorf("discovery context contains an empty source")
	}
	if !sourceIdentifier.MatchString(source) {
		return "", fmt.Errorf("discovery context source %q must be lower snake_case", value)
	}
	return source, nil
}
