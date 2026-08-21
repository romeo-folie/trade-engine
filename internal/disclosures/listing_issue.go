package disclosures

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// ListingRowIssue describes one source row that could not safely become a
// filing. It deliberately excludes source-provided values: list responses can
// contain personal information and untrusted URLs. The fingerprint allows an
// operator to correlate repeated failures without exposing that content.
type ListingRowIssue struct {
	Row         int
	Reason      string
	Fingerprint string
}

func newListingRowIssue(row int, reason string, value any) ListingRowIssue {
	encoded, err := json.Marshal(value)
	if err != nil {
		encoded = []byte("unencodable listing row")
	}
	sum := sha256.Sum256(encoded)
	return ListingRowIssue{
		Row:         row,
		Reason:      reason,
		Fingerprint: hex.EncodeToString(sum[:8]),
	}
}

func allListingRowsMalformed(rowCount int, issues []ListingRowIssue) bool {
	return rowCount > 0 && len(issues) == rowCount
}
