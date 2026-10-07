package filelock

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A second handle's Lock must wait until the first handle unlocks.
func TestLockExcludesSecondHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.lock")
	open := func() *os.File {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}
	a, b := open(), open()

	if err := Lock(a); err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() { got <- Lock(b) }()

	select {
	case err := <-got:
		t.Fatalf("second Lock returned while the first was held (err=%v)", err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := Unlock(a); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second Lock still blocked after the first unlocked")
	}
	if err := Unlock(b); err != nil {
		t.Fatal(err)
	}
}

// A handle opened with AppendFlags must be lockable and must append.
func TestAppendFlagsHandleLocksAndAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.jsonl")
	if err := os.WriteFile(path, []byte("a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|AppendFlags, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := Lock(f); err != nil {
		t.Fatalf("lock append handle: %v", err)
	}
	if _, err := f.Write([]byte("b\n")); err != nil {
		t.Fatal(err)
	}
	if err := Unlock(f); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "a\nb\n" {
		t.Fatalf("got %q, want %q", data, "a\nb\n")
	}
}
