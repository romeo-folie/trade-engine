package knowledge

import (
	"context"
	"strings"

	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/store"
)

// CompanyProfileProvider supplies a company's full name for a ticker.
// Returning ("", nil) means the provider has no profile for the symbol
// (e.g. some ETFs) — that is a skip, not a failure. *data.FinnhubClient
// satisfies this.
type CompanyProfileProvider interface {
	FetchCompanyProfile(ctx context.Context, ticker string) (string, error)
}

// BackfillCompanyNames upserts a company entity for every active-watchlist or
// recent-analysis ticker that has no entities row, so UI surfaces (analysis
// cards, modal) can show the organization's full name instead of falling back
// to the model string. The news enricher only creates entities for tickers it
// has processed articles about, which leaves gaps this sweep closes.
//
// One-shot and idempotent: safe to run at every boot in a goroutine. The
// provider's own throttle paces the external calls. Returns how many entities
// were created.
func BackfillCompanyNames(ctx context.Context, provider CompanyProfileProvider) (int, error) {
	if !store.IsConnected() {
		logger.Warn("company-name backfill: DB not connected, skipping")
		return 0, nil
	}

	tickers, err := store.GetTickersMissingEntity(ctx, 30)
	if err != nil {
		return 0, err
	}
	if len(tickers) == 0 {
		return 0, nil
	}

	added := 0
	for _, tk := range tickers {
		name, err := provider.FetchCompanyProfile(ctx, tk)
		if err != nil {
			if ctx.Err() != nil {
				return added, ctx.Err()
			}
			logger.Warn("company-name backfill: profile fetch failed", "ticker", tk, "err", err.Error())
			continue
		}
		if name == "" {
			logger.Info("company-name backfill: no profile for ticker, skipping", "ticker", tk)
			continue
		}
		if _, err := store.UpsertEntity(ctx, store.Entity{
			EntityType: "company",
			Name:       name,
			Ticker:     strings.ToUpper(tk),
		}); err != nil {
			logger.Warn("company-name backfill: upsert failed", "ticker", tk, "err", err.Error())
			continue
		}
		added++
	}
	logger.Info("company-name backfill complete", "missing", len(tickers), "added", added)
	return added, nil
}
