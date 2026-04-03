// Package web implements the TradeEngine observability web server (Phase 14).
//
// Usage:
//
//	srv := web.NewServer(web.Config{Port: "8080", Host: "127.0.0.1"}, bus)
//	if err := srv.Start(ctx); err != nil { ... }
//	defer srv.Stop()
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"

	appconfig "github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/logger"
	"github.com/romeo-folie/trade-engine/internal/portfolio"
)

// frontendFS holds the compiled React app embedded at build time.
// The embed path is relative to this file (internal/web/), so the frontend
// dist must be copied here before go build. The Makefile handles this.
//
// NOTE: //go:embed requires the "dist" directory to exist at compile time.
// For dev builds without a frontend, either:
//   - Run `cd web/frontend && npm run build` first, or
//   - Use the `nofrontend` build tag (planned) to skip embedding.
//
// At runtime, fs.Sub failing is not expected — it would indicate a build
// toolchain issue, not a missing directory.
//
//go:embed all:dist
var frontendFS embed.FS

// Config holds web-server configuration.
type Config struct {
	Port string // e.g. "8080"
	Host string // e.g. "127.0.0.1" (defaults to 127.0.0.1 for local-only safety)

	// AllowedOrigins lists the origins permitted to open WebSocket connections.
	// If empty, only same-origin requests are accepted (the nhooyr/websocket default).
	AllowedOrigins []string

	// EngineConfig optionally carries the loaded engine configuration so the
	// web layer can seed pipeline nodes from config (analyst cards, news label).
	EngineConfig *appconfig.Config
}

// Server is the TradeEngine observability web server.
// It accepts WebSocket connections on /ws and serves a React SPA on /.
// REST drill-down endpoints are registered on /api/*.
type Server struct {
	cfg        Config
	engineCfg  *appconfig.Config // nil when not provided
	bus        *events.Bus
	httpServer *http.Server

	// hub manages connected WebSocket clients.
	hub *clientHub

	// tracker provides live portfolio metrics (equity, cash, positions).
	tracker *portfolio.Tracker
}


// NewServer creates a Server with a reference to the live portfolio tracker.
func NewServer(cfg Config, bus *events.Bus, tracker *portfolio.Tracker) *Server {
	s := &Server{
		cfg:       cfg,
		engineCfg: cfg.EngineConfig,
		bus:       bus,
		hub:       newClientHub(),
		tracker:   tracker,
	}
	return s
}

// Start binds the HTTP server and begins forwarding engine events to clients.
// It blocks until the listener is up, then runs async in background goroutines.
// The caller must call Stop() or cancel ctx to shut down.
func (s *Server) Start(ctx context.Context) error {
	host := s.cfg.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := s.cfg.Port
	if port == "" {
		port = "8080"
	}
	addr := host + ":" + port

	mux := http.NewServeMux()

	// Static frontend — strip the "frontend/dist" prefix from embedded paths.
	distFS, err := fs.Sub(frontendFS, "dist")
	if err != nil {
		// Frontend assets not embedded (pre-build). Serve a placeholder page.
		mux.HandleFunc("/", serveNoFrontend)
	} else {
		mux.Handle("/", spaFallback(http.FileServer(http.FS(distFS)), distFS))
	}

	mux.HandleFunc("/ws", s.handleWebSocket)
	s.registerAPIHandlers(mux)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}

	s.httpServer = &http.Server{
		Handler:      mux,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0, // streaming; no write timeout
		IdleTimeout:  60 * time.Second,
	}

	// Broadcast engine events to all connected clients.
	go s.broadcastLoop(ctx)

	// Serve HTTP in background.
	go func() {
		logger.Info("web: server listening", "addr", ln.Addr().String())
		if err := s.httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Warn("web: server error", "err", err.Error())
		}
	}()

	return nil
}

// Stop gracefully shuts down the HTTP server.
func (s *Server) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if s.httpServer != nil {
		_ = s.httpServer.Shutdown(ctx)
	}
}

// Addr returns the configured address (not the bound address — use for display).
func (s *Server) Addr() string {
	host := s.cfg.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := s.cfg.Port
	if port == "" {
		port = "8080"
	}
	return host + ":" + port
}

// ── WebSocket handler ─────────────────────────────────────────────────────────

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	opts := &websocket.AcceptOptions{}
	if len(s.cfg.AllowedOrigins) > 0 {
		opts.OriginPatterns = s.cfg.AllowedOrigins
	}
	conn, err := websocket.Accept(w, r, opts)
	if err != nil {
		logger.Warn("web: ws accept failed", "err", err.Error())
		return
	}

	client := s.hub.add(conn)
	defer s.hub.remove(client)

	ctx := conn.CloseRead(r.Context())

	// Send initial snapshot so the UI isn't blank on first load.
	snap := buildSnapshot(ctx, s.engineCfg)
	if err := wsjson.Write(ctx, conn, snap); err != nil {
		return
	}

	// Hold connection open until closed (CloseRead drains pings/control frames).
	<-ctx.Done()
}

// ── Event broadcast loop ──────────────────────────────────────────────────────

func (s *Server) broadcastLoop(ctx context.Context) {
	sub := s.bus.SubscribeAll()
	defer s.bus.Unsubscribe(sub)

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-sub.C:
			if !ok {
				return
			}
			msgs := toWebEvents(ev)
			for _, msg := range msgs {
				s.hub.broadcast(ctx, msg)
			}
		}
	}
}

// ── Client hub ────────────────────────────────────────────────────────────────

type wsClient struct {
	conn   *websocket.Conn
	id     int
	sendCh chan any // buffered send channel; single writer goroutine drains this
	done   chan struct{}
}

// clientSendBuffer is the per-client outbound message buffer size.
// Messages are dropped (with a log) when the buffer is full, applying
// backpressure instead of spawning unbounded goroutines.
const clientSendBuffer = 64

type clientHub struct {
	mu      sync.Mutex
	clients map[int]*wsClient
	nextID  int
}

func newClientHub() *clientHub {
	return &clientHub{clients: make(map[int]*wsClient)}
}

func (h *clientHub) add(conn *websocket.Conn) *wsClient {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	c := &wsClient{
		conn:   conn,
		id:     h.nextID,
		sendCh: make(chan any, clientSendBuffer),
		done:   make(chan struct{}),
	}
	h.clients[c.id] = c

	// Single writer goroutine per client — serializes writes and bounds
	// goroutine count to O(clients) instead of O(clients × events).
	go c.writePump()

	return c
}

// writePump drains the client's send channel and writes each message to the
// WebSocket connection. It exits when sendCh is closed (via hub.remove).
func (c *wsClient) writePump() {
	defer close(c.done)
	for msg := range c.sendCh {
		writeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := wsjson.Write(writeCtx, c.conn, msg); err != nil {
			cancel()
			logger.Warn("web: write failed, dropping client", "client", c.id, "err", err.Error())
			return
		}
		cancel()
	}
}

func (h *clientHub) remove(c *wsClient) {
	h.mu.Lock()
	delete(h.clients, c.id)
	h.mu.Unlock()

	// Close the send channel to stop the write pump, then close the conn.
	close(c.sendCh)
	<-c.done // wait for writePump to exit
	_ = c.conn.Close(websocket.StatusNormalClosure, "")
}

func (h *clientHub) broadcast(_ context.Context, msg any) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, c := range h.clients {
		select {
		case c.sendCh <- msg:
		default:
			// Buffer full — drop message for this client (backpressure).
			logger.Warn("web: send buffer full, dropping message", "client", c.id)
		}
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// spaFallback wraps an http.FileServer so that requests for paths that don't
// match a real file are served index.html, enabling HTML5 history-based routing.
func spaFallback(fileServer http.Handler, root fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// SPA fallback only applies to navigational requests.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.NotFound(w, r)
			return
		}

		// Try to open the requested path in the embedded FS.
		cleaned := path.Clean(r.URL.Path)

		// Guard against unusual requests (e.g. "OPTIONS *", empty path)
		// where path.Clean returns non-/-prefixed values like "*" or ".".
		if cleaned == "." || !strings.HasPrefix(cleaned, "/") {
			cleaned = "/"
		}

		// API and WebSocket paths must never fall through to the SPA;
		// let them 404 normally so typos don't return 200 HTML.
		if strings.HasPrefix(cleaned, "/api/") || cleaned == "/api" || cleaned == "/ws" || strings.HasPrefix(cleaned, "/ws/") {
			http.NotFound(w, r)
			return
		}
		if cleaned == "/" {
			// Serve index.html directly — FileServer would 301-redirect "/" → "./"
			// when it detects a directory, causing an infinite redirect loop in browsers.
			data, err := fs.ReadFile(root, "index.html")
			if err != nil {
				http.Error(w, "index.html not found", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
		// Strip leading slash for fs.Stat (fs.FS paths must not start with /).
		fsPath := strings.TrimPrefix(cleaned, "/")
		info, err := fs.Stat(root, fsPath)
		if err != nil || info.IsDir() {
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				logger.Error("web: unexpected fs.Stat error", "path", fsPath, "err", err.Error())
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			// Asset-like paths (containing a file extension) should 404 rather than
			// returning index.html, which would cause confusing runtime errors.
			if path.Ext(cleaned) != "" {
				http.NotFound(w, r)
				return
			}
			// File not found or extension-less directory — treat as a client-side
			// route and serve index.html so React Router can handle it.
			// Serve directly (not via FileServer) to avoid 301 redirect from http.FS.
			data, err2 := fs.ReadFile(root, "index.html")
			if err2 != nil {
				http.Error(w, "index.html not found", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(data)
			return
		}
		// Serve the file with the cleaned path.
		r.URL.Path = cleaned
		fileServer.ServeHTTP(w, r)
	})
}

func serveNoFrontend(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`<!doctype html>
<html><head><title>TradeEngine Web</title></head>
<body style="background:#07060f;color:#00f5ff;font-family:monospace;padding:2rem">
<h1>🔧 TradeEngine Observability</h1>
<p>Frontend not built yet. Run <code>cd web/frontend && npm run build</code>.</p>
<p>WebSocket endpoint: <code>ws://&lt;host&gt;/ws</code></p>
</body></html>`))
}

// toWebEvents converts an internal engine event to one or more wire-format WebEvents.
// Returns an empty slice for event types that should not be forwarded.
func toWebEvents(ev events.Event) []*WebEvent {
	// Handle fan-out events that produce multiple web pulses (Complex Routing).
	switch ev.Type {
	case events.AnalysisStarted:
		payload, ok := ev.Payload.(events.AnalysisStartedPayload)
		if !ok {
			return nil
		}
		var msgs []*WebEvent
		tickerJSON, _ := json.Marshal(map[string]string{"ticker": payload.Ticker})
		for _, target := range payload.SelectedAnalysts {
			msgs = append(msgs, &WebEvent{
				Type:      "AnalysisStarted",
				Timestamp: ev.Timestamp,
				Source:    "prompt_assembly",
				Target:    target,
				Payload:   json.RawMessage(tickerJSON),
			})
		}
		return msgs

	case events.AnalystVotesPublished:
		payload, ok := ev.Payload.(events.AnalystVotesPublishedPayload)
		if !ok {
			return nil
		}
		raw, _ := json.Marshal(payload)
		// Emit a single AnalystVotesPublished event per cycle — frontend reads all votes
		// from the payload. Emitting one per non-skipped vote was redundant and inflated WS traffic.
		return []*WebEvent{{
			Type:      "AnalystVotesPublished",
			Timestamp: ev.Timestamp,
			Source:    "llm_analysis",
			Target:    "consensus",
			Payload:   json.RawMessage(raw),
		}}
	}

	src, dst, evType := mapEventEdge(ev.Type)
	if evType == "" {
		return nil
	}
	payload, err := json.Marshal(ev.Payload)
	if err != nil {
		logger.Warn("web: failed to marshal event payload",
			"type", string(ev.Type), "err", err.Error())
		payload = nil
	}
	return []*WebEvent{{
		Type:      evType,
		Timestamp: ev.Timestamp,
		Source:    src,
		Target:    dst,
		Payload:   json.RawMessage(payload),
	}}
}
