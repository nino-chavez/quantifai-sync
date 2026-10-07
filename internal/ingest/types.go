// Package ingest is the client side of the QuantifAI server's
// POST /api/v1/ingest contract (apps/app/src/lib/server/ingest.ts in the
// quantifai repo). The server takes a normalized batch —
// {unitsOfWork?, sessions?, messages?, gitEvents?} — not raw per-line
// records, and it replaces a session's totals on every upsert rather than
// adding to them. Everything in this package exists to produce rows that
// are identical to what the reference importer
// (apps/app/scripts/import-claude-jsonl.ts) writes for the same files.
package ingest

import "fmt"

// MaxBatchSize mirrors the server's MAX_BATCH_SIZE. It counts
// messages + sessions + gitEvents together; unitsOfWork are not counted.
const MaxBatchSize = 10_000

// Unit is one units_of_work row (server: UnitOfWorkInput).
type Unit struct {
	Kind        string `json:"kind"`   // "initiative" | "project"
	Name        string `json:"name"`   // last path segment
	Source      string `json:"source"` // always "path" from this client
	ProjectPath string `json:"projectPath"`
}

// Session is one sessions row with the session's FULL totals
// (server: IngestSessionAggregate). The server overwrites tokens, cost and
// message_count with these values, so a partial total here destroys data.
type Session struct {
	SessionID       string   `json:"sessionId"`
	UnitProjectPath string   `json:"unitProjectPath"`
	ProjectPath     string   `json:"projectPath"`
	Model           string   `json:"model"`
	Provider        string   `json:"provider"`
	Editor          *string  `json:"editor"`
	InputTokens     int64    `json:"inputTokens"`
	OutputTokens    int64    `json:"outputTokens"`
	CacheRead       int64    `json:"cacheRead"`
	CacheCreation   int64    `json:"cacheCreation"`
	TotalCost       float64  `json:"totalCost"`
	CostProvenance  string   `json:"costProvenance"`
	MessageCount    int64    `json:"messageCount"`
	StartedAt       *string  `json:"startedAt"`
	EndedAt         *string  `json:"endedAt"`
	ToolNames       []string `json:"toolNames"`
	Source          string   `json:"source"`
}

// Message is one messages row (server: MessageRow). The server inserts
// these with ON CONFLICT (message_id) DO NOTHING, so re-sending is safe.
type Message struct {
	SessionID      string  `json:"sessionId"`
	MessageID      string  `json:"messageId"`
	Timestamp      string  `json:"timestamp"`
	Model          string  `json:"model"`
	Provider       string  `json:"provider"`
	InputTokens    int64   `json:"inputTokens"`
	OutputTokens   int64   `json:"outputTokens"`
	CacheRead      int64   `json:"cacheRead"`
	CacheCreation  int64   `json:"cacheCreation"`
	EstCost        float64 `json:"estCost"`
	CostProvenance string  `json:"costProvenance"`
	RecordType     *string `json:"recordType"`
}

// GitEvent is one raw commit (server: IngestGitEvent). The server resolves
// the unit and does the time-window session join itself.
type GitEvent struct {
	Repo            string  `json:"repo"`
	CommitSha       string  `json:"commitSha"`
	AuthoredAt      string  `json:"authoredAt"`
	Message         *string `json:"message"`
	UnitProjectPath *string `json:"unitProjectPath"`
	IsMerge         bool    `json:"isMerge"`
	// NoteSessionID comes only from a local refs/notes/quantifai note.
	// The server records it as a deterministic git_notes link, so it must
	// never carry a guess.
	NoteSessionID *string `json:"noteSessionId,omitempty"`
}

// Batch is the request body for POST /api/v1/ingest.
type Batch struct {
	UnitsOfWork []Unit     `json:"unitsOfWork,omitempty"`
	Sessions    []Session  `json:"sessions,omitempty"`
	Messages    []Message  `json:"messages,omitempty"`
	GitEvents   []GitEvent `json:"gitEvents,omitempty"`
}

// Size is the count the server checks against MaxBatchSize.
func (b Batch) Size() int {
	return len(b.Messages) + len(b.Sessions) + len(b.GitEvents)
}

// Empty reports whether the batch would write nothing.
func (b Batch) Empty() bool {
	return b.Size() == 0 && len(b.UnitsOfWork) == 0
}

// Result is the server's response body (server: IngestResult).
type Result struct {
	UnitsOfWork int `json:"unitsOfWork"`
	Sessions    int `json:"sessions"`
	Messages    struct {
		Accepted int `json:"accepted"`
		Errors   int `json:"errors"`
	} `json:"messages"`
	GitEvents struct {
		Accepted      int `json:"accepted"`
		Linked        int `json:"linked"`
		Deterministic int `json:"deterministic"`
	} `json:"gitEvents"`
}

// Check decides whether a 2xx response actually wrote the batch.
//
// The server answers 200 with all-zero counts for a body it does not
// recognize, so a status code alone proves nothing. The server always
// echoes the number of units and sessions it upserted and the number of
// git events it accepted, so those must equal what was sent. Message
// "accepted" is NOT checked: a replay of already-stored messages
// legitimately returns 0 there because of ON CONFLICT DO NOTHING. That is
// why every batch that carries messages also carries their sessions —
// the session count is what proves the server read the body.
func Check(sent Batch, res Result) error {
	if sent.Empty() {
		return nil
	}
	if sent.Size() > 0 && res.UnitsOfWork == 0 && res.Sessions == 0 &&
		res.Messages.Accepted == 0 && res.Messages.Errors == 0 && res.GitEvents.Accepted == 0 {
		return fmt.Errorf("ingest: server returned all-zero counts for a batch of %d items (wrong request shape or wrong server)", sent.Size())
	}
	if want := distinctUnits(sent.UnitsOfWork); res.UnitsOfWork != want {
		return fmt.Errorf("ingest: server upserted %d units, sent %d", res.UnitsOfWork, want)
	}
	if res.Sessions != len(sent.Sessions) {
		return fmt.Errorf("ingest: server upserted %d sessions, sent %d", res.Sessions, len(sent.Sessions))
	}
	if res.Messages.Errors != 0 {
		return fmt.Errorf("ingest: server failed to insert %d of %d messages", res.Messages.Errors, len(sent.Messages))
	}
	if res.GitEvents.Accepted != len(sent.GitEvents) {
		return fmt.Errorf("ingest: server accepted %d git events, sent %d", res.GitEvents.Accepted, len(sent.GitEvents))
	}
	return nil
}

// distinctUnits matches the server's count, which is the size of a map
// keyed by projectPath.
func distinctUnits(units []Unit) int {
	seen := make(map[string]struct{}, len(units))
	for _, u := range units {
		seen[u.ProjectPath] = struct{}{}
	}
	return len(seen)
}
