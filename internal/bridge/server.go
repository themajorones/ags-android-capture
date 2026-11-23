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
	config       Config
	mu           sync.Mutex
	streams      map[*streamSession]struct{}
	deviceStarts map[string]*deviceStartGate
	last         time.Time
}

type streamSession struct {
	serial string
}

type deviceStartGate struct {
	token chan struct{}
	users int
}

func New(config Config) *Server {
	if config.IdleTimeout == 0 {
		config.IdleTimeout = time.Minute
	}
	if config.SessionWaitTime <= 0 {
		config.SessionWaitTime = 20 * time.Second
	}
	return &Server{
		config:       config,
		streams:      make(map[*streamSession]struct{}),
		deviceStarts: make(map[string]*deviceStartGate),
		last:         time.Now(),
	}
}

// Serialize only scrcpy startup for a given device. Concurrent startup pushes
// the same server archive path on Android and can make app_process launch a
// partially replaced jar; once the first stream header arrives, both streams
// may run independently.
func (s *Server) acquireDeviceStartup(ctx context.Context, serial string) (func(), error) {
	s.mu.Lock()
	gate := s.deviceStarts[serial]
	if gate == nil {
		gate = &deviceStartGate{token: make(chan struct{}, 1)}
		gate.token <- struct{}{}
		s.deviceStarts[serial] = gate
	}
	gate.users++
	s.mu.Unlock()

	releaseUser := func() {
		s.mu.Lock()
		gate.users--
		if gate.users == 0 && s.deviceStarts[serial] == gate {
			delete(s.deviceStarts, serial)
		}
		s.mu.Unlock()
	}
	select {
	case <-ctx.Done():
		releaseUser()
		return nil, ctx.Err()
	case <-gate.token:
		if err := ctx.Err(); err != nil {
			gate.token <- struct{}{}
			releaseUser()
			return nil, err
		}
		return func() {
			gate.token <- struct{}{}
			releaseUser()
		}, nil
	}
}

func (s *Server) Health(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	streamCount := s.activeStreamCount()
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":       "ok",
		"streamActive": streamCount > 0,
		"streamCount":  streamCount,
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
		// The helper intentionally accepts AGS pages from any host. Disable the
		// library's host-only check so machine-specific hostnames work.
		InsecureSkipVerify: true,
		CompressionMode:    websocket.CompressionDisabled,
	})
	if err != nil {
		log.Printf("browser stream handshake rejected: %v", err)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "stream closed")

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	session := &streamSession{serial: request.serial}
	s.mu.Lock()
	s.streams[session] = struct{}{}
	s.last = time.Now()
	activeStreams := len(s.streams)
	s.mu.Unlock()
	log.Printf("browser stream started for %s (active streams: %d)", request.serial, activeStreams)
	defer func() {
		cancel()
		s.mu.Lock()
		delete(s.streams, session)
		s.last = time.Now()
		activeStreams := len(s.streams)
		s.mu.Unlock()
		log.Printf("browser stream closed for %s (active streams: %d)", request.serial, activeStreams)
	}()

	if err := s.runStream(ctx, conn, request); err != nil && ctx.Err() == nil {
		log.Printf("device stream ended: %v", err)
		failure, _ := json.Marshal(map[string]string{"kind": "error", "message": err.Error()})
		_ = writeWebSocket(ctx, conn, websocket.MessageText, failure)
		_ = conn.Close(websocket.StatusInternalError, "device stream failed")
	}
}

func (s *Server) activeStreamCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.streams)
}

func (s *Server) originAllowed(origin string) bool {
	if origin == "" {
		return false
	}
	for _, allowed := range s.config.AllowedOrigins {
		if allowed == "*" || origin == allowed {
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
			idle := len(s.streams) == 0 && time.Since(s.last) >= s.config.IdleTimeout
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
