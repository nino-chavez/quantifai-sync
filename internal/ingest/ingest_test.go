package ingest

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

const fixtureRoot = "../../testdata/parity/projects"

// canonical combines batches the way testdata/parity/combine.py combines
// the reference importer's captured POST bodies: distinct units, one row
// per (projectPath, sessionId), every message, each list sorted.
func canonical(t *testing.T, batches []Batch) any {
	t.Helper()
	units := map[string]Unit{}
	sessions := map[[2]string]Session{}
	var messages []Message
	for _, b := range batches {
		for _, u := range b.UnitsOfWork {
			units[u.ProjectPath] = u
		}
		for _, s := range b.Sessions {
			k := [2]string{s.ProjectPath, s.SessionID}
			if prev, ok := sessions[k]; ok && !reflect.DeepEqual(prev, s) {
				t.Fatalf("conflicting rows for session %v", k)
			}
			sessions[k] = s
		}
		messages = append(messages, b.Messages...)
	}
	out := Batch{}
	for _, u := range units {
		out.UnitsOfWork = append(out.UnitsOfWork, u)
	}
	sort.Slice(out.UnitsOfWork, func(i, j int) bool { return out.UnitsOfWork[i].ProjectPath < out.UnitsOfWork[j].ProjectPath })
	for _, s := range sessions {
		out.Sessions = append(out.Sessions, s)
	}
	sort.Slice(out.Sessions, func(i, j int) bool {
		a, b := out.Sessions[i], out.Sessions[j]
		if a.ProjectPath != b.ProjectPath {
			return a.ProjectPath < b.ProjectPath
		}
		return a.SessionID < b.SessionID
	})
	out.Messages = messages
	sort.SliceStable(out.Messages, func(i, j int) bool {
		a, b := out.Messages[i], out.Messages[j]
		if a.MessageID != b.MessageID {
			return a.MessageID < b.MessageID
		}
		if a.SessionID != b.SessionID {
			return a.SessionID < b.SessionID
		}
		return a.Timestamp < b.Timestamp
	})
	return roundTrip(t, out)
}

func roundTrip(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestParityWithReferenceImporter compares the collector's output for the
// fixture tree with golden.json, which is what the real TypeScript importer
// POSTed for the same tree. Floats are compared exactly.
func TestParityWithReferenceImporter(t *testing.T) {
	cyc := NewCollector(fixtureRoot, false).Collect(func(string) int64 { return 0 })
	got := canonical(t, BuildBatches(cyc.Groups, MaxBatchSize))

	data, err := os.ReadFile("../../testdata/parity/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var want any
	if err := json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		g, _ := json.MarshalIndent(got, "", " ")
		t.Fatalf("collector output differs from the reference importer's\n--- got ---\n%s", g)
	}
}

func copyTree(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
}

func sessionByID(groups []Group, id string) *Group {
	for i := range groups {
		if groups[i].Session.SessionID == id {
			return &groups[i]
		}
	}
	return nil
}

// TestIncrementalCycleSendsFullSessionTotals is the replace-semantics
// guard: after new lines land in one subagent file, the session row must
// still carry the whole session, exactly as a fresh full read computes it,
// while only the new messages are sent.
func TestIncrementalCycleSendsFullSessionTotals(t *testing.T) {
	root := copyTree(t, fixtureRoot)
	committed := map[string]int64{}
	get := func(p string) int64 { return committed[p] }

	c := NewCollector(root, false)
	first := c.Collect(get)
	for p, off := range first.Offsets {
		committed[p] = off
	}

	idle := c.Collect(get)
	if len(idle.Groups) != 0 || len(idle.Offsets) != 0 {
		t.Fatalf("idle cycle should send nothing, got %d groups, %d offsets", len(idle.Groups), len(idle.Offsets))
	}

	sub := filepath.Join(root, "-Users-test-repo-alpha", "s1", "subagents", "agent-1.jsonl")
	appendLine(t, sub, `{"type":"assistant","sessionId":"s1","uuid":"a1-new","timestamp":"2026-10-01T11:00:00.000Z","message":{"model":"claude-opus-4-6","usage":{"input_tokens":10,"output_tokens":20}}}`+"\n")
	// A partial line still being written must wait for its newline.
	appendLine(t, sub, `{"type":"assistant","sessionId":"s1","uuid":"a1-partial"`)

	inc := c.Collect(get)
	if len(inc.Groups) != 1 {
		t.Fatalf("want 1 dirty session, got %d", len(inc.Groups))
	}
	g := inc.Groups[0]
	if g.Session.SessionID != "s1" || len(g.Messages) != 1 || g.Messages[0].MessageID != "a1-new" {
		t.Fatalf("unexpected group: session %s, %d messages", g.Session.SessionID, len(g.Messages))
	}

	fresh := NewCollector(root, false).Collect(func(string) int64 { return 0 })
	want := sessionByID(fresh.Groups, "s1")
	if !reflect.DeepEqual(g.Session, want.Session) {
		t.Fatalf("incremental totals differ from a full read:\n got %+v\nwant %+v", g.Session, want.Session)
	}
	if g.Session.MessageCount != 8 {
		t.Fatalf("s1 should count all 8 usage records across both files, got %d", g.Session.MessageCount)
	}

	fi, _ := os.Stat(sub)
	if off := inc.Offsets[sub]; off >= fi.Size() || off <= committed[sub] {
		t.Fatalf("offset %d should stop before the partial line (size %d, was %d)", off, fi.Size(), committed[sub])
	}
}

// TestUncommittedCycleIsResent covers the failure path: when the caller
// does not commit a cycle's offsets (a POST failed), the next cycle sends
// the same messages again instead of skipping them.
func TestUncommittedCycleIsResent(t *testing.T) {
	root := copyTree(t, fixtureRoot)
	committed := map[string]int64{}
	get := func(p string) int64 { return committed[p] }
	c := NewCollector(root, false)

	first := c.Collect(get)
	second := c.Collect(get) // nothing committed in between
	if first.Messages() == 0 || first.Messages() != second.Messages() {
		t.Fatalf("uncommitted messages must be re-sent: first %d, second %d", first.Messages(), second.Messages())
	}
	if !reflect.DeepEqual(canonical(t, BuildBatches(first.Groups, 0)), canonical(t, BuildBatches(second.Groups, 0))) {
		t.Fatal("re-sent cycle differs from the original")
	}
}

func TestRewrittenFileStartsOver(t *testing.T) {
	root := copyTree(t, fixtureRoot)
	committed := map[string]int64{}
	get := func(p string) int64 { return committed[p] }
	c := NewCollector(root, false)
	for p, off := range c.Collect(get).Offsets {
		committed[p] = off
	}

	path := filepath.Join(root, "-Users-test-no-cwd", "s3.jsonl")
	line := `{"type":"assistant","sessionId":"s3","uuid":"b3-x","timestamp":"2026-10-03T13:00:00.000Z","message":{"model":"claude-opus-4-6","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	cyc := c.Collect(get)
	g := sessionByID(cyc.Groups, "s3")
	if g == nil || len(g.Messages) != 1 || g.Session.MessageCount != 1 {
		t.Fatalf("a shrunken file must be re-read from zero: %+v", g)
	}
}

func TestLiteModeStripsPaths(t *testing.T) {
	cyc := NewCollector(fixtureRoot, true).Collect(func(string) int64 { return 0 })
	for _, g := range cyc.Groups {
		for _, p := range []string{g.Unit.ProjectPath, g.Session.ProjectPath, g.Session.UnitProjectPath} {
			if strings.Contains(p, "/") {
				t.Fatalf("lite mode leaked a path: %q", p)
			}
		}
	}
}

func TestEstimateCost(t *testing.T) {
	cases := []struct {
		model string
		want  float64
	}{
		{"claude-opus-4-6", 15 + 75 + 1.5 + 18.75},
		{"Claude-HAIKU-5", 0.8 + 4 + 0.08 + 1},
		{"claude-sonnet-4-6", 3 + 15 + 0.3 + 3.75},
		{"unknown", 3 + 15 + 0.3 + 3.75}, // sonnet fallback
	}
	for _, c := range cases {
		if got := EstimateCost(c.model, 1e6, 1e6, 1e6, 1e6); got != c.want {
			t.Errorf("%s: got %v want %v", c.model, got, c.want)
		}
	}
}

// Cases mirror apps/app/src/lib/attribution/project-path.test.ts in the
// quantifai repo; path spellings are real production project_path shapes.
func TestNormalizeProjectPath(t *testing.T) {
	cases := []struct {
		dir, cwd, path, name string
		normalized           bool
	}{
		{"-Users-a-repo", "/Users/a/repo", "/Users/a/repo", "repo", true},
		{"-Users-nino-Workspace-dev-wip-quantifai-next", "/Users/nino/Workspace/dev/wip/quantifai-next", "/Users/nino/Workspace/dev/wip/quantifai-next", "quantifai-next", true},
		// Fallback: the raw encoded name, undecoded.
		{"-Users-nino-Workspace-dev-wip-quantifai-next", "", "Users-nino-Workspace-dev-wip-quantifai-next", "Users-nino-Workspace-dev-wip-quantifai-next", false},
		{"-some-dir", "relative/path", "some-dir", "some-dir", false},
		{"-", "/", "/", "/", true},
		// <repo>/.claude/worktrees/<agent-id>[/subdir]
		{"x", "/Users/nino/Workspace/dev/wip/quantifai-next/.claude/worktrees/agent-a77504b022bdad251", "/Users/nino/Workspace/dev/wip/quantifai-next", "quantifai-next", true},
		{"x", "/Users/nino/Workspace/dev/wip/quantifai-next/.claude/worktrees/agent-xyz/apps/app", "/Users/nino/Workspace/dev/wip/quantifai-next", "quantifai-next", true},
		// <repo>/.worktrees/<branch>[/subdir], branch may contain slashes.
		{"x", "/Users/nino/Workspace/dev/apps/minder/.worktrees/s7-marquee", "/Users/nino/Workspace/dev/apps/minder", "minder", true},
		{"x", "/Users/nino/Workspace/dev/apps/letspepper/.worktrees/feat/gallery-announce", "/Users/nino/Workspace/dev/apps/letspepper", "letspepper", true},
		{"x", "/Users/nino/Workspace/dev/apps/minder/.worktrees/port-jump-points/ios/Minder", "/Users/nino/Workspace/dev/apps/minder", "minder", true},
		// ~/.codex/worktrees/<id>/<repo>[/subdir]: keep through <repo>.
		{"x", "/Users/nino/.codex/worktrees/672f/630-marketing-automation/site", "/Users/nino/.codex/worktrees/672f/630-marketing-automation", "630-marketing-automation", true},
		{"x", "/Users/nino/.codex/worktrees/672f/630-marketing-automation", "/Users/nino/.codex/worktrees/672f/630-marketing-automation", "630-marketing-automation", true},
		// Nested: cut at the EARLIEST marker.
		{"x", "/dev/apps/quantifai/quantifai/.worktrees/fix/git-event-linking/.claude/worktrees/agent-abc", "/dev/apps/quantifai/quantifai", "quantifai", true},
		// TS repoKey applies repoRoot to the already-collapsed path, so the
		// name and the path can disagree here. Ported as-is for parity.
		{"x", "/Users/nino/.codex/worktrees/672f/blog/sub/.worktrees/b", "/Users/nino/.codex/worktrees/672f/blog/sub", "blog", true},
	}
	for _, c := range cases {
		path, name, normalized := NormalizeProjectPath(c.dir, c.cwd)
		if path != c.path || name != c.name || normalized != c.normalized {
			t.Errorf("(%q,%q): got (%q,%q,%v) want (%q,%q,%v)", c.dir, c.cwd, path, name, normalized, c.path, c.name, c.normalized)
		}
	}
}

func TestRepoKeyJoinsEverySpelling(t *testing.T) {
	spellings := []string{
		"/Users/nino/Workspace/dev/wip/atelier",                            // pre-reorg location
		"/Users/nino/Workspace/dev/labs/atelier",                           // current location
		"/Users/nino.chavez/Workspace/dev/wip/atelier",                     // the other Mac
		"/Users/nino/Workspace/dev/labs/atelier/.worktrees/feat/x",         // workspace worktree
		"/Users/nino/Workspace/dev/labs/atelier/.claude/worktrees/agent-1", // agent worktree
		"/Users/nino/.codex/worktrees/002b/atelier",                        // Codex worktree
	}
	for _, p := range spellings {
		if got := repoKey(p); got != "atelier" {
			t.Errorf("repoKey(%q) = %q, want atelier", p, got)
		}
	}
	if repoKey("/Users/nino/Workspace/dev/apps/photography-vnext-p1") == repoKey("/Users/nino/Workspace/dev/apps/photography") {
		t.Error("photography-vnext-p1 must not share photography's key")
	}
}

func TestBlueprintMakesInitiative(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "blueprint.yml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if u := unitFor("x", dir); u.Kind != "initiative" {
		t.Fatalf("got kind %q", u.Kind)
	}
	if u := unitFor("x", filepath.Join(dir, "nope")); u.Kind != "project" {
		t.Fatalf("got kind %q", u.Kind)
	}
}

func TestCheck(t *testing.T) {
	sent := Batch{
		UnitsOfWork: []Unit{{ProjectPath: "/a"}, {ProjectPath: "/a"}},
		Sessions:    []Session{{SessionID: "s"}},
		Messages:    []Message{{MessageID: "m1"}, {MessageID: "m2"}},
	}
	ok := Result{UnitsOfWork: 1, Sessions: 1}
	// A replay of stored messages inserts nothing and is still a success.
	if err := Check(sent, ok); err != nil {
		t.Fatalf("replay rejected: %v", err)
	}

	var zero Result // what the server returns for a body it does not read
	if err := Check(sent, zero); err == nil {
		t.Fatal("all-zero counts accepted")
	}
	if err := Check(Batch{Messages: sent.Messages}, zero); err == nil {
		t.Fatal("all-zero counts accepted for a messages-only batch")
	}

	short := ok
	short.Sessions = 0
	if err := Check(sent, short); err == nil {
		t.Fatal("missing session count accepted")
	}
	failed := ok
	failed.Messages.Errors = 2
	if err := Check(sent, failed); err == nil {
		t.Fatal("message insert errors accepted")
	}
	git := Batch{GitEvents: []GitEvent{{CommitSha: "a"}, {CommitSha: "b"}}}
	gitRes := Result{}
	gitRes.GitEvents.Accepted = 1
	if err := Check(git, gitRes); err == nil {
		t.Fatal("partial git acceptance accepted")
	}
	if err := Check(Batch{}, zero); err != nil {
		t.Fatalf("empty batch rejected: %v", err)
	}
}

func TestBuildBatchesRespectsLimitAndCarriesSessions(t *testing.T) {
	msgs := make([]Message, 25)
	for i := range msgs {
		msgs[i] = Message{MessageID: string(rune('a' + i))}
	}
	groups := []Group{
		{Unit: Unit{ProjectPath: "/p"}, Session: Session{SessionID: "big"}, Messages: msgs},
		{Unit: Unit{ProjectPath: "/p"}, Session: Session{SessionID: "small"}, Messages: msgs[:2]},
	}
	batches := BuildBatches(groups, 10)
	total := 0
	for _, b := range batches {
		if b.Size() > 10 {
			t.Fatalf("batch size %d over limit", b.Size())
		}
		if len(b.Messages) > 0 && len(b.Sessions) == 0 {
			t.Fatal("batch has messages but no session to prove the write")
		}
		if len(b.UnitsOfWork) != 1 {
			t.Fatalf("want the unit in every batch, got %d", len(b.UnitsOfWork))
		}
		total += len(b.Messages)
	}
	if total != 27 {
		t.Fatalf("messages lost or duplicated: %d", total)
	}
}
