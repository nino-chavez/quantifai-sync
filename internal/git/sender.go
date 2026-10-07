package git

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/quantifai/sync/internal/ingest"
	"github.com/quantifai/sync/internal/logger"
)

// defaultQueuePath returns the path to the commit events queue file.
func defaultQueuePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "quantifai", "commit-events.jsonl")
}

// QueueCommitEvent appends a commit event as a JSON line to the local
// queue file.  File locking (flock) prevents concurrent writes from
// multiple repos' post-commit hooks.  This function does no network
// I/O and returns immediately.
func QueueCommitEvent(event *CommitEvent) error {
	return queueCommitEvent(defaultQueuePath(), event)
}

func queueCommitEvent(path string, event *CommitEvent) error {
	// Ensure parent directory exists
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create queue dir: %w", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("open queue file: %w", err)
	}
	defer f.Close()

	// Acquire exclusive lock
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock queue file: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	data = append(data, '\n')

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write event: %w", err)
	}

	return nil
}

// ReadQueue returns up to max queued commit events without removing them,
// plus the number of bytes they occupy. Call AckQueue with that count once the
// events are stored on the server. The old read-and-truncate flow lost
// every event whose POST failed with a non-retryable status.
func ReadQueue(path string, max int) ([]*CommitEvent, int64, error) {
	if path == "" {
		path = defaultQueuePath()
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
		return nil, 0, fmt.Errorf("open queue file: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		return nil, 0, fmt.Errorf("lock queue file: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, 0, fmt.Errorf("read queue file: %w", err)
	}
	// Only whole lines; a hook may be mid-append.
	var events []*CommitEvent
	var n int64
	for len(events) < max {
		i := bytes.IndexByte(data[n:], '\n')
		if i < 0 {
			break
		}
		line := bytes.TrimSpace(data[n : n+int64(i)])
		n += int64(i) + 1
		if len(line) == 0 {
			continue
		}
		var ev CommitEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue // skip malformed lines
		}
		events = append(events, &ev)
	}
	return events, n, nil
}

// AckQueue removes the first n bytes of the queue (the events returned by
// ReadQueue), keeping anything a hook appended since.
func AckQueue(path string, n int64) error {
	if path == "" {
		path = defaultQueuePath()
	}
	if n <= 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open queue file: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock queue file: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	data, err := io.ReadAll(f)
	if err != nil {
		return fmt.Errorf("read queue file: %w", err)
	}
	if n > int64(len(data)) {
		n = int64(len(data))
	}
	rest := data[n:]
	if err := f.Truncate(0); err != nil {
		return fmt.Errorf("truncate queue file: %w", err)
	}
	if _, err := f.WriteAt(rest, 0); err != nil {
		return fmt.Errorf("rewrite queue file: %w", err)
	}
	return nil
}

// BatchSender posts one ingest batch and verifies the server wrote it.
type BatchSender func(ctx context.Context, b ingest.Batch) (ingest.Result, error)

// FlushCommitQueue sends queued commits to POST /api/v1/ingest as
// gitEvents and removes them from the queue only after the server has
// accepted all of them. It returns the number of events stored.
func FlushCommitQueue(ctx context.Context, path string, send BatchSender, lite bool, log *logger.Logger) int {
	events, n, err := ReadQueue(path, ingest.MaxBatchSize)
	if err != nil {
		log.Warn("failed to read commit queue", map[string]any{"error": err.Error()})
		return 0
	}
	if len(events) == 0 {
		if n > 0 {
			_ = AckQueue(path, n) // only malformed lines
		}
		return 0
	}

	batch := ingest.Batch{GitEvents: make([]ingest.GitEvent, 0, len(events))}
	for _, ev := range events {
		batch.GitEvents = append(batch.GitEvents, toGitEvent(ev, lite))
	}
	if _, err := send(ctx, batch); err != nil {
		log.Warn("commit events not stored; kept in queue", map[string]any{
			"error":  err.Error(),
			"events": len(batch.GitEvents),
		})
		return 0
	}
	if err := AckQueue(path, n); err != nil {
		log.Warn("commit events stored but queue not cleared; they will be re-sent (the server upserts by repo+sha)", map[string]any{"error": err.Error()})
	}
	return len(batch.GitEvents)
}

// toGitEvent maps a queued commit to the server's IngestGitEvent, the way
// scripts/import-git-events.ts does: repo is the repository's last path
// segment, unitProjectPath its top-level path, message the subject line.
func toGitEvent(ev *CommitEvent, lite bool) ingest.GitEvent {
	out := ingest.GitEvent{
		CommitSha:  ev.CommitSHA,
		AuthoredAt: ev.Timestamp,
		IsMerge:    ev.MergeCommit,
	}
	if ev.RepoPath != "" {
		path, name, _ := ingest.NormalizeProjectPath(ev.RepoPath, ev.RepoPath)
		out.Repo = name
		if lite {
			path = name
		}
		out.UnitProjectPath = &path
		if id := noteSessionID(ev.RepoPath, ev.CommitSHA); id != "" {
			out.NoteSessionID = &id
		}
	} else {
		// Queued by an older build: no repo path to resolve a unit from.
		out.Repo = filepath.Base(ev.RepoRemoteURL)
	}
	if ev.Subject != "" && !lite {
		subject := ev.Subject
		out.Message = &subject
	}
	return out
}

// noteSessionID reads the refs/notes/quantifai note the quantifai
// post-commit hook writes, and returns its session id. Only a note counts:
// the server records this as a deterministic link, so a guess (such as a
// process scan) must never be passed here. Any failure means "no note".
func noteSessionID(repoPath, sha string) string {
	ctx, cancel := context.WithTimeout(context.Background(), gitCmdTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "-C", repoPath, "notes", "--ref=refs/notes/quantifai", "show", sha).Output()
	if err != nil {
		return ""
	}
	var note struct {
		SessionID string   `json:"session_id"`
		Source    string   `json:"source"`
		Ts        *float64 `json:"ts"`
	}
	if json.Unmarshal(bytes.TrimSpace(out), &note) != nil || note.SessionID == "" || note.Source == "" || note.Ts == nil {
		return ""
	}
	return note.SessionID
}
