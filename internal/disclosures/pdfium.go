package disclosures

import (
	"fmt"
	"strings"
	"time"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/tetratelabs/wazero"
)

// PDFiumExtractor uses the embedded PDFium WebAssembly runtime. The empty
// filesystem configuration intentionally exposes no host paths to untrusted
// filing PDFs; documents are loaded from memory.
type PDFiumExtractor struct {
	pool pdfium.Pool
}

func NewPDFiumExtractor() (*PDFiumExtractor, error) {
	pool, err := webassembly.Init(webassembly.Config{
		MaxTotal:      1,
		FSConfig:      wazero.NewFSConfig(),
		RuntimeConfig: wazero.NewRuntimeConfig().WithMemoryLimitPages(8192),
		// Do not reuse workers: WebAssembly cannot return grown memory to the
		// operating system until the instance closes.
		ReuseWorkers: false,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize PDFium WebAssembly: %w", err)
	}
	return &PDFiumExtractor{pool: pool}, nil
}

func (e *PDFiumExtractor) Close() error {
	if e == nil || e.pool == nil {
		return nil
	}
	return e.pool.Close()
}

func (e *PDFiumExtractor) Extract(data []byte) ([]PageText, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("PDF is empty")
	}
	instance, err := e.pool.GetInstance(time.Minute)
	if err != nil {
		return nil, fmt.Errorf("claim PDFium instance: %w", err)
	}
	defer instance.Close() //nolint:errcheck
	document, err := instance.OpenDocument(&requests.OpenDocument{File: &data})
	if err != nil {
		return nil, fmt.Errorf("open PDF document: %w", err)
	}
	defer instance.FPDF_CloseDocument(&requests.FPDF_CloseDocument{
		Document: document.Document,
	}) //nolint:errcheck
	count, err := instance.FPDF_GetPageCount(&requests.FPDF_GetPageCount{
		Document: document.Document,
	})
	if err != nil {
		return nil, fmt.Errorf("count PDF pages: %w", err)
	}
	pages := make([]PageText, 0, count.PageCount)
	for pageIndex := 0; pageIndex < count.PageCount; pageIndex++ {
		structured, err := instance.GetPageTextStructured(
			&requests.GetPageTextStructured{
				Page: requests.Page{ByIndex: &requests.PageByIndex{
					Document: document.Document, Index: pageIndex,
				}},
				Mode: requests.GetPageTextStructuredModeRects,
			},
		)
		if err != nil {
			return nil, fmt.Errorf("extract PDF page %d: %w", pageIndex+1, err)
		}
		page := PageText{Number: pageIndex + 1}
		for _, rect := range structured.Rects {
			text := strings.TrimSpace(strings.ReplaceAll(rect.Text, "\x00", ""))
			if text == "" {
				continue
			}
			position := rect.PointPosition
			page.Boxes = append(page.Boxes, TextBox{
				Text: text, Left: position.Left, Top: position.Top,
				Right: position.Right, Bottom: position.Bottom,
			})
		}
		pages = append(pages, page)
	}
	return pages, nil
}
