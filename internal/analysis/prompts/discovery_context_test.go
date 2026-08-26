package prompts

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

func testDiscoveryContext() *tradeprofile.DiscoveryContext {
	return &tradeprofile.DiscoveryContext{
		Version: tradeprofile.DiscoveryContextVersion,
		Sources: []string{"weekly_leader"}, Thesis: "Relative-strength leader with orderly support.",
		EvidenceBySource: map[string]json.RawMessage{
			"weekly_leader": json.RawMessage(`{"rank":2,"return_3m_pct":18.4}`),
		},
	}
}

func TestBuildStockPromptWithDiscoveryContextRendersStructuredProvenance(t *testing.T) {
	_, user := BuildStockPromptWithDiscoveryContext(
		"NVDA", []string{"momentum"}, "1D", nil, Indicators{}, nil, Portfolio{},
		"", nil, "", false, testDiscoveryContext(),
	)
	for _, want := range []string{"### DISCOVERY CONTEXT", "Sources: weekly_leader", "Relative-strength leader", `Evidence[weekly_leader]: {"rank":2,"return_3m_pct":18.4}`, "not as an execution lens"} {
		if !strings.Contains(user, want) {
			t.Errorf("discovery context missing %q:\n%s", want, user)
		}
	}
}

func TestBuildRolePromptWithDiscoveryContextPropagatesToSpecialists(t *testing.T) {
	_, user := BuildRolePromptWithDiscoveryContext(
		"technical", "NVDA", []string{"momentum"}, "1D", nil, Indicators{}, nil, Portfolio{},
		"", nil, "", testDiscoveryContext(),
	)
	if !strings.Contains(user, "### DISCOVERY CONTEXT") || !strings.Contains(user, "Evidence[weekly_leader]") {
		t.Fatalf("specialist prompt did not receive discovery context:\n%s", user)
	}
}

func testPoliticalDiscoveryContext() *tradeprofile.DiscoveryContext {
	return &tradeprofile.DiscoveryContext{
		Version: tradeprofile.DiscoveryContextVersion,
		Sources: []string{"political_disclosure"},
		Thesis:  "GOV surfaced by structured political-disclosure facts.",
		EvidenceBySource: map[string]json.RawMessage{
			"political_disclosure": json.RawMessage(`{
				"version":2,"ticker":"GOV","distinct_filer_count":2,"distinct_buyer_count":2,
				"qualifying_filer_count":1,"qualifying_buyer_count":1,"qualifying_purchase_minimum":1001,"latest_qualifying_purchase_date":"2026-08-10","latest_qualifying_filing_date":"2026-08-15",
				"report_count":3,"independent_report_count":2,"amendment_report_count":1,"transaction_count":4,"independent_transaction_count":3,"amendment_transaction_count":1,"purchase_count":3,"sale_count":1,
				"exchange_count":0,"other_transaction_count":0,
				"purchase_amounts":{"known_transaction_count":3,"unknown_transaction_count":0,"minimum_total":3003,"maximum_total":45000},
				"sale_amounts":{"known_transaction_count":1,"unknown_transaction_count":0,"minimum_total":1001,"maximum_total":15000},
				"latest_transaction_date":"2026-08-10","latest_filing_or_publication_date":"2026-08-15",
				"disclosure_lag_days":{"known_transaction_count":4,"minimum_days":2,"maximum_days":20,"latest_transaction_days":5},
				"repeated_activity_by_one_filer":true,"independent_buyer_activity":true,"contradictory_buying_and_selling":true,
				"filers":[{"canonical_filer_id":"house:house:alex-example","filer_name":"Hon. Alex Example","office":"CA12","chamber":"house","source":"house","reported_owners":["Self","Spouse"],"report_count":2,"independent_report_count":1,"amendment_report_count":1,"transaction_count":3,"independent_transaction_count":2,"amendment_transaction_count":1,"purchase_count":2,"sale_count":1,"exchange_count":0,"other_transaction_count":0,"purchase_amounts":{"known_transaction_count":2,"unknown_transaction_count":0,"minimum_total":2002,"maximum_total":30000},"sale_amounts":{"known_transaction_count":1,"unknown_transaction_count":0,"minimum_total":1001,"maximum_total":15000},"latest_transaction_date":"2026-08-10","latest_filing_or_publication_date":"2026-08-15","disclosure_lag_days":{"known_transaction_count":3,"minimum_days":2,"maximum_days":20,"latest_transaction_days":5},"repeated_activity":true,"contradictory_buying_and_selling":true,"reports":[{"report_id":"house-report-123","report_type":"periodic_transaction_amendment","is_amendment":true,"document_url":"https://example.test/house-report-123.pdf","filing_or_publication_date":"2026-08-15","latest_transaction_date":"2026-08-10","disclosure_lag_days":5,"transaction_count":3,"purchase_count":2,"sale_count":1,"exchange_count":0,"reported_owners":["Self","Spouse"],"extraction_status":"completed"}]}],
				"data_quality":{"canonical_identity_basis":"normalized source, chamber, and filer-name tokens","limitations":["amount totals exclude missing ranges"]}
			}`),
		},
	}
}

func TestPoliticalDisclosureFactsReachAllDeepAnalysisPrompts(t *testing.T) {
	context := testPoliticalDiscoveryContext()
	_, standard := BuildStockPromptWithDiscoveryContext(
		"GOV", []string{"momentum"}, "4H", nil, Indicators{}, nil, Portfolio{},
		"", nil, "", false, context,
	)
	_, directional := BuildRolePromptWithDiscoveryContext(
		"sentiment", "GOV", []string{"momentum"}, "4H", nil, Indicators{}, nil,
		Portfolio{}, "", nil, "", context,
	)
	_, risk := BuildRiskManagerReviewPromptWithDiscoveryContext(
		"GOV", []string{"momentum"}, "4H", nil, Indicators{}, nil, Portfolio{},
		"", nil, "", "directional findings", context,
	)
	for name, prompt := range map[string]string{
		"standard": standard, "directional specialist": directional, "risk manager": risk,
	} {
		for _, want := range []string{
			"filers=2; buyers=2; reports=3; independent_reports=2; amendments=1; raw_transaction_rows=4",
			"qualifying_trigger: filers=1; buyers=1; purchase_minimum=$1001; latest_purchase=2026-08-10; latest_filing_or_publication=2026-08-15",
			"qualification fields apply only to the claimed trigger-window disclosures",
			"repeated_filer=true; independent_buyers=true; contradictory_buying_and_selling=true",
			"id=house:house:alex-example", "owners=Self, Spouse",
			"id=house-report-123", "https://example.test/house-report-123.pdf",
			"type=periodic_transaction_amendment; amendment=true",
			"identity_basis=normalized source, chamber, and filer-name tokens",
			"evidence, not as an execution lens",
			"relationship may corroborate the thesis, but is optional",
		} {
			if !strings.Contains(prompt, want) {
				t.Errorf("%s prompt missing %q:\n%s", name, want, prompt)
			}
		}
		if strings.Contains(strings.ToLower(prompt), "political_disclosure strategy") {
			t.Errorf("%s prompt treated provenance as a strategy:\n%s", name, prompt)
		}
	}
}
