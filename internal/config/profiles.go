package config

// ApplyProfile overlays profile-driven defaults onto cfg. In Load(), the call
// sequence is:
//
//  1. Defaults()       — baseline safe values
//  2. peek profile key — extract cost.profile from YAML without full parse
//  3. ApplyProfile()   — profile-specific values (overwrites baseline)
//  4. yaml.Unmarshal() — user's explicit values (always win, overwrite profile)
//
// ApplyProfile always sets profile-owned fields so the profile contract is
// guaranteed when no explicit value exists in the user's config file.
//
// Profiles (PRD §8):
//
//	budget:   Slowest tick, cheapest analysts, aggressive caching, strict batch.
//	balanced: Default. Moderate tick, adaptive depth 1–2, 120m cache TTL.
//	pro:      Full speed, adaptive depth 1–3, 60m cache TTL, standard arbiter.
func ApplyProfile(cfg *Config) {
	switch cfg.Cost.Profile {
	case "budget":
		applyBudgetProfile(cfg)
	case "pro":
		applyProProfile(cfg)
	default:
		// "balanced" is the default; unknown values fall here (validated separately).
		applyBalancedProfile(cfg)
	}
}

func applyBudgetProfile(cfg *Config) {
	cfg.Engine.TickIntervalSec = 5400 // 90m
	cfg.Analysis.HoldCooldownMin = 240
	cfg.Analysis.Classifier.Mode = "active"
	cfg.Analysis.AdaptiveDepth.Enabled = false // depth always 1
	cfg.Cost.CacheTTLMin = 180
	cfg.Notifications.Telegram.BatchEnabled = true
}

func applyBalancedProfile(cfg *Config) {
	cfg.Engine.TickIntervalSec = 3600 // 60m
	cfg.Analysis.HoldCooldownMin = 120
	cfg.Analysis.Classifier.Mode = "active"
	cfg.Analysis.AdaptiveDepth.Enabled = true // depth 1–2 adaptive
	cfg.Cost.CacheTTLMin = 120
	cfg.Notifications.Telegram.BatchEnabled = true
}

func applyProProfile(cfg *Config) {
	cfg.Engine.TickIntervalSec = 3600 // 60m
	cfg.Analysis.HoldCooldownMin = 60
	cfg.Analysis.Classifier.Mode = "active"
	cfg.Analysis.AdaptiveDepth.Enabled = true // full depth 1–3
	cfg.Cost.CacheTTLMin = 60
	// BatchEnabled not forced at pro — leave as configured.
}
