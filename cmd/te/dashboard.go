package main

import (
	"context"
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/romeo-folie/trade-engine/internal/tui"
	"github.com/spf13/cobra"
)

func runDashboard(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// Resolve mode: DB override > config.
	mode := cfg.Engine.Mode
	if err := store.Connect(context.Background()); err == nil {
		defer store.Close()
		if dbMode, ok, _ := store.GetOverride(context.Background(), "engine.mode"); ok {
			mode = dbMode
		}
	}

	// Create a standalone event bus for the TUI.
	// When launched alongside the engine, we'd share the engine's bus;
	// for now the dashboard works in standalone mode with DB polling.
	bus := events.New()

	model := tui.New(bus, mode)
	p := tea.NewProgram(model, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("dashboard: %w", err)
	}
	return nil
}
