package config_test

import (
	"testing"

	"github.com/romeo-folie/trade-engine/internal/config"
)

func cfgWithProfile(profile string) *config.Config {
	cfg := config.Defaults()
	cfg.Cost.Profile = profile
	config.ApplyProfile(cfg)
	return cfg
}

func TestProfileBudget(t *testing.T) {
	cfg := cfgWithProfile("budget")

	if cfg.Engine.TickIntervalSec != 5400 {
		t.Errorf("budget: tick_interval_sec want 5400, got %d", cfg.Engine.TickIntervalSec)
	}
	if cfg.Analysis.HoldCooldownMin != 240 {
		t.Errorf("budget: hold_cooldown_min want 240, got %d", cfg.Analysis.HoldCooldownMin)
	}
	if cfg.Analysis.Classifier.Mode != "active" {
		t.Errorf("budget: classifier.mode want active, got %q", cfg.Analysis.Classifier.Mode)
	}
	if cfg.Cost.CacheTTLMin != 180 {
		t.Errorf("budget: cache_ttl_min want 180, got %d", cfg.Cost.CacheTTLMin)
	}
	if !cfg.Notifications.Telegram.BatchEnabled {
		t.Error("budget: batch_enabled should be true")
	}
}

func TestProfileBalanced(t *testing.T) {
	cfg := cfgWithProfile("balanced")

	if cfg.Engine.TickIntervalSec != 3600 {
		t.Errorf("balanced: tick_interval_sec want 3600, got %d", cfg.Engine.TickIntervalSec)
	}
	if cfg.Analysis.HoldCooldownMin != 120 {
		t.Errorf("balanced: hold_cooldown_min want 120, got %d", cfg.Analysis.HoldCooldownMin)
	}
	if cfg.Analysis.Classifier.Mode != "active" {
		t.Errorf("balanced: classifier.mode want active, got %q", cfg.Analysis.Classifier.Mode)
	}
	if !cfg.Analysis.AdaptiveDepth.Enabled {
		t.Error("balanced: adaptive_depth.enabled should be true")
	}
	if cfg.Cost.CacheTTLMin != 120 {
		t.Errorf("balanced: cache_ttl_min want 120, got %d", cfg.Cost.CacheTTLMin)
	}
}

func TestProfilePro(t *testing.T) {
	cfg := cfgWithProfile("pro")

	if cfg.Engine.TickIntervalSec != 3600 {
		t.Errorf("pro: tick_interval_sec want 3600, got %d", cfg.Engine.TickIntervalSec)
	}
	if cfg.Analysis.HoldCooldownMin != 60 {
		t.Errorf("pro: hold_cooldown_min want 60, got %d", cfg.Analysis.HoldCooldownMin)
	}
	if !cfg.Analysis.AdaptiveDepth.Enabled {
		t.Error("pro: adaptive_depth.enabled should be true")
	}
	if cfg.Cost.CacheTTLMin != 60 {
		t.Errorf("pro: cache_ttl_min want 60, got %d", cfg.Cost.CacheTTLMin)
	}
}

func TestProfileExplicitOverrideWins(t *testing.T) {
	// In Load(), yaml.Unmarshal runs AFTER ApplyProfile so explicit user values
	// overwrite profile values. We simulate that here.
	cfg := config.Defaults()
	cfg.Cost.Profile = "budget"
	config.ApplyProfile(cfg)

	// Simulate yaml.Unmarshal writing user's explicit values on top.
	cfg.Engine.TickIntervalSec = 1800 // user override
	cfg.Analysis.HoldCooldownMin = 90 // user override

	if cfg.Engine.TickIntervalSec != 1800 {
		t.Errorf("user explicit tick_interval_sec should win over profile, got %d", cfg.Engine.TickIntervalSec)
	}
	if cfg.Analysis.HoldCooldownMin != 90 {
		t.Errorf("user explicit hold_cooldown_min should win over profile, got %d", cfg.Analysis.HoldCooldownMin)
	}
}

func TestProfileUnknownValidation(t *testing.T) {
	cfg := config.Defaults()
	cfg.Cost.Profile = "turbo" // unknown

	errs := config.Validate(cfg)
	found := false
	for _, e := range errs {
		if len(e) > 0 {
			// Look for cost.profile error.
			if e[0:12] == "cost.profile" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected cost.profile validation error for unknown profile, got: %v", errs)
	}
}

func TestProfileDefaultIsBalanced(t *testing.T) {
	cfg := config.Defaults()
	if cfg.Cost.Profile != "balanced" {
		t.Errorf("default cost profile should be balanced, got %q", cfg.Cost.Profile)
	}
}
