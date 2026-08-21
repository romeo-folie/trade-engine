package disclosures

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	stdhtml "html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// SenateSourceBlockedError means the source needs explicit browser
// authorization. It is deliberately distinct from a transient document error.
type SenateSourceBlockedError struct{ Err error }

func (e *SenateSourceBlockedError) Error() string { return "Senate source blocked: " + e.Err.Error() }
func (e *SenateSourceBlockedError) Unwrap() error { return e.Err }
func senateSourceBlocked(err error) error {
	if errors.Is(err, ErrSenateAuthorizationRequired) {
		return &SenateSourceBlockedError{Err: ErrSenateAuthorizationRequired}
	}
	return err
}

var (
	senatePTRLinkPattern = regexp.MustCompile(`(?i)href=["']([^"']*/search/view/ptr/([^/"']+)/?)["']`)
	dateInTextPattern    = regexp.MustCompile(`\b\d{2}/\d{2}/\d{4}\b`)
)

const (
	maxSenateAgreementResponseSize = 2 << 20
	maxSenateSearchResponseSize    = 10 << 20
	maxSenatePTRResponseSize       = 10 << 20
)

type SenateFiling struct {
	ExternalID  string
	FilerName   string
	Office      string
	FilingDate  time.Time
	DocumentURL string
}

// SenateTransport performs eFD requests in an already-authorized client. The
// production implementation runs requests inside Chrome so the official site
// sees the browser's own cookies and network fingerprint. Tests and the legacy
// HTTP fallback can leave it unset.
type SenateTransport interface {
	SearchPTRs(context.Context, time.Time, time.Time, int, int) ([]byte, error)
	FetchPTR(context.Context, string) ([]byte, error)
	Close() error
}

type SenateClient struct {
	BaseURL            string
	HTTP               *http.Client
	RequestDelay       time.Duration
	UserAgent          string
	UseImportedSession bool
	Transport          SenateTransport
	pacer              requestPacer
}

func NewSenateClient(baseURL string, timeout time.Duration) (*SenateClient, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &SenateClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		HTTP:    &http.Client{Timeout: timeout, Jar: jar},
	}, nil
}

func (c *SenateClient) ListPTRs(
	ctx context.Context, since, until time.Time, maxReports int,
) ([]SenateFiling, error) {
	if c.Transport != nil {
		if err := c.pacer.Wait(ctx, c.RequestDelay); err != nil {
			return nil, err
		}
		body, err := c.Transport.SearchPTRs(ctx, since, until, 0, maxReports)
		if err != nil {
			return nil, err
		}
		return parseSenateSearchResponse(c.BaseURL, body)
	}
	body, err := c.searchPTRsHTTP(ctx, since, until, 0, maxReports)
	if err != nil {
		return nil, err
	}
	return parseSenateSearchResponse(c.BaseURL, body)
}

func (c *SenateClient) searchPTRsHTTP(
	ctx context.Context, since, until time.Time, start, length int,
) ([]byte, error) {
	token := ""
	var err error
	if c.UseImportedSession {
		token = c.csrfCookie()
		if token == "" {
			return nil, &SenateSourceBlockedError{Err: ErrSenateAuthorizationRequired}
		}
	} else {
		token, err = c.acceptAgreement(ctx)
		if err != nil {
			return nil, err
		}
	}
	form := url.Values{
		"csrfmiddlewaretoken":  {token},
		"start":                {strconv.Itoa(start)},
		"length":               {strconv.Itoa(length)},
		"report_types":         {"[11]"},
		"filer_types":          {"[]"},
		"submitted_start_date": {since.Format("01/02/2006") + " 00:00:00"},
		"submitted_end_date":   {until.Format("01/02/2006") + " 23:59:59"},
		"candidate_state":      {""},
		"senator_state":        {""},
		"office_id":            {""},
		"first_name":           {""},
		"last_name":            {""},
	}
	return c.postForm(ctx, c.BaseURL+"/search/report/data/", form, token)
}

type SenateFilingPage struct {
	Filings         []SenateFiling
	RowCount, Total int
	Issues          []ListingRowIssue
}

func (c *SenateClient) ListPTRPage(ctx context.Context, since, until time.Time, start, length int) (SenateFilingPage, error) {
	var body []byte
	var err error
	if c.Transport != nil {
		if err := c.pacer.Wait(ctx, c.RequestDelay); err != nil {
			return SenateFilingPage{}, err
		}
		body, err = c.Transport.SearchPTRs(ctx, since, until, start, length)
	} else {
		body, err = c.searchPTRsHTTP(ctx, since, until, start, length)
	}
	if err != nil {
		return SenateFilingPage{}, err
	}
	return parseSenateSearchPageAt(c.BaseURL, body, start)
}

func parseSenateSearchResponse(baseURL string, body []byte) ([]SenateFiling, error) {
	page, err := parseSenateSearchPage(baseURL, body)
	return page.Filings, err
}

func parseSenateSearchPage(baseURL string, body []byte) (SenateFilingPage, error) {
	return parseSenateSearchPageAt(baseURL, body, 0)
}

func parseSenateSearchPageAt(baseURL string, body []byte, start int) (SenateFilingPage, error) {
	var response struct {
		Data            []json.RawMessage `json:"data"`
		RecordsFiltered int               `json:"recordsFiltered"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return SenateFilingPage{}, fmt.Errorf("decode Senate eFD search response: %w", err)
	}
	if response.RecordsFiltered < 0 || response.RecordsFiltered < len(response.Data) {
		return SenateFilingPage{}, fmt.Errorf(
			"Senate eFD search response has inconsistent recordsFiltered %d for %d rows",
			response.RecordsFiltered, len(response.Data),
		)
	}
	filings := make([]SenateFiling, 0, len(response.Data))
	issues := make([]ListingRowIssue, 0)
	if len(response.Data) > 0 {
		for index, rawRow := range response.Data {
			var row []any
			if err := json.Unmarshal(rawRow, &row); err != nil {
				issues = append(issues, newListingRowIssue(start+index+1, "invalid_listing_row", rawRow))
				continue
			}
			cells := make([]string, 0, len(row))
			for _, value := range row {
				cells = append(cells, fmt.Sprint(value))
			}
			filing, reason := parseSenateSearchRow(baseURL, cells)
			if reason != "" {
				issues = append(issues, newListingRowIssue(start+index+1, reason, rawRow))
				continue
			}
			filings = append(filings, filing)
		}
	}
	if allListingRowsMalformed(len(response.Data), issues) {
		// Return the safe row diagnostics with the fatal page error so the runner
		// can log them before stopping the source sync.
		return SenateFilingPage{RowCount: len(response.Data), Total: response.RecordsFiltered, Issues: issues}, fmt.Errorf("Senate eFD search page at start %d has no usable rows", start)
	}
	return SenateFilingPage{Filings: filings, RowCount: len(response.Data), Total: response.RecordsFiltered, Issues: issues}, nil
}

func (c *SenateClient) FetchPTR(ctx context.Context, filing SenateFiling) (Report, error) {
	if err := c.pacer.Wait(ctx, c.RequestDelay); err != nil {
		return Report{}, err
	}
	document, err := trustedDocumentURL(c.BaseURL, filing.DocumentURL, "/search/view/ptr/")
	if err != nil {
		return Report{}, err
	}
	if c.Transport != nil {
		body, err := c.Transport.FetchPTR(ctx, document.String())
		if err != nil {
			return Report{}, err
		}
		return ParseSenatePTR(filing, body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, document.String(), nil)
	if err != nil {
		return Report{}, err
	}
	req.Header.Set("Referer", c.BaseURL+"/search/")
	req.Header.Set("User-Agent", c.userAgent())
	resp, err := sameOriginDocumentClient(c.HTTP, c.BaseURL, "/search/view/ptr/").Do(req)
	if err != nil {
		return Report{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Report{}, disclosureHTTPStatusError("Senate", resp.StatusCode, resp.Status)
	}
	if resp.Request != nil && resp.Request.URL != nil &&
		resp.Request.URL.Path == "/search/home/" {
		return Report{}, &SenateSourceBlockedError{Err: ErrSenateAuthorizationRequired}
	}
	body, err := readBoundedSenateResponse(resp.Body, resp.ContentLength, maxSenatePTRResponseSize, "PTR")
	if err != nil {
		return Report{}, err
	}
	report, err := ParseSenatePTR(filing, body)
	if err != nil {
		return Report{}, err
	}
	return report, nil
}

func (c *SenateClient) Close() error {
	if c == nil || c.Transport == nil {
		return nil
	}
	return c.Transport.Close()
}

func (c *SenateClient) acceptAgreement(ctx context.Context) (string, error) {
	homeURL := c.BaseURL + "/search/home/"
	if err := c.pacer.Wait(ctx, c.RequestDelay); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, homeURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", c.userAgent())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	body, readErr := readBoundedSenateResponse(resp.Body, resp.ContentLength, maxSenateAgreementResponseSize, "agreement")
	resp.Body.Close()
	if readErr != nil {
		return "", readErr
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET Senate eFD agreement: HTTP %s", resp.Status)
	}
	token := csrfToken(body)
	if token == "" {
		return "", &SenateSourceBlockedError{Err: ErrSenateAuthorizationRequired}
	}
	_, err = c.postForm(ctx, homeURL, url.Values{
		"csrfmiddlewaretoken":   {token},
		"prohibition_agreement": {"1"},
	}, token)
	if err != nil {
		return "", err
	}
	home, _ := url.Parse(homeURL)
	for _, cookie := range c.HTTP.Jar.Cookies(home) {
		if cookie.Name == "csrftoken" || cookie.Name == "csrf" {
			return cookie.Value, nil
		}
	}
	return token, nil
}

func (c *SenateClient) postForm(
	ctx context.Context, endpoint string, form url.Values, token string,
) ([]byte, error) {
	if err := c.pacer.Wait(ctx, c.RequestDelay); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(
		ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()),
	)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRFToken", token)
	referer := c.BaseURL + "/search/"
	if strings.HasSuffix(endpoint, "/search/home/") {
		referer = endpoint
	}
	req.Header.Set("Referer", referer)
	req.Header.Set("User-Agent", c.userAgent())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return nil, fmt.Errorf("POST %s: HTTP %s", endpoint, resp.Status)
	}
	if !strings.HasSuffix(endpoint, "/search/home/") && resp.Request != nil &&
		resp.Request.URL != nil && resp.Request.URL.Path == "/search/home/" {
		return nil, &SenateSourceBlockedError{Err: ErrSenateAuthorizationRequired}
	}
	return readBoundedSenateResponse(resp.Body, resp.ContentLength, maxSenateSearchResponseSize, "search")
}

func readBoundedSenateResponse(body io.Reader, contentLength int64, limit int, resource string) ([]byte, error) {
	if contentLength > int64(limit) {
		return nil, fmt.Errorf("senate eFD %s response exceeds %d-byte limit", resource, limit)
	}
	data, err := io.ReadAll(io.LimitReader(body, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("senate eFD %s response exceeds %d-byte limit", resource, limit)
	}
	return data, nil
}

func (c *SenateClient) csrfCookie() string {
	home, err := url.Parse(c.BaseURL + "/search/")
	if err != nil || c.HTTP == nil || c.HTTP.Jar == nil {
		return ""
	}
	for _, cookie := range c.HTTP.Jar.Cookies(home) {
		if cookie.Name == "csrftoken" || cookie.Name == "csrf" {
			return cookie.Value
		}
	}
	return ""
}

func (c *SenateClient) userAgent() string {
	if strings.TrimSpace(c.UserAgent) != "" {
		return c.UserAgent
	}
	return "trade-engine congressional disclosure ingester"
}

func csrfToken(body []byte) string {
	document, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return ""
	}
	var token string
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if token != "" {
			return
		}
		if node.Type == html.ElementNode && node.Data == "input" {
			name, value := "", ""
			for _, attr := range node.Attr {
				switch attr.Key {
				case "name":
					name = attr.Val
				case "value":
					value = attr.Val
				}
			}
			if name == "csrfmiddlewaretoken" {
				token = value
				return
			}
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(document)
	return token
}

func parseSenateSearchRow(baseURL string, cells []string) (SenateFiling, string) {
	var filing SenateFiling
	var names []string
	ptrLinkFound := false
	ptrLinkTrusted := false
	for index, raw := range cells {
		plain := strings.Join(strings.Fields(stripHTML(raw)), " ")
		if match := senatePTRLinkPattern.FindStringSubmatch(raw); len(match) == 3 {
			ptrLinkFound = true
			filing.ExternalID = match[2]
			reference, err := url.Parse(match[1])
			if err == nil {
				if !reference.IsAbs() {
					reference, err = url.Parse(strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(reference.Path, "/"))
				}
				if err == nil {
					if document, trustedErr := trustedDocumentURL(baseURL, reference.String(), "/search/view/ptr/"); trustedErr == nil {
						filing.DocumentURL = document.String()
						ptrLinkTrusted = true
					}
				}
			}
		}
		if dateText := dateInTextPattern.FindString(plain); dateText != "" {
			if parsed, err := parseDate(dateText); err == nil {
				filing.FilingDate = parsed
			}
		}
		if index < 2 && plain != "" {
			names = append(names, plain)
		} else if index == 2 {
			filing.Office = plain
		}
	}
	filing.FilerName = strings.Join(names, " ")
	switch {
	case !ptrLinkFound:
		return SenateFiling{}, "missing_ptr_link"
	case !ptrLinkTrusted || filing.ExternalID == "" || filing.DocumentURL == "":
		return SenateFiling{}, "untrusted_ptr_link"
	case filing.FilerName == "":
		return SenateFiling{}, "missing_filer_name"
	case filing.FilingDate.IsZero():
		return SenateFiling{}, "invalid_filing_date"
	default:
		return filing, ""
	}
}

func ParseSenatePTR(filing SenateFiling, body []byte) (Report, error) {
	document, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return Report{}, fmt.Errorf("parse Senate PTR HTML: %w", err)
	}
	transactions, parseErr := senateTransactions(document)
	if parseErr != nil {
		return Report{}, parseErr
	}
	report := Report{
		Source: "senate", ExternalID: filing.ExternalID, Chamber: "senate",
		FilerName: filing.FilerName, Office: filing.Office,
		ReportType: "periodic_transaction", FilingDate: filing.FilingDate,
		DocumentURL: filing.DocumentURL, ExtractionStatus: "completed",
		ContentHash: hashBytes(body), Transactions: transactions,
	}
	if len(transactions) == 0 {
		// A canonical header proves this is a PTR document with text, but the
		// absence of rows can be caused by a changed table shape. Keep it
		// retryable rather than treating it as a terminal OCR/no-text result.
		report.ExtractionStatus = "failed"
		report.ExtractionError = "Senate PTR canonical transaction table had no transaction rows to reconstruct"
		report.RetryableFailure = true
	}
	return report, nil
}

func senateTransactions(document *html.Node) ([]Transaction, error) {
	type canonicalTable struct {
		number int
		rows   [][]string
		index  map[string]int
	}
	var canonical []canonicalTable
	for tableNumber, table := range htmlTables(document) {
		if len(table) == 0 {
			continue
		}
		indexes := senateRequiredHeaderIndexes(table[0])
		if indexes == nil {
			continue
		}
		canonical = append(canonical, canonicalTable{number: tableNumber + 1, rows: table, index: indexes})
	}
	if len(canonical) == 0 {
		return nil, fmt.Errorf("senate PTR has no canonical transaction table")
	}
	if len(canonical) != 1 {
		return nil, fmt.Errorf("senate PTR has %d canonical transaction tables", len(canonical))
	}
	table := canonical[0]
	if len(table.rows) == 1 {
		return nil, nil
	}
	optional := senateOptionalHeaderIndexes(table.rows[0])
	var out []Transaction
	for rowIndex, row := range table.rows[1:] {
		context := fmt.Sprintf("table %d row %d", table.number, rowIndex+2)
		asset, ok := cell(row, table.index["asset"])
		if !ok || strings.TrimSpace(asset) == "" {
			return nil, fmt.Errorf("senate PTR has malformed transaction %s: asset is required", context)
		}
		dateText, ok := cell(row, table.index["date"])
		if !ok || strings.TrimSpace(dateText) == "" {
			return nil, fmt.Errorf("senate PTR has malformed transaction %s: date is required", context)
		}
		transactionDate, err := parseDate(dateInTextPattern.FindString(dateText))
		if err != nil {
			return nil, fmt.Errorf("senate PTR has malformed transaction %s: invalid date", context)
		}
		amount, ok := cell(row, table.index["amount"])
		if !ok || strings.TrimSpace(amount) == "" {
			return nil, fmt.Errorf("senate PTR has malformed transaction %s: amount is required", context)
		}
		transactionType, ok := cell(row, table.index["type"])
		transactionType = normalizeTransactionType(transactionType)
		if !ok || (transactionType != "purchase" && transactionType != "sale" && transactionType != "exchange") {
			return nil, fmt.Errorf("senate PTR has malformed transaction %s: unknown action %q", context, transactionType)
		}
		minimum, maximum := parseAmount(amount)
		ticker := senateOptionalCell(row, optional, "ticker")
		ticker = sanitizeSenateTicker(ticker)
		if ticker == "" {
			ticker = tickerFromAsset(asset)
		}
		owner := senateOptionalCell(row, optional, "owner")
		assetType := senateOptionalCell(row, optional, "asset type")
		comment := senateOptionalCell(row, optional, "comment")
		raw, _ := json.Marshal(row)
		out = append(out, Transaction{
			Ordinal: len(out) + 1, Owner: owner, AssetName: asset,
			Ticker: strings.ToUpper(strings.TrimSpace(ticker)), AssetType: assetType,
			TransactionType: transactionType,
			TransactionDate: transactionDate, AmountMin: minimum, AmountMax: maximum,
			AmountText: amount, Comment: comment, RawData: raw,
		})
	}
	return out, nil
}

func senateRequiredHeaderIndexes(header []string) map[string]int {
	required := map[string][]string{
		"asset":  {"asset name", "asset"},
		"type":   {"transaction type", "type"},
		"date":   {"transaction date", "date"},
		"amount": {"amount"},
	}
	indexes := make(map[string]int, len(required))
	for field, names := range required {
		for column, value := range header {
			for _, name := range names {
				if normalizeHeader(value) == name {
					if _, duplicate := indexes[field]; duplicate {
						return nil
					}
					indexes[field] = column
				}
			}
		}
		if _, found := indexes[field]; !found {
			return nil
		}
	}
	seen := make(map[int]bool, len(indexes))
	for _, index := range indexes {
		if seen[index] {
			return nil
		}
		seen[index] = true
	}
	return indexes
}

func senateOptionalHeaderIndexes(header []string) map[string]int {
	indexes := make(map[string]int)
	for column, value := range header {
		name := normalizeHeader(value)
		for _, field := range []string{"owner", "ticker", "comment", "asset type"} {
			if name == field {
				if _, duplicate := indexes[field]; !duplicate {
					indexes[field] = column
				}
			}
		}
	}
	return indexes
}

// senateOptionalCell returns an optional field only when the table header
// actually supplied its column. Missing map entries have Go's zero value of
// zero, which must not be confused with a legitimate optional column at index
// zero. A truncated row also leaves the optional field empty.
func senateOptionalCell(row []string, indexes map[string]int, field string) string {
	index, ok := indexes[field]
	if !ok {
		return ""
	}
	value, _ := cell(row, index)
	return value
}

func sanitizeSenateTicker(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	value = strings.TrimSpace(strings.TrimLeft(value, "-–— "))
	if value == "" || value == "N/A" || value == "NA" {
		return ""
	}
	fields := strings.Fields(value)
	if len(fields) > 0 {
		value = fields[len(fields)-1]
	}
	if !regexp.MustCompile(`^[A-Z][A-Z0-9.-]{0,9}$`).MatchString(value) {
		return ""
	}
	return value
}

func htmlTables(document *html.Node) [][][]string {
	var tables [][][]string
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "table" {
			var rows [][]string
			for row := node.FirstChild; row != nil; row = row.NextSibling {
				collectTableRows(row, &rows)
			}
			tables = append(tables, rows)
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(document)
	return tables
}

func collectTableRows(node *html.Node, rows *[][]string) {
	if node.Type == html.ElementNode && node.Data == "tr" {
		var cells []string
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			if child.Type == html.ElementNode && (child.Data == "th" || child.Data == "td") {
				cells = append(cells, strings.Join(strings.Fields(nodeText(child)), " "))
			}
		}
		if len(cells) > 0 {
			*rows = append(*rows, cells)
		}
		return
	}
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		collectTableRows(child, rows)
	}
}

func nodeText(node *html.Node) string {
	if node.Type == html.TextNode {
		return node.Data
	}
	var builder strings.Builder
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		builder.WriteString(nodeText(child))
		builder.WriteByte(' ')
	}
	return stdhtml.UnescapeString(builder.String())
}

func stripHTML(value string) string {
	nodes, err := html.ParseFragment(strings.NewReader(value), nil)
	if err != nil {
		return value
	}
	var builder strings.Builder
	for _, node := range nodes {
		builder.WriteString(nodeText(node))
	}
	return builder.String()
}

func normalizeHeader(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func cell(row []string, index int) (string, bool) {
	if index < 0 || index >= len(row) {
		return "", false
	}
	return strings.TrimSpace(row[index]), true
}
