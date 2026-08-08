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
// Vite writes directly to this package's dist directory before go build.
//
// NOTE: //go:embed requires the "dist" directory to exist at compile time.
// The tracked .gitkeep satisfies that requirement in a fresh checkout; run
// `make frontend-build` before a production build to embed the complete UI.
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

	// brokerView serves broker-direct portfolio data independent of engine ticks.
	// Nil when no real broker is configured (orb-only modes, paper-only).
	brokerView *brokerCache
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

// SetBrokerView attaches a BrokerView implementation (already wrapped in
// brokerCache) to the server. Call this after NewServer when a real broker
// is configured. Safe to skip for paper / orb-only modes.
func (s *Server) SetBrokerView(bv *brokerCache) {
	s.brokerView = bv
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

	// Static frontend. A fresh checkout embeds only dist/.gitkeep so ordinary
	// Go builds remain valid; frontendHandler serves a placeholder until Vite
	// has generated dist/index.html.
	mux.Handle("/", frontendHandler(frontendFS))

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
	snap := buildSnapshot(ctx, s.engineCfg, s.tracker)
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
			msgs := toWebEvents(ev, s.engineCfg)
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

// frontendHandler serves the built SPA when index.html exists and otherwise
// serves build instructions from the placeholder-only embed filesystem.
func frontendHandler(root fs.FS) http.Handler {
	distFS, err := fs.Sub(root, "dist")
	if err != nil {
		return noFrontendHandler()
	}
	if _, err := fs.Stat(distFS, "index.html"); err != nil {
		return noFrontendHandler()
	}
	return spaFallback(http.FileServer(http.FS(distFS)), distFS)
}

func frontendRequestPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	// Frontend fallback applies only to navigational requests.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.NotFound(w, r)
		return "", false
	}

	cleaned := path.Clean(r.URL.Path)
	if cleaned == "." || !strings.HasPrefix(cleaned, "/") {
		cleaned = "/"
	}
	// API and WebSocket paths must never fall through to either the SPA or its
	// no-build placeholder; typos need to remain ordinary 404 responses.
	if strings.HasPrefix(cleaned, "/api/") || cleaned == "/api" ||
		cleaned == "/ws" || strings.HasPrefix(cleaned, "/ws/") {
		http.NotFound(w, r)
		return "", false
	}
	return cleaned, true
}

func noFrontendHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := frontendRequestPath(w, r); !ok {
			return
		}
		serveNoFrontend(w, r)
	})
}

// spaFallback wraps an http.FileServer so that requests for paths that don't
// match a real file are served index.html, enabling HTML5 history-based routing.
func spaFallback(fileServer http.Handler, root fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cleaned, ok := frontendRequestPath(w, r)
		if !ok {
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
<p>Frontend not built yet. Run <code>make build</code> (or <code>make frontend-build</code> then rebuild/restart the <code>te</code> binary).</p>
<p>WebSocket endpoint: <code>ws://&lt;host&gt;/ws</code></p>
</body></html>`))
}

// toWebEvents converts an internal engine event to one or more wire-format WebEvents.
// Returns an empty slice for event types that should not be forwarded.
func toWebEvents(ev events.Event, cfg *appconfig.Config) []*WebEvent {
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
			sources := payload.SourceNodes
			if len(sources) == 0 {
				sources = []string{"indicator_engine"}
			}
			for _, source := range sources {
				msgs = append(msgs, &WebEvent{
					Type:      "AnalysisStarted",
					Timestamp: ev.Timestamp,
					Source:    source,
					Target:    target,
					Payload:   json.RawMessage(tickerJSON),
				})
			}
		}
		return msgs

	case events.AnalystVotesPublished:
		payload, ok := ev.Payload.(events.AnalystVotesPublishedPayload)
		if !ok {
			return nil
		}
		raw, _ := json.Marshal(payload)
		activeAnalysts := routedAnalystNodeIDs(cfg, payload.Votes)
		source, target, webType := activeAnalysts[0], "consensus", "AnalystVotesPublished"
		for _, vote := range payload.Votes {
			if vote.Role == "risk_manager" {
				source, target, webType = "specialist-risk-manager", "risk_manager", "SpecialistReviewCompleted"
				break
			}
		}
		if topologyMode(cfg) == "single" || len(payload.Votes) <= 1 {
			target = "risk_manager"
		}
		msgs := []*WebEvent{{
			Type:      webType,
			Timestamp: ev.Timestamp,
			Source:    source,
			Target:    target,
			Payload:   json.RawMessage(raw),
		}}
		// Carry the complete vote payload once. Routing-only events animate the
		// remaining analyst fan-in without duplicating votes in frontend stores.
		if webType == "AnalystVotesPublished" && target == "consensus" {
			for _, analystID := range activeAnalysts[1:] {
				msgs = append(msgs, &WebEvent{
					Type:      "AnalystVoteRouted",
					Timestamp: ev.Timestamp,
					Source:    analystID,
					Target:    target,
				})
			}
		}
		return msgs

	case events.AnalysisCompleted:
		payload, ok := ev.Payload.(events.AnalysisCompletedPayload)
		if !ok {
			return nil
		}
		source := payload.SourceNode
		if source == "" {
			source = finalAnalysisNodeID(cfg, payload.FastPathed)
		}
		return singleWebEvent(ev, "AnalysisCompleted", source, source)

	case events.SignalGenerated:
		payload, ok := ev.Payload.(events.SignalGeneratedPayload)
		if !ok {
			return nil
		}
		source := payload.SourceNode
		if source == "" {
			source = finalAnalysisNodeID(cfg, false)
		}
		return singleWebEvent(ev, "SignalGenerated", source, "risk_manager")
	}

	src, dst, evType := mapEventEdge(ev.Type)
	if evType == "" {
		return nil
	}
	// mapEventEdge attributes executions to risk_manager, which is only true for
	// signals that executed straight off risk approval. Executions released from
	// the pending queue — a manual approval, or a first-tick deferral picked up
	// by processDeferredSignals — carry SourceNode="approvals" and must be
	// routed from there. Without this the approvals → executor edge is declared
	// in the topology but no event ever traverses it, so the path renders
	// permanently idle: in manual mode that is the *only* route to the executor,
	// and in auto/paper it is the entire first-tick deferral flow.
	//
	// Keyed off the payload rather than the configured mode, because auto/paper
	// have both paths live and the mode alone cannot tell them apart.
	if src == "risk_manager" && dst == "executor" {
		if node := executionSourceNode(ev.Payload); node != "" {
			src = node
		}
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

func singleWebEvent(ev events.Event, eventType, source, target string) []*WebEvent {
	payload, err := json.Marshal(ev.Payload)
	if err != nil {
		logger.Warn("web: failed to marshal event payload",
			"type", string(ev.Type), "err", err.Error())
		payload = nil
	}
	return []*WebEvent{{
		Type: eventType, Timestamp: ev.Timestamp, Source: source, Target: target,
		Payload: json.RawMessage(payload),
	}}
}

func analystNodeIDs(cfg *appconfig.Config) []string {
	nodes, _ := pipelineTopology(cfg)
	var ids []string
	for _, node := range nodes {
		if node.Kind == "analyst" {
			ids = append(ids, node.ID)
		}
	}
	if len(ids) == 0 {
		return []string{"llm_analysis"}
	}
	return ids
}

func firstAnalystNodeID(cfg *appconfig.Config) string {
	return analystNodeIDs(cfg)[0]
}

func routedAnalystNodeIDs(cfg *appconfig.Config, votes []events.AnalystVotePayload) []string {
	nodes, _ := pipelineTopology(cfg)
	var analysts []NodeState
	for _, node := range nodes {
		if node.Kind == "analyst" {
			analysts = append(analysts, node)
		}
	}
	if len(analysts) == 0 {
		return []string{"llm_analysis"}
	}

	used := make(map[string]bool, len(analysts))
	ids := make([]string, 0, len(votes))
	for _, vote := range votes {
		for _, node := range analysts {
			if used[node.ID] {
				continue
			}
			model, _ := node.Meta["model"].(string)
			provider, _ := node.Meta["provider"].(string)
			role, _ := node.Meta["role"].(string)
			if (vote.Role != "" && vote.Role == role) ||
				(vote.Model == model && (vote.Provider == "" || vote.Provider == provider)) {
				ids = append(ids, node.ID)
				used[node.ID] = true
				break
			}
		}
	}
	for _, node := range analysts {
		if len(ids) >= len(votes) {
			break
		}
		if !used[node.ID] {
			ids = append(ids, node.ID)
		}
	}
	if len(ids) == 0 {
		return []string{analysts[0].ID}
	}
	return ids
}

func topologyMode(cfg *appconfig.Config) string {
	_, topology := pipelineTopology(cfg)
	return topology.AnalysisMode
}

func finalAnalysisNodeID(cfg *appconfig.Config, fastPathed bool) string {
	switch topologyMode(cfg) {
	case "specialist":
		return "specialist-risk-manager"
	case "ensemble":
		if !fastPathed && cfg != nil && cfg.Models.ArbiterConfig().Model != "" {
			return "arbiter_node"
		}
		return "consensus"
	default:
		return firstAnalystNodeID(cfg)
	}
}
