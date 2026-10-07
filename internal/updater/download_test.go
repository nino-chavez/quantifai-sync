package updater

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"
)

// sendSlowly writes data in pieces, flushing each one, so the whole body
// takes about total to arrive while bytes keep coming.
func sendSlowly(pieces int, total time.Duration) func(http.ResponseWriter, *http.Request, []byte) {
	return func(w http.ResponseWriter, r *http.Request, data []byte) {
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		size := (len(data) + pieces - 1) / pieces
		for len(data) > 0 {
			n := min(size, len(data))
			w.Write(data[:n])
			w.(http.Flusher).Flush()
			data = data[n:]
			select {
			case <-time.After(total / time.Duration(pieces)):
			case <-r.Context().Done():
				return
			}
		}
	}
}

// A slow link that keeps delivering must be able to finish the download,
// however long the whole archive takes. The client's timeout stands in
// for the 30s limit that used to cover the entire download.
func TestDownloadFinishesOnSlowLink(t *testing.T) {
	t.Setenv(UpdatedToEnv, "")
	g, exe, _ := fakeReleaseServing(t, "per-file", sendSlowly(8, time.Second))
	g.client.Timeout = 300 * time.Millisecond
	// Shorter than the whole download, longer than the gap between pieces.
	g.stallTimeout = 400 * time.Millisecond

	applied, err := g.CheckAndApply(context.Background())
	if err != nil || !applied {
		t.Fatalf("CheckAndApply = %v, %v; want true, nil", applied, err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "new binary v9.9.9" {
		t.Fatalf("installed %q, want the new binary", got)
	}
}

// sendHalfThenStop sends the headers and half the archive, then sends
// nothing more until the client gives up.
func sendHalfThenStop(w http.ResponseWriter, r *http.Request, data []byte) {
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data[:len(data)/2])
	w.(http.Flusher).Flush()
	<-r.Context().Done()
}

// sendNothing never answers until the client gives up.
func sendNothing(_ http.ResponseWriter, r *http.Request, _ []byte) {
	<-r.Context().Done()
}

// A download that stops delivering is abandoned after the stall timeout,
// is retried soon, and leaves the installed binary alone.
func TestDownloadAbandonedWhenStalled(t *testing.T) {
	for name, serve := range map[string]func(http.ResponseWriter, *http.Request, []byte){
		"mid-body":   sendHalfThenStop,
		"no headers": sendNothing,
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(UpdatedToEnv, "")
			g, exe, _ := fakeReleaseServing(t, "per-file", serve)
			g.stallTimeout = 200 * time.Millisecond

			start := time.Now()
			applied, err := g.CheckAndApply(context.Background())
			took := time.Since(start)
			if applied || err == nil {
				t.Fatalf("CheckAndApply = %v, %v; want an error", applied, err)
			}
			var se *stalledError
			if !errors.As(err, &se) {
				t.Fatalf("error %q is not a stall", err)
			}
			if !retryable(err) {
				t.Fatalf("stall %q is not retryable", err)
			}
			if took > 2*time.Second {
				t.Fatalf("gave up after %s, want about the 200ms stall timeout", took)
			}
			if got, _ := os.ReadFile(exe); string(got) != "old binary" {
				t.Fatalf("installed binary changed to %q", got)
			}
		})
	}
}

// Shutting down during a download is reported as the shutdown, not as a
// stall.
func TestDownloadCancelledIsNotStall(t *testing.T) {
	t.Setenv(UpdatedToEnv, "")
	g, _, _ := fakeReleaseServing(t, "per-file", sendHalfThenStop)
	g.stallTimeout = time.Minute

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	_, err := g.CheckAndApply(ctx)
	var se *stalledError
	if err == nil || errors.As(err, &se) {
		t.Fatalf("CheckAndApply after shutdown = %v; want a cancellation, not a stall", err)
	}
}
