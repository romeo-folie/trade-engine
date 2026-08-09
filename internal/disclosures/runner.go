package disclosures

import (
	"context"
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
	Cfg       config.DisclosuresConfig
	House     *HouseClient
	Senate    *SenateClient
	OGE       *OGEClient
	Extractor PDFExtractor
	Now       func() time.Time
}

type SyncResult struct {
	HouseProcessed  int
	SenateProcessed int
	OGEProcessed    int
}

func NewRunner(cfg config.DisclosuresConfig) (*Runner, error) {
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
	now := r.now()
	since := now.AddDate(0, 0, -r.Cfg.LookbackDays)
	_, err := r.SyncRange(ctx, since, now, r.Cfg.MaxReportsPerSync)
	return err
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
	remaining := limit
	var failures []error
	if r.House != nil && remaining > 0 {
		processed, err := r.syncHouse(ctx, since, until, remaining)
		result.HouseProcessed = processed
		remaining -= processed
		if err != nil {
			failures = append(failures, err)
		}
	}
	if r.Senate != nil && remaining > 0 {
		processed, err := r.syncSenate(ctx, since, until, remaining)
		result.SenateProcessed = processed
		remaining -= processed
		if err != nil {
			failures = append(failures, err)
		}
	}
	if r.OGE != nil && remaining > 0 {
		processed, err := r.syncOGE(ctx, since, until, remaining)
		result.OGEProcessed = processed
		remaining -= processed
		if err != nil {
			failures = append(failures, err)
		}
	}
	if len(failures) > 0 {
		return result, fmt.Errorf("congressional disclosure sync: %v", failures)
	}
	return result, nil
}

func (r *Runner) syncHouse(
	ctx context.Context, since, now time.Time, limit int,
) (int, error) {
	firstYear, lastYear := since.Year(), now.Year()
	var filings []HouseFiling
	for year := lastYear; year >= firstYear; year-- {
		rows, err := r.House.FetchIndex(ctx, year)
		if err != nil {
			return 0, fmt.Errorf("fetch House %d filing index: %w", year, err)
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
		if processed >= limit || ctx.Err() != nil {
			break
		}
		completed, err := store.PoliticalDisclosureReportCompleted(ctx, "house", filing.DocumentID)
		if err != nil {
			return processed, err
		}
		if completed {
			continue
		}
		processed++
		pdfData, documentURL, err := r.House.FetchPDF(ctx, filing)
		if err != nil {
			failures = append(failures, err)
			r.saveFailure(ctx, filing, documentURL, err)
			continue
		}
		pages, err := r.Extractor.Extract(pdfData)
		if err != nil {
			failures = append(failures, err)
			r.saveFailure(ctx, filing, documentURL, err)
			continue
		}
		report := ParseHousePTR(filing, documentURL, pdfData, pages)
		if err := saveReport(ctx, report); err != nil {
			return processed, err
		}
	}
	if len(failures) > 0 {
		return processed, fmt.Errorf("%d House filing(s) failed", len(failures))
	}
	return processed, nil
}

func (r *Runner) saveFailure(
	ctx context.Context, filing HouseFiling, documentURL string, cause error,
) {
	metadata, _ := json.Marshal(filing)
	report := Report{
		Source: "house", ExternalID: filing.DocumentID, Chamber: "house",
		FilerName: filing.FilerName(), Office: filing.StateDistrict,
		ReportType: "periodic_transaction", FilingDate: filing.FilingDate,
		DocumentURL: documentURL, ExtractionStatus: "failed",
		ExtractionError: cause.Error(), RawMetadata: metadata,
	}
	if err := saveReport(ctx, report); err != nil {
		slog.Warn("persist failed House disclosure extraction",
			"document_id", filing.DocumentID, "err", err)
	}
}

func (r *Runner) syncSenate(
	ctx context.Context, since, now time.Time, limit int,
) (int, error) {
	processed := 0
	var failures int
	start, pageSize := 0, 100
	for processed < limit && ctx.Err() == nil {
		page, err := r.Senate.ListPTRPage(ctx, since, now, start, pageSize)
		if err != nil {
			return processed, err
		}
		for _, filing := range page.Filings {
			if processed >= limit || ctx.Err() != nil {
				break
			}
			completed, err := store.PoliticalDisclosureReportCompleted(ctx, "senate", filing.ExternalID)
			if err != nil {
				return processed, err
			}
			if completed {
				continue
			}
			processed++
			report, err := r.Senate.FetchPTR(ctx, filing)
			if err != nil {
				failures++
				failed := Report{
					Source: "senate", ExternalID: filing.ExternalID, Chamber: "senate",
					FilerName: filing.FilerName, Office: filing.Office,
					ReportType: "periodic_transaction", FilingDate: filing.FilingDate,
					DocumentURL: filing.DocumentURL, ExtractionStatus: "failed",
					ExtractionError: err.Error(),
				}
				if saveErr := saveReport(ctx, failed); saveErr != nil {
					return processed, saveErr
				}
				continue
			}
			if err := saveReport(ctx, report); err != nil {
				return processed, err
			}
		}
		start += page.RowCount
		if page.RowCount == 0 || (page.Total > 0 && start >= page.Total) {
			break
		}
	}
	if failures > 0 {
		return processed, fmt.Errorf("%d Senate filing(s) failed", failures)
	}
	return processed, nil
}

func (r *Runner) syncOGE(ctx context.Context, since, until time.Time, limit int) (int, error) {
	processed, failures := 0, 0
	start, pageSize := 0, 100
	for processed < limit && ctx.Err() == nil {
		page, err := r.OGE.ListFilingsPage(ctx, since, until, start, pageSize)
		if err != nil {
			return processed, err
		}
		for _, filing := range page.Filings {
			if processed >= limit || ctx.Err() != nil {
				break
			}
			completed, err := store.PoliticalDisclosureReportCompleted(ctx, "oge", filing.ExternalID)
			if err != nil {
				return processed, err
			}
			if completed {
				continue
			}
			processed++
			pdf, err := r.OGE.FetchPDF(ctx, filing)
			if err != nil {
				failures++
				if saveErr := saveReport(ctx, Report{Source: "oge", ExternalID: filing.ExternalID, Chamber: "executive", FilerName: filing.FilerName, Office: filing.Office, ReportType: filing.ReportType, FilingDate: filing.FilingDate, DocumentURL: filing.DocumentURL, ExtractionStatus: "failed", ExtractionError: err.Error(), RawMetadata: filing.RawMetadata}); saveErr != nil {
					return processed, saveErr
				}
				continue
			}
			pages, err := r.Extractor.Extract(pdf)
			if err != nil {
				failures++
				if saveErr := saveReport(ctx, Report{Source: "oge", ExternalID: filing.ExternalID, Chamber: "executive", FilerName: filing.FilerName, Office: filing.Office, ReportType: filing.ReportType, FilingDate: filing.FilingDate, DocumentURL: filing.DocumentURL, ContentHash: hashBytes(pdf), ExtractionStatus: "failed", ExtractionError: err.Error(), RawMetadata: filing.RawMetadata}); saveErr != nil {
					return processed, saveErr
				}
				continue
			}
			if err := saveReport(ctx, ParseOGEPDF(filing, pdf, pages)); err != nil {
				return processed, err
			}
		}
		start += page.RowCount
		if page.RowCount == 0 || start >= page.Total {
			break
		}
	}
	if failures > 0 {
		return processed, fmt.Errorf("%d OGE filing(s) failed", failures)
	}
	return processed, nil
}

func saveReport(ctx context.Context, report Report) error {
	stored := store.PoliticalDisclosureReport{
		Source: report.Source, ExternalID: report.ExternalID,
		Chamber: report.Chamber, FilerName: report.FilerName, Office: report.Office,
		ReportType: report.ReportType, FilingDate: report.FilingDate,
		DocumentURL: report.DocumentURL, ContentHash: report.ContentHash,
		ExtractionStatus: report.ExtractionStatus,
		ExtractionError:  report.ExtractionError, RawMetadata: report.RawMetadata,
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
	return store.SavePoliticalDisclosureReport(ctx, stored)
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}
