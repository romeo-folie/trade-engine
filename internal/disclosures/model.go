package disclosures

import (
	"context"
	"sync"
	"time"
)

type Report struct {
	Source           string
	ExternalID       string
	Chamber          string
	FilerName        string
	Office           string
	ReportType       string
	FilingDate       time.Time
	DocumentURL      string
	ContentHash      string
	ExtractionStatus string
	ExtractionError  string
	RetryableFailure bool
	RawMetadata      []byte
	Transactions     []Transaction
}

type Transaction struct {
	Ordinal          int
	Owner            string
	AssetName        string
	Ticker           string
	AssetType        string
	TransactionType  string
	TransactionDate  time.Time
	NotificationDate *time.Time
	AmountMin        *int64
	AmountMax        *int64
	AmountText       string
	Comment          string
	RawData          []byte
}

type TextBox struct {
	Text                     string
	Left, Top, Right, Bottom float64
}

type PageText struct {
	Number int
	Boxes  []TextBox
}

type PDFExtractor interface {
	Extract(pdf []byte) ([]PageText, error)
	Close() error
}

// requestPacer serializes source requests and guarantees a minimum delay
// between their start times. Each client owns its pacer so House and Senate do
// not unnecessarily block one another.
type requestPacer struct {
	mu   sync.Mutex
	last time.Time
}

func (p *requestPacer) Wait(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if remaining := time.Until(p.last.Add(delay)); remaining > 0 {
		timer := time.NewTimer(remaining)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	p.last = time.Now()
	return nil
}
