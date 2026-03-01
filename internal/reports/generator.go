// Package reports generates self-contained HTML performance reports
// using go-echarts for interactive charts.
package reports

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-echarts/go-echarts/v2/components"
	"github.com/romeo-folie/trade-engine/internal/config"
)

// ── Generator ─────────────────────────────────────────────────────────────────

// Generator manages report generation: resolves output paths, inlines chart
// assets for offline use, and optionally opens the resulting file in a browser.
type Generator struct {
	cfg       config.ReportsConfig
	outputDir string

	mu        sync.Mutex
	scriptMap map[string]string // CDN URL → JS content (cached)
}

// New creates a Generator backed by the given reports config.
func New(cfg config.ReportsConfig) (*Generator, error) {
	dir := expandHome(cfg.OutputDir)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("create reports dir: %w", err)
	}
	return &Generator{cfg: cfg, outputDir: dir, scriptMap: make(map[string]string)}, nil
}

// WritePage renders the provided Page with optional header/content injection,
// inlines all external JS assets, writes the file, and optionally opens it.
// It returns the absolute path of the written file.
//
// headerExtra is injected into <head> (e.g. custom <style> blocks).
// bodyPrefix is injected immediately after <body> (e.g. metrics tables).
func (g *Generator) WritePage(
	filename string,
	page *components.Page,
	headerExtra string,
	bodyPrefix string,
) (string, error) {
	var raw bytes.Buffer
	if err := page.Render(&raw); err != nil {
		return "", fmt.Errorf("render page: %w", err)
	}
	html := raw.String()

	// Inject custom CSS / header content before </head>.
	if headerExtra != "" {
		html = strings.Replace(html, "</head>", darkPageCSS()+"\n"+headerExtra+"\n</head>", 1)
	} else {
		html = strings.Replace(html, "</head>", darkPageCSS()+"\n</head>", 1)
	}

	// Inject content immediately after <body>.
	if bodyPrefix != "" {
		html = strings.Replace(html, "<body>", "<body>\n"+bodyPrefix, 1)
	}

	// Inline all external <script src="..."> assets.
	html = g.inlineScripts(html)

	path := filepath.Join(g.outputDir, filename)
	if err := os.WriteFile(path, []byte(html), 0o640); err != nil {
		return "", fmt.Errorf("write report: %w", err)
	}

	if g.cfg.AutoOpenBrowser {
		_ = openBrowser(path)
	}

	return path, nil
}

// WriteReader renders an arbitrary io.Reader as an HTML file (raw passthrough
// with script inlining). Useful when generating HTML without go-echarts Page.
func (g *Generator) WriteReader(filename string, r io.Reader) (string, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("read report content: %w", err)
	}
	html := g.inlineScripts(string(raw))

	path := filepath.Join(g.outputDir, filename)
	if err := os.WriteFile(path, []byte(html), 0o640); err != nil {
		return "", fmt.Errorf("write report: %w", err)
	}
	if g.cfg.AutoOpenBrowser {
		_ = openBrowser(path)
	}
	return path, nil
}

// OutputDir returns the resolved output directory path.
func (g *Generator) OutputDir() string { return g.outputDir }

// ── Asset inlining ────────────────────────────────────────────────────────────

// scriptSrcPattern matches <script src="..."></script> tags.
const scriptSrcPattern = `<script src="`

// inlineScripts replaces remote <script src="..."> tags with inline <script>
// blocks so that the resulting HTML works without any internet connection.
func (g *Generator) inlineScripts(html string) string {
	var result strings.Builder
	remaining := html
	for {
		idx := strings.Index(remaining, scriptSrcPattern)
		if idx < 0 {
			result.WriteString(remaining)
			break
		}
		result.WriteString(remaining[:idx])
		after := remaining[idx+len(scriptSrcPattern):]

		end := strings.Index(after, `"></script>`)
		if end < 0 {
			result.WriteString(scriptSrcPattern)
			remaining = after
			continue
		}
		url := after[:end]
		remaining = after[end+len(`"></script>`):]

		content, err := g.fetchScript(url)
		if err != nil {
			// Fall back to original CDN tag on failure.
			result.WriteString(scriptSrcPattern)
			result.WriteString(url)
			result.WriteString(`"></script>`)
			continue
		}
		result.WriteString("<script>\n")
		result.WriteString(content)
		result.WriteString("\n</script>")
	}
	return result.String()
}

// fetchScript returns JS content for the given URL, caching after first fetch.
func (g *Generator) fetchScript(url string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if content, ok := g.scriptMap[url]; ok {
		return content, nil
	}

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, url)
	}

	var buf bytes.Buffer
	if _, err := io.Copy(&buf, resp.Body); err != nil {
		return "", err
	}

	content := buf.String()
	g.scriptMap[url] = content
	return content, nil
}

// ── Dark page CSS ─────────────────────────────────────────────────────────────

func darkPageCSS() string {
	return `<style>
body, html { background: #1a1a2e; color: #e0e0e0; font-family: 'Segoe UI', system-ui, monospace; margin: 0; padding: 0; }
h1, h2, h3 { color: #00bcd4; margin: 1rem 1.5rem 0.5rem; }
.report-header { background: #0d0d1f; padding: 1.5rem 1.5rem; border-bottom: 1px solid #2a2a4a; }
.report-header h1 { color: #00bcd4; margin: 0 0 0.25rem; font-size: 1.6rem; }
.report-header .subtitle { color: #90a4ae; font-size: 0.9rem; }
.metrics-grid { display: flex; flex-wrap: wrap; gap: 0.75rem; padding: 1rem 1.5rem; }
.metrics-block { background: #0d0d1f; border: 1px solid #2a2a4a; border-radius: 6px; padding: 1rem 1.2rem; min-width: 280px; flex: 1; }
.metrics-block h3 { color: #00bcd4; margin: 0 0 0.75rem; font-size: 0.95rem; text-transform: uppercase; letter-spacing: 0.05em; }
.metrics-table { border-collapse: collapse; width: 100%; font-size: 0.88rem; }
.metrics-table td { padding: 0.3rem 0.5rem; border-bottom: 1px solid #2a2a4a; }
.metric-name { color: #90a4ae; width: 60%; }
.metric-value { color: #e0e0e0; font-weight: 600; text-align: right; }
.metric-value.positive { color: #00e676; }
.metric-value.negative { color: #ff1744; }
.box { background: #1a1a2e !important; }
.section-title { color: #90a4ae; font-size: 0.75rem; text-transform: uppercase; letter-spacing: 0.1em; padding: 0.5rem 1.5rem; margin-top: 1rem; }
.compare-grid { display: grid; grid-template-columns: 1fr 1fr; gap: 1rem; padding: 1rem 1.5rem; }
.compare-col { background: #0d0d1f; border: 1px solid #2a2a4a; border-radius: 6px; padding: 1rem; }
.compare-col h3 { color: #ffc107; margin-top: 0; }
</style>`
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func openBrowser(path string) error {
	return exec.Command("open", path).Start()
}

// ReportFilename generates a timestamped filename for a report type.
func ReportFilename(kind string) string {
	return fmt.Sprintf("%s-%s.html", kind, time.Now().Format("2006-01-02T150405"))
}
