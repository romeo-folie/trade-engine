package disclosures

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	dollarNumberPattern = regexp.MustCompile(`\$\s*([0-9][0-9,]*)`)
	tickerPattern       = regexp.MustCompile(`\(([A-Z][A-Z0-9.-]{0,9})\)`)
	assetTypePattern    = regexp.MustCompile(`\[([A-Z]{1,5})\]`)
)

var nonTickerParentheticals = map[string]struct{}{
	"IRA": {}, "ROTH": {}, "ETF": {}, "ETN": {}, "EIF": {},
	"LLC": {}, "LP": {}, "L.P": {}, "TRUST": {}, "JOINT": {},
	"JT": {}, "SP": {}, "DC": {}, "DB": {}, "401K": {},
}

func parseDate(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	for _, layout := range []string{
		"01/02/2006", "1/2/2006", "2006-01-02", "01/02/2006 03:04 PM",
	} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, &time.ParseError{Value: value, Layout: "MM/DD/YYYY"}
}

func parseAmount(value string) (*int64, *int64) {
	matches := dollarNumberPattern.FindAllStringSubmatch(value, -1)
	values := make([]int64, 0, len(matches))
	for _, match := range matches {
		n, err := strconv.ParseInt(strings.ReplaceAll(match[1], ",", ""), 10, 64)
		if err == nil {
			values = append(values, n)
		}
	}
	if len(values) == 0 {
		return nil, nil
	}
	minimum := values[0]
	if len(values) == 1 || strings.Contains(strings.ToLower(value), "over") {
		return &minimum, nil
	}
	maximum := values[1]
	return &minimum, &maximum
}

func tickerFromAsset(asset string) string {
	matches := tickerPattern.FindAllStringSubmatch(strings.ToUpper(asset), -1)
	for i := len(matches) - 1; i >= 0; i-- {
		candidate := matches[i][1]
		if _, rejected := nonTickerParentheticals[candidate]; rejected {
			continue
		}
		return candidate
	}
	return ""
}

func normalizeTransactionType(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if strings.HasPrefix(value, "p (") {
		return "purchase"
	}
	if strings.HasPrefix(value, "s (") {
		return "sale"
	}
	switch value {
	case "p", "purchase", "buy":
		return "purchase"
	case "s", "sale", "sale (full)", "sale (partial)", "sell":
		return "sale"
	case "e", "exchange":
		return "exchange"
	default:
		return value
	}
}
