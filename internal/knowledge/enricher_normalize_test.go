package knowledge

import "testing"

func TestNormalizeEntityName_UnescapeAndSuffixTrim(t *testing.T) {
	got := normalizeEntityName("  Hims &amp; Hers Health, Inc.  ")
	if got != "Hims & Hers Health" {
		t.Fatalf("normalizeEntityName = %q, want %q", got, "Hims & Hers Health")
	}
}
