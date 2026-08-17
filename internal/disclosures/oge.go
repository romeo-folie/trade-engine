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
	IsAmendment                                            bool
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

const (
	maxOGEIndexSize = 10 << 20
	maxOGEPDFSize   = 25 << 20
)

func NewOGEClient(base string, timeout time.Duration) *OGEClient {
	return &OGEClient{BaseURL: strings.TrimRight(base, "/"), HTTP: &http.Client{Timeout: timeout}}
}

type OGEFilingPage struct {
	Filings         []OGEFiling
	RowCount, Total int
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
		if page.RowCount == 0 || start >= page.Total {
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
	if resp.StatusCode/100 != 2 {
		return OGEFilingPage{}, fmt.Errorf("OGE API returned %s", resp.Status)
	}
	if resp.ContentLength > maxOGEIndexSize {
		return OGEFilingPage{}, fmt.Errorf("OGE API response exceeds %d-byte limit", maxOGEIndexSize)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxOGEIndexSize+1))
	if err != nil {
		return OGEFilingPage{}, err
	}
	if len(b) > maxOGEIndexSize {
		return OGEFilingPage{}, fmt.Errorf("OGE API response exceeds %d-byte limit", maxOGEIndexSize)
	}
	var raw ogePage
	if err := json.Unmarshal(b, &raw); err != nil {
		return OGEFilingPage{}, err
	}
	if raw.RecordsTotal < 0 || (raw.RecordsTotal > 0 && start > raw.RecordsTotal) {
		return OGEFilingPage{}, fmt.Errorf("OGE API returned inconsistent recordsTotal %d for start %d", raw.RecordsTotal, start)
	}
	out := make([]OGEFiling, 0, len(raw.Data))
	for index, row := range raw.Data {
		link := ogePDF.FindString(html.UnescapeString(row.Type))
		if link == "" {
			label := strings.ToLower(stripHTML(html.UnescapeString(row.Type)))
			if strings.Contains(label, "request") || strings.Contains(label, "not available") {
				continue
			}
			return OGEFilingPage{}, fmt.Errorf("OGE filing row %d has no downloadable PDF", index+1)
		}
		document, err := trustedDocumentURL(c.BaseURL, link, ogeDocumentPathPrefix(c.BaseURL))
		if err != nil || !strings.HasSuffix(strings.ToLower(document.Path), ".pdf") {
			return OGEFilingPage{}, fmt.Errorf("OGE filing row %d has an untrusted PDF URL", index+1)
		}
		name := strings.TrimSpace(html.UnescapeString(row.Name))
		if name == "" {
			return OGEFilingPage{}, fmt.Errorf("OGE filing row %d has no filer name", index+1)
		}
		d, ok := parseOGEFilingDate(row.DocDate)
		if !ok {
			return OGEFilingPage{}, fmt.Errorf("OGE filing row %d has invalid filing date", index+1)
		}
		filingDay := disclosureCalendarDay(d)
		if filingDay.Before(disclosureCalendarDay(since)) {
			continue
		}
		if filingDay.After(disclosureCalendarDay(until)) {
			continue
		}
		documentURL := document.String()
		sum := sha256.Sum256([]byte(documentURL))
		meta, _ := json.Marshal(row)
		isAmendment := ogeLabelIsAmendment(strings.ToLower(stripHTML(html.UnescapeString(row.Type)))) || ogeAmendedFieldIsAmendment(row.Amended)
		if len(meta) == 0 {
			return OGEFilingPage{}, fmt.Errorf("OGE filing row %d metadata is invalid", index+1)
		}
		out = append(out, OGEFiling{ExternalID: "oge-" + hex.EncodeToString(sum[:8]), FilerName: name, Office: strings.Trim(strings.Join([]string{row.Title, row.Agency}, " — "), " —"), ReportType: normalizeOGEReportType(row.Type, row.Amended), IsAmendment: isAmendment, FilingDate: d, DocumentURL: documentURL, RawMetadata: meta})
	}
	return OGEFilingPage{Filings: out, RowCount: len(raw.Data), Total: raw.RecordsTotal}, nil
}

func parseOGEFilingDate(value string) (time.Time, bool) {
	for _, layout := range []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05",
		"2006-01-02",
	} {
		parsed, err := time.Parse(layout, strings.TrimSpace(value))
		if err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func (c *OGEClient) FetchPDF(ctx context.Context, f OGEFiling) ([]byte, error) {
	if err := c.pacer.Wait(ctx, c.RequestDelay); err != nil {
		return nil, err
	}
	document, err := trustedDocumentURL(c.BaseURL, f.DocumentURL, ogeDocumentPathPrefix(c.BaseURL))
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, document.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := sameOriginDocumentClient(c.HTTP, c.BaseURL, ogeDocumentPathPrefix(c.BaseURL)).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, disclosureHTTPStatusError("OGE", resp.StatusCode, resp.Status)
	}
	if resp.ContentLength > maxOGEPDFSize {
		return nil, fmt.Errorf("OGE PDF exceeds %d-byte limit", maxOGEPDFSize)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxOGEPDFSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxOGEPDFSize {
		return nil, fmt.Errorf("OGE PDF exceeds %d-byte limit", maxOGEPDFSize)
	}
	return b, nil
}
func normalizeOGEReportType(v, amended string) string {
	label := strings.ToLower(strings.TrimSpace(stripHTML(html.UnescapeString(v))))

	var base string
	switch {
	case strings.Contains(label, "transaction"):
		base = "periodic_transaction"
	case strings.Contains(label, "annual"):
		base = "annual"
	case strings.Contains(label, "termination"):
		base = "termination"
	default:
		base = strings.Trim(strings.ReplaceAll(label, "amended", ""), " -_()")
	}
	return base
}

func ogeLabelIsAmendment(label string) bool {
	return strings.Contains(label, "amend") || strings.Contains(label, "correct")
}

func ogeAmendedFieldIsAmendment(value string) bool {
	v := strings.ToLower(strings.TrimSpace(stripHTML(html.UnescapeString(value))))
	switch v {
	case "", "0", "false", "f", "no", "n", "none", "null", "nil", "not amended", "n/a", "na", "not applicable":
		return false
	}
	return true
}

func ParseOGEPDF(f OGEFiling, pdf []byte, pages []PageText) Report {
	r := Report{Source: "oge", ExternalID: f.ExternalID, Chamber: "executive", FilerName: f.FilerName, Office: f.Office, ReportType: f.ReportType, IsAmendment: f.IsAmendment, FilingDate: f.FilingDate, DocumentURL: f.DocumentURL, RawMetadata: f.RawMetadata, ContentHash: hashBytes(pdf), ExtractionStatus: "no_text"}
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
