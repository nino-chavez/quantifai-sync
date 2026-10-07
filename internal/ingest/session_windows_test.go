package ingest

import "testing"

// Windows cwds name the repo by its last segment, like unix ones, with the
// path stored in forward slashes.
func TestNormalizeProjectPathWindows(t *testing.T) {
	cases := []struct{ cwd, path, name string }{
		{`C:\Users\a\repo`, "C:/Users/a/repo", "repo"},
		{`C:\Users\a\repo\.worktrees\feat\x`, "C:/Users/a/repo", "repo"},
		{`C:\Users\a\repo\.claude\worktrees\agent-1\sub`, "C:/Users/a/repo", "repo"},
		{`\\server\share\repo`, "//server/share/repo", "repo"},
	}
	for _, c := range cases {
		path, name, normalized := NormalizeProjectPath("x", c.cwd)
		if path != c.path || name != c.name || !normalized {
			t.Errorf("%q: got (%q,%q,%v) want (%q,%q,true)", c.cwd, path, name, normalized, c.path, c.name)
		}
	}
}
