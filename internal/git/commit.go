// Package git provides commit event capture, AI trailer parsing,
// process detection, and git hook management for the quantifai-sync
// agent.  The hook-post-commit path is performance-critical: all
// operations must complete within 100ms with no network I/O.
package git

import (
	"crypto/sha256"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// CommitEvent holds metadata captured from a git post-commit hook.
// It is serialized as JSONL to the local queue file for later batch
// upload by the agent pipeline.
type CommitEvent struct {
	EventType         string   `json:"event_type"`
	CommitSHA         string   `json:"commit_sha"`
	Timestamp         string   `json:"timestamp"`
	RepoRemoteURL     string   `json:"repo_remote_url"`
	Branch            string   `json:"branch"`
	AuthorEmail       string   `json:"author_email"`
	FilesChanged      []string `json:"files_changed"`
	LinesAdded        int      `json:"lines_added"`
	LinesRemoved      int      `json:"lines_removed"`
	AITrailers        []string `json:"ai_trailers"`
	AIToolDetected    string   `json:"ai_tool_detected,omitempty"`
	AIConfidence      string   `json:"ai_confidence"`
	LinkedSessionID   string   `json:"linked_session_id,omitempty"`
	MergeCommit       bool     `json:"merge_commit"`
	CommitMessageHash string   `json:"commit_message_hash"`
	// RepoPath is the repository's top-level directory and Subject the
	// commit's first line. The server's gitEvents need both; events queued
	// by older builds lack them.
	RepoPath string `json:"repo_path,omitempty"`
	Subject  string `json:"subject,omitempty"`
}

// gitCmdTimeout is the maximum duration for any single git subprocess.
const gitCmdTimeout = 3 * time.Second

// CaptureCommit gathers metadata about the most recent commit in the
// current working directory.  It shells out to git with short timeouts
// to stay within the 100ms hook budget.
func CaptureCommit() (*CommitEvent, error) {
	ev := &CommitEvent{EventType: "commit"}

	// commit SHA
	sha, err := gitOutput("rev-parse", "HEAD")
	if err != nil {
		return nil, fmt.Errorf("rev-parse HEAD: %w", err)
	}
	ev.CommitSHA = sha

	// author timestamp (ISO 8601)
	ts, err := gitOutput("log", "-1", "--format=%aI")
	if err != nil {
		return nil, fmt.Errorf("log timestamp: %w", err)
	}
	ev.Timestamp = ts

	// author email
	email, err := gitOutput("log", "-1", "--format=%ae")
	if err != nil {
		return nil, fmt.Errorf("log email: %w", err)
	}
	ev.AuthorEmail = email

	// commit body + trailers for AI detection
	body, err := gitOutput("log", "-1", "--format=%B")
	if err != nil {
		return nil, fmt.Errorf("log body: %w", err)
	}
	ev.AITrailers, ev.AIToolDetected = ParseAITrailers(body)

	// commit message hash (SHA-256 of full message)
	h := sha256.Sum256([]byte(body))
	ev.CommitMessageHash = fmt.Sprintf("%x", h)

	// repository top level and subject line, for the server's gitEvents
	ev.RepoPath, _ = gitOutput("rev-parse", "--show-toplevel")
	ev.Subject, _ = gitOutput("log", "-1", "--format=%s")

	// remote URL (best effort -- may be empty for local-only repos)
	remote, _ := gitOutput("remote", "get-url", "origin")
	ev.RepoRemoteURL = normalizeRemoteURL(remote)

	// branch name
	branch, err := gitOutput("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		branch = "unknown"
	}
	ev.Branch = branch

	// files changed
	filesRaw, _ := gitOutput("diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
	if filesRaw != "" {
		ev.FilesChanged = strings.Split(filesRaw, "\n")
	} else {
		ev.FilesChanged = []string{}
	}

	// lines added/removed via numstat
	numstat, _ := gitOutput("diff-tree", "--no-commit-id", "--numstat", "-r", "HEAD")
	ev.LinesAdded, ev.LinesRemoved = parseNumstat(numstat)

	// merge commit detection (>1 parent)
	parents, _ := gitOutput("log", "-1", "--format=%P")
	ev.MergeCommit = len(strings.Fields(parents)) > 1

	return ev, nil
}

// gitOutput runs a git command and returns its trimmed stdout.
func gitOutput(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// parseNumstat sums the additions and deletions from git diff-tree --numstat output.
// Each line is: <added>\t<removed>\t<filename>
// Binary files show "-" for both counts and are skipped.
func parseNumstat(raw string) (added, removed int) {
	if raw == "" {
		return 0, 0
	}
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		a, errA := strconv.Atoi(fields[0])
		r, errR := strconv.Atoi(fields[1])
		if errA != nil || errR != nil {
			continue // binary file, skip
		}
		added += a
		removed += r
	}
	return added, removed
}

// normalizeRemoteURL converts SSH remote URLs to HTTPS format and
// strips trailing .git suffix for consistent matching.
func normalizeRemoteURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}

	// Convert git@host:owner/repo.git -> https://host/owner/repo
	if strings.HasPrefix(raw, "git@") {
		raw = strings.TrimPrefix(raw, "git@")
		raw = strings.Replace(raw, ":", "/", 1)
		raw = "https://" + raw
	}

	raw = strings.TrimSuffix(raw, ".git")
	return raw
}
