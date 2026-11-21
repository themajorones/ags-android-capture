package bridge

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"nhooyr.io/websocket"
)

type Config struct {
	ADBPath         string
	ScrcpyPath      string
	AllowedOrigins  []string
	IdleTimeout     time.Duration
	SessionWaitTime time.Duration
}

type Server struct {
	config Config
	mu     sync.Mutex
	active bool
	last   time.Time
}

func New(config Config) *Server {
	if config.IdleTimeout == 0 {
		config.IdleTimeout = time.Minute
	}
	if config.SessionWaitTime <= 0 {
		config.SessionWaitTime = 20 * time.Second
	}
	return &Server{config: config, last: time.Now()}
}

func (s *Server) Health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":       "ok",
		"streamActive": s.isActive(),
		"streamMode":   "local scrcpy packets",
	})
}

func (s *Server) Stream(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if !s.originAllowed(origin) {
		http.Error(w, "browser origin is not allowed", http.StatusForbidden)
		return
	}
	request, err := parseStreamRequest(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The exact Origin allowlist was checked above. Disable the library's
		// host-only check so AGS may run behind a different host name.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		log.Printf("browser stream handshake rejected: %v", err)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "stream closed")

	s.mu.Lock()
	if s.active {
		s.mu.Unlock()
		_ = conn.Close(websocket.StatusTryAgainLater, "another local stream is active")
		return
	}
	s.active = true
	s.last = time.Now()
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active = false
		s.last = time.Now()
		s.mu.Unlock()
	}()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if err := s.runStream(ctx, conn, request); err != nil && ctx.Err() == nil {
		log.Printf("device stream ended: %v", err)
		_ = conn.Close(websocket.StatusInternalError, "device stream ended")
	}
}

func (s *Server) isActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

func (s *Server) originAllowed(origin string) bool {
	if origin == "" {
		return false
	}
	for _, allowed := range s.config.AllowedOrigins {
		if origin == allowed {
			return true
		}
	}
	return false
}

func (s *Server) ExitWhenIdle(ctx context.Context, server *http.Server) {
	if s.config.IdleTimeout < 0 {
		return
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			idle := !s.active && time.Since(s.last) >= s.config.IdleTimeout
			s.mu.Unlock()
			if idle {
				log.Print("no local stream requested; stopping the per-user bridge")
				shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				_ = server.Shutdown(shutdownCtx)
				cancel()
				return
			}
		}
	}
}
