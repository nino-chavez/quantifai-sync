package cmd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quantifai/sync/internal/config"
	"github.com/quantifai/sync/internal/health"
	"github.com/quantifai/sync/internal/ingest"
	"github.com/quantifai/sync/internal/logger"
	"github.com/quantifai/sync/internal/sender"
	"github.com/quantifai/sync/internal/state"
)

// fakeServer mimics POST /api/v1/ingest: it stores messages by id (insert
// or ignore), replaces sessions by id, and answers with the server's counts.
// With broken set, it answers 200 with all-zero counts, like the real server
// does for a body shape it does not read.
type fakeServer struct {
	mu       sync.Mutex
	broken   atomic.Bool
	posts    int
	messages map[string]ingest.Message
	sessions map[string]ingest.Session
}

func newFakeServer() *fakeServer {
	return &fakeServer{messages: map[string]ingest.Message{}, sessions: map[string]ingest.Session{}}
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts++
	var res ingest.Result
	if f.broken.Load() {
		json.NewEncoder(w).Encode(res)
		return
	}
	var b ingest.Batch
	json.Unmarshal(data, &b)
	units := map[string]bool{}
	for _, u := range b.UnitsOfWork {
		units[u.ProjectPath] = true
	}
	res.UnitsOfWork = len(units)
	for _, s := range b.Sessions {
		f.sessions[s.SessionID] = s
	}
	res.Sessions = len(b.Sessions)
	for _, m := range b.Messages {
		if _, ok := f.messages[m.MessageID]; !ok {
			f.messages[m.MessageID] = m
			res.Messages.Accepted++
		}
	}
	res.GitEvents.Accepted = len(b.GitEvents)
	json.NewEncoder(w).Encode(res)
}

type harness struct {
	t         *testing.T
	root      string
	statePath string
	cfg       config.Config
	srv       *fakeServer
	snd       *sender.Sender
	state     *state.Manager
	health    *health.HealthState
	col       *ingest.Collector
	log       *logger.Logger
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	write := func(rel string, lines ...string) {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		var data []byte
		for _, l := range lines {
			data = append(data, l+"\n"...)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("-r-repo/s1.jsonl",
		asstLine("s1", "m1", "2026-10-06T10:00:00Z", 100),
		`{"type":"user","sessionId":"s1","uuid":"u1","timestamp":"2026-10-06T10:00:01Z","cwd":"/r/repo"}`,
		asstLine("s1", "m2", "2026-10-06T10:01:00Z", 200))
	write("-r-repo/s1/subagents/a.jsonl", asstLine("s1", "m3", "2026-10-06T10:02:00Z", 300))

	srv := newFakeServer()
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	log, _ := logger.New(logger.LevelError, "")
	snd, err := sender.New(hs.URL, "k", log, sender.WithSleepFunc(func(time.Duration) {}), sender.WithMaxRetries(0))
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(t.TempDir(), "state.json")
	st, err := state.NewManager(statePath)
	if err != nil {
		t.Fatal(err)
	}
	return &harness{
		t: t, root: root, statePath: statePath, srv: srv, snd: snd, state: st, log: log,
		cfg:    config.Config{BatchSize: 5000, GitEnabled: false},
		health: health.NewHealthState("test"),
		col:    ingest.NewCollector(root, false),
	}
}

func asstLine(session, id, ts string, out int) string {
	b, _ := json.Marshal(map[string]any{
		"type": "assistant", "sessionId": session, "uuid": id, "timestamp": ts, "cwd": "/r/repo",
		"message": map[string]any{"model": "claude-opus-4-6", "usage": map[string]any{"input_tokens": 1, "output_tokens": out}},
	})
	return string(b)
}

func (h *harness) cycle() bool {
	return runCycle(context.Background(), h.cfg, h.col, h.snd, h.state, h.health, false, h.log)
}

func (h *harness) savedOffsets() map[string]int64 {
	h.t.Helper()
	fresh, err := state.NewManager(h.statePath)
	if err != nil {
		h.t.Fatal(err)
	}
	out := map[string]int64{}
	filepath.Walk(h.root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			out[p] = fresh.Get(p).ByteOffset
		}
		return nil
	})
	return out
}

func TestCycleSendsAndCommitsOnlyAfterAck(t *testing.T) {
	h := newHarness(t)
	if !h.cycle() {
		t.Fatal("first cycle failed")
	}
	if len(h.srv.messages) != 3 || h.srv.sessions["s1"].MessageCount != 3 || h.srv.sessions["s1"].OutputTokens != 600 {
		t.Fatalf("server state wrong: %d messages, session %+v", len(h.srv.messages), h.srv.sessions["s1"])
	}
	for p, off := range h.savedOffsets() {
		if fi, _ := os.Stat(p); off != fi.Size() {
			t.Fatalf("%s: committed offset %d, size %d", p, off, fi.Size())
		}
	}
	if h.health.Snapshot().Status != "ok" {
		t.Fatalf("health after an acknowledged cycle: %s", h.health.Snapshot().Status)
	}

	// New activity in the subagent file: the session row must still carry
	// the whole session, because the server replaces it.
	f, _ := os.OpenFile(filepath.Join(h.root, "-r-repo/s1/subagents/a.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(asstLine("s1", "m4", "2026-10-06T10:03:00Z", 400) + "\n")
	f.Close()
	if !h.cycle() {
		t.Fatal("second cycle failed")
	}
	s := h.srv.sessions["s1"]
	if s.MessageCount != 4 || s.OutputTokens != 1000 || len(h.srv.messages) != 4 {
		t.Fatalf("session overwritten with a partial total: %+v", s)
	}
}

// The silent-loss bug: a 200 with zero counts advanced offsets. Now the
// cycle fails, nothing is committed (not even on shutdown), and the data
// goes out on the next healthy cycle.
func TestCycleCommitsNothingWhenServerWritesNothing(t *testing.T) {
	h := newHarness(t)
	h.srv.broken.Store(true)
	if h.cycle() {
		t.Fatal("cycle reported success against a server that wrote nothing")
	}
	if code := gracefulShutdown(h.state, h.log); code != 0 {
		t.Fatalf("shutdown exit %d", code)
	}
	for p, off := range h.savedOffsets() {
		if off != 0 {
			t.Fatalf("%s: offset %d committed for data the server never stored", p, off)
		}
	}
	if st := h.health.Snapshot().Status; st != "degraded" {
		t.Fatalf("health with no acknowledged sync: %s", st)
	}

	h.srv.broken.Store(false)
	if !h.cycle() {
		t.Fatal("recovery cycle failed")
	}
	if len(h.srv.messages) != 3 || h.srv.sessions["s1"].MessageCount != 3 {
		t.Fatalf("data not delivered after recovery: %d messages", len(h.srv.messages))
	}
}

func TestIdleCycleRefreshesHealth(t *testing.T) {
	h := newHarness(t)
	h.cycle()
	posts := h.srv.posts
	h.health.SetLastSyncTime(time.Now().Add(-time.Hour))
	if !h.cycle() {
		t.Fatal("idle cycle failed")
	}
	if h.srv.posts != posts {
		t.Fatal("idle cycle posted")
	}
	if st := h.health.Snapshot().Status; st != "ok" {
		t.Fatalf("an idle machine with nothing to send should stay ok, got %s", st)
	}
}
