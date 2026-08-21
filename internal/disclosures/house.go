package disclosures

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type HouseFiling struct {
	Prefix, Last, First, Suffix string
	FilingType, StateDistrict   string
	Year                        int
	FilingDate                  time.Time
	DocumentID                  string
}

func (f HouseFiling) FilerName() string {
	return strings.Join(strings.Fields(strings.Join(
		[]string{f.Prefix, f.First, f.Last, f.Suffix}, " ")), " ")
}

func ParseHouseIndex(data []byte) ([]HouseFiling, error) {
	reader := csv.NewReader(bytes.NewReader(data))
	reader.Comma = '\t'
	reader.FieldsPerRecord = -1
	// The official index occasionally contains unescaped quote characters in
	// names. Treat them as literal data while retaining tab field boundaries.
	reader.LazyQuotes = true
	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parse house filing index: %w", err)
	}
	if len(records) == 0 {
		return nil, nil
	}
	var out []HouseFiling
	for _, row := range records[1:] {
		if len(row) < 9 {
			// One malformed official row must not discard the remaining annual
			// index. The row cannot form a valid filing identity anyway.
			continue
		}
		if strings.TrimSpace(row[4]) != "P" {
			continue
		}
		year, err := strconv.Atoi(strings.TrimSpace(row[6]))
		if err != nil {
			continue
		}
		filedAt, err := parseDate(row[7])
		if err != nil {
			continue
		}
		documentID := strings.TrimSpace(row[8])
		filerName := strings.Join(strings.Fields(strings.Join(
			[]string{row[0], row[2], row[1], row[3]}, " ")), " ")
		if documentID == "" || filerName == "" {
			continue
		}
		out = append(out, HouseFiling{
			Prefix: strings.TrimSpace(row[0]), Last: strings.TrimSpace(row[1]),
			First: strings.TrimSpace(row[2]), Suffix: strings.TrimSpace(row[3]),
			FilingType: "P", StateDistrict: strings.TrimSpace(row[5]), Year: year,
			FilingDate: filedAt, DocumentID: documentID,
		})
	}
	return out, nil
}

type HouseClient struct {
	BaseURL      string
	HTTP         *http.Client
	RequestDelay time.Duration
	pacer        requestPacer
}

const maxHouseResponseSize = 25 << 20

func (c *HouseClient) FetchIndex(ctx context.Context, year int) ([]HouseFiling, error) {
	endpoint := strings.TrimRight(c.BaseURL, "/") +
		fmt.Sprintf("/financial-pdfs/%dFD.txt", year)
	body, err := c.get(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	return ParseHouseIndex(body)
}

func (c *HouseClient) FetchPDF(ctx context.Context, filing HouseFiling) ([]byte, string, error) {
	endpoint := strings.TrimRight(c.BaseURL, "/") + fmt.Sprintf(
		"/ptr-pdfs/%d/%s.pdf", filing.Year, url.PathEscape(filing.DocumentID),
	)
	body, err := c.get(ctx, endpoint)
	return body, endpoint, err
}

func (c *HouseClient) get(ctx context.Context, endpoint string) ([]byte, error) {
	if err := c.pacer.Wait(ctx, c.RequestDelay); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "trade-engine congressional disclosure ingester")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, disclosureHTTPStatusError("House", resp.StatusCode, resp.Status)
	}
	if resp.ContentLength > maxHouseResponseSize {
		return nil, fmt.Errorf("GET %s: response exceeds %d-byte limit", endpoint, maxHouseResponseSize)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHouseResponseSize+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxHouseResponseSize {
		return nil, fmt.Errorf("GET %s: response exceeds %d-byte limit", endpoint, maxHouseResponseSize)
	}
	return body, nil
}

func ParseHousePTR(filing HouseFiling, documentURL string, pdfData []byte, pages []PageText) Report {
	hash := sha256.Sum256(pdfData)
	metadata, _ := json.Marshal(filing)
	report := Report{
		Source: "house", ExternalID: filing.DocumentID, Chamber: "house",
		FilerName: filing.FilerName(), Office: filing.StateDistrict,
		ReportType: "periodic_transaction", FilingDate: filing.FilingDate,
		DocumentURL: documentURL, ContentHash: hex.EncodeToString(hash[:]),
		ExtractionStatus: "completed", RawMetadata: metadata,
	}
	for _, page := range pages {
		report.Transactions = append(report.Transactions, parseHousePage(page)...)
	}
	for index := range report.Transactions {
		report.Transactions[index].Ordinal = index + 1
	}
	if len(report.Transactions) == 0 {
		if housePTRMeaningfulTextBoxCount(pages) == 0 {
			report.ExtractionStatus = "no_text"
			report.ExtractionError = "PDF has no extractable text; legacy paper/scanned PTR requires OCR"
		} else {
			report.ExtractionStatus = "failed"
			report.ExtractionError = "House PTR text was extracted but no transaction rows were reconstructed"
			report.RetryableFailure = true
		}
	}
	return report
}

func housePTRMeaningfulTextBoxCount(pages []PageText) int {
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

func parseHousePage(page PageText) []Transaction {
	boxes := append([]TextBox(nil), page.Boxes...)
	sort.SliceStable(boxes, func(i, j int) bool {
		if boxes[i].Top != boxes[j].Top {
			return boxes[i].Top < boxes[j].Top
		}
		return boxes[i].Left < boxes[j].Left
	})
	anchors := housePTRTransactionDateAnchors(boxes)
	var out []Transaction
	for index, anchor := range anchors {
		y := anchor.Top
		typeText := joinHouseBoxes(boxes, y, 2.5, 245, 300)
		transactionType := normalizeTransactionType(typeText)
		if transactionType != "purchase" && transactionType != "sale" &&
			transactionType != "exchange" {
			continue
		}
		notificationText := joinHouseBoxes(boxes, y, 2.5, 380, 440)
		notificationDate, err := parseDate(notificationText)
		if err != nil {
			continue
		}
		asset := joinHouseBoxes(boxes, y, 2.5, 95, 260)
		previousAsset := wrappedHouseAsset(boxes, y, housePTRAnchorLowerBound(anchors, index))
		if previousAsset != "" {
			asset = strings.TrimSpace(asset + " " + previousAsset)
		}
		asset = strings.Join(strings.Fields(asset), " ")
		if asset == "" {
			// A structurally recognizable row without an asset is not a
			// transaction. Keep scanning so mixed-quality PDFs retain their
			// valid rows; ParseHousePTR's postcondition classifies an all-empty
			// reconstruction as retryable.
			continue
		}
		amount := joinHouseBoxes(boxes, y, 2.5, 440, 520)
		if strings.HasSuffix(strings.TrimSpace(amount), "-") {
			amount += " " + nearestWrappedHouseText(boxes, y, housePTRAnchorLowerBound(anchors, index), 440, 520, func(text string) bool {
				return strings.Contains(text, "$")
			})
		}
		assetType := ""
		if match := assetTypePattern.FindStringSubmatch(asset); len(match) == 2 {
			assetType = match[1]
		}
		minimum, maximum := parseAmount(amount)
		owner := canonicalHouseOwner(joinHouseBoxes(boxes, y, 2.5, 0, 95))
		raw, _ := json.Marshal(map[string]any{"page": page.Number, "y": y})
		out = append(out, Transaction{
			Owner:     owner,
			AssetName: asset,
			Ticker:    tickerFromAsset(asset), AssetType: assetType,
			TransactionType: transactionType, TransactionDate: anchor.Date,
			NotificationDate: &notificationDate, AmountMin: minimum, AmountMax: maximum,
			AmountText: strings.Join(strings.Fields(amount), " "), RawData: raw,
		})
	}
	return out
}

type housePTRDateAnchor struct {
	Top  float64
	Date time.Time
}

// housePTRTransactionDateAnchors establishes the vertical ownership bands before
// validating a row. A dated but otherwise malformed row still owns the space
// around it, so a following valid row cannot consume its wrapped fields.
func housePTRTransactionDateAnchors(boxes []TextBox) []housePTRDateAnchor {
	anchors := make([]housePTRDateAnchor, 0)
	for _, box := range boxes {
		if box.Left < 300 || box.Left > 380 {
			continue
		}
		date, err := parseDate(box.Text)
		if err != nil {
			continue
		}
		if len(anchors) > 0 && abs(box.Top-anchors[len(anchors)-1].Top) <= 2.5 {
			continue
		}
		anchors = append(anchors, housePTRDateAnchor{Top: box.Top, Date: date})
	}
	return anchors
}

func housePTRAnchorLowerBound(anchors []housePTRDateAnchor, index int) float64 {
	if index == 0 {
		return -1 << 62
	}
	return (anchors[index-1].Top + anchors[index].Top) / 2
}

func canonicalHouseOwner(value string) string {
	value = strings.ToUpper(strings.Join(strings.Fields(value), " "))
	switch value {
	case "", "SELF":
		return "Self"
	case "SP", "SPOUSE":
		return "Spouse"
	case "DC", "DEPENDENT", "DEPENDENT CHILD":
		return "Dependent Child"
	case "JT", "JOINT", "JOINTLY":
		return "Joint"
	default:
		return strings.TrimSpace(value)
	}
}

func wrappedHouseAsset(boxes []TextBox, y, lowerBound float64) string {
	seen := make(map[int]string)
	for _, box := range boxes {
		if box.Top < lowerBound || box.Top < y-32 || box.Top >= y-3 || box.Left < 95 || box.Left >= 260 {
			continue
		}
		key := int(box.Top * 2)
		line := joinHouseBoxes(boxes, box.Top, 2.5, 95, 260)
		if strings.Contains(line, "[") || tickerPattern.MatchString(line) {
			seen[key] = line
		}
	}
	keys := make([]int, 0, len(seen))
	for key := range seen {
		keys = append(keys, key)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(keys)))
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		if len(parts) == 0 || parts[len(parts)-1] != seen[key] {
			parts = append(parts, seen[key])
		}
	}
	return strings.Join(parts, " ")
}

func joinHouseBoxes(boxes []TextBox, y, tolerance, minX, maxX float64) string {
	var selected []TextBox
	for _, box := range boxes {
		if box.Left >= minX && box.Left < maxX && abs(box.Top-y) <= tolerance {
			selected = append(selected, box)
		}
	}
	sort.SliceStable(selected, func(i, j int) bool { return selected[i].Left < selected[j].Left })
	parts := make([]string, 0, len(selected))
	for _, box := range selected {
		parts = append(parts, box.Text)
	}
	return strings.Join(parts, " ")
}

func nearestWrappedHouseText(
	boxes []TextBox, y, lowerBound, minX, maxX float64, accept func(string) bool,
) string {
	bestY := -1.0
	for _, box := range boxes {
		if box.Top >= lowerBound && box.Top >= y-28 && box.Top < y-3 && box.Left >= minX && box.Left < maxX && box.Top > bestY {
			line := joinHouseBoxes(boxes, box.Top, 2.5, minX, maxX)
			if accept(line) {
				bestY = box.Top
			}
		}
	}
	if bestY < 0 {
		return ""
	}
	return joinHouseBoxes(boxes, bestY, 2.5, minX, maxX)
}

func abs(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}
