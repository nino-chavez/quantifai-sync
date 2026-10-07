// Package sender posts ingest batches to the QuantifAI server with retry
// and backoff: retry on HTTP 429, 5xx and network errors with exponential
// backoff (3 retries, base delay 1s), fail immediately on other 4xx.
//
// A 2xx is not trusted on its own. The response counts must show the
// server wrote what was sent (see ingest.Check); otherwise the batch is
// reported as failed so the caller keeps its offsets and retries.
package sender

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/quantifai/sync/internal/ingest"
	"github.com/quantifai/sync/internal/logger"
)

// Sender posts ingest.Batch bodies to {apiURL}/api/v1/ingest with Bearer
// token authentication and automatic retry on transient failures.
type Sender struct {
	apiURL     string
	apiKey     string
	client     *http.Client
	log        *logger.Logger
	maxRetries int
	baseDelay  time.Duration

	// sleepFn is the function used to wait between retries.  It defaults
	// to time.Sleep but can be replaced in tests to avoid real delays.
	sleepFn func(time.Duration)
}

// Option configures optional Sender parameters.
type Option func(*Sender)

// WithHTTPClient overrides the default http.Client (useful for testing).
// The client's redirect policy is replaced with NoCrossHostRedirect.
func WithHTTPClient(c *http.Client) Option {
	return func(s *Sender) { s.client = c }
}

// WithMaxRetries overrides the default retry count (3).
func WithMaxRetries(n int) Option {
	return func(s *Sender) { s.maxRetries = n }
}

// WithBaseDelay overrides the default base backoff delay (1s).
func WithBaseDelay(d time.Duration) Option {
	return func(s *Sender) { s.baseDelay = d }
}

// WithSleepFunc overrides time.Sleep for testing to avoid real delays.
func WithSleepFunc(fn func(time.Duration)) Option {
	return func(s *Sender) { s.sleepFn = fn }
}

// NoCrossHostRedirect is an http.Client CheckRedirect policy. Go drops the
// Authorization header when a redirect changes host (www.quantifai.app
// 308s to quantifai.app), so following one turns into a 401 that looks
// like a bad key. Refusing it surfaces the real cause: the configured
// api_url is not the canonical host.
func NoCrossHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > 0 && req.URL.Host != via[0].URL.Host {
		return fmt.Errorf("refusing redirect from %s to %s: set api_url to %s://%s",
			via[0].URL.Host, req.URL.Host, req.URL.Scheme, req.URL.Host)
	}
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	return nil
}

// NewClient returns an http.Client with the given timeout and the
// cross-host redirect refusal applied.
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: NoCrossHostRedirect}
}

// New creates a Sender that posts to {apiURL}/api/v1/ingest.
// TLS is enforced unless the URL targets localhost (for local dev).
func New(apiURL, apiKey string, log *logger.Logger, opts ...Option) (*Sender, error) {
	if apiURL == "" {
		return nil, fmt.Errorf("sender: api_url is required")
	}

	// Enforce TLS unless the target is localhost (for local development)
	lower := strings.ToLower(apiURL)
	if strings.HasPrefix(lower, "http://") && !isLocalhost(lower) {
		return nil, fmt.Errorf("sender: plaintext HTTP is not allowed for non-localhost api_url %q; use HTTPS", apiURL)
	}

	s := &Sender{
		apiURL:     strings.TrimRight(apiURL, "/"),
		apiKey:     apiKey,
		client:     NewClient(60 * time.Second),
		log:        log,
		maxRetries: 3,
		baseDelay:  1 * time.Second,
		sleepFn:    time.Sleep,
	}

	for _, opt := range opts {
		opt(s)
	}
	s.client.CheckRedirect = NoCrossHostRedirect
	return s, nil
}

// Send posts one batch and returns nil only when the server's response
// shows the batch was written (ingest.Check).
//
// Retry strategy:
//   - HTTP 429 or 5xx: retry up to maxRetries times with exponential
//     backoff (baseDelay * 2^attempt).
//   - Network error (including a refused redirect): same backoff.
//   - Other HTTP 4xx: fail without retry.
//   - 2xx whose counts don't match the batch: fail without retry. The
//     caller keeps its offsets, so the data is retried next cycle.
func (s *Sender) Send(ctx context.Context, batch ingest.Batch) (ingest.Result, error) {
	var res ingest.Result
	if batch.Empty() {
		return res, nil
	}
	if batch.Size() > ingest.MaxBatchSize {
		return res, fmt.Errorf("sender: batch of %d exceeds server limit %d", batch.Size(), ingest.MaxBatchSize)
	}
	url := s.apiURL + "/api/v1/ingest"

	body, err := json.Marshal(batch)
	if err != nil {
		return res, fmt.Errorf("sender: marshal: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt <= s.maxRetries; attempt++ {
		if attempt > 0 {
			delay := s.backoffDelay(attempt - 1)
			s.log.Info("retrying ingest", map[string]any{
				"attempt":     attempt,
				"max_retries": s.maxRetries,
				"error":       lastErr.Error(),
				"delay_ms":    delay.Milliseconds(),
			})
			s.sleepFn(delay)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return res, fmt.Errorf("sender: build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+s.apiKey)

		resp, err := s.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			lastErr = err
			continue
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		status := resp.StatusCode

		if status >= 200 && status < 300 {
			if err := json.Unmarshal(respBody, &res); err != nil {
				return res, fmt.Errorf("sender: HTTP %d with unreadable body: %w", status, err)
			}
			if err := ingest.Check(batch, res); err != nil {
				return res, err
			}
			s.log.Info("batch written", map[string]any{
				"units":             res.UnitsOfWork,
				"sessions":          res.Sessions,
				"messages_sent":     len(batch.Messages),
				"messages_inserted": res.Messages.Accepted,
				"git_events":        res.GitEvents.Accepted,
			})
			return res, nil
		}

		lastErr = fmt.Errorf("HTTP %d: %s", status, truncate(string(respBody), 300))
		if status == 429 || status >= 500 {
			continue
		}
		return res, fmt.Errorf("sender: non-retryable %w", lastErr)
	}
	return res, fmt.Errorf("sender: retries exhausted: %w", lastErr)
}

// backoffDelay calculates the exponential backoff delay for a given attempt.
// Formula: baseDelay * 2^attempt
func (s *Sender) backoffDelay(attempt int) time.Duration {
	multiplier := 1 << attempt // 2^attempt
	return s.baseDelay * time.Duration(multiplier)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// isLocalhost returns true if the URL targets a localhost address,
// which is the only case where plaintext HTTP is permitted.
func isLocalhost(url string) bool {
	return strings.Contains(url, "://localhost") ||
		strings.Contains(url, "://127.0.0.1") ||
		strings.Contains(url, "://[::1]")
}
