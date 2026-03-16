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

	"github.com/romeo-folie/trade-engine/internal/events"
	"github.com/romeo-folie/trade-engine/internal/logger"
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
}

// Server is the TradeEngine observability web server.
// It accepts WebSocket connections on /ws and serves a React SPA on /.
// REST drill-down endpoints are registered on /api/*.
type Server struct {
	cfg        Config
	bus        *events.Bus
	httpServer *http.Server

	// hub manages connected WebSocket clients.
	hub *clientHub
}

// NewServer creates a Server. Call Start to bind and begin serving.
func NewServer(cfg Config, bus *events.Bus) *Server {
	s := &Server{
		cfg: cfg,
		bus: bus,
		hub: newClientHub(),
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
	registerAPIHandlers(mux)

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
	snap := buildSnapshot(ctx)
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
			msg := toWebEvent(ev)
			if msg == nil {
				continue
			}
			s.hub.broadcast(ctx, msg)
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
		// API and WebSocket paths must never fall through to the SPA;
		// let them 404 normally so typos don't return 200 HTML.
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/ws") {
			http.NotFound(w, r)
			return
		}

		// Try to open the requested path in the embedded FS.
		cleaned := path.Clean(r.URL.Path)
		if cleaned == "/" {
			r.URL.Path = cleaned
			fileServer.ServeHTTP(w, r)
			return
		}
		// Strip leading slash for fs.Stat (fs.FS paths must not start with /).
		fsPath := strings.TrimPrefix(cleaned, "/")
		if _, err := fs.Stat(root, fsPath); err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			// File not found — serve index.html for SPA client-side routing.
			r.URL.Path = "/"
			fileServer.ServeHTTP(w, r)
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

// toWebEvent converts an internal engine event to a wire-format WebEvent.
// Returns nil for event types that should not be forwarded to the browser.
func toWebEvent(ev events.Event) *WebEvent {
	src, dst, evType := mapEventEdge(ev.Type)
	if evType == "" {
		return nil
	}
	payload, err := json.Marshal(ev.Payload)
	if err != nil {
		logger.Warn("web: failed to marshal event payload",
			"type", string(ev.Type), "err", err.Error())
		// Send event without payload so the UI still sees pipeline activity.
		payload = nil
	}
	return &WebEvent{
		Type:      evType,
		Timestamp: ev.Timestamp,
		Source:    src,
		Target:    dst,
		Payload:   json.RawMessage(payload),
	}
}
