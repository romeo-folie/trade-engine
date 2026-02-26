package main

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/tui"
	"github.com/spf13/cobra"
)

func runDashboard(cmd *cobra.Command, args []string) error {
	// Create a standalone event bus for the TUI.
	// When launched alongside the engine, we'd share the engine's bus;
	// for now the dashboard works in standalone mode with DB polling.
	bus := events.New()

	model := tui.New(bus)
	p := tea.NewProgram(model, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("dashboard: %w", err)
	}
	return nil
}
