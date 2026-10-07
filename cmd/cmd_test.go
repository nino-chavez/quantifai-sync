package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Test 1: version subcommand outputs version string
// ---------------------------------------------------------------------------

func TestVersionSubcommandOutputsVersionString(t *testing.T) {
	code := RunVersion()
	if code != 0 {
		t.Errorf("expected exit code 0, got %d", code)
	}

	// Verify the version constant is set
	if Version == "" {
		t.Error("Version constant is empty")
	}
	if Version != "0.1.0" {
		t.Errorf("Version: got %q, want %q", Version, "0.1.0")
	}
}

// ---------------------------------------------------------------------------
// Test 2: healthcheck subcommand hits localhost health endpoint and returns
//         exit code 0 for "ok" status, 1 for failure
// ---------------------------------------------------------------------------

func TestHealthcheckReturnsCorrectExitCodes(t *testing.T) {
	// Subtest: healthy server returns exit code 0
	t.Run("ok_status_returns_0", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/healthz" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"status":           "ok",
				"version":          "0.1.0",
				"uptime_seconds":   3600,
				"last_sync_time":   "2026-03-01T12:00:00Z",
				"files_tracked":    42,
				"records_buffered": 0,
				"errors_last_hour": 0,
			})
		}))
		defer srv.Close()

		// Extract port from test server URL
		port := extractPort(t, srv.URL)
		code := runHealthcheckWithClient(srv.Client(), port)
		if code != 0 {
			t.Errorf("expected exit code 0 for ok status, got %d", code)
		}
	})

	// Subtest: degraded server returns exit code 1
	t.Run("degraded_status_returns_1", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"status":           "degraded",
				"version":          "0.1.0",
				"uptime_seconds":   100,
				"errors_last_hour": 5,
			})
		}))
		defer srv.Close()

		port := extractPort(t, srv.URL)
		code := runHealthcheckWithClient(srv.Client(), port)
		if code != 1 {
			t.Errorf("expected exit code 1 for degraded status, got %d", code)
		}
	})

	// Subtest: unreachable server returns exit code 1
	t.Run("unreachable_returns_1", func(t *testing.T) {
		// Use a port where nothing is listening
		code := runHealthcheckWithClient(&http.Client{}, 19999)
		if code != 1 {
			t.Errorf("expected exit code 1 for unreachable server, got %d", code)
		}
	})
}

// extractPort parses the port number from a test server URL like
// "http://127.0.0.1:12345".
func extractPort(t *testing.T, url string) int {
	t.Helper()
	var port int
	// URL format: http://127.0.0.1:PORT
	_, err := fmt.Sscanf(url, "http://127.0.0.1:%d", &port)
	if err != nil {
		// Try the [::] format
		_, err = fmt.Sscanf(url, "http://[::1]:%d", &port)
		if err != nil {
			t.Fatalf("could not extract port from %q: %v", url, err)
		}
	}
	return port
}

// healthcheck shows the problem a waiting agent reports and exits 1.
func TestHealthcheckPrintsProblem(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"status":  "error",
			"version": "v1",
			"problem": "no API key: run `quantifai-sync install --api-key <key>`",
		})
	}))
	defer srv.Close()

	var out strings.Builder
	code := runHealthcheck(srv.Client(), extractPort(t, srv.URL), &out)
	if code != 1 || !strings.Contains(out.String(), "problem: no API key") {
		t.Fatalf("exit %d, output:\n%s\nwant exit 1 and the problem", code, out.String())
	}
}
