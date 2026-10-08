package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// startTestServer creates a health server on an ephemeral port, starts
// it, and returns the server.  The caller must call Shutdown when done.
// The Listen/Serve split ensures Addr() is safe to call immediately
// after this function returns (no data race on the listener field).
func startTestServer(t *testing.T, state *HealthState) *Server {
	t.Helper()
	srv := NewServer(0, state)
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen failed: %v", err)
	}
	go srv.Serve()
	return srv
}

// ---------------------------------------------------------------------------
// Test 1: /healthz returns correct JSON schema
// ---------------------------------------------------------------------------

func TestHealthzReturnsCorrectJSONSchema(t *testing.T) {
	state := NewHealthState("1.2.3")
	state.SetFilesTracked(42)
	state.SetRecordsBuffered(128)
	state.SetErrorsLastHour(0)
	state.SetLastSyncTime(time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC))

	srv := startTestServer(t, state)
	defer srv.Shutdown(context.Background())

	resp, err := http.Get(fmt.Sprintf("http://%s/healthz", srv.Addr()))
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}

	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type: got %q, want application/json", ct)
	}

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode JSON: %v", err)
	}

	// Verify all required fields are present
	requiredFields := []string{
		"status",
		"version",
		"uptime_seconds",
		"last_sync_time",
		"files_tracked",
		"records_buffered",
		"errors_last_hour",
	}
	for _, field := range requiredFields {
		if _, ok := body[field]; !ok {
			t.Errorf("missing required field %q in /healthz response", field)
		}
	}

	// Verify specific values. The fixed sync date is months old, so the
	// status must report the pipeline as stale.
	if body["status"] != "degraded" {
		t.Errorf("status: got %q, want %q", body["status"], "degraded")
	}
	if body["version"] != "1.2.3" {
		t.Errorf("version: got %q, want %q", body["version"], "1.2.3")
	}
	if body["last_sync_time"] != "2026-03-01T12:00:00Z" {
		t.Errorf("last_sync_time: got %q, want %q", body["last_sync_time"], "2026-03-01T12:00:00Z")
	}
	if body["files_tracked"] != float64(42) {
		t.Errorf("files_tracked: got %v, want 42", body["files_tracked"])
	}
	if body["records_buffered"] != float64(128) {
		t.Errorf("records_buffered: got %v, want 128", body["records_buffered"])
	}
	if body["errors_last_hour"] != float64(0) {
		t.Errorf("errors_last_hour: got %v, want 0", body["errors_last_hour"])
	}

	// uptime_seconds should be a non-negative number
	uptime, ok := body["uptime_seconds"].(float64)
	if !ok || uptime < 0 {
		t.Errorf("uptime_seconds: got %v, want non-negative number", body["uptime_seconds"])
	}
}

// ---------------------------------------------------------------------------
// Test 2: /healthz status transitions: ok -> degraded -> error
// ---------------------------------------------------------------------------

func TestHealthzStatusTransitions(t *testing.T) {
	state := NewHealthState("1.0.0")

	srv := startTestServer(t, state)
	defer srv.Shutdown(context.Background())

	addr := srv.Addr()

	// Helper to fetch and parse the status field. It also checks the HTTP
	// code: 503 for "error", so a plain HTTP check sees the failure, and
	// 200 otherwise.
	getStatus := func() string {
		t.Helper()
		resp, err := http.Get(fmt.Sprintf("http://%s/healthz", addr))
		if err != nil {
			t.Fatalf("GET /healthz failed: %v", err)
		}
		defer resp.Body.Close()

		var body map[string]any
		json.NewDecoder(resp.Body).Decode(&body)
		status := body["status"].(string)
		want := http.StatusOK
		if status == "error" {
			want = http.StatusServiceUnavailable
		}
		if resp.StatusCode != want {
			t.Errorf("status %q answered HTTP %d, want %d", status, resp.StatusCode, want)
		}
		return status
	}

	// Never synced: degraded, even though nothing has failed
	if got := getStatus(); got != "degraded" {
		t.Errorf("never-synced status: got %q, want %q", got, "degraded")
	}

	// A recent acknowledged sync earns "ok"
	state.SetLastSyncTime(time.Now())
	if got := getStatus(); got != "ok" {
		t.Errorf("synced status: got %q, want %q", got, "ok")
	}

	// A sync older than the stale threshold degrades again
	state.SetStaleAfter(time.Minute)
	state.SetLastSyncTime(time.Now().Add(-2 * time.Minute))
	if got := getStatus(); got != "degraded" {
		t.Errorf("stale status: got %q, want %q", got, "degraded")
	}
	state.SetLastSyncTime(time.Now())

	// Simulate send failures: transition to "degraded"
	state.SetStatus(StatusDegraded)
	if got := getStatus(); got != "degraded" {
		t.Errorf("degraded status: got %q, want %q", got, "degraded")
	}

	// Simulate all retries exhausted: transition to "error"
	state.SetStatus(StatusError)
	if got := getStatus(); got != "error" {
		t.Errorf("error status: got %q, want %q", got, "error")
	}

	// Simulate recovery: transition back to "ok"
	state.SetStatus(StatusOK)
	if got := getStatus(); got != "ok" {
		t.Errorf("recovered status: got %q, want %q", got, "ok")
	}

	// An agent waiting on a config problem reports "error"
	state.SetProblem("no API key")
	if got := getStatus(); got != "error" {
		t.Errorf("waiting status: got %q, want %q", got, "error")
	}
	state.SetProblem("")
	if got := getStatus(); got != "ok" {
		t.Errorf("problem cleared status: got %q, want %q", got, "ok")
	}
}

// ---------------------------------------------------------------------------
// Test 3: Health server binds to 127.0.0.1 only (not 0.0.0.0)
// ---------------------------------------------------------------------------

func TestHealthServerBindsToLocalhostOnly(t *testing.T) {
	state := NewHealthState("1.0.0")

	srv := startTestServer(t, state)
	defer srv.Shutdown(context.Background())

	// Verify the listener is bound to 127.0.0.1
	addr := srv.Addr()
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("failed to parse addr %q: %v", addr, err)
	}

	if host != "127.0.0.1" {
		t.Errorf("server bound to %q, want 127.0.0.1 (localhost only)", host)
	}

	// Verify a request to 127.0.0.1 succeeds (proves the binding works)
	_, port, _ := net.SplitHostPort(addr)
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%s/healthz", port))
	if err != nil {
		t.Fatalf("request to 127.0.0.1 failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200 from 127.0.0.1, got %d", resp.StatusCode)
	}
}

// While the agent waits on a config problem, /health says so: status
// "error" and the problem itself. Clearing it restores the usual status.
func TestSnapshotReportsProblem(t *testing.T) {
	h := NewHealthState("v1")
	h.SetLastSyncTime(time.Now())
	h.SetProblem("no API key: run `quantifai-sync install --api-key <key>`")

	b, _ := json.Marshal(h.Snapshot())
	got := string(b)
	for _, want := range []string{`"status":"error"`, `"problem":"no API key: run`} {
		if !strings.Contains(got, want) {
			t.Errorf("snapshot missing %s:\n%s", want, got)
		}
	}

	h.SetProblem("")
	b, _ = json.Marshal(h.Snapshot())
	if strings.Contains(string(b), "problem") || !strings.Contains(string(b), `"status":"ok"`) {
		t.Errorf("after clearing the problem: %s", b)
	}
}

// A port held by another process (the old agent during a reinstall) must
// not leave the server without an endpoint: it binds once the port frees.
func TestListenAndServeWaitsForTakenPort(t *testing.T) {
	old := listenRetryInterval
	listenRetryInterval = 50 * time.Millisecond
	defer func() { listenRetryInterval = old }()

	holder, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := holder.Addr().(*net.TCPAddr).Port

	srv := NewServer(port, NewHealthState("v-test"))
	failed := make(chan error, 2)
	srv.ListenFailed = func(err error) { failed <- err }
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	defer srv.Shutdown(context.Background())

	select {
	case <-failed:
	case <-time.After(2 * time.Second):
		t.Fatal("ListenFailed was not called while the port was taken")
	}
	holder.Close()

	url := fmt.Sprintf("http://127.0.0.1:%d/healthz", port)
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("health endpoint never came up after the port was freed: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(failed) != 0 {
		t.Error("ListenFailed was called more than once")
	}
}

func TestShutdownStopsListenRetry(t *testing.T) {
	old := listenRetryInterval
	listenRetryInterval = 50 * time.Millisecond
	defer func() { listenRetryInterval = old }()

	holder, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	srv := NewServer(holder.Addr().(*net.TCPAddr).Port, NewHealthState("v-test"))
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	time.Sleep(150 * time.Millisecond)
	srv.Shutdown(context.Background())
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("ListenAndServe returned %v, want http.ErrServerClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ListenAndServe kept retrying after Shutdown")
	}
}
