package updater

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryable(t *testing.T) {
	resp := func(code int, remaining string) *http.Response {
		r := &http.Response{StatusCode: code, Header: http.Header{}}
		if remaining != "" {
			r.Header.Set("X-RateLimit-Remaining", remaining)
		}
		return r
	}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"TLS handshake timeout", fmt.Errorf("download archive: %w", &url.Error{Op: "Get", URL: "x", Err: errors.New("net/http: TLS handshake timeout")}), true},
		{"connection refused", fmt.Errorf("fetch latest release: %w", &net.OpError{Op: "dial", Err: errors.New("connection refused")}), true},
		{"body cut off", fmt.Errorf("download archive: %w", io.ErrUnexpectedEOF), true},
		{"server error", newStatusError(resp(502, ""), "GitHub API returned 502"), true},
		{"too many requests", newStatusError(resp(429, ""), "GitHub API returned 429"), true},
		{"rate limited", newStatusError(resp(403, "0"), "GitHub API returned 403"), true},
		{"forbidden", newStatusError(resp(403, "42"), "GitHub API returned 403"), false},
		{"not found", newStatusError(resp(404, ""), "GitHub API returned 404"), false},
		{"checksum mismatch", errors.New("checksum verification failed: checksum mismatch"), false},
		{"no asset", errors.New("no asset found for linux/amd64 in release v9"), false},
	}
	for _, tt := range tests {
		if got := retryable(tt.err); got != tt.want {
			t.Errorf("%s: retryable = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestNextRetry(t *testing.T) {
	var got []time.Duration
	var d time.Duration
	for i := 0; i < 7; i++ {
		d = nextRetry(d, time.Minute, 24*time.Minute)
		got = append(got, d)
	}
	want := []time.Duration{1, 2, 4, 8, 16, 24, 24}
	for i := range want {
		if got[i] != want[i]*time.Minute {
			t.Fatalf("retry waits %v, want 1m 2m 4m 8m 16m 24m 24m", got)
		}
	}
}

// failFirst fails its first n requests with a network error.
type failFirst struct {
	base http.RoundTripper
	n    atomic.Int32
}

func (f *failFirst) RoundTrip(req *http.Request) (*http.Response, error) {
	if f.n.Add(-1) >= 0 {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	}
	return f.base.RoundTrip(req)
}

// A network failure is retried within the retry base, not after the hour-long
// interval, so the update still lands.
func TestRunRetriesSoonAfterNetworkError(t *testing.T) {
	t.Setenv(UpdatedToEnv, "")
	g, _, downloads := fakeRelease(t)
	flaky := &failFirst{base: g.client.Transport}
	flaky.n.Store(2)
	g.client.Transport = flaky
	g.retryBase = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Run(ctx)

	select {
	case <-g.Applied():
	case <-time.After(10 * time.Second):
		t.Fatal("update not applied after two network failures; Run waited for the interval")
	}
	if n := downloads.Load(); n != 1 {
		t.Fatalf("archive downloaded %d times, want 1", n)
	}
}

// A failure that would repeat (here a 404) waits the full interval.
func TestRunWaitsIntervalAfterPermanentError(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.NotFound(w, r)
	}))
	defer server.Close()
	g := &GithubUpdater{
		log:       newDiscardLogger(),
		version:   "v0.3.0",
		repo:      "o/r",
		interval:  time.Hour,
		retryBase: 10 * time.Millisecond,
		client:    &http.Client{Transport: &rewriteTransport{base: http.DefaultTransport, target: server.URL}},
		applied:   make(chan struct{}, 1),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { g.Run(ctx); close(done) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	<-done
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d requests in 300ms after a 404, want 1 (no early retry)", n)
	}
}
