package disclosures

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"math"
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

type ogeAction struct {
	box             ogePositionedBox
	transactionType string
}

type ogePositionedBox struct {
	TextBox
	index int
}

type ogeTransactionRow struct {
	boxes   []ogePositionedBox
	date    ogePositionedBox
	actions []ogeAction
}

type ogeColumnBands struct {
	date, action, asset, amount             float64
	hasDate, hasAction, hasAsset, hasAmount bool
}

func ParseOGEPDF(f OGEFiling, pdf []byte, pages []PageText) Report {
	r := Report{Source: "oge", ExternalID: f.ExternalID, Chamber: "executive", FilerName: f.FilerName, Office: f.Office, ReportType: f.ReportType, IsAmendment: f.IsAmendment, FilingDate: f.FilingDate, DocumentURL: f.DocumentURL, RawMetadata: f.RawMetadata, ContentHash: hashBytes(pdf), ExtractionStatus: "no_text"}
	for _, p := range pages {
		bands := ogeHeaderBands(p)
		for _, row := range ogeRows(p, bands) {
			dateText := ogeDate.FindString(row.date.Text)
			date, err := parseDate(dateText)
			if err != nil {
				continue
			}
			action := ogeTransactionType(row)
			if action != "purchase" && action != "sale" && action != "exchange" {
				continue
			}
			amountText := ogeRowAmountText(row, bands)
			amountMin, amountMax := parseAmount(amountText)
			if amountMin == nil {
				continue
			}
			asset := ogeRowAsset(row, bands)
			if asset == "" {
				continue
			}
			raw, _ := json.Marshal(map[string]any{"page": p.Number, "line": ogeRowText(row)})
			r.Transactions = append(r.Transactions, Transaction{Ordinal: len(r.Transactions) + 1, AssetName: asset, Ticker: tickerFromAsset(asset), TransactionType: action, TransactionDate: date, AmountMin: amountMin, AmountMax: amountMax, AmountText: amountText, RawData: raw})
		}
	}
	if len(r.Transactions) > 0 {
		r.ExtractionStatus = "completed"
	} else {
		if ogeMeaningfulTextBoxCount(pages) == 0 {
			r.ExtractionError = "OGE PDF has no extractable text; legacy paper/scanned filing requires OCR"
		} else {
			r.ExtractionStatus = "failed"
			r.ExtractionError = "OGE text was extracted but no transaction rows were reconstructed"
			r.RetryableFailure = true
		}
	}
	return r
}

// ogeTransactionType determines the reported action from the positioned action
// box nearest to the transaction date. It accepts both expanded words and the
// compact P/S/E codes, while ignoring action words inside asset descriptions.
func ogeTransactionType(row ogeTransactionRow) string {
	action, ok := ogeTransactionAction(row)
	if !ok {
		return ""
	}
	return action.transactionType
}

func ogeTransactionAction(row ogeTransactionRow) (ogeAction, bool) {
	best, bestDistance := "", 0.0
	var selected ogeAction
	for _, action := range row.actions {
		distance := action.box.Left - row.date.Left
		if distance < 0 {
			distance = -distance
		}
		if best == "" || distance < bestDistance {
			best, bestDistance, selected = action.transactionType, distance, action
		}
	}
	return selected, best != ""
}

func ogeStandaloneAction(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "p" || text == "s" || text == "e" || text == "purchase" || text == "sale" || text == "exchange" || text == "buy" || text == "sell" {
		return normalizeTransactionType(text)
	}
	if strings.HasPrefix(text, "p (") || strings.HasPrefix(text, "s (") || strings.HasPrefix(text, "e (") {
		return normalizeTransactionType(text)
	}
	return ""
}

// ogeRows only treats dates in the transaction-date column as row anchors.
// Asset descriptions often include their own dates; using those as anchors
// would split a single transaction into multiple, malformed rows.
func ogeRows(p PageText, bands ogeColumnBands) []ogeTransactionRow {
	boxes := append([]TextBox(nil), p.Boxes...)
	sort.SliceStable(boxes, func(i, j int) bool {
		if boxes[i].Top != boxes[j].Top {
			return boxes[i].Top < boxes[j].Top
		}
		return boxes[i].Left < boxes[j].Left
	})
	positioned := make([]ogePositionedBox, 0, len(boxes))
	for i, box := range boxes {
		positioned = append(positioned, ogePositionedBox{TextBox: box, index: i})
	}
	dateBand, ok := ogeDateAnchorBand(positioned, bands)
	if !ok {
		return nil
	}
	anchors := make([]ogePositionedBox, 0)
	for _, box := range positioned {
		if dateText := ogeDate.FindString(box.Text); dateText != "" {
			if _, err := parseDate(dateText); err == nil && dateBand.contains(box.Left) {
				anchors = append(anchors, box)
			}
		}
	}
	rows := make([]ogeTransactionRow, 0, len(anchors))
	for i, anchor := range anchors {
		rowEnd := 1e100
		if i+1 < len(anchors) {
			// The next dated row owns every box at its y-coordinate. Everything
			// above it, including wrapped field continuations, belongs here.
			rowEnd = anchors[i+1].Top
		}
		row := ogeTransactionRow{date: anchor}
		for _, box := range positioned {
			if box.Top < anchor.Top-3 || box.Top >= rowEnd {
				continue
			}
			row.boxes = append(row.boxes, box)
			if typ := ogeStandaloneAction(box.Text); typ != "" {
				row.actions = append(row.actions, ogeAction{box: box, transactionType: typ})
			}
		}
		rows = append(rows, row)
	}
	return rows
}

type ogeDateBand struct{ min, max float64 }

func (b ogeDateBand) contains(left float64) bool {
	return left >= b.min && left <= b.max
}

// ogeDateAnchorBand prefers the explicit date header. For headerless PDFs it
// infers the date column from repeated date/action/currency row geometry.
func ogeDateAnchorBand(boxes []ogePositionedBox, headers ogeColumnBands) (ogeDateBand, bool) {
	if headers.hasDate {
		// The next known column is the natural right edge of the date column.
		right := math.Inf(1)
		for _, column := range []struct {
			left float64
			has  bool
		}{
			{headers.action, headers.hasAction},
			{headers.asset, headers.hasAsset},
			{headers.amount, headers.hasAmount},
		} {
			if column.has && column.left > headers.date && column.left < right {
				right = column.left
			}
		}
		if math.IsInf(right, 1) {
			// A lone Date header still supplies a useful narrow column band.
			right = headers.date + 30
		} else {
			right = headers.date + (right-headers.date)/2
		}
		return ogeDateBand{min: headers.date - 12, max: right}, true
	}

	// Headerless documents need at least one row-shaped date candidate. Group
	// candidates by horizontal position so a date embedded in every asset name
	// cannot outvote the actual left-most date column.
	type candidate struct {
		left        float64
		occurrences int
		evidence    int
	}
	candidates := make([]candidate, 0)
	for _, box := range boxes {
		dateText := ogeDate.FindString(box.Text)
		if dateText == "" {
			continue
		}
		if _, err := parseDate(dateText); err != nil {
			continue
		}
		index := -1
		for i := range candidates {
			if math.Abs(candidates[i].left-box.Left) <= 12 {
				index = i
				break
			}
		}
		if index == -1 {
			candidates = append(candidates, candidate{left: box.Left})
			index = len(candidates) - 1
		}
		candidates[index].occurrences++
		if ogeDateRowEvidence(box, boxes) {
			candidates[index].evidence++
		}
	}
	best := -1
	for i := range candidates {
		if candidates[i].evidence == 0 || (best >= 0 &&
			(candidates[i].evidence < candidates[best].evidence ||
				(candidates[i].evidence == candidates[best].evidence && candidates[i].occurrences < candidates[best].occurrences) ||
				(candidates[i].evidence == candidates[best].evidence && candidates[i].occurrences == candidates[best].occurrences && candidates[i].left >= candidates[best].left))) {
			continue
		}
		best = i
	}
	if best < 0 {
		return ogeDateBand{}, false
	}
	return ogeDateBand{min: candidates[best].left - 12, max: candidates[best].left + 12}, true
}

func ogeDateRowEvidence(date ogePositionedBox, boxes []ogePositionedBox) bool {
	actionLeft := math.Inf(1)
	for _, box := range boxes {
		if math.Abs(box.Top-date.Top) > 4 || box.Left <= date.Left || ogeStandaloneAction(box.Text) == "" {
			continue
		}
		if box.Left < actionLeft {
			actionLeft = box.Left
		}
	}
	if math.IsInf(actionLeft, 1) {
		return false
	}
	for _, box := range boxes {
		if math.Abs(box.Top-date.Top) <= 4 && box.Left > actionLeft && strings.Contains(box.Text, "$") {
			return true
		}
	}
	return false
}

func ogeHeaderBands(p PageText) ogeColumnBands {
	var bands ogeColumnBands
	for _, box := range p.Boxes {
		text := strings.ToLower(strings.TrimSpace(box.Text))
		switch {
		case strings.Contains(text, "transaction date") || text == "date":
			bands.date, bands.hasDate = box.Left, true
		case strings.Contains(text, "transaction type") || text == "action":
			bands.action, bands.hasAction = box.Left, true
		case strings.Contains(text, "asset") || strings.Contains(text, "description"):
			bands.asset, bands.hasAsset = box.Left, true
		case strings.Contains(text, "amount") || strings.Contains(text, "value"):
			bands.amount, bands.hasAmount = box.Left, true
		}
	}
	return bands
}

func ogeRowAmountText(row ogeTransactionRow, bands ogeColumnBands) string {
	amountStart := 0.0
	found := false
	for _, box := range row.boxes {
		if !strings.Contains(box.Text, "$") {
			continue
		}
		if !found || box.Left < amountStart {
			amountStart, found = box.Left, true
		}
	}
	if !found {
		return ""
	}
	if bands.hasAmount && bands.amount > amountStart-4 {
		amountStart = bands.amount
	}
	parts := make([]string, 0)
	for _, box := range row.boxes {
		if box.Left < amountStart-3 {
			continue
		}
		text := strings.TrimSpace(box.Text)
		if text == "" || !ogeAmountFieldText(text) {
			continue
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, " ")
}

func ogeAmountFieldText(text string) bool {
	text = strings.TrimSpace(text)
	if len(text) >= len("over ") && strings.EqualFold(text[:len("over ")], "over ") {
		text = strings.TrimSpace(text[len("over "):])
	}
	for _, r := range text {
		if (r >= '0' && r <= '9') || r == '$' || r == ',' || r == '-' || r == '–' || r == ' ' {
			continue
		}
		return false
	}
	return true
}

func ogeRowAsset(row ogeTransactionRow, bands ogeColumnBands) string {
	action, ok := ogeTransactionAction(row)
	if !ok {
		return ""
	}
	amountStart := 1e100
	for _, box := range row.boxes {
		if strings.Contains(box.Text, "$") && box.Left < amountStart {
			amountStart = box.Left
		}
	}
	if bands.hasAmount && bands.amount < amountStart {
		amountStart = bands.amount
	}
	assetStart := action.box.Left + 0.01
	if bands.hasAsset {
		assetStart = bands.asset - 3
	}
	parts := make([]string, 0)
	for _, box := range row.boxes {
		if box.index == row.date.index || box.index == action.box.index || (box.Left < assetStart || box.Left >= amountStart-3) {
			continue
		}
		if strings.Contains(box.Text, "$") {
			continue
		}
		if text := strings.TrimSpace(box.Text); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, " ")
}

func ogeRowText(row ogeTransactionRow) string {
	parts := make([]string, 0, len(row.boxes))
	for _, box := range row.boxes {
		if text := strings.TrimSpace(box.Text); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, " ")
}

func ogeMeaningfulTextBoxCount(pages []PageText) int {
	count := 0
	for _, page := range pages {
		for _, box := range page.Boxes {
			if strings.TrimSpace(box.Text) != "" {
				count++
			}
		}
	}
	return count
}

func hashBytes(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
