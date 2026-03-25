package main

import (
	"os"
	_ "time/tzdata" // Embed IANA timezone database so LoadLocation works in minimal/container environments.

	"github.com/joho/godotenv"
	"github.com/romeo-folie/trade-engine/internal/logger"
)

func main() {
	// Auto-load .env from the current working directory (silent — no error if absent).
	// This allows `te run`, `te dashboard`, etc. to work without manually
	// exporting DATABASE_URL and other vars from the project's .env file.
	_ = godotenv.Load()

	// Logger is initialised lazily by individual commands after config loads.
	// Ensure it gets flushed on exit.
	defer logger.Close()

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
