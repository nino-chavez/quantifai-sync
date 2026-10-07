//go:build darwin

package tray

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The agent sends status "error" with HTTP 503. The tray must still read
// the report, so it shows "Waiting: <problem>" rather than "Not Running".
func TestFetchHealthReadsErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]any{"status": "error", "problem": "no API key"})
	}))
	defer srv.Close()

	h, ok := fetchHealth(srv.URL)
	if !ok || h.Status != "error" || h.Problem != "no API key" {
		t.Fatalf("fetchHealth = %+v, %v; want the error report", h, ok)
	}
}
