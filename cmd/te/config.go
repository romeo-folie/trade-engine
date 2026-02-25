package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Manage TradeEngine configuration",
}

var configShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Print the resolved configuration (secrets masked)",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		fmt.Printf("Config file: %s\n\n", config.ConfigPath())

		// Marshal to YAML and mask secrets before printing.
		data, err := yaml.Marshal(cfg)
		if err != nil {
			return err
		}
		fmt.Println(maskSecrets(string(data)))
		return nil
	},
}

var configSetRuntime bool

var configSetCmd = &cobra.Command{
	Use:   "set <key> <value>",
	Short: "Set a config value (dotted path)",
	Long: `Set a configuration value by dotted path.

Without --runtime: updates config.yaml.
With --runtime:    writes a DB override (requires DATABASE_URL).

Examples:
  te config set engine.mode paper
  te config set risk.max_position_pct 3.0
  te config set engine.mode auto --runtime`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		key, value := args[0], args[1]

		if configSetRuntime {
			ctx := context.Background()
			if err := store.Connect(ctx); err != nil {
				return fmt.Errorf("connect to database: %w", err)
			}
			defer store.Close()
			if err := store.SetOverride(ctx, key, value); err != nil {
				return fmt.Errorf("set runtime override: %w", err)
			}
			fmt.Printf("Runtime override set: %s = %q\n", key, value)
		} else {
			if err := config.SetField(key, value); err != nil {
				return fmt.Errorf("update config.yaml: %w", err)
			}
			fmt.Printf("Config updated: %s = %q\n", key, value)
		}
		return nil
	},
}

var configValidateCmd = &cobra.Command{
	Use:   "validate",
	Short: "Validate the current configuration",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		errs := config.Validate(cfg)
		if len(errs) == 0 {
			fmt.Printf("✓ Config is valid (%s)\n", config.ConfigPath())
			return nil
		}
		fmt.Printf("✗ Config validation failed (%d issue(s)):\n", len(errs))
		for _, e := range errs {
			fmt.Printf("  - %s\n", e)
		}
		return fmt.Errorf("invalid configuration")
	},
}

func init() {
	configSetCmd.Flags().BoolVar(&configSetRuntime, "runtime", false, "Write to DB override instead of YAML")
	configCmd.AddCommand(configShowCmd)
	configCmd.AddCommand(configSetCmd)
	configCmd.AddCommand(configValidateCmd)
}

// maskSecrets replaces API key and token values with "***".
func maskSecrets(input string) string {
	secretWords := []string{"api_key", "secret", "token", "password"}
	lines := strings.Split(input, "\n")
	for i, line := range lines {
		lower := strings.ToLower(line)
		for _, word := range secretWords {
			if strings.Contains(lower, word+":") {
				idx := strings.Index(line, ":")
				if idx >= 0 {
					lines[i] = line[:idx+1] + " ***"
				}
				break
			}
		}
	}
	return strings.Join(lines, "\n")
}
