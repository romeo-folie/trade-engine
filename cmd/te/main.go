package main

import (
	"os"
	_ "time/tzdata" // Embed IANA timezone database so LoadLocation works in minimal/container environments.

	"github.com/romeo-folie/trade-engine/internal/logger"
)

func main() {
	// Logger is initialised lazily by individual commands after config loads.
	// Ensure it gets flushed on exit.
	defer logger.Close()

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
