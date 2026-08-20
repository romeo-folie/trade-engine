package tradeprofile

import (
	"encoding/json"
	"strconv"
	"strings"
)

// MergeDiscoveryContexts produces the compact prompt representation of all
// currently active reasons a ticker is being analysed. Duplicate sources keep
// the first (oldest) context, preserving deterministic provenance.
func MergeDiscoveryContexts(values ...*DiscoveryContext) *DiscoveryContext {
	merged := &DiscoveryContext{Version: DiscoveryContextVersion, EvidenceBySource: map[string]json.RawMessage{}}
	theses := make([]string, 0, len(values))
	for _, value := range values {
		if value == nil {
			continue
		}
		canonical, err := value.Canonicalize()
		if err != nil {
			continue
		}
		for _, source := range canonical.Sources {
			mergedSource := source
			for sequence := 2; ; sequence++ {
				if _, exists := merged.EvidenceBySource[mergedSource]; !exists {
					break
				}
				mergedSource = source + "_" + strconv.Itoa(sequence)
			}
			merged.Sources = append(merged.Sources, mergedSource)
			merged.EvidenceBySource[mergedSource] = canonical.EvidenceBySource[source]
		}
		if canonical.Thesis != "" {
			theses = append(theses, canonical.Thesis)
		}
	}
	if len(merged.Sources) == 0 {
		return nil
	}
	merged.Thesis = strings.Join(theses, " | ")
	canonical, err := merged.Canonicalize()
	if err != nil {
		return nil
	}
	return &canonical
}
