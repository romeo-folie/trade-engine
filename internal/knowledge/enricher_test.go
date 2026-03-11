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
