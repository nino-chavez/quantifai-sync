package ingest

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Collector turns ~/.claude/projects into ingest batches, incrementally.
//
// Messages are sent from each file's committed offset onward. Sessions are
// different: the server replaces a session's totals on every upsert, so a
// session row must always carry the whole session, which can span the
// parent <uuid>.jsonl plus <uuid>/subagents/**. The collector therefore
// keeps every file's contribution in memory (rebuilt by one full read when
// the process starts) and recomputes a session's totals from all of its
// files whenever any of them grows.
type Collector struct {
	root  string
	lite  bool
	files map[string]*fileCache
}

type fileCache struct {
	parsed   int64 // bytes already folded into sessions
	firstCwd string
	sessions map[string]*sessionPart
	order    []string // session ids in first-seen order
}

func newFileCache() *fileCache {
	return &fileCache{sessions: map[string]*sessionPart{}}
}

// NewCollector reads session files under root. In lite mode, absolute
// project paths are cut to their last segment before they leave the machine.
func NewCollector(root string, lite bool) *Collector {
	return &Collector{root: root, lite: lite, files: map[string]*fileCache{}}
}

// Group is one session's full totals plus its messages read this cycle.
type Group struct {
	Unit     Unit
	Session  Session
	Messages []Message
}

// Cycle is the result of one collection pass.
type Cycle struct {
	Groups []Group
	// Offsets holds the new committed offset for every file read this
	// cycle. Persist them only after every batch built from Groups has
	// been acknowledged.
	Offsets map[string]int64
	// Files is the number of session files seen.
	Files int
	// ReadErrors lists files that could not be read; they are retried
	// next cycle from their unchanged offsets.
	ReadErrors map[string]error
}

// Messages counts the messages across all groups.
func (c *Cycle) Messages() int {
	n := 0
	for _, g := range c.Groups {
		n += len(g.Messages)
	}
	return n
}

type projectFiles struct {
	dirName string
	paths   []string
}

// Collect walks the tree in the reference importer's order and returns the
// sessions that gained messages since the committed offsets.
// committed(path) returns a file's last acknowledged byte offset.
func (c *Collector) Collect(committed func(path string) int64) *Cycle {
	cyc := &Cycle{Offsets: map[string]int64{}, ReadErrors: map[string]error{}}
	projects := walkProjects(c.root)

	type dirtyKey struct{ project, session string }
	newMsgs := map[dirtyKey][]Message{}

	for _, p := range projects {
		for _, path := range p.paths {
			cyc.Files++
			fc := c.files[path]
			if fc == nil {
				fc = newFileCache()
				c.files[path] = fc
			}
			acked := committed(path)

			fi, err := os.Stat(path)
			if err != nil {
				cyc.ReadErrors[path] = err
				continue
			}
			size := fi.Size()
			if size < acked || size < fc.parsed {
				// The file shrank, so it was rewritten: start it over.
				fc = newFileCache()
				c.files[path] = fc
				acked = 0
				cyc.Offsets[path] = 0
			}
			start := acked
			if fc.parsed < start {
				start = fc.parsed
			}
			if start >= size {
				continue
			}

			end, err := readLines(path, start, func(lineStart, lineEnd int64, line []byte) {
				u := ExtractUsage(line)
				if u == nil {
					return
				}
				if lineEnd > fc.parsed {
					fc.fold(u)
				}
				if lineStart >= acked {
					k := dirtyKey{p.dirName, u.SessionID}
					newMsgs[k] = append(newMsgs[k], u.MessageRow())
				}
			})
			if end > fc.parsed {
				fc.parsed = end
			}
			if end > acked {
				cyc.Offsets[path] = end
			}
			if err != nil {
				cyc.ReadErrors[path] = err
			}
		}
	}

	// Emit dirty sessions per project directory in walk order, each session
	// in first-seen order, with totals merged across all of its files.
	for _, p := range projects {
		var firstCwd string
		var order []string
		seen := map[string]bool{}
		for _, path := range p.paths {
			fc := c.files[path]
			if fc == nil {
				continue
			}
			if firstCwd == "" {
				firstCwd = fc.firstCwd
			}
			for _, id := range fc.order {
				if !seen[id] {
					seen[id] = true
					order = append(order, id)
				}
			}
		}

		var unit *Unit
		for _, id := range order {
			msgs, dirty := newMsgs[dirtyKey{p.dirName, id}]
			if !dirty {
				continue
			}
			if unit == nil {
				u := unitFor(p.dirName, firstCwd)
				if c.lite {
					u.ProjectPath = u.Name
				}
				unit = &u
			}
			var parts []*sessionPart
			for _, path := range p.paths {
				if fc := c.files[path]; fc != nil && fc.sessions[id] != nil {
					parts = append(parts, fc.sessions[id])
				}
			}
			cyc.Groups = append(cyc.Groups, Group{
				Unit:     *unit,
				Session:  merge(id, unit.ProjectPath, parts),
				Messages: msgs,
			})
		}
	}

	// Forget files that disappeared, so their sessions stop contributing.
	live := make(map[string]bool, cyc.Files)
	for _, p := range projects {
		for _, path := range p.paths {
			live[path] = true
		}
	}
	for path := range c.files {
		if !live[path] {
			delete(c.files, path)
		}
	}
	return cyc
}

func (fc *fileCache) fold(u *Usage) {
	if fc.firstCwd == "" && u.Cwd != "" {
		fc.firstCwd = u.Cwd
	}
	part := fc.sessions[u.SessionID]
	if part == nil {
		part = newSessionPart()
		fc.sessions[u.SessionID] = part
		fc.order = append(fc.order, u.SessionID)
	}
	part.add(u)
}

// walkProjects lists every top-level project directory and the .jsonl files
// beneath it, depth first. os.ReadDir sorts by byte order, which is the
// order Node's readdirSync returns (libuv sorts scandir output with
// strcmp), so files are visited in the same order as the reference importer.
// Symlinks are followed, as statSync does.
func walkProjects(root string) []projectFiles {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []projectFiles
	for _, e := range entries {
		full := filepath.Join(root, e.Name())
		fi, err := os.Stat(full)
		if err != nil || !fi.IsDir() {
			continue
		}
		paths := walkJsonl(full)
		if len(paths) > 0 {
			out = append(out, projectFiles{dirName: e.Name(), paths: paths})
		}
	}
	return out
}

func walkJsonl(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		fi, err := os.Stat(full)
		if err != nil {
			continue
		}
		if fi.IsDir() {
			out = append(out, walkJsonl(full)...)
		} else if strings.HasSuffix(e.Name(), ".jsonl") {
			out = append(out, full)
		}
	}
	return out
}

// readLines calls fn for every complete, non-blank line from offset start,
// and returns the offset just past the last complete line. A trailing line
// with no newline may still be mid-write, so it is left for the next cycle.
func readLines(path string, start int64, fn func(lineStart, lineEnd int64, line []byte)) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return start, err
	}
	defer f.Close()
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return start, err
	}
	r := bufio.NewReaderSize(f, 1<<20)
	pos := start
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return pos, nil
			}
			return pos, err
		}
		lineStart := pos
		pos += int64(len(line))
		body := line[:len(line)-1]
		if len(bytes.TrimSpace(body)) > 0 {
			fn(lineStart, pos, body)
		}
	}
}

// BuildBatches packs groups into POST bodies of at most max items
// (sessions + messages). Every batch carries the units and sessions of the
// messages in it: the session count the server echoes back is how Check
// tells a real write from a 200 that ignored the body. A session with more
// messages than fit in one batch is repeated in each batch its messages
// span, which is harmless because the server's session upsert replaces.
func BuildBatches(groups []Group, max int) []Batch {
	if max <= 1 || max > MaxBatchSize {
		max = MaxBatchSize
	}
	var out []Batch
	var cur Batch
	units := map[string]bool{}
	flush := func() {
		if !cur.Empty() {
			out = append(out, cur)
		}
		cur = Batch{}
		units = map[string]bool{}
	}
	add := func(g Group, msgs []Message) {
		if !units[g.Unit.ProjectPath] {
			units[g.Unit.ProjectPath] = true
			cur.UnitsOfWork = append(cur.UnitsOfWork, g.Unit)
		}
		cur.Sessions = append(cur.Sessions, g.Session)
		cur.Messages = append(cur.Messages, msgs...)
	}
	for _, g := range groups {
		msgs := g.Messages
		if cur.Size()+1+len(msgs) > max {
			flush()
		}
		for 1+len(msgs) > max {
			add(g, msgs[:max-1])
			flush()
			msgs = msgs[max-1:]
		}
		add(g, msgs)
	}
	flush()
	return out
}
