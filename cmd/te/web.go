package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

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

The server connects to the same database as 'te run'. If 'te run' is also
active on this machine the WebSocket feed will carry live engine events;
otherwise the REST API surfaces historical data from the database.`,
	RunE: runWebCmd,
}

var webPort string

func init() {
	webCmd.Flags().StringVarP(&webPort, "port", "p", "8080", "Port to listen on")
}

func runWebCmd(cmd *cobra.Command, _ []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Connect to the database (needed for REST API drill-down endpoints).
	if err := store.Connect(ctx); err != nil {
		return fmt.Errorf("database connection failed: %w\n(Is DATABASE_URL set?)", err)
	}

	bus := events.New()

	srv := web.NewServer(web.Config{Port: webPort}, bus)
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
