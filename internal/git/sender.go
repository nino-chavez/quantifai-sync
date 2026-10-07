package git

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// lockQueue takes an exclusive flock on a sidecar file next to the queue
// and returns the unlock function. Locking the sidecar rather than the
// queue file itself is what lets AckQueue replace the queue with an atomic
// rename: an appender that opens the queue only after holding this lock
// can never be left writing into a file that was just renamed away.
func lockQueue(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("create queue dir: %w", err)
	}
	lf, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open queue lock: %w", err)
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		lf.Close()
		return nil, fmt.Errorf("lock queue: %w", err)
	}
	return func() {
		syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
		lf.Close()
	}, nil
}

// QueueCommitEvent appends a commit event as a JSON line to the local
// queue file.  The queue lock prevents concurrent writes from multiple
// repos' post-commit hooks.  This function does no network I/O and
// returns immediately.
func QueueCommitEvent(event *CommitEvent) error {
	return queueCommitEvent(defaultQueuePath(), event)
}

func queueCommitEvent(path string, event *CommitEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	data = append(data, '\n')

	unlock, err := lockQueue(path)
	if err != nil {
		return err
	}
	defer unlock()

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return fmt.Errorf("open queue file: %w", err)
	}
	defer f.Close()
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
	unlock, err := lockQueue(path)
	if err != nil {
		return nil, 0, err
	}
	defer unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, nil
		}
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
// ReadQueue), keeping anything a hook appended since. The remainder is
// written to a temp file, synced, and renamed over the queue, so a crash at
// any point leaves either the old queue or the new one, never a truncated
// file.
func AckQueue(path string, n int64) error {
	if path == "" {
		path = defaultQueuePath()
	}
	if n <= 0 {
		return nil
	}
	unlock, err := lockQueue(path)
	if err != nil {
		return err
	}
	defer unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read queue file: %w", err)
	}
	if n > int64(len(data)) {
		n = int64(len(data))
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp queue: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(data[n:]); err != nil {
		tmp.Close()
		return fmt.Errorf("write temp queue: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync temp queue: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp queue: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0600); err != nil {
		return fmt.Errorf("chmod temp queue: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replace queue: %w", err)
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
