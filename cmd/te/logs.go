package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/spf13/cobra"
)

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "View engine logs",
}

var logsTailCmd = &cobra.Command{
	Use:   "tail",
	Short: "Stream live engine logs (tail -f)",
	RunE: func(cmd *cobra.Command, args []string) error {
		logPath := logger.LogFilePath()
		if _, err := os.Stat(logPath); os.IsNotExist(err) {
			return fmt.Errorf("log file not found: %s\nStart the engine first with 'te run'.", logPath)
		}
		fmt.Printf("Tailing %s (Ctrl+C to stop)\n\n", logPath)
		c := exec.Command("tail", "-f", logPath)
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		return c.Run()
	},
}

var logsSearchCmd = &cobra.Command{
	Use:   "search <query>",
	Short: "Search engine logs",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		logPath := logger.LogFilePath()
		if _, err := os.Stat(logPath); os.IsNotExist(err) {
			return fmt.Errorf("log file not found: %s", logPath)
		}
		c := exec.Command("grep", "-i", args[0], logPath)
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if err := c.Run(); err != nil {
			// grep exits 1 when no matches — not a real error.
			if c.ProcessState != nil && c.ProcessState.ExitCode() == 1 {
				fmt.Printf("No matches for %q in %s\n", args[0], logPath)
				return nil
			}
			return err
		}
		return nil
	},
}

func init() {
	logsCmd.AddCommand(logsTailCmd)
	logsCmd.AddCommand(logsSearchCmd)
}
