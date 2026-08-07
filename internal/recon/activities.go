package recon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// activityPageSize is Alpaca's maximum. Pagination is by page_token carrying the
// last id seen, NOT by offset — an offset-based loop silently repeats rows.
const activityPageSize = 100

// maxActivityPages bounds the walk so a pagination bug cannot spin forever.
// At 100/page this covers 50k fills, far beyond any paper account's history.
const maxActivityPages = 500

// alpacaActivity is the subset of Alpaca's FILL activity we consume.
type alpacaActivity struct {
	ID              string `json:"id"`
	ActivityType    string `json:"activity_type"`
	Symbol          string `json:"symbol"`
	Side            string `json:"side"`
	Qty             string `json:"qty"`
	Price           string `json:"price"`
	TransactionTime string `json:"transaction_time"`
}

// ActivityFetcher pulls fill history from Alpaca's account activities API.
type ActivityFetcher struct {
	BaseURL string // e.g. https://paper-api.alpaca.markets
	APIKey  string
	Secret  string
	Client  *http.Client
}

// FetchFills returns every FILL activity in [after, until), oldest first.
//
// Only FILL is requested. The other types this account produces (FEE, and the
// single JNLC that funded it) move cash, not shares, so they cannot affect a
// share-based P&L walk. Corporate actions WOULD matter — a split or merger
// changes share count with no fill behind it — so if any ever appear, this
// walk's assumptions need revisiting rather than silently absorbing them.
func (f ActivityFetcher) FetchFills(ctx context.Context, after, until time.Time) ([]Fill, error) {
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	var out []Fill
	var pageToken string
	for page := range maxActivityPages {
		q := url.Values{}
		q.Set("after", after.UTC().Format(time.RFC3339))
		q.Set("until", until.UTC().Format(time.RFC3339))
		q.Set("direction", "asc")
		q.Set("page_size", strconv.Itoa(activityPageSize))
		if pageToken != "" {
			q.Set("page_token", pageToken)
		}

		endpoint := f.BaseURL + "/v2/account/activities/FILL?" + q.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("build activities request: %w", err)
		}
		req.Header.Set("APCA-API-KEY-ID", f.APIKey)
		req.Header.Set("APCA-API-SECRET-KEY", f.Secret)

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("activities page %d: %w", page, err)
		}
		var batch []alpacaActivity
		decErr := json.NewDecoder(resp.Body).Decode(&batch)
		status := resp.StatusCode
		resp.Body.Close()
		if status != http.StatusOK {
			return nil, fmt.Errorf("activities page %d: HTTP %d", page, status)
		}
		if decErr != nil {
			return nil, fmt.Errorf("decode activities page %d: %w", page, decErr)
		}
		if len(batch) == 0 {
			break
		}

		for _, a := range batch {
			if a.ActivityType != "FILL" || a.Symbol == "" {
				continue
			}
			qty, err := strconv.ParseFloat(a.Qty, 64)
			if err != nil {
				return nil, fmt.Errorf("activity %s: bad qty %q: %w", a.ID, a.Qty, err)
			}
			price, err := strconv.ParseFloat(a.Price, 64)
			if err != nil {
				return nil, fmt.Errorf("activity %s: bad price %q: %w", a.ID, a.Price, err)
			}
			ts, err := time.Parse(time.RFC3339, a.TransactionTime)
			if err != nil {
				return nil, fmt.Errorf("activity %s: bad transaction_time %q: %w", a.ID, a.TransactionTime, err)
			}
			out = append(out, Fill{
				ID: a.ID, Symbol: a.Symbol, Side: a.Side,
				Qty: qty, Price: price, Time: ts,
			})
		}

		if len(batch) < activityPageSize {
			break
		}
		pageToken = batch[len(batch)-1].ID
	}
	return out, nil
}
