package disclosures

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/store"
)

type Runner struct {
	Cfg                config.DisclosuresConfig
	House              *HouseClient
	Senate             *SenateClient
	OGE                *OGEClient
	Extractor          PDFExtractor
	Now                func() time.Time
	RetryLease         time.Duration
	reportTerminal     func(context.Context, string, string) (bool, error)
	persistReport      func(context.Context, Report) error
	claimDueRetries    func(context.Context, string, time.Time, int, time.Duration) ([]store.PoliticalDisclosureReport, error)
	saveRetryResult    func(context.Context, store.PoliticalDisclosureReport, string) error
	releaseRetryClaims func(context.Context, string) error
	renewRetryLease    func(context.Context, string, string, string, time.Time, time.Duration) (bool, error)
}

const politicalDisclosureRetryLease = 15 * time.Minute

type SyncResult struct {
	HouseProcessed  int
	SenateProcessed int
	OGEProcessed    int
}

func NewRunner(cfg config.DisclosuresConfig) (*Runner, error) {
	if validation := config.ValidateDisclosuresConfig(cfg); len(validation) > 0 {
		return nil, fmt.Errorf("invalid disclosure config: %s", strings.Join(validation, "; "))
	}
	timeout := time.Duration(cfg.RequestTimeoutSeconds) * time.Second
	runner := &Runner{Cfg: cfg, Now: time.Now}
	if cfg.House.Enabled {
		runner.House = &HouseClient{
			BaseURL:      cfg.House.BaseURL,
			HTTP:         &http.Client{Timeout: timeout},
			RequestDelay: time.Duration(cfg.House.RequestDelayMilliseconds) * time.Millisecond,
		}
		extractor, err := NewPDFiumExtractor()
		if err != nil {
			return nil, err
		}
		runner.Extractor = extractor
	}
	if cfg.OGE.Enabled {
		runner.OGE = NewOGEClient(cfg.OGE.BaseURL, timeout)
		runner.OGE.RequestDelay = time.Duration(cfg.OGE.RequestDelayMilliseconds) * time.Millisecond
		if runner.Extractor == nil {
			extractor, err := NewPDFiumExtractor()
			if err != nil {
				return nil, err
			}
			runner.Extractor = extractor
		}
	}
	if cfg.Senate.Enabled {
		client, err := NewSenateClient(cfg.Senate.BaseURL, timeout)
		if err != nil {
			runner.Close() //nolint:errcheck
			return nil, err
		}
		runner.Senate = client
		runner.Senate.RequestDelay =
			time.Duration(cfg.Senate.RequestDelayMilliseconds) * time.Millisecond
		transport := strings.ToLower(strings.TrimSpace(cfg.Senate.Transport))
		if transport == "" || transport == "browser" {
			browser, browserErr := NewSenateChromeTransport(SenateBrowserOptions{
				BaseURL: cfg.Senate.BaseURL, ProfileDir: cfg.Senate.BrowserProfileDir,
				ExecutablePath: cfg.Senate.BrowserExecutable, Background: true,
				Timeout: timeout,
			})
			if browserErr != nil {
				runner.Close() //nolint:errcheck
				return nil, browserErr
			}
			runner.Senate.Transport = browser
		}
	}
	return runner, nil
}

func (r *Runner) Close() error {
	if r == nil {
		return nil
	}
	var extractorErr, senateErr error
	if r.Extractor != nil {
		extractorErr = r.Extractor.Close()
	}
	if r.Senate != nil {
		senateErr = r.Senate.Close()
	}
	return errors.Join(extractorErr, senateErr)
}

func (r *Runner) Run(ctx context.Context) error {
	defer r.Close() //nolint:errcheck
	if err := r.RunOnce(ctx); err != nil && ctx.Err() == nil {
		slog.Warn("congressional disclosure initial sync failed", "err", err)
	}
	interval := time.Duration(r.Cfg.SyncIntervalHours) * time.Hour
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := r.RunOnce(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("congressional disclosure sync failed", "err", err)
			}
		}
	}
}

func (r *Runner) RunOnce(ctx context.Context) error {
	since, until := scheduledDisclosureRange(r.now(), r.Cfg.LookbackDays)
	budgets, err := distributeSyncBudget(r.Cfg.MaxReportsPerSync, r.House != nil, r.Senate != nil, r.OGE != nil)
	if err != nil {
		return err
	}
	var failures []error
	if r.House != nil {
		processed, retryErr := r.retryHouse(ctx, budgets[0])
		if retryErr != nil {
			failures = append(failures, retryErr)
		}
		if err := ctx.Err(); err != nil {
			return scheduledSyncError(failures, err)
		}
		if remaining := budgets[0] - processed; remaining > 0 {
			if _, syncErr := r.syncHouse(ctx, since, until, remaining); syncErr != nil {
				failures = append(failures, syncErr)
			}
			if err := ctx.Err(); err != nil {
				return scheduledSyncError(failures, err)
			}
		}
	}
	if r.Senate != nil {
		processed, retryErr := r.retrySenate(ctx, budgets[1])
		if retryErr != nil {
			failures = append(failures, retryErr)
		}
		if err := ctx.Err(); err != nil {
			return scheduledSyncError(failures, err)
		}
		if remaining := budgets[1] - processed; remaining > 0 && !errors.Is(retryErr, ErrSenateAuthorizationRequired) {
			if _, syncErr := r.syncSenate(ctx, since, until, remaining); syncErr != nil {
				failures = append(failures, syncErr)
			}
			if err := ctx.Err(); err != nil {
				return scheduledSyncError(failures, err)
			}
		}
	}
	if r.OGE != nil {
		processed, retryErr := r.retryOGE(ctx, budgets[2])
		if retryErr != nil {
			failures = append(failures, retryErr)
		}
		if err := ctx.Err(); err != nil {
			return scheduledSyncError(failures, err)
		}
		if remaining := budgets[2] - processed; remaining > 0 {
			if _, syncErr := r.syncOGE(ctx, since, until, remaining); syncErr != nil {
				failures = append(failures, syncErr)
			}
			if err := ctx.Err(); err != nil {
				return scheduledSyncError(failures, err)
			}
		}
	}
	if len(failures) > 0 {
		return scheduledSyncError(failures)
	}
	return nil
}

func scheduledSyncError(failures []error, extra ...error) error {
	all := append(append([]error(nil), failures...), extra...)
	if len(all) == 0 {
		return nil
	}
	return fmt.Errorf("congressional disclosure scheduled sync: %w", errors.Join(all...))
}

func (r *Runner) claimRetries(ctx context.Context, source string, limit int) ([]store.PoliticalDisclosureReport, error) {
	if limit <= 0 {
		return nil, nil
	}
	now := r.now().UTC()
	if r.claimDueRetries != nil {
		return r.claimDueRetries(ctx, source, now, limit, r.retryLease())
	}
	token, err := randomRetryClaimToken()
	if err != nil {
		return nil, err
	}
	return store.ClaimPoliticalDisclosureRetries(ctx, source, now, limit, r.retryLease(), token)
}

func (r *Runner) retryLease() time.Duration {
	if r.RetryLease > 0 {
		return r.RetryLease
	}
	return politicalDisclosureRetryLease
}

func (r *Runner) heartbeatRetries(ctx context.Context, claims []store.PoliticalDisclosureReport) func() {
	if len(claims) == 0 || claims[0].RetryClaimToken == "" {
		return func() {}
	}
	hbCtx, cancel := context.WithCancel(ctx)
	every := r.retryLease() / 3
	if every <= 0 {
		every = time.Millisecond
	}
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				for _, c := range claims {
					if r.renewRetryLease != nil {
						_, _ = r.renewRetryLease(hbCtx, c.Source, c.ExternalID, c.RetryClaimToken, r.now().UTC(), r.retryLease())
					} else {
						_, _ = store.RenewPoliticalDisclosureRetryLease(hbCtx, c.Source, c.ExternalID, c.RetryClaimToken, r.now().UTC(), r.retryLease())
					}
				}
			}
		}
	}()
	return cancel
}

func randomRetryClaimToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func (r *Runner) saveRetry(ctx context.Context, report Report, token string) error {
	stored := politicalDisclosureStoreReport(report)
	if r.saveRetryResult != nil {
		return r.saveRetryResult(ctx, stored, token)
	}
	if token != "" {
		return store.SavePoliticalDisclosureRetryResult(ctx, stored, token)
	}
	return r.saveReport(ctx, report)
}

func (r *Runner) releaseRetries(ctx context.Context, token string) {
	if token == "" {
		return
	}
	if r.releaseRetryClaims != nil {
		_ = r.releaseRetryClaims(ctx, token)
		return
	}
	_ = store.ReleasePoliticalDisclosureRetryClaims(ctx, token)
}

// releaseRetriesAfterShutdown detaches from the canceled sync context and
// bounds the database call so a dead connection pool cannot stall Runner.Run
// during shutdown.
func (r *Runner) releaseRetriesAfterShutdown(token string) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()
	r.releaseRetries(releaseCtx, token)
}

func retryFailure(report store.PoliticalDisclosureReport, cause error) Report {
	return Report{Source: report.Source, ExternalID: report.ExternalID, Chamber: report.Chamber,
		FilerName: report.FilerName, Office: report.Office, ReportType: report.ReportType,
		IsAmendment: report.IsAmendment, FilingDate: report.FilingDate, DocumentURL: report.DocumentURL,
		ContentHash: report.ContentHash, ExtractionStatus: "failed", ExtractionError: cause.Error(),
		RetryableFailure: shouldRetryDisclosureFailure(cause), RawMetadata: report.RawMetadata}
}

func (r *Runner) retryHouse(ctx context.Context, limit int) (int, error) {
	claims, err := r.claimRetries(ctx, "house", limit)
	if err != nil {
		return 0, err
	}
	if len(claims) > 0 {
		defer r.releaseRetriesAfterShutdown(claims[0].RetryClaimToken)
	}
	stopHeartbeat := r.heartbeatRetries(ctx, claims)
	defer stopHeartbeat()
	var failures []error
	processed := 0
	for _, claim := range claims {
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		processed++
		var filing HouseFiling
		if err := json.Unmarshal(claim.RawMetadata, &filing); err != nil || filing.DocumentID == "" || filing.DocumentID != claim.ExternalID {
			if err == nil {
				err = fmt.Errorf("house retry metadata document id %q does not match claimed external id %q", filing.DocumentID, claim.ExternalID)
			}
			if saveErr := r.saveRetry(ctx, retryFailure(claim, permanentDisclosureError(err)), claim.RetryClaimToken); saveErr != nil {
				return processed, fmt.Errorf("persist failed House retry %s: %w", claim.ExternalID, saveErr)
			}
			failures = append(failures, err)
			continue
		}
		pdf, documentURL, fetchErr := r.House.FetchPDF(ctx, filing)
		if fetchErr == nil {
			if r.Extractor == nil {
				fetchErr = permanentDisclosureError(errors.New("house retry has no PDF extractor"))
			} else {
				pages, extractErr := r.Extractor.Extract(pdf)
				if extractErr == nil {
					if saveErr := r.saveRetry(ctx, ParseHousePTR(filing, documentURL, pdf, pages), claim.RetryClaimToken); saveErr != nil {
						return processed, fmt.Errorf("persist House retry %s: %w", claim.ExternalID, saveErr)
					}
					continue
				}
				if extractErr != nil {
					fetchErr = extractErr
				}
			}
		}
		if fetchErr != nil {
			if saveErr := r.saveRetry(ctx, retryFailure(claim, fetchErr), claim.RetryClaimToken); saveErr != nil {
				return processed, fmt.Errorf("persist failed House retry %s: %w", claim.ExternalID, saveErr)
			}
			failures = append(failures, fetchErr)
		}
	}
	if err := ctx.Err(); err != nil {
		return processed, err
	}
	if len(failures) > 0 {
		return processed, fmt.Errorf("%d due House retry(s) failed: %w", len(failures), errors.Join(failures...))
	}
	return processed, nil
}

func (r *Runner) retrySenate(ctx context.Context, limit int) (int, error) {
	claims, err := r.claimRetries(ctx, "senate", limit)
	if err != nil {
		return 0, err
	}
	if len(claims) > 0 {
		defer r.releaseRetriesAfterShutdown(claims[0].RetryClaimToken)
	}
	stopHeartbeat := r.heartbeatRetries(ctx, claims)
	defer stopHeartbeat()
	var failures []error
	processed := 0
	for _, claim := range claims {
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		processed++
		filing := SenateFiling{ExternalID: claim.ExternalID, FilerName: claim.FilerName, Office: claim.Office, FilingDate: claim.FilingDate, DocumentURL: claim.DocumentURL}
		report, fetchErr := r.Senate.FetchPTR(ctx, filing)
		if errors.Is(fetchErr, ErrSenateAuthorizationRequired) {
			r.releaseRetries(ctx, claim.RetryClaimToken)
			return processed - 1, senateSourceBlocked(fetchErr)
		}
		if fetchErr == nil {
			report.RawMetadata = claim.RawMetadata
			if saveErr := r.saveRetry(ctx, report, claim.RetryClaimToken); saveErr != nil {
				return processed, fmt.Errorf("persist Senate retry %s: %w", claim.ExternalID, saveErr)
			}
			continue
		}
		if fetchErr != nil {
			if saveErr := r.saveRetry(ctx, retryFailure(claim, fetchErr), claim.RetryClaimToken); saveErr != nil {
				return processed, fmt.Errorf("persist failed Senate retry %s: %w", claim.ExternalID, saveErr)
			}
			failures = append(failures, fetchErr)
		}
	}
	if err := ctx.Err(); err != nil {
		return processed, err
	}
	if len(failures) > 0 {
		return processed, fmt.Errorf("%d due Senate retry(s) failed: %w", len(failures), errors.Join(failures...))
	}
	return processed, nil
}

func (r *Runner) retryOGE(ctx context.Context, limit int) (int, error) {
	claims, err := r.claimRetries(ctx, "oge", limit)
	if err != nil {
		return 0, err
	}
	if len(claims) > 0 {
		defer r.releaseRetriesAfterShutdown(claims[0].RetryClaimToken)
	}
	stopHeartbeat := r.heartbeatRetries(ctx, claims)
	defer stopHeartbeat()
	var failures []error
	processed := 0
	for _, claim := range claims {
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		processed++
		filing := OGEFiling{ExternalID: claim.ExternalID, FilerName: claim.FilerName, Office: claim.Office, ReportType: claim.ReportType, IsAmendment: claim.IsAmendment, FilingDate: claim.FilingDate, DocumentURL: claim.DocumentURL, RawMetadata: claim.RawMetadata}
		pdf, fetchErr := r.OGE.FetchPDF(ctx, filing)
		if fetchErr == nil {
			if r.Extractor == nil {
				fetchErr = permanentDisclosureError(errors.New("OGE retry has no PDF extractor"))
			} else {
				pages, extractErr := r.Extractor.Extract(pdf)
				if extractErr == nil {
					if saveErr := r.saveRetry(ctx, ParseOGEPDF(filing, pdf, pages), claim.RetryClaimToken); saveErr != nil {
						return processed, fmt.Errorf("persist OGE retry %s: %w", claim.ExternalID, saveErr)
					}
					continue
				} else {
					fetchErr = extractErr
				}
			}
		}
		if fetchErr != nil {
			if saveErr := r.saveRetry(ctx, retryFailure(claim, fetchErr), claim.RetryClaimToken); saveErr != nil {
				return processed, fmt.Errorf("persist failed OGE retry %s: %w", claim.ExternalID, saveErr)
			}
			failures = append(failures, fetchErr)
		}
	}
	if err := ctx.Err(); err != nil {
		return processed, err
	}
	if len(failures) > 0 {
		return processed, fmt.Errorf("%d due OGE retry(s) failed: %w", len(failures), errors.Join(failures...))
	}
	return processed, nil
}

func scheduledDisclosureRange(now time.Time, lookbackDays int) (time.Time, time.Time) {
	until := disclosureCalendarDay(now)
	return until.AddDate(0, 0, -lookbackDays), until
}

// SyncRange ingests filings whose official filing date falls within the
// inclusive range. It is exported for explicit backfills and operational CLI
// runs; the long-running scheduler uses the same path through RunOnce.
func (r *Runner) SyncRange(
	ctx context.Context, since, until time.Time, limit int,
) (SyncResult, error) {
	var result SyncResult
	if since.After(until) {
		return result, fmt.Errorf("disclosure sync start must not be after end")
	}
	if limit <= 0 {
		return result, fmt.Errorf("disclosure sync limit must be positive")
	}
	budgets, err := distributeSyncBudget(limit,
		r.House != nil, r.Senate != nil, r.OGE != nil,
	)
	if err != nil {
		return result, err
	}
	var failures []error
	if budgets[0] > 0 {
		processed, err := r.syncHouse(ctx, since, until, budgets[0])
		result.HouseProcessed = processed
		if err != nil {
			failures = append(failures, err)
		}
		if err := ctx.Err(); err != nil {
			return result, syncRangeError(failures, err)
		}
	}
	if budgets[1] > 0 {
		processed, err := r.syncSenate(ctx, since, until, budgets[1])
		result.SenateProcessed = processed
		if err != nil {
			failures = append(failures, err)
		}
		if err := ctx.Err(); err != nil {
			return result, syncRangeError(failures, err)
		}
	}
	if budgets[2] > 0 {
		processed, err := r.syncOGE(ctx, since, until, budgets[2])
		result.OGEProcessed = processed
		if err != nil {
			failures = append(failures, err)
		}
		if err := ctx.Err(); err != nil {
			return result, syncRangeError(failures, err)
		}
	}
	if len(failures) > 0 {
		return result, syncRangeError(failures)
	}
	return result, nil
}

func syncRangeError(failures []error, extra ...error) error {
	all := append(append([]error(nil), failures...), extra...)
	if len(all) == 0 {
		return nil
	}
	return fmt.Errorf("congressional disclosure sync: %w", errors.Join(all...))
}

// distributeSyncBudget partitions the cross-source limit so one source cannot
// consume all capacity while another enabled source has pending filings.
func distributeSyncBudget(limit int, enabled ...bool) ([]int, error) {
	budgets := make([]int, len(enabled))
	enabledCount := 0
	for _, isEnabled := range enabled {
		if isEnabled {
			enabledCount++
		}
	}
	if enabledCount == 0 {
		return budgets, nil
	}
	if limit < enabledCount {
		return nil, fmt.Errorf(
			"disclosure sync limit %d is smaller than %d enabled sources",
			limit, enabledCount,
		)
	}
	share, remainder := limit/enabledCount, limit%enabledCount
	for index, isEnabled := range enabled {
		if !isEnabled {
			continue
		}
		budgets[index] = share
		if remainder > 0 {
			budgets[index]++
			remainder--
		}
	}
	return budgets, nil
}

func (r *Runner) syncHouse(
	ctx context.Context, since, now time.Time, limit int,
) (int, error) {
	firstYear, lastYear := since.Year(), now.Year()
	var filings []HouseFiling
	var indexFailures []error
	for year := lastYear; year >= firstYear; year-- {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		rows, err := r.House.FetchIndex(ctx, year)
		if err != nil {
			indexFailures = append(indexFailures, fmt.Errorf("fetch House %d filing index: %w", year, err))
			continue
		}
		for _, filing := range rows {
			if !filing.FilingDate.Before(since) && !filing.FilingDate.After(now) {
				filings = append(filings, filing)
			}
		}
	}
	sort.SliceStable(filings, func(i, j int) bool {
		return filings[i].FilingDate.After(filings[j].FilingDate)
	})
	processed := 0
	var failures []error
	for _, filing := range filings {
		if processed >= limit {
			break
		}
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		terminal, err := r.isReportTerminal(ctx, "house", filing.DocumentID)
		if err != nil {
			return processed, err
		}
		if terminal {
			continue
		}
		processed++
		pdfData, documentURL, err := r.House.FetchPDF(ctx, filing)
		if err != nil {
			if ctx.Err() != nil {
				return processed, ctx.Err()
			}
			failures = append(failures, err)
			if saveErr := r.saveFailure(ctx, filing, documentURL, err); saveErr != nil {
				return processed, saveErr
			}
			continue
		}
		pages, err := r.Extractor.Extract(pdfData)
		if err != nil {
			if ctx.Err() != nil {
				return processed, ctx.Err()
			}
			failures = append(failures, err)
			if saveErr := r.saveFailure(ctx, filing, documentURL, err); saveErr != nil {
				return processed, saveErr
			}
			continue
		}
		report := ParseHousePTR(filing, documentURL, pdfData, pages)
		if err := r.saveReport(ctx, report); err != nil {
			return processed, err
		}
	}
	if err := ctx.Err(); err != nil {
		return processed, err
	}
	if len(indexFailures) > 0 {
		failures = append(failures, indexFailures...)
	}
	if len(failures) > 0 {
		return processed, fmt.Errorf("%d House filing(s) failed: %w", len(failures), errors.Join(failures...))
	}
	return processed, nil
}

func (r *Runner) saveFailure(
	ctx context.Context, filing HouseFiling, documentURL string, cause error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	metadata, _ := json.Marshal(filing)
	report := Report{
		Source: "house", ExternalID: filing.DocumentID, Chamber: "house",
		FilerName: filing.FilerName(), Office: filing.StateDistrict,
		ReportType: "periodic_transaction", FilingDate: filing.FilingDate,
		DocumentURL: documentURL, ExtractionStatus: "failed",
		ExtractionError: cause.Error(), RetryableFailure: shouldRetryDisclosureFailure(cause), RawMetadata: metadata,
	}
	if err := r.saveReport(ctx, report); err != nil {
		return fmt.Errorf("persist failed House disclosure extraction %s: %w", filing.DocumentID, err)
	}
	return nil
}

func (r *Runner) syncSenate(
	ctx context.Context, since, now time.Time, limit int,
) (int, error) {
	processed := 0
	var failures []error
	start, pageSize := 0, 100
	for processed < limit {
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		page, err := r.Senate.ListPTRPage(ctx, since, now, start, pageSize)
		if err != nil {
			return processed, senateSourceBlocked(err)
		}
		for _, filing := range page.Filings {
			if processed >= limit {
				break
			}
			if err := ctx.Err(); err != nil {
				return processed, err
			}
			terminal, err := r.isReportTerminal(ctx, "senate", filing.ExternalID)
			if err != nil {
				return processed, err
			}
			if terminal {
				continue
			}
			processed++
			report, err := r.Senate.FetchPTR(ctx, filing)
			if err != nil {
				if ctx.Err() != nil {
					return processed, ctx.Err()
				}
				if errors.Is(err, ErrSenateAuthorizationRequired) {
					return processed - 1, senateSourceBlocked(err)
				}
				failures = append(failures, err)
				metadata, _ := json.Marshal(filing)
				failed := Report{
					Source: "senate", ExternalID: filing.ExternalID, Chamber: "senate",
					FilerName: filing.FilerName, Office: filing.Office,
					ReportType: "periodic_transaction", FilingDate: filing.FilingDate,
					DocumentURL: filing.DocumentURL, ExtractionStatus: "failed",
					ExtractionError: err.Error(), RetryableFailure: shouldRetryDisclosureFailure(err),
					RawMetadata: metadata,
				}
				if saveErr := r.saveReport(ctx, failed); saveErr != nil {
					return processed, saveErr
				}
				continue
			}
			metadata, _ := json.Marshal(filing)
			report.RawMetadata = metadata
			if err := r.saveReport(ctx, report); err != nil {
				return processed, err
			}
		}
		start += page.RowCount
		if page.RowCount == 0 || (page.Total > 0 && start >= page.Total) {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return processed, err
	}
	if len(failures) > 0 {
		return processed, fmt.Errorf("%d Senate filing(s) failed: %w", len(failures), errors.Join(failures...))
	}
	return processed, nil
}

func (r *Runner) syncOGE(ctx context.Context, since, until time.Time, limit int) (int, error) {
	processed := 0
	var failures []error
	start, pageSize := 0, 100
	for processed < limit {
		if err := ctx.Err(); err != nil {
			return processed, err
		}
		page, err := r.OGE.ListFilingsPage(ctx, since, until, start, pageSize)
		if err != nil {
			return processed, err
		}
		for _, filing := range page.Filings {
			if processed >= limit {
				break
			}
			if err := ctx.Err(); err != nil {
				return processed, err
			}
			terminal, err := r.isReportTerminal(ctx, "oge", filing.ExternalID)
			if err != nil {
				return processed, err
			}
			if terminal {
				continue
			}
			processed++
			pdf, err := r.OGE.FetchPDF(ctx, filing)
			if err != nil {
				if ctx.Err() != nil {
					return processed, ctx.Err()
				}
				failures = append(failures, err)
				if saveErr := r.saveReport(ctx, Report{Source: "oge", ExternalID: filing.ExternalID, Chamber: "executive", FilerName: filing.FilerName, Office: filing.Office, ReportType: filing.ReportType, IsAmendment: filing.IsAmendment, FilingDate: filing.FilingDate, DocumentURL: filing.DocumentURL, ExtractionStatus: "failed", ExtractionError: err.Error(), RetryableFailure: shouldRetryDisclosureFailure(err), RawMetadata: filing.RawMetadata}); saveErr != nil {
					return processed, saveErr
				}
				continue
			}
			pages, err := r.Extractor.Extract(pdf)
			if err != nil {
				if ctx.Err() != nil {
					return processed, ctx.Err()
				}
				failures = append(failures, err)
				if saveErr := r.saveReport(ctx, Report{Source: "oge", ExternalID: filing.ExternalID, Chamber: "executive", FilerName: filing.FilerName, Office: filing.Office, ReportType: filing.ReportType, IsAmendment: filing.IsAmendment, FilingDate: filing.FilingDate, DocumentURL: filing.DocumentURL, ContentHash: hashBytes(pdf), ExtractionStatus: "failed", ExtractionError: err.Error(), RetryableFailure: true, RawMetadata: filing.RawMetadata}); saveErr != nil {
					return processed, saveErr
				}
				continue
			}
			if err := r.saveReport(ctx, ParseOGEPDF(filing, pdf, pages)); err != nil {
				return processed, err
			}
		}
		start += page.RowCount
		if page.RowCount == 0 || start >= page.Total {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return processed, err
	}
	if len(failures) > 0 {
		return processed, fmt.Errorf("%d OGE filing(s) failed: %w", len(failures), errors.Join(failures...))
	}
	return processed, nil
}

func (r *Runner) isReportTerminal(
	ctx context.Context, source, externalID string,
) (bool, error) {
	if r.reportTerminal != nil {
		return r.reportTerminal(ctx, source, externalID)
	}
	return store.PoliticalDisclosureReportTerminal(ctx, source, externalID)
}

func (r *Runner) saveReport(ctx context.Context, report Report) error {
	if r.persistReport != nil {
		return r.persistReport(ctx, report)
	}
	return store.SavePoliticalDisclosureFirstPass(ctx, politicalDisclosureStoreReport(report))
}

// politicalDisclosureStoreReport is the sole conversion boundary between
// parsed-source reports and their durable representation. Retry completion
// must persist exactly the same transaction evidence as first-pass ingestion.
func politicalDisclosureStoreReport(report Report) store.PoliticalDisclosureReport {
	stored := store.PoliticalDisclosureReport{
		Source: report.Source, ExternalID: report.ExternalID,
		Chamber: report.Chamber, FilerName: report.FilerName, Office: report.Office,
		ReportType: report.ReportType, IsAmendment: report.IsAmendment, FilingDate: report.FilingDate,
		DocumentURL: report.DocumentURL, ContentHash: report.ContentHash,
		ExtractionStatus: report.ExtractionStatus,
		ExtractionError:  report.ExtractionError,
		RetryableFailure: report.RetryableFailure, RawMetadata: report.RawMetadata,
		Transactions: make([]store.PoliticalDisclosureTransaction, 0, len(report.Transactions)),
	}
	for _, item := range report.Transactions {
		stored.Transactions = append(stored.Transactions, store.PoliticalDisclosureTransaction{
			Ordinal: item.Ordinal, Owner: item.Owner, AssetName: item.AssetName,
			Ticker: item.Ticker, AssetType: item.AssetType,
			TransactionType:  item.TransactionType,
			TransactionDate:  item.TransactionDate,
			NotificationDate: item.NotificationDate,
			AmountMin:        item.AmountMin, AmountMax: item.AmountMax,
			AmountText: item.AmountText, Comment: item.Comment, RawData: item.RawData,
		})
	}
	return stored
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
