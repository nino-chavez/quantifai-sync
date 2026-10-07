// Package editor handles editor event ingestion from the VS Code extension.
// Events are queued locally as JSONL and flushed to the central API.
package editor

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/quantifai/sync/internal/filelock"
	"github.com/quantifai/sync/internal/logger"
)

// EditorEvent represents a single AI edit event from the VS Code extension.
type EditorEvent struct {
	EventType          string `json:"event_type"`
	Timestamp          string `json:"timestamp"`
	FilePath           string `json:"file_path,omitempty"`
	LineRange          string `json:"line_range,omitempty"`
	AIProvider         string `json:"ai_provider,omitempty"`
	AIInterface        string `json:"ai_interface,omitempty"`
	CharactersInserted int    `json:"characters_inserted"`
	Accepted           bool   `json:"accepted"`
	WorkspaceRoot      string `json:"workspace_root,omitempty"`
	GitRemoteURL       string `json:"git_remote_url,omitempty"`
}

// EditorEventBatch is the payload sent by the VS Code extension.
type EditorEventBatch struct {
	Events []EditorEvent `json:"events"`
}

func defaultQueuePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "quantifai", "editor-events.jsonl")
}

// QueueEvents appends editor events as JSON lines to the local queue file.
func QueueEvents(events []EditorEvent) error {
	path := defaultQueuePath()

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create queue dir: %w", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|filelock.AppendFlags, 0600)
	if err != nil {
		return fmt.Errorf("open queue file: %w", err)
	}
	defer f.Close()

	if err := filelock.Lock(f); err != nil {
		return fmt.Errorf("lock queue file: %w", err)
	}
	defer filelock.Unlock(f)

	for _, ev := range events {
		data, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		data = append(data, '\n')
		if _, err := f.Write(data); err != nil {
			return fmt.Errorf("write event: %w", err)
		}
	}

	return nil
}

// ReadAndClearQueue reads all editor events from the queue and truncates the file.
func ReadAndClearQueue() ([]EditorEvent, error) {
	path := defaultQueuePath()

	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open queue file: %w", err)
	}
	defer f.Close()

	if err := filelock.Lock(f); err != nil {
		return nil, fmt.Errorf("lock queue file: %w", err)
	}
	defer filelock.Unlock(f)

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read queue file: %w", err)
	}

	if err := f.Truncate(0); err != nil {
		return nil, fmt.Errorf("truncate queue file: %w", err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, fmt.Errorf("seek queue file: %w", err)
	}

	var events []EditorEvent
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev EditorEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		events = append(events, ev)
	}

	return events, nil
}

// There is no flush. The QuantifAI server has no editor-events endpoint or
// table (its ingest contract is units, sessions, messages and git events),
// and the old flush POSTed to /api/v1/ingest/editor-events, got a 404, and
// dropped the events it had already truncated from the queue. Events now
// stay in the local queue until the server has somewhere to put them.

// HandleEditorEvents returns an HTTP handler that accepts POST /api/v1/editor-events
// from the VS Code extension and queues them locally.
func HandleEditorEvents(log *logger.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB max
		if err != nil {
			http.Error(w, "failed to read body", http.StatusBadRequest)
			return
		}

		var batch EditorEventBatch
		if err := json.Unmarshal(body, &batch); err != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		if len(batch.Events) == 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"accepted":0,"errors":0}`))
			return
		}

		if err := QueueEvents(batch.Events); err != nil {
			log.Error("failed to queue editor events", map[string]any{"error": err.Error()})
			http.Error(w, "queue error", http.StatusInternalServerError)
			return
		}

		log.Info("editor events queued", map[string]any{"count": len(batch.Events)})

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		resp, _ := json.Marshal(map[string]int{
			"accepted": len(batch.Events),
			"errors":   0,
		})
		w.Write(resp)
	}
}
