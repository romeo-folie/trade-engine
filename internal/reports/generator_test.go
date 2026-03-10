package reports

import (
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
)

func TestNewAndWriteReader(t *testing.T) {
	tmp := t.TempDir()
	g, err := New(config.ReportsConfig{OutputDir: tmp})
	if err != nil {
		t.Fatalf("New error: %v", err)
	}
	if g.OutputDir() != tmp {
		t.Fatalf("OutputDir=%q want %q", g.OutputDir(), tmp)
	}

	path, err := g.WriteReader("x.html", strings.NewReader("<html><body>ok</body></html>"))
	if err != nil {
		t.Fatalf("WriteReader error: %v", err)
	}
	if !strings.HasSuffix(path, "/x.html") {
		t.Fatalf("unexpected path: %s", path)
	}
}

func TestInlineScriptsUsesCacheAndLeavesRelative(t *testing.T) {
	g := &Generator{scriptMap: map[string]string{"https://cdn.example/app.js": "console.log('ok')"}}
	in := `<html><head><script src="https://cdn.example/app.js"></script><script src="/local.js"></script></head></html>`
	out := g.inlineScripts(in)
	if !strings.Contains(out, "console.log('ok')") {
		t.Fatalf("expected inlined cached script: %s", out)
	}
	if !strings.Contains(out, `<script src="/local.js"></script>`) {
		t.Fatalf("expected relative script untouched: %s", out)
	}
}

func TestExpandHomeAndReportFilename(t *testing.T) {
	if got := expandHome("/tmp/x"); got != "/tmp/x" {
		t.Fatalf("expandHome absolute changed unexpectedly: %q", got)
	}
	name := ReportFilename("daily")
	if !strings.HasPrefix(name, "daily-") || !strings.HasSuffix(name, ".html") {
		t.Fatalf("unexpected report filename: %s", name)
	}
	if _, err := time.Parse("2006-01-02T150405", strings.TrimSuffix(strings.TrimPrefix(name, "daily-"), ".html")); err != nil {
		t.Fatalf("timestamp format invalid: %v", err)
	}
}
