package main

import (
	"strings"
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/data"
)

func TestUniverseFundamentalsProvider(t *testing.T) {
	t.Run("constructs Finnhub only with Finnhub credentials", func(t *testing.T) {
		provider, err := universeFundamentalsProvider(config.NewsAPIConfig{
			Provider:  "finnhub",
			APIKeyEnv: "FINNHUB_API_KEY",
			APIKey:    "finnhub-token",
		})
		if err != nil {
			t.Fatalf("universeFundamentalsProvider: %v", err)
		}
		if _, ok := provider.(*data.FinnhubClient); !ok {
			t.Fatalf("provider = %T, want *data.FinnhubClient", provider)
		}
	})

	t.Run("rejects a Benzinga key before constructing Finnhub", func(t *testing.T) {
		provider, err := universeFundamentalsProvider(config.NewsAPIConfig{
			Provider:  "benzinga",
			APIKeyEnv: "BENZINGA_API_KEY",
			APIKey:    "benzinga-token",
		})
		if err == nil {
			t.Fatal("expected provider mismatch error")
		}
		if provider != nil {
			t.Fatalf("provider = %T, want nil on provider mismatch", provider)
		}
		if !strings.Contains(err.Error(), `markets.news.provider must be "finnhub"`) ||
			!strings.Contains(err.Error(), `"benzinga"`) {
			t.Fatalf("provider mismatch error is not actionable: %v", err)
		}
	})

	t.Run("requires configured Finnhub credentials", func(t *testing.T) {
		provider, err := universeFundamentalsProvider(config.NewsAPIConfig{
			Provider:  "finnhub",
			APIKeyEnv: "MY_FINNHUB_KEY",
		})
		if err == nil {
			t.Fatal("expected missing credentials error")
		}
		if provider != nil {
			t.Fatalf("provider = %T, want nil without credentials", provider)
		}
		if !strings.Contains(err.Error(), "export MY_FINNHUB_KEY") {
			t.Fatalf("missing credentials error is not actionable: %v", err)
		}
	})

	t.Run("uses standard env name when config omits it", func(t *testing.T) {
		_, err := universeFundamentalsProvider(config.NewsAPIConfig{Provider: "finnhub"})
		if err == nil || !strings.Contains(err.Error(), "export FINNHUB_API_KEY") {
			t.Fatalf("missing credentials error = %v, want FINNHUB_API_KEY guidance", err)
		}
	})
}
