package reports

import (
	"strings"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/store"
)

func TestComputeDrawdownSeries(t *testing.T) {
	vals := []float64{100, 110, 90, 95, 120, 100}
	dds := computeDrawdownSeries(func(i int) float64 { return vals[i] }, len(vals))
	if len(dds) != len(vals) {
		t.Fatalf("len(dds)=%d want %d", len(dds), len(vals))
	}
	if dds[2] >= 0 {
		t.Fatalf("expected negative drawdown at index 2, got %v", dds[2])
	}
	if dds[4] != 0 {
		t.Fatalf("new peak should have 0 drawdown, got %v", dds[4])
	}
}

func TestBuildBacktestEquityCurveAndUnifiedAxis(t *testing.T) {
	trades := []store.BacktestTrade{
		{EntryTS: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ExitTS: ptrTime2(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)), PnL: ptrF2(100)},
		{EntryTS: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), ExitTS: ptrTime2(time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)), PnL: ptrF2(-50)},
	}
	pts := BuildBacktestEquityCurve(trades, 1000)
	if len(pts) < 2 {
		t.Fatalf("expected at least 2 points, got %d", len(pts))
	}

	x, yA, yB := unifiedEquityAxis(
		[]BacktestPoint{{Date: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Value: 100}},
		[]BacktestPoint{{Date: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Value: 200}},
	)
	if len(x) != 2 || len(yA) != 2 || len(yB) != 2 {
		t.Fatalf("unexpected unified axis lens: x=%d yA=%d yB=%d", len(x), len(yA), len(yB))
	}
}

func TestRenderMetricsTableEscapesHTML(t *testing.T) {
	html := RenderMetricsTable("T", []MetricRow{{Name: "<b>x</b>", Value: "<script>alert(1)</script>"}})
	if strings.Contains(html, "<script>") || strings.Contains(html, "<b>") {
		t.Fatalf("expected escaped HTML, got: %s", html)
	}
}

func TestAssembleAndRenderPage(t *testing.T) {
	p := AssemblePage("Test")
	buf, err := RenderPage(p)
	if err != nil {
		t.Fatalf("RenderPage error: %v", err)
	}
	if !strings.Contains(buf.String(), "<html") {
		t.Fatalf("rendered output not html: %s", buf.String()[:80])
	}
}

func TestFloatOpt(t *testing.T) {
	f := floatOpt(1.23)
	if f == nil {
		t.Fatal("floatOpt returned nil")
	}
	if *f != float32(1.23) {
		t.Fatalf("floatOpt value mismatch: got %v want 1.23", *f)
	}
}

func ptrTime2(t time.Time) *time.Time { return &t }
func ptrF2(v float64) *float64       { return &v }
