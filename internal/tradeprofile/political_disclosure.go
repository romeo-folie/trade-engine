package tradeprofile

// PoliticalDisclosureEvidenceVersion identifies the structured disclosure
// provenance schema carried from scanner discovery into deep analysis.
const PoliticalDisclosureEvidenceVersion = 2

// PoliticalDisclosureEvidence contains filing facts only. None of these fields
// are an execution signal or permission to trade.
type PoliticalDisclosureEvidence struct {
	Version                       int                                `json:"version"`
	Ticker                        string                             `json:"ticker"`
	DistinctFilerCount            int                                `json:"distinct_filer_count"`
	DistinctBuyerCount            int                                `json:"distinct_buyer_count"`
	ReportCount                   int                                `json:"report_count"`
	AmendmentReportCount          int                                `json:"amendment_report_count"`
	IndependentReportCount        int                                `json:"independent_report_count"`
	IndependentTransactionCount   int                                `json:"independent_transaction_count"`
	AmendmentTransactionCount     int                                `json:"amendment_transaction_count"`
	TransactionCount              int                                `json:"transaction_count"`
	PurchaseCount                 int                                `json:"purchase_count"`
	SaleCount                     int                                `json:"sale_count"`
	ExchangeCount                 int                                `json:"exchange_count"`
	OtherTransactionCount         int                                `json:"other_transaction_count"`
	PurchaseAmounts               PoliticalDisclosureAmountSummary   `json:"purchase_amounts"`
	SaleAmounts                   PoliticalDisclosureAmountSummary   `json:"sale_amounts"`
	LatestTransactionDate         string                             `json:"latest_transaction_date,omitempty"`
	LatestFilingOrPublicationDate string                             `json:"latest_filing_or_publication_date,omitempty"`
	DisclosureLagDays             PoliticalDisclosureLagSummary      `json:"disclosure_lag_days"`
	RepeatedFilerActivity         bool                               `json:"repeated_activity_by_one_filer"`
	IndependentBuyerActivity      bool                               `json:"independent_buyer_activity"`
	ContradictoryActivity         bool                               `json:"contradictory_buying_and_selling"`
	Filers                        []PoliticalDisclosureFilerEvidence `json:"filers"`
	DataQuality                   PoliticalDisclosureDataQuality     `json:"data_quality"`
}

type PoliticalDisclosureAmountSummary struct {
	KnownTransactionCount   int   `json:"known_transaction_count"`
	UnknownTransactionCount int   `json:"unknown_transaction_count"`
	MinimumTotal            int64 `json:"minimum_total"`
	MaximumTotal            int64 `json:"maximum_total"`
}

type PoliticalDisclosureLagSummary struct {
	KnownTransactionCount int `json:"known_transaction_count"`
	MinimumDays           int `json:"minimum_days"`
	MaximumDays           int `json:"maximum_days"`
	LatestTransactionDays int `json:"latest_transaction_days"`
}

type PoliticalDisclosureFilerEvidence struct {
	CanonicalFilerID              string                              `json:"canonical_filer_id"`
	FilerName                     string                              `json:"filer_name"`
	Office                        string                              `json:"office,omitempty"`
	Chamber                       string                              `json:"chamber,omitempty"`
	Source                        string                              `json:"source"`
	ReportedOwners                []string                            `json:"reported_owners,omitempty"`
	ReportCount                   int                                 `json:"report_count"`
	AmendmentReportCount          int                                 `json:"amendment_report_count"`
	IndependentReportCount        int                                 `json:"independent_report_count"`
	IndependentTransactionCount   int                                 `json:"independent_transaction_count"`
	AmendmentTransactionCount     int                                 `json:"amendment_transaction_count"`
	TransactionCount              int                                 `json:"transaction_count"`
	PurchaseCount                 int                                 `json:"purchase_count"`
	SaleCount                     int                                 `json:"sale_count"`
	ExchangeCount                 int                                 `json:"exchange_count"`
	OtherTransactionCount         int                                 `json:"other_transaction_count"`
	PurchaseAmounts               PoliticalDisclosureAmountSummary    `json:"purchase_amounts"`
	SaleAmounts                   PoliticalDisclosureAmountSummary    `json:"sale_amounts"`
	LatestTransactionDate         string                              `json:"latest_transaction_date,omitempty"`
	LatestFilingOrPublicationDate string                              `json:"latest_filing_or_publication_date,omitempty"`
	DisclosureLagDays             PoliticalDisclosureLagSummary       `json:"disclosure_lag_days"`
	RepeatedActivity              bool                                `json:"repeated_activity"`
	ContradictoryActivity         bool                                `json:"contradictory_buying_and_selling"`
	Reports                       []PoliticalDisclosureReportEvidence `json:"reports"`
}

type PoliticalDisclosureReportEvidence struct {
	ReportID                string   `json:"report_id"`
	ReportType              string   `json:"report_type,omitempty"`
	IsAmendment             bool     `json:"is_amendment"`
	DocumentURL             string   `json:"document_url"`
	FilingOrPublicationDate string   `json:"filing_or_publication_date,omitempty"`
	LatestTransactionDate   string   `json:"latest_transaction_date,omitempty"`
	DisclosureLagDays       *int     `json:"disclosure_lag_days,omitempty"`
	TransactionCount        int      `json:"transaction_count"`
	PurchaseCount           int      `json:"purchase_count"`
	SaleCount               int      `json:"sale_count"`
	ExchangeCount           int      `json:"exchange_count"`
	ReportedOwners          []string `json:"reported_owners,omitempty"`
	ExtractionStatus        string   `json:"extraction_status,omitempty"`
}

type PoliticalDisclosureDataQuality struct {
	CanonicalIdentityBasis   string   `json:"canonical_identity_basis"`
	Limitations              []string `json:"limitations,omitempty"`
	OmittedFilerDetailCount  int      `json:"omitted_filer_detail_count,omitempty"`
	OmittedReportDetailCount int      `json:"omitted_report_detail_count,omitempty"`
}
