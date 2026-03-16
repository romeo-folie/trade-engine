package knowledge

import "testing"

func TestParseExtractionResponse_ArrayAndFencedJSON(t *testing.T) {
	raw := "```json\n[{\"from_entity\":\"A\",\"to_entity\":\"B\",\"relationship_type\":\"partner_of\",\"confidence\":0.9}]\n```"
	rels, err := parseExtractionResponse(raw)
	if err != nil {
		t.Fatalf("parseExtractionResponse error: %v", err)
	}
	if len(rels) != 1 {
		t.Fatalf("expected 1 relationship, got %d", len(rels))
	}
	if rels[0].FromEntity != "A" || rels[0].ToEntity != "B" {
		t.Fatalf("unexpected relationship: %+v", rels[0])
	}
}

func TestParseExtractionResponse_SingleObject(t *testing.T) {
	raw := "{\"from_entity\":\"A\",\"to_entity\":\"B\",\"relationship_type\":\"supplies_to\",\"confidence\":0.8}"
	rels, err := parseExtractionResponse(raw)
	if err != nil {
		t.Fatalf("parseExtractionResponse error: %v", err)
	}
	if len(rels) != 1 {
		t.Fatalf("expected 1 relationship, got %d", len(rels))
	}
}

func TestValidRelationshipTypes(t *testing.T) {
	// Expected canonical set — must exactly match validRelationshipTypes.
	expected := []string{
		"affects_contract", "affected_by_event",
		"competes_with",
		"invested_in_by", "invests_in",
		"manufactures_for", "manufactured_by",
		"parent_of", "partner_of",
		"regulates", "regulated_by",
		"sector_member",
		"subsidiary_of",
		"supplied_by", "supplies_to",
	}

	// Every expected key must be accepted.
	for _, typ := range expected {
		if !validRelationshipTypes[typ] {
			t.Errorf("expected %q to be a valid relationship type", typ)
		}
	}

	// Unknown types must be rejected.
	for _, typ := range []string{"unknown_type", "invests", "SUPPLIES_TO", ""} {
		if validRelationshipTypes[typ] {
			t.Errorf("expected %q to be an invalid relationship type", typ)
		}
	}

	// Set equality: map must not contain extra keys beyond the expected list.
	if got := len(validRelationshipTypes); got != len(expected) {
		t.Errorf("validRelationshipTypes has %d entries, expected %d — map and test list are out of sync", got, len(expected))
	}
	for k := range validRelationshipTypes {
		found := false
		for _, e := range expected {
			if e == k {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("validRelationshipTypes contains unexpected key %q", k)
		}
	}
}
