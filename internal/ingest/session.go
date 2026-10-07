package ingest

import (
	"os"
	"path/filepath"
	"strings"
)

// sessionPart is what one file contributes to one session. It keeps
// first-seen order for models and tools, and every message cost in line
// order, so that merging parts file by file in walk order reproduces the
// reference importer's single sequential pass exactly — including the
// floating-point order of the cost sum and the tie-break in dominantModel.
type sessionPart struct {
	editor     string
	modelOrder []string
	modelCount map[string]int64
	in, out    int64
	cacheRead  int64
	cacheCr    int64
	costs      []float64
	count      int64
	startedAt  string
	endedAt    string
	tools      []string
	toolSet    map[string]struct{}
}

func newSessionPart() *sessionPart {
	return &sessionPart{modelCount: map[string]int64{}, toolSet: map[string]struct{}{}}
}

// add ports accumulate() for one usage record.
func (p *sessionPart) add(u *Usage) {
	if p.editor == "" && u.Editor != "" {
		p.editor = u.Editor
	}
	if _, ok := p.modelCount[u.Model]; !ok {
		p.modelOrder = append(p.modelOrder, u.Model)
	}
	p.modelCount[u.Model]++
	p.in += u.InputTokens
	p.out += u.OutputTokens
	p.cacheRead += u.CacheRead
	p.cacheCr += u.CacheCreation
	p.costs = append(p.costs, u.CostUSD)
	p.count++
	if p.startedAt == "" || u.Timestamp < p.startedAt {
		p.startedAt = u.Timestamp
	}
	if p.endedAt == "" || u.Timestamp > p.endedAt {
		p.endedAt = u.Timestamp
	}
	for _, t := range u.ToolNames {
		if _, ok := p.toolSet[t]; !ok {
			p.toolSet[t] = struct{}{}
			p.tools = append(p.tools, t)
		}
	}
}

// merge folds parts (already in walk order) into one session row.
func merge(sessionID, projectPath string, parts []*sessionPart) Session {
	acc := newSessionPart()
	var cost float64
	for _, p := range parts {
		if acc.editor == "" {
			acc.editor = p.editor
		}
		for _, m := range p.modelOrder {
			if _, ok := acc.modelCount[m]; !ok {
				acc.modelOrder = append(acc.modelOrder, m)
			}
			acc.modelCount[m] += p.modelCount[m]
		}
		acc.in += p.in
		acc.out += p.out
		acc.cacheRead += p.cacheRead
		acc.cacheCr += p.cacheCr
		for _, c := range p.costs {
			cost += c
		}
		acc.count += p.count
		if acc.startedAt == "" || (p.startedAt != "" && p.startedAt < acc.startedAt) {
			acc.startedAt = p.startedAt
		}
		if acc.endedAt == "" || p.endedAt > acc.endedAt {
			acc.endedAt = p.endedAt
		}
		for _, t := range p.tools {
			if _, ok := acc.toolSet[t]; !ok {
				acc.toolSet[t] = struct{}{}
				acc.tools = append(acc.tools, t)
			}
		}
	}

	// dominantModel: most messages wins; a tie goes to the model seen first.
	model, best := "unknown", int64(-1)
	for _, m := range acc.modelOrder {
		if acc.modelCount[m] > best {
			model, best = m, acc.modelCount[m]
		}
	}

	s := Session{
		SessionID:       sessionID,
		UnitProjectPath: projectPath,
		ProjectPath:     projectPath,
		Model:           model,
		Provider:        "anthropic",
		InputTokens:     acc.in,
		OutputTokens:    acc.out,
		CacheRead:       acc.cacheRead,
		CacheCreation:   acc.cacheCr,
		TotalCost:       cost,
		CostProvenance:  "estimated", // list-price valuation of subscription usage, never api_metered
		MessageCount:    acc.count,
		ToolNames:       acc.tools,
		Source:          "interactive",
	}
	if s.ToolNames == nil {
		s.ToolNames = []string{}
	}
	if acc.editor != "" {
		s.Editor = strPtr(acc.editor)
	}
	if acc.startedAt != "" {
		s.StartedAt = strPtr(acc.startedAt)
	}
	if acc.endedAt != "" {
		s.EndedAt = strPtr(acc.endedAt)
	}
	return s
}

func strPtr(s string) *string { return &s }

// worktreeMarker matches the server's WORKTREE_MARKER. Only this suffix is
// collapsed; collapsing anything else would key rows differently from the
// reference importer.
const worktreeMarker = "/.claude/worktrees/"

// NormalizeProjectPath ports normalizeProjectPath. A real absolute cwd wins;
// otherwise the encoded directory name, minus its leading dash, is the key.
func NormalizeProjectPath(projectDirName, sampleCwd string) (projectPath, repoName string, normalized bool) {
	if strings.HasPrefix(sampleCwd, "/") {
		collapsed := sampleCwd
		if i := strings.Index(collapsed, worktreeMarker); i != -1 {
			collapsed = collapsed[:i]
		}
		repoName = collapsed
		var segs []string
		for _, s := range strings.Split(collapsed, "/") {
			if s != "" {
				segs = append(segs, s)
			}
		}
		if len(segs) > 0 {
			repoName = segs[len(segs)-1]
		}
		return collapsed, repoName, true
	}
	raw := strings.TrimPrefix(projectDirName, "-")
	return raw, raw, false
}

// unitFor builds the units_of_work row the reference importer writes for a
// project directory: an initiative when the real project holds blueprint.yml.
func unitFor(projectDirName, firstCwd string) Unit {
	path, name, normalized := NormalizeProjectPath(projectDirName, firstCwd)
	kind := "project"
	if normalized {
		if _, err := os.Stat(filepath.Join(path, "blueprint.yml")); err == nil {
			kind = "initiative"
		}
	}
	return Unit{Kind: kind, Name: name, Source: "path", ProjectPath: path}
}
