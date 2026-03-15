// Package web implements the TradeEngine observability web server (Phase 14).
//
// Usage:
//
//	srv := web.NewServer(cfg, bus, store, portfolioStore)
//	if err := srv.Start(ctx, ":8080"); err != nil { ... }
//	defer srv.Stop()
package web

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
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
// If the dist directory is absent (dev mode), the server falls back to a
// placeholder HTML page and the frontend is served via `npm run dev` instead.
//
//go:embed all:dist
var frontendFS embed.FS

// Config holds web-server configuration.
type Config struct {
	Port string // e.g. "8080"
	Host string // e.g. "127.0.0.1" (defaults to 127.0.0.1 for local-only safety)

	// InsecureAllowAllOrigins disables WebSocket origin verification.
	// Set to true only in development; never enable in production.
	InsecureAllowAllOrigins bool
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
		mux.Handle("/", http.FileServer(http.FS(distFS)))
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
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: s.cfg.InsecureAllowAllOrigins,
	})
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
	conn *websocket.Conn
	id   int
}

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
	c := &wsClient{conn: conn, id: h.nextID}
	h.clients[c.id] = c
	return c
}

func (h *clientHub) remove(c *wsClient) {
	h.mu.Lock()
	delete(h.clients, c.id)
	h.mu.Unlock()
	// Close the connection after releasing the lock to avoid blocking hub
	// operations (add/broadcast/remove) if Close blocks on network I/O.
	_ = c.conn.Close(websocket.StatusNormalClosure, "")
}

func (h *clientHub) broadcast(ctx context.Context, msg any) {
	h.mu.Lock()
	conns := make([]*websocket.Conn, 0, len(h.clients))
	for _, c := range h.clients {
		conns = append(conns, c.conn)
	}
	h.mu.Unlock()

	for _, conn := range conns {
		// Each goroutine owns its own timeout context so that broadcast
		// returning does not cancel an in-flight write (deferred cancel on
		// a shared context would fire before the goroutines finish).
		go func(c *websocket.Conn) {
			writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			_ = wsjson.Write(writeCtx, c, msg)
		}(conn)
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

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
	payload, _ := json.Marshal(ev.Payload)
	return &WebEvent{
		Type:      evType,
		Timestamp: ev.Timestamp,
		Source:    src,
		Target:    dst,
		Payload:   json.RawMessage(payload),
	}
}
