package disclosures

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
)

func TestNewRunnerUsesSenateBrowserTransportByDefault(t *testing.T) {
	cfg := config.Defaults().Disclosures
	cfg.House.Enabled = false
	cfg.Senate.Enabled = true
	runner, err := NewRunner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close() //nolint:errcheck
	if _, ok := runner.Senate.Transport.(*SenateChromeTransport); !ok {
		t.Fatalf("transport = %T", runner.Senate.Transport)
	}
}

func TestNewRunnerAllowsExplicitSenateHTTPFallback(t *testing.T) {
	cfg := config.Defaults().Disclosures
	cfg.House.Enabled = false
	cfg.Senate.Enabled = true
	cfg.Senate.Transport = "http"
	runner, err := NewRunner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close() //nolint:errcheck
	if runner.Senate.Transport != nil {
		t.Fatalf("transport = %T, want legacy HTTP", runner.Senate.Transport)
	}
}
