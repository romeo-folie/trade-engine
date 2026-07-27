package config

import (
	"strings"
	"testing"
)

func TestDefaultsPremarketEpisodicDisabledAndSIP(t *testing.T) {
	got := Defaults().Scanner.PremarketEpisodic
	if got.Enabled || got.PromoteCandidates {
		t.Fatalf("premarket scanner must default off: %+v", got)
	}
	if got.Feed != "sip" || got.HardCutoff != "09:25" ||
		len(got.Checkpoints) != 3 {
		t.Fatalf("unsafe premarket defaults: %+v", got)
	}
}

func TestValidatePremarketEpisodic(t *testing.T) {
	valid := Defaults()
	valid.Scanner.Enabled = true
	valid.Scanner.Universe.Enabled = true
	valid.Scanner.PremarketEpisodic.Enabled = true
	if errs := Validate(valid); containsStr(errs, "scanner.premarket_episodic") {
		t.Fatalf("valid premarket config: %v", errs)
	}

	tests := []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"scanner disabled", func(c *Config) { c.Scanner.Enabled = false }, "requires scanner.enabled"},
		{"universe disabled", func(c *Config) { c.Scanner.Universe.Enabled = false }, "requires scanner.universe.enabled"},
		{"iex feed", func(c *Config) { c.Scanner.PremarketEpisodic.Feed = "iex" }, "feed must be sip"},
		{"bad cutoff", func(c *Config) { c.Scanner.PremarketEpisodic.HardCutoff = "9:25" }, "hard_cutoff"},
		{"cutoff at open", func(c *Config) { c.Scanner.PremarketEpisodic.HardCutoff = "09:30" }, "before 09:30"},
		{"checkpoint after cutoff", func(c *Config) {
			c.Scanner.PremarketEpisodic.Checkpoints = []string{"09:26"}
		}, "precede hard_cutoff"},
		{"checkpoint before feed session", func(c *Config) {
			c.Scanner.PremarketEpisodic.Checkpoints = []string{"03:59"}
		}, "at or after 04:00"},
		{"duplicate checkpoint", func(c *Config) {
			c.Scanner.PremarketEpisodic.Checkpoints = []string{"09:00", "09:00"}
		}, "must be unique"},
		{"bar coverage", func(c *Config) {
			c.Scanner.PremarketEpisodic.MinBarCoverage = 1.1
		}, "min_bar_coverage"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := *valid
			cfg.Scanner.PremarketEpisodic.Checkpoints =
				append([]string(nil), valid.Scanner.PremarketEpisodic.Checkpoints...)
			tc.edit(&cfg)
			errs := Validate(&cfg)
			if len(errs) == 0 || !strings.Contains(strings.Join(errs, "\n"), tc.want) {
				t.Fatalf("Validate() = %v; want %q", errs, tc.want)
			}
		})
	}
}
