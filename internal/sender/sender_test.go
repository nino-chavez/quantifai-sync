package sender

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quantifai/sync/internal/ingest"
	"github.com/quantifai/sync/internal/logger"
)

// newTestLogger creates a logger that discards output (debug level so all
// messages are processed but written to nowhere meaningful in tests).
func newTestLogger() *logger.Logger {
	l, _ := logger.New(logger.LevelDebug, "")
	return l
}

// noopSleep replaces time.Sleep in tests to avoid real delays.
func noopSleep(_ time.Duration) {}

func testBatch() ingest.Batch {
	return ingest.Batch{
		UnitsOfWork: []ingest.Unit{{Kind: "project", Name: "repo", Source: "path", ProjectPath: "/r/repo"}},
		Sessions:    []ingest.Session{{SessionID: "s1", ProjectPath: "/r/repo", UnitProjectPath: "/r/repo"}},
		Messages:    []ingest.Message{{SessionID: "s1", MessageID: "m1"}, {SessionID: "s1", MessageID: "m2"}},
	}
}

// countingServer answers like the real ingest endpoint: it echoes the
// counts of what it received in the server's own field names.
func countingHandler(t *testing.T, got *ingest.Batch) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var b ingest.Batch
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if got != nil {
			*got = b
		}
		var res ingest.Result
		units := map[string]bool{}
		for _, u := range b.UnitsOfWork {
			units[u.ProjectPath] = true
		}
		res.UnitsOfWork = len(units)
		res.Sessions = len(b.Sessions)
		res.Messages.Accepted = len(b.Messages)
		res.GitEvents.Accepted = len(b.GitEvents)
		json.NewEncoder(w).Encode(res)
	}
}

func newSender(t *testing.T, url string) *Sender {
	t.Helper()
	s, err := New(url, "test-key", newTestLogger(), WithSleepFunc(noopSleep))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSenderPostsServerShapeWithHeaders(t *testing.T) {
	var got ingest.Batch
	var auth, ctype, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth, ctype, path = r.Header.Get("Authorization"), r.Header.Get("Content-Type"), r.URL.Path
		countingHandler(t, &got)(w, r)
	}))
	defer srv.Close()

	if _, err := newSender(t, srv.URL).Send(context.Background(), testBatch()); err != nil {
		t.Fatalf("send: %v", err)
	}
	if auth != "Bearer test-key" || ctype != "application/json" || path != "/api/v1/ingest" {
		t.Fatalf("auth=%q ctype=%q path=%q", auth, ctype, path)
	}
	if len(got.Sessions) != 1 || len(got.Messages) != 2 || len(got.UnitsOfWork) != 1 {
		t.Fatalf("server did not receive the batch: %+v", got)
	}
}

// The failure that motivated this rewrite: the server answered 200 with
// all-zero counts for a body it did not recognize, and the old client
// counted that as delivered.
func TestSenderRejects2xxThatWroteNothing(t *testing.T) {
	for name, body := range map[string]string{
		"zero counts": `{"unitsOfWork":0,"sessions":0,"messages":{"accepted":0,"errors":0},"gitEvents":{"accepted":0,"linked":0,"deterministic":0}}`,
		"empty":       `{}`,
		"not json":    `<html>ok</html>`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(body))
			}))
			defer srv.Close()
			if _, err := newSender(t, srv.URL).Send(context.Background(), testBatch()); err == nil {
				t.Fatal("a 2xx that wrote nothing was treated as success")
			}
		})
	}
}

func TestSenderAcceptsReplayOfStoredMessages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Messages already stored: ON CONFLICT DO NOTHING inserts none.
		w.Write([]byte(`{"unitsOfWork":1,"sessions":1,"messages":{"accepted":0,"errors":0},"gitEvents":{"accepted":0,"linked":0,"deterministic":0}}`))
	}))
	defer srv.Close()
	if _, err := newSender(t, srv.URL).Send(context.Background(), testBatch()); err != nil {
		t.Fatalf("replay rejected: %v", err)
	}
}

func TestSenderRetriesOn429And5xx(t *testing.T) {
	for _, status := range []int{429, 500, 503} {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) < 3 {
				w.WriteHeader(status)
				return
			}
			countingHandler(t, nil)(w, r)
		}))
		if _, err := newSender(t, srv.URL).Send(context.Background(), testBatch()); err != nil {
			t.Fatalf("status %d: %v", status, err)
		}
		if calls.Load() != 3 {
			t.Fatalf("status %d: want 3 calls, got %d", status, calls.Load())
		}
		srv.Close()
	}
}

func TestSenderGivesUpAfterMaxRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(503)
	}))
	defer srv.Close()
	if _, err := newSender(t, srv.URL).Send(context.Background(), testBatch()); err == nil {
		t.Fatal("expected failure")
	}
	if calls.Load() != 4 {
		t.Fatalf("want 1 try + 3 retries, got %d", calls.Load())
	}
}

func TestSenderFailsImmediatelyOnNonRetryable4xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "Ingest key required", 401)
	}))
	defer srv.Close()
	_, err := newSender(t, srv.URL).Send(context.Background(), testBatch())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want a 401 error, got %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("4xx must not retry, got %d calls", calls.Load())
	}
}

// www.quantifai.app 308s to the apex. Go drops Authorization on a
// cross-host redirect, so following it produces a misleading 401.
func TestSenderRefusesCrossHostRedirect(t *testing.T) {
	var apexAuth atomic.Value
	apex := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apexAuth.Store(r.Header.Get("Authorization"))
		countingHandler(t, nil)(w, r)
	}))
	defer apex.Close()
	// Same port, different host name: 127.0.0.1 vs localhost.
	apexAsLocalhost := strings.Replace(apex.URL, "127.0.0.1", "localhost", 1)
	www := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, apexAsLocalhost+r.URL.Path, http.StatusPermanentRedirect)
	}))
	defer www.Close()

	_, err := newSender(t, www.URL).Send(context.Background(), testBatch())
	if err == nil || !strings.Contains(err.Error(), "refusing redirect") {
		t.Fatalf("want a refused redirect, got %v", err)
	}
	if v := apexAuth.Load(); v != nil {
		t.Fatalf("redirect was followed (apex saw Authorization %q)", v)
	}
}

func TestSenderRejectsOversizedBatch(t *testing.T) {
	b := ingest.Batch{Messages: make([]ingest.Message, ingest.MaxBatchSize+1)}
	if _, err := newSender(t, "http://localhost:1").Send(context.Background(), b); err == nil {
		t.Fatal("oversized batch accepted")
	}
}

// ---------------------------------------------------------------------------
// Verify TLS enforcement rejects plaintext HTTP for non-localhost
// ---------------------------------------------------------------------------

func TestSenderRejectPlaintextHTTP(t *testing.T) {
	_, err := New("http://example.com", "key", newTestLogger())
	if err == nil {
		t.Fatal("expected error for plaintext HTTP to non-localhost")
	}

	// Localhost should be allowed with plaintext
	s, err := New("http://localhost:8080", "key", newTestLogger())
	if err != nil {
		t.Fatalf("localhost plaintext should be allowed: %v", err)
	}
	if s == nil {
		t.Fatal("expected non-nil sender for localhost")
	}
}
