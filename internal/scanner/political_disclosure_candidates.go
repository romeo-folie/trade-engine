package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/romeo-folie/trade-engine/internal/data"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/romeo-folie/trade-engine/internal/tradeprofile"
)

const (
	maxPoliticalDisclosureFilerDetails  = 8
	maxPoliticalDisclosureReportDetails = 4
)

func (s *MarketScanner) politicalDisclosureCandidates(
	ctx context.Context,
) ([]SwingCandidate, error) {
	cfg := s.Cfg.PoliticalDisclosures
	if !cfg.Enabled || !cfg.PromoteCandidates || !store.IsConnected() {
		return nil, nil
	}
	token, err := newWeeklyLeaderPromotionClaimToken()
	if err != nil {
		return nil, err
	}
	now := s.now()
	rows, err := store.ClaimPoliticalDisclosureCandidatesWithEvidence(
		ctx, now.AddDate(0, 0, -cfg.TriggerWindowDays()),
		now.AddDate(0, 0, -cfg.EvidenceHorizonDays()), now,
		time.Duration(cfg.ClaimLeaseMinutes)*time.Minute,
		cfg.MaxCandidatesPerRun, token,
	)
	if err != nil {
		return nil, err
	}
	byTicker := make(map[string][]store.PoliticalDisclosureCandidate)
	for _, row := range rows {
		byTicker[row.Ticker] = append(byTicker[row.Ticker], row)
	}
	tickers := make([]string, 0, len(byTicker))
	for ticker := range byTicker {
		tickers = append(tickers, ticker)
	}
	sort.Strings(tickers)
	out := make([]SwingCandidate, 0, len(tickers))
	for _, ticker := range tickers {
		out = append(out, buildPoliticalDisclosureCandidate(ticker, token, byTicker[ticker]))
	}
	return s.hydratePoliticalDisclosureMarketData(ctx, out), nil
}

type politicalDisclosureAggregate struct {
	evidence                    tradeprofile.PoliticalDisclosureEvidence
	filers                      map[string]*politicalDisclosureFilerAggregate
	latestTransactionDate       time.Time
	latestTransactionFilingDate time.Time
	lagKnown                    bool
	independentTransactionCount int
}

type politicalDisclosureFilerAggregate struct {
	evidence                    tradeprofile.PoliticalDisclosureFilerEvidence
	owners                      map[string]struct{}
	purchasers                  map[string]struct{}
	latestTransactionDate       time.Time
	latestTransactionFilingDate time.Time
	lagKnown                    bool
	independentTransactionCount int
}

func buildPoliticalDisclosureCandidate(
	ticker, token string, items []store.PoliticalDisclosureCandidate,
) SwingCandidate {
	aggregate := politicalDisclosureAggregate{
		evidence: tradeprofile.PoliticalDisclosureEvidence{
			Version: tradeprofile.PoliticalDisclosureEvidenceVersion,
			Ticker:  ticker,
			DataQuality: tradeprofile.PoliticalDisclosureDataQuality{
				CanonicalIdentityBasis: "normalized source, chamber, and filer-name tokens",
			},
		},
		filers: make(map[string]*politicalDisclosureFilerAggregate),
	}
	reportIDs := make([]string, 0, len(items))
	for _, item := range items {
		if item.Claimed {
			reportIDs = append(reportIDs, item.ReportID)
		}
		aggregate.addReport(item)
	}
	aggregate.finish()
	facts, err := json.Marshal(aggregate.evidence)
	if err != nil {
		facts = json.RawMessage(`{}`)
	}
	return SwingCandidate{
		Ticker: ticker, Sources: []string{"political_disclosure"},
		PoliticalDisclosureClaimToken:     token,
		PoliticalDisclosureClaimReportIDs: reportIDs,
		PoliticalDisclosureEvidence:       summarizePoliticalDisclosure(aggregate.evidence),
		PoliticalDisclosureFacts:          facts,
	}
}

func (a *politicalDisclosureAggregate) addReport(item store.PoliticalDisclosureCandidate) {
	a.evidence.ReportCount++
	a.evidence.TransactionCount += item.TransactionCount
	// is_amendment is canonical. The suffix fallback preserves evidence from
	// rows ingested before the explicit column was added.
	isAmendment := item.IsAmendment || isPoliticalDisclosureAmendment(item.ReportType)
	if isAmendment {
		a.evidence.AmendmentReportCount++
	} else {
		a.evidence.IndependentReportCount++
	}
	reportIdentity := strings.TrimSpace(item.ExternalID)
	if reportIdentity == "" {
		reportIdentity = item.ReportID
	}
	if status := strings.ToLower(strings.TrimSpace(item.ExtractionStatus)); status != "" && status != "completed" {
		a.addLimitation(fmt.Sprintf("report %s extraction status is %s", reportIdentity, status))
	}
	if item.FilingDate.After(parseEvidenceDate(a.evidence.LatestFilingOrPublicationDate)) {
		a.evidence.LatestFilingOrPublicationDate = evidenceDate(item.FilingDate)
	}
	if !isAmendment && item.LatestTransaction.After(parseEvidenceDate(a.evidence.LatestTransactionDate)) {
		a.evidence.LatestTransactionDate = evidenceDate(item.LatestTransaction)
	}

	filerID := canonicalPoliticalFilerID(item.Source, item.Chamber, item.FilerName)
	filer := a.filers[filerID]
	if filer == nil {
		filer = &politicalDisclosureFilerAggregate{
			evidence: tradeprofile.PoliticalDisclosureFilerEvidence{
				CanonicalFilerID: filerID, FilerName: strings.TrimSpace(item.FilerName),
				Office: strings.TrimSpace(item.Office), Chamber: strings.TrimSpace(item.Chamber),
				Source: strings.ToLower(strings.TrimSpace(item.Source)),
			},
			owners:     make(map[string]struct{}),
			purchasers: make(map[string]struct{}),
		}
		a.filers[filerID] = filer
	}
	filer.evidence.ReportCount++
	if isAmendment {
		filer.evidence.AmendmentReportCount++
	} else {
		filer.evidence.IndependentReportCount++
	}
	filer.evidence.TransactionCount += item.TransactionCount
	if isAmendment {
		a.evidence.AmendmentTransactionCount += item.TransactionCount
		filer.evidence.AmendmentTransactionCount += item.TransactionCount
	} else {
		a.evidence.IndependentTransactionCount += item.TransactionCount
		filer.evidence.IndependentTransactionCount += item.TransactionCount
	}
	if item.FilingDate.After(parseEvidenceDate(filer.evidence.LatestFilingOrPublicationDate)) {
		filer.evidence.LatestFilingOrPublicationDate = evidenceDate(item.FilingDate)
	}
	if !isAmendment && item.LatestTransaction.After(parseEvidenceDate(filer.evidence.LatestTransactionDate)) {
		filer.evidence.LatestTransactionDate = evidenceDate(item.LatestTransaction)
	}

	report := tradeprofile.PoliticalDisclosureReportEvidence{
		ReportID: reportIdentity, ReportType: strings.TrimSpace(item.ReportType),
		IsAmendment: isAmendment, DocumentURL: strings.TrimSpace(item.DocumentURL),
		FilingOrPublicationDate: evidenceDate(item.FilingDate),
		LatestTransactionDate:   evidenceDate(item.LatestTransaction),
		TransactionCount:        item.TransactionCount, ExtractionStatus: item.ExtractionStatus,
	}
	var transactions []store.PoliticalDisclosureTransaction
	if err := json.Unmarshal(item.Evidence, &transactions); err != nil {
		a.addLimitation(fmt.Sprintf("report %s transaction evidence could not be decoded", report.ReportID))
	} else {
		if len(transactions) != item.TransactionCount {
			a.addLimitation(fmt.Sprintf(
				"report %s declared %d transactions but exposed %d evidence rows",
				report.ReportID, item.TransactionCount, len(transactions),
			))
		}
		for _, transaction := range transactions {
			a.addTransaction(filer, &report, item.FilingDate, !isAmendment, transaction)
		}
	}
	report.ReportedOwners = sortedSet(filerOwnersForReport(transactions))
	if !item.LatestTransaction.IsZero() && !item.FilingDate.IsZero() {
		lag := calendarDayDifference(item.LatestTransaction, item.FilingDate)
		report.DisclosureLagDays = &lag
	}
	if len(filer.evidence.Reports) < maxPoliticalDisclosureReportDetails {
		filer.evidence.Reports = append(filer.evidence.Reports, report)
	} else {
		a.evidence.DataQuality.OmittedReportDetailCount++
	}
}

func (a *politicalDisclosureAggregate) addTransaction(
	filer *politicalDisclosureFilerAggregate,
	report *tradeprofile.PoliticalDisclosureReportEvidence,
	filingDate time.Time, independent bool,
	transaction store.PoliticalDisclosureTransaction,
) {
	action := strings.ToLower(strings.TrimSpace(transaction.TransactionType))
	switch action {
	case "purchase":
		report.PurchaseCount++
		if independent {
			a.evidence.PurchaseCount++
			filer.evidence.PurchaseCount++
			if purchaser := canonicalPoliticalPurchaser(transaction.Owner); purchaser != "" {
				filer.purchasers[purchaser] = struct{}{}
			}
			addPoliticalAmount(&a.evidence.PurchaseAmounts, transaction)
			addPoliticalAmount(&filer.evidence.PurchaseAmounts, transaction)
		}
	case "sale":
		report.SaleCount++
		if independent {
			a.evidence.SaleCount++
			filer.evidence.SaleCount++
			addPoliticalAmount(&a.evidence.SaleAmounts, transaction)
			addPoliticalAmount(&filer.evidence.SaleAmounts, transaction)
		}
	case "exchange":
		report.ExchangeCount++
		if independent {
			a.evidence.ExchangeCount++
			filer.evidence.ExchangeCount++
		}
	default:
		if independent {
			a.evidence.OtherTransactionCount++
			filer.evidence.OtherTransactionCount++
			a.addLimitation("one or more transaction actions were unrecognized")
		}
	}
	if independent {
		a.independentTransactionCount++
		filer.independentTransactionCount++
	}
	if !independent {
		return
	}
	if owner := strings.TrimSpace(transaction.Owner); owner != "" {
		filer.owners[owner] = struct{}{}
	} else {
		a.addLimitation("one or more transactions did not identify a reported owner")
	}
	if transaction.TransactionDate.IsZero() || filingDate.IsZero() {
		return
	}
	lag := calendarDayDifference(transaction.TransactionDate, filingDate)
	if lag < 0 {
		a.addLimitation("one or more transaction dates follow the filing or publication date")
	}
	addPoliticalLag(&a.evidence.DisclosureLagDays, &a.lagKnown, lag)
	addPoliticalLag(&filer.evidence.DisclosureLagDays, &filer.lagKnown, lag)
	if transaction.TransactionDate.After(a.latestTransactionDate) ||
		(transaction.TransactionDate.Equal(a.latestTransactionDate) &&
			filingDate.After(a.latestTransactionFilingDate)) {
		a.latestTransactionDate = transaction.TransactionDate
		a.latestTransactionFilingDate = filingDate
		a.evidence.DisclosureLagDays.LatestTransactionDays = lag
	}
	if transaction.TransactionDate.After(filer.latestTransactionDate) ||
		(transaction.TransactionDate.Equal(filer.latestTransactionDate) &&
			filingDate.After(filer.latestTransactionFilingDate)) {
		filer.latestTransactionDate = transaction.TransactionDate
		filer.latestTransactionFilingDate = filingDate
		filer.evidence.DisclosureLagDays.LatestTransactionDays = lag
	}
}

func isPoliticalDisclosureAmendment(reportType string) bool {
	value := strings.ToLower(strings.TrimSpace(reportType))
	return strings.HasSuffix(value, "_amendment") || strings.HasSuffix(value, "_correction")
}

func (a *politicalDisclosureAggregate) finish() {
	ids := make([]string, 0, len(a.filers))
	for id := range a.filers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	purchasingFilerCount := 0
	for _, id := range ids {
		filer := a.filers[id]
		filer.evidence.ReportedOwners = sortedSet(filer.owners)
		filer.evidence.RepeatedActivity = filer.evidence.IndependentReportCount > 1 || filer.independentTransactionCount > 1
		filer.evidence.ContradictoryActivity = filer.evidence.PurchaseCount > 0 && filer.evidence.SaleCount > 0
		a.evidence.DistinctBuyerCount += len(filer.purchasers)
		if filer.evidence.PurchaseCount > 0 {
			purchasingFilerCount++
		}
		if filer.evidence.RepeatedActivity {
			a.evidence.RepeatedFilerActivity = true
		}
		if len(a.evidence.Filers) < maxPoliticalDisclosureFilerDetails {
			a.evidence.Filers = append(a.evidence.Filers, filer.evidence)
		} else {
			a.evidence.DataQuality.OmittedFilerDetailCount++
		}
	}
	a.evidence.DistinctFilerCount = len(ids)
	a.evidence.IndependentBuyerActivity = purchasingFilerCount > 1
	a.evidence.ContradictoryActivity = a.evidence.PurchaseCount > 0 && a.evidence.SaleCount > 0
	if a.evidence.AmendmentReportCount > 0 {
		a.addLimitation("amendment rows remain in report detail and raw transaction counts, but are excluded from independent semantic action, amount, owner, timing, repetition, and contradiction aggregates because no authoritative supersession relationship is available")
	}
	if a.evidence.PurchaseAmounts.UnknownTransactionCount > 0 || a.evidence.SaleAmounts.UnknownTransactionCount > 0 {
		a.addLimitation("amount totals exclude transactions without a complete reported range")
	}
	if a.evidence.DataQuality.OmittedFilerDetailCount > 0 || a.evidence.DataQuality.OmittedReportDetailCount > 0 {
		a.addLimitation("per-filer or per-report details were bounded; aggregate counts include all eligible evidence")
	}
}

func (a *politicalDisclosureAggregate) addLimitation(value string) {
	for _, existing := range a.evidence.DataQuality.Limitations {
		if existing == value {
			return
		}
	}
	a.evidence.DataQuality.Limitations = append(a.evidence.DataQuality.Limitations, value)
}

func addPoliticalAmount(summary *tradeprofile.PoliticalDisclosureAmountSummary, transaction store.PoliticalDisclosureTransaction) {
	if transaction.AmountMin == nil || transaction.AmountMax == nil {
		summary.UnknownTransactionCount++
		return
	}
	summary.KnownTransactionCount++
	summary.MinimumTotal += *transaction.AmountMin
	summary.MaximumTotal += *transaction.AmountMax
}

func addPoliticalLag(summary *tradeprofile.PoliticalDisclosureLagSummary, known *bool, lag int) {
	summary.KnownTransactionCount++
	if !*known || lag < summary.MinimumDays {
		summary.MinimumDays = lag
	}
	if !*known || lag > summary.MaximumDays {
		summary.MaximumDays = lag
	}
	*known = true
}

func canonicalPoliticalFilerID(source, chamber, name string) string {
	tokens := strings.FieldsFunc(strings.ToLower(strings.TrimSpace(name)), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	ignored := map[string]struct{}{
		"hon": {}, "honorable": {}, "mr": {}, "mrs": {}, "ms": {}, "dr": {},
		"sen": {}, "senator": {}, "rep": {}, "representative": {},
	}
	canonicalTokens := tokens[:0]
	for _, token := range tokens {
		if _, skip := ignored[token]; !skip {
			canonicalTokens = append(canonicalTokens, token)
		}
	}
	sort.Strings(canonicalTokens)
	identity := strings.Join(canonicalTokens, "-")
	if identity == "" {
		identity = "unknown"
	}
	return strings.ToLower(strings.TrimSpace(source)) + ":" +
		strings.ToLower(strings.TrimSpace(chamber)) + ":" + identity
}

func calendarDayDifference(transactionDate, filingDate time.Time) int {
	transactionDay := time.Date(transactionDate.UTC().Year(), transactionDate.UTC().Month(), transactionDate.UTC().Day(), 0, 0, 0, 0, time.UTC)
	filingDay := time.Date(filingDate.UTC().Year(), filingDate.UTC().Month(), filingDate.UTC().Day(), 0, 0, 0, 0, time.UTC)
	return int(filingDay.Sub(transactionDay).Hours() / 24)
}

func evidenceDate(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format("2006-01-02")
}

func parseEvidenceDate(value string) time.Time {
	parsed, _ := time.Parse("2006-01-02", value)
	return parsed
}

func canonicalPoliticalPurchaser(owner string) string {
	owner = strings.ToLower(strings.Join(strings.Fields(owner), " "))
	switch owner {
	case "", "unknown":
		return ""
	case "self":
		return "self"
	case "sp", "spouse":
		return "spouse"
	case "dc", "dependent", "dependent child":
		return "dependent child"
	case "jt", "joint", "jointly":
		return "joint"
	default:
		return owner
	}
}

func filerOwnersForReport(transactions []store.PoliticalDisclosureTransaction) map[string]struct{} {
	owners := make(map[string]struct{})
	for _, transaction := range transactions {
		if owner := strings.TrimSpace(transaction.Owner); owner != "" {
			owners[owner] = struct{}{}
		}
	}
	return owners
}

func sortedSet(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func summarizePoliticalDisclosure(evidence tradeprofile.PoliticalDisclosureEvidence) string {
	summary := fmt.Sprintf(
		"%d report(s) (%d independent; %d amendment), %d raw transaction row(s) (%d independent; %d amendment), %d canonical filer(s), %d distinct buyer(s): %d independent purchase(s), %d independent sale(s), %d independent exchange(s)",
		evidence.ReportCount, evidence.IndependentReportCount, evidence.AmendmentReportCount,
		evidence.TransactionCount, evidence.IndependentTransactionCount, evidence.AmendmentTransactionCount, evidence.DistinctFilerCount, evidence.DistinctBuyerCount,
		evidence.PurchaseCount, evidence.SaleCount,
		evidence.ExchangeCount,
	)
	if evidence.LatestTransactionDate != "" {
		summary += "; latest transaction " + evidence.LatestTransactionDate
	}
	if evidence.ContradictoryActivity {
		summary += "; includes both buying and selling"
	}
	return summary
}

func mergePoliticalDisclosureCandidates(
	existing, disclosures []SwingCandidate,
) []SwingCandidate {
	byTicker := make(map[string]int, len(existing)+len(disclosures))
	out := append([]SwingCandidate(nil), existing...)
	for index := range out {
		byTicker[out[index].Ticker] = index
	}
	for _, source := range disclosures {
		index, found := byTicker[source.Ticker]
		if !found {
			byTicker[source.Ticker] = len(out)
			out = append(out, source)
			continue
		}
		candidate := &out[index]
		// Keep a wholly independent no-political lineage before attaching
		// political provenance to nested weekly/industry fallbacks. A shallow
		// copy shares those nodes, which lets a later sequential claim loss
		// resurrect an already-invalid political claim.
		fallback := cloneSwingCandidateLineage(candidate)
		fallback.politicalDisclosureFallback = nil
		fallback.politicalDisclosureSource = nil
		candidate.politicalDisclosureFallback = fallback
		attachPoliticalFallback(candidate.weeklyLeaderFallback, source)
		attachPoliticalFallback(candidate.potentIndustryFallback, source)
		candidate.Sources = mergeSources(candidate.Sources, source.Sources)
		candidate.PoliticalDisclosureClaimToken = source.PoliticalDisclosureClaimToken
		candidate.PoliticalDisclosureClaimReportIDs = append(
			[]string(nil), source.PoliticalDisclosureClaimReportIDs...,
		)
		candidate.PoliticalDisclosureEvidence = source.PoliticalDisclosureEvidence
		candidate.PoliticalDisclosureFacts = append(
			json.RawMessage(nil), source.PoliticalDisclosureFacts...,
		)
		politicalSource := cloneSwingCandidate(source)
		politicalSource.politicalDisclosureFallback = nil
		politicalSource.politicalDisclosureSource = nil
		candidate.politicalDisclosureSource = &politicalSource
	}
	return out
}

func attachPoliticalFallback(fallback *SwingCandidate, source SwingCandidate) {
	if fallback == nil {
		return
	}
	withoutPolitical := cloneSwingCandidate(*fallback)
	withoutPolitical.politicalDisclosureFallback = nil
	withPolitical := cloneSwingCandidate(withoutPolitical)
	withPolitical.Sources = mergeSources(withPolitical.Sources, source.Sources)
	withPolitical.PoliticalDisclosureClaimToken = source.PoliticalDisclosureClaimToken
	withPolitical.PoliticalDisclosureClaimReportIDs = append(
		[]string(nil), source.PoliticalDisclosureClaimReportIDs...,
	)
	withPolitical.PoliticalDisclosureEvidence = source.PoliticalDisclosureEvidence
	withPolitical.PoliticalDisclosureFacts = append(
		json.RawMessage(nil), source.PoliticalDisclosureFacts...,
	)
	withPolitical.politicalDisclosureFallback = &withoutPolitical
	politicalSource := cloneSwingCandidate(source)
	politicalSource.politicalDisclosureFallback = nil
	politicalSource.politicalDisclosureSource = nil
	withPolitical.politicalDisclosureSource = &politicalSource
	*fallback = withPolitical
}

// hydratePoliticalDisclosureMarketData prevents a filing-derived symbol from
// leaving this source without the tradeability and liquidity evidence required
// by the normal scanner. Provider failures release claims for a later attempt;
// definitive asset/snapshot failures are terminally recorded so malformed
// symbols do not churn forever.
func (s *MarketScanner) hydratePoliticalDisclosureMarketData(
	ctx context.Context, candidates []SwingCandidate,
) []SwingCandidate {
	political := make([]string, 0)
	for _, candidate := range candidates {
		if candidate.PoliticalDisclosureClaimToken != "" {
			political = append(political, candidate.Ticker)
		}
	}
	if len(political) == 0 {
		return candidates
	}
	if s.StockAssets == nil || s.StockSnapshots == nil {
		slog.Warn("scanner: political disclosure market-data providers unavailable")
		s.releasePoliticalDisclosureClaims(ctx, candidates)
		return nil
	}

	assets, err := s.StockAssets.FetchStockAssets(ctx)
	if err != nil {
		slog.Warn("scanner: validate political disclosure assets", "err", err)
		s.releasePoliticalDisclosureClaims(ctx, candidates)
		return nil
	}
	tradable := make(map[string]struct{}, len(assets))
	for _, asset := range assets {
		if strings.EqualFold(asset.Status, "active") && asset.Tradable &&
			strings.EqualFold(asset.AssetClass, "us_equity") {
			tradable[strings.ToUpper(strings.TrimSpace(asset.Symbol))] = struct{}{}
		}
	}
	snapshots, err := s.StockSnapshots.FetchStockSnapshots(ctx, political, 100)
	if err != nil {
		slog.Warn("scanner: hydrate political disclosure snapshots", "err", err)
		s.releasePoliticalDisclosureClaims(ctx, candidates)
		return nil
	}

	out := make([]SwingCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.PoliticalDisclosureClaimToken == "" {
			out = append(out, candidate)
			continue
		}
		if _, ok := tradable[candidate.Ticker]; !ok {
			s.rejectPoliticalDisclosureMarketData(ctx, candidate,
				"political disclosure symbol is not an active, tradeable US equity")
			continue
		}
		snapshot, ok := snapshots.Snapshots[candidate.Ticker]
		if !ok {
			if failure := snapshots.Failures[candidate.Ticker]; failure != nil {
				s.releasePoliticalDisclosureMarketData(ctx, candidate,
					fmt.Sprintf("political disclosure snapshot unavailable: %v", failure))
				continue
			}
			s.rejectPoliticalDisclosureMarketData(ctx, candidate,
				"political disclosure symbol has no current market snapshot")
			continue
		}
		if snapshot.PreviousDailyBar == nil ||
			snapshot.PreviousDailyBar.Close <= 0 ||
			snapshot.PreviousDailyBar.Volume <= 0 {
			s.releasePoliticalDisclosureMarketData(ctx, candidate,
				"political disclosure completed-session liquidity is unavailable")
			continue
		}
		hydratePoliticalSnapshot(&candidate, snapshot)
		if candidate.Price <= 0 {
			s.releasePoliticalDisclosureMarketData(ctx, candidate,
				"political disclosure current price is unavailable")
			continue
		}
		if candidate.DollarVolume < pullbackMinDollarVolume {
			s.rejectPoliticalDisclosureMarketData(ctx, candidate, fmt.Sprintf(
				"political disclosure market evidence failed liquidity gate: price=%.4f volume=%d dollar_volume=%.2f minimum=%.2f",
				candidate.Price, candidate.Volume, candidate.DollarVolume,
				pullbackMinDollarVolume,
			))
			continue
		}
		out = append(out, candidate)
	}
	return out
}

func hydratePoliticalSnapshot(candidate *SwingCandidate, snapshot data.StockSnapshotEvidence) {
	price := snapshot.LatestTradePrice
	if price <= 0 && snapshot.MinuteBar != nil {
		price = snapshot.MinuteBar.Close
	}
	if price <= 0 && snapshot.DailyBar != nil {
		price = snapshot.DailyBar.Close
	}
	if price > 0 {
		candidate.Price = price
	}
	if snapshot.PreviousDailyBar != nil {
		candidate.Volume = snapshot.PreviousDailyBar.Volume
		candidate.DollarVolume = snapshot.PreviousDailyBar.Close *
			float64(snapshot.PreviousDailyBar.Volume)
	}
	if snapshot.PreviousDailyBar != nil && snapshot.PreviousDailyBar.Close > 0 && candidate.Price > 0 {
		candidate.ChangePct = (candidate.Price/snapshot.PreviousDailyBar.Close - 1) * 100
	}
}

func (s *MarketScanner) rejectPoliticalDisclosureMarketData(
	ctx context.Context, candidate SwingCandidate, reason string,
) {
	// This is a definitive asset/snapshot failure, not a transient scoring
	// failure. It must remain terminal instead of being requeued forever.
	s.markPoliticalDisclosureCandidate(ctx, candidate, "rejected", "", reason)
	slog.Warn("scanner: political disclosure candidate rejected",
		"ticker", candidate.Ticker, "reason", reason)
}

func (s *MarketScanner) releasePoliticalDisclosureMarketData(
	ctx context.Context, candidate SwingCandidate, reason string,
) {
	s.markPoliticalDisclosureCandidate(ctx, candidate, "pending", "", reason)
	slog.Warn("scanner: political disclosure candidate released",
		"ticker", candidate.Ticker, "reason", reason)
}

func (s *MarketScanner) markPoliticalDisclosureCandidate(
	ctx context.Context, candidate SwingCandidate,
	status, scannerRunID, reason string,
) {
	if candidate.PoliticalDisclosureClaimToken == "" || !store.IsConnected() {
		return
	}
	// Scanner failures and capacity/diversification suppression are transient:
	// they do not invalidate the disclosure evidence. Requeue only this
	// candidate so another cycle can evaluate it after capacity recovers.
	if politicalDisclosurePromotionRequeues(status) {
		if err := store.MarkPoliticalDisclosurePromotion(
			ctx, candidate.Ticker, candidate.PoliticalDisclosureClaimToken,
			candidate.PoliticalDisclosureClaimReportIDs, "pending", "", reason, s.now(),
		); err != nil {
			slog.Warn("scanner: requeue political disclosure promotion",
				"ticker", candidate.Ticker, "status", status, "err", err)
		}
		return
	}
	if err := store.MarkPoliticalDisclosurePromotion(
		ctx, candidate.Ticker, candidate.PoliticalDisclosureClaimToken,
		candidate.PoliticalDisclosureClaimReportIDs, status, scannerRunID, reason, s.now(),
	); err != nil {
		slog.Warn("scanner: mark political disclosure promotion",
			"ticker", candidate.Ticker, "status", status, "err", err)
	}
}

func politicalDisclosurePromotionRequeues(status string) bool {
	return status == "failed" || status == "cap_suppressed"
}

func (s *MarketScanner) releasePoliticalDisclosureClaims(
	ctx context.Context, candidates []SwingCandidate,
) {
	if len(candidates) == 0 || !store.IsConnected() {
		return
	}
	token := candidates[0].PoliticalDisclosureClaimToken
	if token == "" {
		return
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := store.ReleasePoliticalDisclosurePromotionClaims(
		releaseCtx, token, "scanner cycle ended before a terminal outcome",
	); err != nil {
		slog.Warn("scanner: release political disclosure promotion claims", "err", err)
	}
}
