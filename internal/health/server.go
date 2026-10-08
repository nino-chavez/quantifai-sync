// Package health provides a localhost-only /healthz HTTP endpoint that
// reports pipeline status.  The main pipeline goroutines update a shared
// HealthState struct; the HTTP handler reads it under a RWMutex to build
// the JSON response.  The server binds to 127.0.0.1 exclusively so that
// the health endpoint is never exposed to the network.
package health

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// Status represents the overall pipeline health.
type Status string

const (
	StatusOK       Status = "ok"
	StatusDegraded Status = "degraded"
	StatusError    Status = "error"
)

// HealthState holds the mutable pipeline metrics that the /healthz
// endpoint reports.  Pipeline goroutines update fields via the Set*
// methods; the health handler reads them via Snapshot().  All access
// is protected by a sync.RWMutex for safe concurrent use.
type HealthState struct {
	mu              sync.RWMutex
	status          Status
	version         string
	startTime       time.Time
	lastSyncTime    time.Time
	filesTracked    int
	recordsBuffered int
	errorsLastHour  int
	staleAfter      time.Duration
	problem         string
}

// DefaultStaleAfter is how long after the last fully acknowledged sync
// cycle the status turns degraded when no other threshold is set.
const DefaultStaleAfter = 15 * time.Minute

// NewHealthState creates a HealthState initialized to "ok" with the
// given version string and the current time as the start time.
func NewHealthState(version string) *HealthState {
	return &HealthState{
		status:     StatusOK,
		version:    version,
		startTime:  time.Now(),
		staleAfter: DefaultStaleAfter,
	}
}

// SetStaleAfter sets how old the last successful sync may be before the
// reported status degrades.
func (h *HealthState) SetStaleAfter(d time.Duration) {
	h.mu.Lock()
	h.staleAfter = d
	h.mu.Unlock()
}

// SetStatus updates the pipeline status.
func (h *HealthState) SetStatus(s Status) {
	h.mu.Lock()
	h.status = s
	h.mu.Unlock()
}

// SetLastSyncTime records when a sync cycle last finished with every batch
// acknowledged by the server (including cycles with nothing to send).
func (h *HealthState) SetLastSyncTime(t time.Time) {
	h.mu.Lock()
	h.lastSyncTime = t
	h.mu.Unlock()
}

// SetFilesTracked updates the count of files currently being watched.
func (h *HealthState) SetFilesTracked(n int) {
	h.mu.Lock()
	h.filesTracked = n
	h.mu.Unlock()
}

// SetRecordsBuffered updates the count of records waiting in the buffer.
func (h *HealthState) SetRecordsBuffered(n int) {
	h.mu.Lock()
	h.recordsBuffered = n
	h.mu.Unlock()
}

// SetErrorsLastHour updates the rolling error count.
func (h *HealthState) SetErrorsLastHour(n int) {
	h.mu.Lock()
	h.errorsLastHour = n
	h.mu.Unlock()
}

// healthResponse is the JSON schema returned by GET /healthz.
type healthResponse struct {
	Status          string `json:"status"`
	Version         string `json:"version"`
	UptimeSeconds   int64  `json:"uptime_seconds"`
	LastSyncTime    string `json:"last_sync_time"`
	FilesTracked    int    `json:"files_tracked"`
	RecordsBuffered int    `json:"records_buffered"`
	ErrorsLastHour  int    `json:"errors_last_hour"`
	Problem         string `json:"problem,omitempty"`
}

// SetProblem reports why the agent cannot sync yet (for example a missing
// API key). While set, the status is "error" and the problem is included
// in the response; "" clears it.
func (h *HealthState) SetProblem(problem string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.problem = problem
}

// Snapshot returns a point-in-time copy of the health state formatted
// as a healthResponse suitable for JSON serialization.
func (h *HealthState) Snapshot() healthResponse {
	h.mu.RLock()
	defer h.mu.RUnlock()

	lastSync := ""
	if !h.lastSyncTime.IsZero() {
		lastSync = h.lastSyncTime.UTC().Format(time.RFC3339)
	}

	// "ok" has to be earned by a recent acknowledged sync. A pipeline that
	// has never synced, or stopped syncing, is degraded however healthy its
	// goroutines look; that is what made the silent-loss failure invisible.
	status := h.status
	if status == StatusOK && (h.lastSyncTime.IsZero() || time.Since(h.lastSyncTime) > h.staleAfter) {
		status = StatusDegraded
	}
	if h.problem != "" {
		status = StatusError
	}

	return healthResponse{
		Status:          string(status),
		Version:         h.version,
		UptimeSeconds:   int64(time.Since(h.startTime).Seconds()),
		LastSyncTime:    lastSync,
		FilesTracked:    h.filesTracked,
		RecordsBuffered: h.recordsBuffered,
		ErrorsLastHour:  h.errorsLastHour,
		Problem:         h.problem,
	}
}

// Server is a localhost-only HTTP server that exposes the /healthz
// endpoint.  It wraps net/http.Server and binds exclusively to
// 127.0.0.1 to prevent network exposure.
type Server struct {
	httpServer *http.Server
	state      *HealthState

	// ListenFailed, when set, is called once with the error of the first
	// failed bind in ListenAndServe, before it starts retrying.
	ListenFailed func(error)

	// mu protects the listener field which is set in Listen and
	// read in Addr, and closed, which Shutdown sets.
	mu       sync.RWMutex
	listener net.Listener
	closed   bool
}

// listenRetryInterval is how often ListenAndServe retries a port it could
// not bind.
var listenRetryInterval = 500 * time.Millisecond

// NewServer creates a health Server bound to 127.0.0.1 on the given
// port.  It does NOT start serving; call ListenAndServe or the
// Listen + Serve pair for that.
func NewServer(port int, state *HealthState) *Server {
	mux := http.NewServeMux()
	s := &Server{
		httpServer: &http.Server{
			Addr:    fmt.Sprintf("127.0.0.1:%d", port),
			Handler: mux,
		},
		state: state,
	}

	mux.HandleFunc("/healthz", s.handleHealthz)
	// Alias for VS Code extension compatibility
	mux.HandleFunc("/health", s.handleHealthz)
	return s
}

// RegisterHandler adds a custom handler to the server's mux.
// This allows the shipper to expose additional endpoints (e.g. editor events)
// on the same localhost-only HTTP server.
func (s *Server) RegisterHandler(pattern string, handler http.HandlerFunc) {
	s.httpServer.Handler.(*http.ServeMux).HandleFunc(pattern, handler)
}

// handleHealthz writes the current health state as JSON. An "error"
// status is sent with 503, so a check that reads only the HTTP code sees
// it; "ok" and "degraded" (running, not synced recently) are sent with
// 200. The body is the same either way.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	snap := s.state.Snapshot()

	code := http.StatusOK
	if snap.Status == string(StatusError) {
		code = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(snap)
}

// Listen creates the TCP listener on 127.0.0.1:{port} without
// starting to serve.  This is separated from Serve so that callers
// (especially tests) can read the actual bound address via Addr()
// before starting the serve loop, avoiding data races.
func (s *Server) Listen() error {
	ln, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return fmt.Errorf("health server listen: %w", err)
	}
	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()
	return nil
}

// Serve starts serving HTTP on the previously created listener.
// It blocks until the server is shut down.  Listen must be called
// before Serve.
func (s *Server) Serve() error {
	s.mu.RLock()
	ln := s.listener
	s.mu.RUnlock()
	if ln == nil {
		return fmt.Errorf("health server: Listen must be called before Serve")
	}
	return s.httpServer.Serve(ln)
}

// ListenAndServe calls Listen then Serve. For production use where you do
// not need to read Addr() before serving begins.
//
// A port it cannot bind is retried until the bind succeeds or Shutdown is
// called. On Windows a reinstall starts the new agent while the old one,
// killed with its supervisor, can still hold the port for a moment; one
// failed bind left the new agent without a health endpoint for its whole
// run.
func (s *Server) ListenAndServe() error {
	for failed := false; ; failed = true {
		err := s.Listen()
		if err == nil {
			break
		}
		if !failed && s.ListenFailed != nil {
			s.ListenFailed(err)
		}
		if s.isClosed() {
			return http.ErrServerClosed
		}
		time.Sleep(listenRetryInterval)
		if s.isClosed() {
			return http.ErrServerClosed
		}
	}
	return s.Serve()
}

func (s *Server) isClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.closed
}

// Addr returns the listener's network address, or the configured
// address if the server has not started yet.  This is useful in tests
// that bind to port 0 to get an ephemeral port.
func (s *Server) Addr() string {
	s.mu.RLock()
	ln := s.listener
	s.mu.RUnlock()
	if ln != nil {
		return ln.Addr().String()
	}
	return s.httpServer.Addr
}

// Shutdown gracefully shuts down the health server without interrupting
// any active connections.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return s.httpServer.Shutdown(ctx)
}
