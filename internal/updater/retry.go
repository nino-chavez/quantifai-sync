package updater

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// defaultRetryBase is the first wait after a check fails for a reason that
// may clear up on its own; each further failure doubles it, up to the
// normal check interval.
const defaultRetryBase = time.Minute

// statusError is a non-200 response from GitHub.
type statusError struct {
	msg         string
	code        int
	rateLimited bool
}

func (e *statusError) Error() string { return e.msg }

func newStatusError(resp *http.Response, format string, args ...any) error {
	return &statusError{
		msg:         fmt.Sprintf(format, args...),
		code:        resp.StatusCode,
		rateLimited: resp.Header.Get("X-RateLimit-Remaining") == "0",
	}
}

// retryable reports whether a failed check is worth retrying soon: a
// network failure (timeout, refused connection, TLS handshake), a GitHub
// server error, or rate limiting. Anything else, such as a checksum
// mismatch or a missing asset, would fail the same way again.
func retryable(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		return se.code >= 500 || se.code == http.StatusTooManyRequests || se.rateLimited
	}
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, io.ErrUnexpectedEOF)
}

// nextRetry doubles the previous wait, starting at base, never past limit.
func nextRetry(prev, base, limit time.Duration) time.Duration {
	next := base
	if prev > 0 {
		next = 2 * prev
	}
	if next > limit {
		next = limit
	}
	return next
}
