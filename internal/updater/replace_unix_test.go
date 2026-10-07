//go:build !windows

package updater

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestReplaceExecutableGivesNewInode guards against replacing the binary in
// place. macOS ties a binary's code-signature cache to its vnode, so new
// contents written into an inode that has already run are SIGKILLed on exec.
func TestReplaceExecutableGivesNewInode(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "quantifai-sync")
	if err := os.WriteFile(dst, []byte("old binary"), 0755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "download")
	if err := os.WriteFile(src, []byte("new binary"), 0755); err != nil {
		t.Fatal(err)
	}
	before := inode(t, dst)
	if err := replaceExecutable(src, dst); err != nil {
		t.Fatal(err)
	}
	if inode(t, dst) == before {
		t.Error("replaceExecutable rewrote the old inode; it must rename a new file over it")
	}
}

func inode(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return uint64(fi.Sys().(*syscall.Stat_t).Ino)
}
