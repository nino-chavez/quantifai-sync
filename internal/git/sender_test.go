package git

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/quantifai/sync/internal/ingest"
	"github.com/quantifai/sync/internal/logger"
)

func testLogger() *logger.Logger {
	l, _ := logger.New(logger.LevelError, "")
	return l
}

func TestFlushKeepsQueueWhenSendFails(t *testing.T) {
	q := filepath.Join(t.TempDir(), "commit-events.jsonl")
	for _, sha := range []string{"aaa", "bbb"} {
		if err := queueCommitEvent(q, &CommitEvent{CommitSHA: sha, Timestamp: "2026-10-06T10:00:00Z", RepoPath: "/r/repo", Subject: "s"}); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := os.ReadFile(q)

	failing := func(context.Context, ingest.Batch) (ingest.Result, error) {
		return ingest.Result{}, errors.New("HTTP 404")
	}
	if n := FlushCommitQueue(context.Background(), q, failing, false, testLogger()); n != 0 {
		t.Fatalf("reported %d stored on failure", n)
	}
	after, _ := os.ReadFile(q)
	if string(before) != string(after) {
		t.Fatal("queue changed after a failed send; events would be lost")
	}

	var sent ingest.Batch
	ok := func(_ context.Context, b ingest.Batch) (ingest.Result, error) {
		sent = b
		return ingest.Result{}, nil
	}
	if n := FlushCommitQueue(context.Background(), q, ok, false, testLogger()); n != 2 {
		t.Fatalf("want 2 stored, got %d", n)
	}
	if len(sent.GitEvents) != 2 || len(sent.Messages) != 0 {
		t.Fatalf("unexpected batch: %+v", sent)
	}
	if data, _ := os.ReadFile(q); len(data) != 0 {
		t.Fatalf("queue not cleared after success: %q", data)
	}
}

func TestAckQueueKeepsEventsAppendedDuringSend(t *testing.T) {
	q := filepath.Join(t.TempDir(), "commit-events.jsonl")
	queueCommitEvent(q, &CommitEvent{CommitSHA: "aaa"})
	events, n, err := ReadQueue(q, 100)
	if err != nil || len(events) != 1 {
		t.Fatalf("read: %v, %d events", err, len(events))
	}
	queueCommitEvent(q, &CommitEvent{CommitSHA: "bbb"}) // a hook fires mid-send
	if err := AckQueue(q, n); err != nil {
		t.Fatal(err)
	}
	rest, _, _ := ReadQueue(q, 100)
	if len(rest) != 1 || rest[0].CommitSHA != "bbb" {
		t.Fatalf("want only bbb left, got %+v", rest)
	}
}

func TestReadQueueStopsAtMax(t *testing.T) {
	q := filepath.Join(t.TempDir(), "commit-events.jsonl")
	for _, sha := range []string{"a", "b", "c"} {
		queueCommitEvent(q, &CommitEvent{CommitSHA: sha})
	}
	events, n, _ := ReadQueue(q, 2)
	if len(events) != 2 {
		t.Fatalf("want 2, got %d", len(events))
	}
	AckQueue(q, n)
	rest, _, _ := ReadQueue(q, 10)
	if len(rest) != 1 || rest[0].CommitSHA != "c" {
		t.Fatalf("want c left, got %+v", rest)
	}
}

func TestToGitEventMatchesImporterShape(t *testing.T) {
	ev := &CommitEvent{
		CommitSHA: "abc", Timestamp: "2026-10-06T10:00:00-05:00", MergeCommit: true,
		RepoPath: "/Users/x/repo/.claude/worktrees/agent-1", Subject: "fix: thing",
	}
	g := toGitEvent(ev, false)
	if g.Repo != "repo" || g.UnitProjectPath == nil || *g.UnitProjectPath != "/Users/x/repo" ||
		g.Message == nil || *g.Message != "fix: thing" || !g.IsMerge || g.AuthoredAt != ev.Timestamp {
		t.Fatalf("unexpected event: %+v", g)
	}
	if g.NoteSessionID != nil {
		t.Fatal("no note exists, so no deterministic session id may be sent")
	}

	lite := toGitEvent(ev, true)
	if lite.Message != nil || *lite.UnitProjectPath != "repo" {
		t.Fatalf("lite mode leaked message or path: %+v", lite)
	}

	legacy := toGitEvent(&CommitEvent{CommitSHA: "d", RepoRemoteURL: "https://github.com/o/legacy"}, false)
	if legacy.Repo != "legacy" || legacy.UnitProjectPath != nil {
		t.Fatalf("legacy event mapped wrong: %+v", legacy)
	}
}

func TestNoteSessionIDReadsQuantifaiNote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return string(out)
	}
	run("init", "-q")
	run("commit", "-q", "--allow-empty", "-m", "c1")
	sha := run("rev-parse", "HEAD")
	sha = sha[:len(sha)-1]

	if id := noteSessionID(dir, sha); id != "" {
		t.Fatalf("no note yet, got %q", id)
	}
	run("notes", "--ref=refs/notes/quantifai", "add", "-m", `{"session_id":"sess-1","source":"env","ts":1}`, sha)
	if id := noteSessionID(dir, sha); id != "sess-1" {
		t.Fatalf("want sess-1, got %q", id)
	}
	run("notes", "--ref=refs/notes/quantifai", "add", "-f", "-m", `{"session_id":"sess-1"}`, sha)
	if id := noteSessionID(dir, sha); id != "" {
		t.Fatalf("malformed note must not link, got %q", id)
	}
}
