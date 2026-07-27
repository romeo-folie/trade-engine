package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/store"
	"github.com/romeo-folie/trade-engine/internal/web"
)

var webCmd = &cobra.Command{
	Use:   "web",
	Short: "Start the real-time web observability interface",
	Long: `Starts the TradeEngine observability web server.

Opening http://localhost:<port> in a browser shows a live, animated data-flow
graph visualizing activity flowing through the engine pipeline in real time.

The server creates its own in-process event bus and connects to the same
database as 'te run'. The REST API surfaces historical data from the database.
Note: events emitted by a separate 'te run' process are NOT forwarded over
the WebSocket — only events generated within this process are streamed live.
Cross-process event bridging is planned for a future release.`,
	RunE: runWebCmd,
}

var webPort string

func init() {
	webCmd.Flags().StringVarP(&webPort, "port", "p", "8080", "Port to listen on")
}

// webServerConfig builds the standalone web server's configuration.
//
// EngineConfig must be carried through: internal/web derives the pipeline
// snapshot (nodes, edges, analysis mode) from it. A nil config makes the
// snapshot fall back to a generic single-analyst graph, which silently
// misrepresents an ensemble or specialist-mode deployment.
func webServerConfig(port string, cfg *config.Config) web.Config {
	return web.Config{
		Port:           port,
		Host:           "127.0.0.1",
		AllowedOrigins: []string{"http://localhost:" + port, "http://127.0.0.1:" + port},
		EngineConfig:   cfg,
	}
}

func runWebCmd(cmd *cobra.Command, _ []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Load engine config so /api/system/config reflects real values instead of
	// 503ing, and so the pipeline snapshot describes the configured topology.
	// Non-fatal: the UI still loads without it, but on a nil config the snapshot
	// degrades to the single-analyst fallback graph.
	cfg, err := config.Load()
	if err != nil {
		cfg = nil
		cmd.PrintErrf("Warning: config unavailable — settings page will show no data (%v)\n", err)
	}

	// Connect to the database (needed for REST API drill-down endpoints).
	// Non-fatal: if DATABASE_URL is unset or unreachable, the UI still loads
	// but historical data endpoints return empty results.
	if err := store.Connect(ctx); err != nil {
		cmd.PrintErrf("Warning: database unavailable — REST API will return empty results (%v)\n", err)
	} else {
		defer store.Close()
	}

	bus := events.New()

	srv := web.NewServer(webServerConfig(webPort, cfg), bus, nil)
	if err := srv.Start(ctx); err != nil {
		return fmt.Errorf("web server failed to start: %w", err)
	}

	cmd.Printf("🌐 TradeEngine Web started — http://localhost:%s\n", webPort)
	cmd.Printf("   WebSocket: ws://localhost:%s/ws\n", webPort)
	cmd.Printf("   Press Ctrl+C to stop.\n\n")

	<-ctx.Done()
	srv.Stop()
	cmd.Println("Web server stopped.")
	return nil
}
