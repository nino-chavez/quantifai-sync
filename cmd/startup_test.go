package cmd

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestWaitForRetriesAndReportsEachProblemOnce(t *testing.T) {
	problems := []error{
		errors.New("no API key"),
		errors.New("no API key"),
		errors.New("invalid config: api_url is required"),
		nil,
	}
	calls := 0
	load := func() (string, error) {
		err := problems[calls]
		calls++
		if err != nil {
			return "", err
		}
		return "ready", nil
	}
	var out strings.Builder

	v, ok := waitFor(load, time.Millisecond, nil, &out)
	if !ok || v != "ready" || calls != 4 {
		t.Fatalf("waitFor = %q, %v after %d calls; want ready, true after 4", v, ok, calls)
	}
	got := out.String()
	if n := strings.Count(got, "no API key"); n != 1 {
		t.Errorf("repeated problem printed %d times, want once:\n%s", n, got)
	}
	for _, want := range []string{"invalid config: api_url is required", "config problem resolved"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
}

func TestWaitForStopsOnSignal(t *testing.T) {
	stop := make(chan os.Signal, 1)
	stop <- os.Interrupt
	load := func() (int, error) { return 0, errors.New("sync_enabled is false") }
	var out strings.Builder

	done := make(chan bool)
	go func() {
		_, ok := waitFor(load, time.Hour, stop, &out)
		done <- ok
	}()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("waitFor reported success after a stop signal")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waitFor kept waiting after a stop signal")
	}
}
