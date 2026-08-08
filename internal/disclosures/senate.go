package disclosures

import (
	"context"
	"encoding/json"
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

var (
	senatePTRLinkPattern = regexp.MustCompile(`(?i)href=["']([^"']*/search/view/ptr/([^/"']+)/?)["']`)
	dateInTextPattern    = regexp.MustCompile(`\b\d{2}/\d{2}/\d{4}\b`)
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
	SearchPTRs(context.Context, time.Time, time.Time, int) ([]byte, error)
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
		body, err := c.Transport.SearchPTRs(ctx, since, until, maxReports)
		if err != nil {
			return nil, err
		}
		return parseSenateSearchResponse(c.BaseURL, body)
	}
	token := ""
	var err error
	if c.UseImportedSession {
		token = c.csrfCookie()
		if token == "" {
			return nil, fmt.Errorf("imported Senate session has no CSRF cookie")
		}
	} else {
		token, err = c.acceptAgreement(ctx)
		if err != nil {
			return nil, err
		}
	}
	form := url.Values{
		"csrfmiddlewaretoken":  {token},
		"start":                {"0"},
		"length":               {strconv.Itoa(maxReports)},
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
	body, err := c.postForm(ctx, c.BaseURL+"/search/report/data/", form, token)
	if err != nil {
		return nil, err
	}
	return parseSenateSearchResponse(c.BaseURL, body)
}

func parseSenateSearchResponse(baseURL string, body []byte) ([]SenateFiling, error) {
	var response struct {
		Data [][]any `json:"data"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("decode Senate eFD search response: %w", err)
	}
	filings := make([]SenateFiling, 0, len(response.Data))
	for _, row := range response.Data {
		cells := make([]string, 0, len(row))
		for _, value := range row {
			cells = append(cells, fmt.Sprint(value))
		}
		filing, ok := parseSenateSearchRow(baseURL, cells)
		if ok {
			filings = append(filings, filing)
		}
	}
	return filings, nil
}

func (c *SenateClient) FetchPTR(ctx context.Context, filing SenateFiling) (Report, error) {
	if err := c.pacer.Wait(ctx, c.RequestDelay); err != nil {
		return Report{}, err
	}
	if c.Transport != nil {
		body, err := c.Transport.FetchPTR(ctx, filing.DocumentURL)
		if err != nil {
			return Report{}, err
		}
		return ParseSenatePTR(filing, body)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, filing.DocumentURL, nil)
	if err != nil {
		return Report{}, err
	}
	req.Header.Set("Referer", c.BaseURL+"/search/")
	req.Header.Set("User-Agent", c.userAgent())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Report{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Report{}, fmt.Errorf("GET Senate PTR: HTTP %s", resp.Status)
	}
	if resp.Request != nil && resp.Request.URL != nil &&
		resp.Request.URL.Path == "/search/home/" {
		return Report{}, fmt.Errorf("Senate eFD session expired while fetching PTR")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
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
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	resp.Body.Close()
	if readErr != nil {
		return "", readErr
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET Senate eFD agreement: HTTP %s", resp.Status)
	}
	token := csrfToken(body)
	if token == "" {
		return "", fmt.Errorf("Senate eFD agreement did not contain a CSRF token")
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
		return nil, fmt.Errorf("Senate eFD session expired while posting search")
	}
	return io.ReadAll(io.LimitReader(resp.Body, 10<<20))
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

func parseSenateSearchRow(baseURL string, cells []string) (SenateFiling, bool) {
	var filing SenateFiling
	var names []string
	for index, raw := range cells {
		plain := strings.Join(strings.Fields(stripHTML(raw)), " ")
		if match := senatePTRLinkPattern.FindStringSubmatch(raw); len(match) == 3 {
			filing.ExternalID = match[2]
			reference, err := url.Parse(match[1])
			if err == nil {
				if reference.IsAbs() {
					filing.DocumentURL = reference.String()
				} else {
					filing.DocumentURL = strings.TrimRight(baseURL, "/") + "/" + strings.TrimLeft(reference.Path, "/")
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
	return filing, filing.ExternalID != "" && filing.DocumentURL != "" &&
		filing.FilerName != "" && !filing.FilingDate.IsZero()
}

func ParseSenatePTR(filing SenateFiling, body []byte) (Report, error) {
	document, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return Report{}, fmt.Errorf("parse Senate PTR HTML: %w", err)
	}
	transactions := senateTransactions(document)
	report := Report{
		Source: "senate", ExternalID: filing.ExternalID, Chamber: "senate",
		FilerName: filing.FilerName, Office: filing.Office,
		ReportType: "periodic_transaction", FilingDate: filing.FilingDate,
		DocumentURL: filing.DocumentURL, ExtractionStatus: "completed",
		Transactions: transactions,
	}
	if len(transactions) == 0 {
		report.ExtractionStatus = "no_text"
		report.ExtractionError = "no Senate PTR transaction rows were found"
	}
	return report, nil
}

func senateTransactions(document *html.Node) []Transaction {
	for _, table := range htmlTables(document) {
		if len(table) < 2 {
			continue
		}
		headers := make(map[string]int)
		for index, value := range table[0] {
			headers[normalizeHeader(value)] = index
		}
		assetIndex := headerIndex(headers, "asset name", "asset")
		transactionIndex := headerIndex(headers, "transaction type", "type")
		dateIndex := headerIndex(headers, "transaction date", "date")
		amountIndex := headerIndex(headers, "amount")
		if assetIndex < 0 || transactionIndex < 0 || dateIndex < 0 || amountIndex < 0 {
			continue
		}
		ownerIndex := headerIndex(headers, "owner")
		tickerIndex := headerIndex(headers, "ticker")
		commentIndex := headerIndex(headers, "comment")
		assetTypeIndex := headerIndex(headers, "asset type")
		var out []Transaction
		for _, row := range table[1:] {
			asset, ok := cell(row, assetIndex)
			if !ok || strings.TrimSpace(asset) == "" {
				continue
			}
			dateText, ok := cell(row, dateIndex)
			if !ok {
				continue
			}
			transactionDate, err := parseDate(dateInTextPattern.FindString(dateText))
			if err != nil {
				continue
			}
			amount, _ := cell(row, amountIndex)
			minimum, maximum := parseAmount(amount)
			ticker, _ := cell(row, tickerIndex)
			if ticker == "--" || ticker == "N/A" {
				ticker = ""
			}
			if ticker == "" {
				ticker = tickerFromAsset(asset)
			}
			owner, _ := cell(row, ownerIndex)
			assetType, _ := cell(row, assetTypeIndex)
			transactionType, _ := cell(row, transactionIndex)
			comment, _ := cell(row, commentIndex)
			raw, _ := json.Marshal(row)
			out = append(out, Transaction{
				Ordinal: len(out) + 1, Owner: owner, AssetName: asset,
				Ticker: strings.ToUpper(strings.TrimSpace(ticker)), AssetType: assetType,
				TransactionType: normalizeTransactionType(transactionType),
				TransactionDate: transactionDate, AmountMin: minimum, AmountMax: maximum,
				AmountText: amount, Comment: comment, RawData: raw,
			})
		}
		return out
	}
	return nil
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

func headerIndex(headers map[string]int, names ...string) int {
	// Prefer exact labels across every synonym before considering substring
	// matches. Otherwise the live Senate "Type" column can randomly resolve to
	// "Asset Type" because Go map iteration order is intentionally unstable.
	for _, name := range names {
		if index, ok := headers[name]; ok {
			return index
		}
	}
	for _, name := range names {
		for header, index := range headers {
			if strings.Contains(header, name) {
				return index
			}
		}
	}
	return -1
}

func cell(row []string, index int) (string, bool) {
	if index < 0 || index >= len(row) {
		return "", false
	}
	return strings.TrimSpace(row[index]), true
}
