package disclosures

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

type OGEFiling struct {
	ExternalID, FilerName, Office, ReportType, DocumentURL string
	FilingDate                                             time.Time
	RawMetadata                                            []byte
}
type OGEClient struct {
	BaseURL      string
	HTTP         *http.Client
	RequestDelay time.Duration
	pacer        requestPacer
}
type ogePage struct {
	RecordsTotal int      `json:"recordsTotal"`
	Data         []ogeRow `json:"data"`
}
type ogeRow struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Agency  string `json:"agency"`
	Title   string `json:"title"`
	DocDate string `json:"docDate"`
	Amended string `json:"amended"`
}

var ogePDF = regexp.MustCompile(`(?i)https?://[^'"<>\s]+\.pdf`)
var ogeDate = regexp.MustCompile(`\b\d{1,2}/\d{1,2}/\d{4}\b`)

func NewOGEClient(base string, timeout time.Duration) *OGEClient {
	return &OGEClient{BaseURL: strings.TrimRight(base, "/"), HTTP: &http.Client{Timeout: timeout}}
}

type OGEFilingPage struct {
	Filings         []OGEFiling
	RowCount, Total int
	BeforeRange     bool
}

func (c *OGEClient) ListFilings(ctx context.Context, since, until time.Time, limit int) ([]OGEFiling, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("OGE filing limit must be positive")
	}
	out := make([]OGEFiling, 0)
	start := 0
	for len(out) < limit {
		page, err := c.ListFilingsPage(ctx, since, until, start, 100)
		if err != nil {
			return out, err
		}
		for _, filing := range page.Filings {
			if len(out) >= limit {
				break
			}
			out = append(out, filing)
		}
		start += page.RowCount
		if page.RowCount == 0 || page.BeforeRange || start >= page.Total {
			break
		}
	}
	return out, nil
}

func (c *OGEClient) ListFilingsPage(ctx context.Context, since, until time.Time, start, length int) (OGEFilingPage, error) {
	if err := c.pacer.Wait(ctx, c.RequestDelay); err != nil {
		return OGEFilingPage{}, err
	}
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return OGEFilingPage{}, err
	}
	q := u.Query()
	q.Set("draw", "1")
	q.Set("start", fmt.Sprint(start))
	q.Set("length", fmt.Sprint(length))
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return OGEFilingPage{}, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return OGEFilingPage{}, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return OGEFilingPage{}, err
	}
	if resp.StatusCode/100 != 2 {
		return OGEFilingPage{}, fmt.Errorf("OGE API returned %s", resp.Status)
	}
	var raw ogePage
	if err := json.Unmarshal(b, &raw); err != nil {
		return OGEFilingPage{}, err
	}
	out := make([]OGEFiling, 0, len(raw.Data))
	beforeRange := false
	for _, row := range raw.Data {
		link := ogePDF.FindString(html.UnescapeString(row.Type))
		if link == "" {
			continue
		}
		d, _ := time.Parse("2006-01-02T15:04:05", row.DocDate)
		if d.IsZero() {
			d, _ = time.Parse("2006-01-02", row.DocDate)
		}
		if d.Before(since) {
			beforeRange = true
			continue
		}
		if d.After(until) {
			continue
		}
		sum := sha256.Sum256([]byte(link))
		meta, _ := json.Marshal(row)
		out = append(out, OGEFiling{ExternalID: "oge-" + hex.EncodeToString(sum[:8]), FilerName: html.UnescapeString(row.Name), Office: strings.Trim(strings.Join([]string{row.Title, row.Agency}, " — "), " —"), ReportType: normalizeOGEReportType(row.Type), FilingDate: d, DocumentURL: link, RawMetadata: meta})
	}
	return OGEFilingPage{Filings: out, RowCount: len(raw.Data), Total: raw.RecordsTotal, BeforeRange: beforeRange}, nil
}
func (c *OGEClient) FetchPDF(ctx context.Context, f OGEFiling) ([]byte, error) {
	const maxPDFSize = 25 << 20
	if err := c.pacer.Wait(ctx, c.RequestDelay); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.DocumentURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("OGE PDF returned %s", resp.Status)
	}
	if resp.ContentLength > maxPDFSize {
		return nil, fmt.Errorf("OGE PDF exceeds %d-byte limit", maxPDFSize)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxPDFSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxPDFSize {
		return nil, fmt.Errorf("OGE PDF exceeds %d-byte limit", maxPDFSize)
	}
	return b, nil
}
func normalizeOGEReportType(v string) string {
	v = stripHTML(html.UnescapeString(v))
	v = strings.ToLower(strings.TrimSpace(v))
	switch {
	case strings.Contains(v, "transaction"):
		return "periodic_transaction"
	case strings.Contains(v, "annual"):
		return "annual"
	case strings.Contains(v, "termination"):
		return "termination"
	default:
		return v
	}
}
func ParseOGEPDF(f OGEFiling, pdf []byte, pages []PageText) Report {
	r := Report{Source: "oge", ExternalID: f.ExternalID, Chamber: "executive", FilerName: f.FilerName, Office: f.Office, ReportType: f.ReportType, FilingDate: f.FilingDate, DocumentURL: f.DocumentURL, RawMetadata: f.RawMetadata, ContentHash: hashBytes(pdf), ExtractionStatus: "no_text"}
	for _, p := range pages {
		lines := ogeLines(p)
		for _, text := range lines {
			low := strings.ToLower(text)
			if !strings.Contains(low, "purchase") && !strings.Contains(low, "sale") && !strings.Contains(low, "exchange") {
				continue
			}
			match := ogeDate.FindString(text)
			if match == "" {
				continue
			}
			date, err := parseDate(match)
			if err != nil {
				continue
			}
			typ := normalizeTransactionType(firstType(low))
			amountMin, amountMax := parseAmount(text)
			asset := strings.TrimSpace(ogeDate.ReplaceAllString(text, ""))
			asset = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(asset, "Purchase", ""), "Sale", ""))
			asset = strings.TrimSpace(strings.ReplaceAll(asset, "Exchange", ""))
			raw, _ := json.Marshal(map[string]any{"page": p.Number, "line": text})
			r.Transactions = append(r.Transactions, Transaction{Ordinal: len(r.Transactions) + 1, AssetName: asset, Ticker: tickerFromAsset(asset), TransactionType: typ, TransactionDate: date, AmountMin: amountMin, AmountMax: amountMax, AmountText: strings.TrimSpace(text), RawData: raw})
		}
	}
	if len(r.Transactions) > 0 {
		r.ExtractionStatus = "completed"
	} else {
		r.ExtractionError = "no OGE transaction rows were reconstructed"
	}
	return r
}

func ogeLines(p PageText) []string {
	boxes := append([]TextBox(nil), p.Boxes...)
	sort.SliceStable(boxes, func(i, j int) bool {
		if boxes[i].Top != boxes[j].Top {
			return boxes[i].Top < boxes[j].Top
		}
		return boxes[i].Left < boxes[j].Left
	})
	var lines []string
	for i := 0; i < len(boxes); {
		top := boxes[i].Top
		var parts []string
		for i < len(boxes) && boxes[i].Top-top <= 3 {
			parts = append(parts, boxes[i].Text)
			i++
		}
		lines = append(lines, strings.Join(parts, " "))
	}
	return lines
}
func firstType(s string) string {
	for _, v := range []string{"purchase", "sale", "exchange"} {
		if strings.Contains(s, v) {
			return v
		}
	}
	return ""
}
func hashBytes(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
