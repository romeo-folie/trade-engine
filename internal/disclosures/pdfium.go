package disclosures

import (
	"context"
	"fmt"
	"strings"
	"sync"

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
		MaxTotal: 1,
		FSConfig: wazero.NewFSConfig(),
		// Closing the WebAssembly worker on cancellation lets Kill interrupt a
		// malformed PDF that would otherwise leave a scheduled sync hung.
		RuntimeConfig: wazero.NewRuntimeConfig().WithMemoryLimitPages(8192).WithCloseOnContextDone(true),
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

func (e *PDFiumExtractor) Extract(ctx context.Context, data []byte) ([]PageText, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("PDF is empty")
	}
	instance, err := e.pool.GetInstanceWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("claim PDFium instance: %w", err)
	}
	stopKill := make(chan struct{})
	killed := make(chan struct{})
	var killOnce sync.Once
	kill := func() {
		killOnce.Do(func() {
			// Kill invalidates this worker, rather than returning a potentially
			// wedged WebAssembly instance to the one-worker pool.
			_ = instance.Kill()
		})
	}
	go func() {
		defer close(killed)
		select {
		case <-ctx.Done():
			kill()
		case <-stopKill:
		}
	}()
	defer func() {
		if ctx.Err() == nil {
			close(stopKill)
		} else {
			kill()
		}
		<-killed
		if ctx.Err() == nil {
			_ = instance.Close()
		} else {
			// A cancellation can race the normal-stop signal above; make the
			// worker invalidation deterministic either way.
			kill()
		}
	}()
	document, err := instance.OpenDocument(&requests.OpenDocument{File: &data})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("open PDF document: %w", err)
	}
	defer instance.FPDF_CloseDocument(&requests.FPDF_CloseDocument{
		Document: document.Document,
	}) //nolint:errcheck
	count, err := instance.FPDF_GetPageCount(&requests.FPDF_GetPageCount{
		Document: document.Document,
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("count PDF pages: %w", err)
	}
	pages := make([]PageText, 0, count.PageCount)
	for pageIndex := 0; pageIndex < count.PageCount; pageIndex++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		structured, err := instance.GetPageTextStructured(
			&requests.GetPageTextStructured{
				Page: requests.Page{ByIndex: &requests.PageByIndex{
					Document: document.Document, Index: pageIndex,
				}},
				Mode: requests.GetPageTextStructuredModeRects,
			},
		)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return pages, nil
}
