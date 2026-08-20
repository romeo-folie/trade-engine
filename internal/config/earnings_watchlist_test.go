package config

import "testing"

func TestEarningsWatchlistDefaultsAndValidation(t *testing.T) {
	defaults := Defaults().Scanner.EarningsWatchlist
	if !defaults.Enabled || defaults.AfterCloseDelayMinutes != 20 || defaults.CandidateCap != 5 || defaults.WatchlistSessions != 5 || defaults.BarBatchSize != 100 {
		t.Fatalf("defaults = %+v", defaults)
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Scanner.EarningsWatchlist.AfterCloseDelayMinutes = -1 },
		func(c *Config) { c.Scanner.EarningsWatchlist.AfterCloseDelayMinutes = 5 },
		func(c *Config) { c.Scanner.Universe.HistoricalFeed = "" },
		func(c *Config) { c.Scanner.EarningsWatchlist.CandidateCap = 0 },
		func(c *Config) { c.Scanner.EarningsWatchlist.WatchlistSessions = 0 },
		func(c *Config) { c.Scanner.EarningsWatchlist.BarBatchSize = 0 },
	} {
		cfg := Defaults()
		mutate(cfg)
		if errs := Validate(cfg); len(errs) == 0 {
			t.Fatal("enabled invalid earnings watchlist config passed validation")
		}
	}
}
