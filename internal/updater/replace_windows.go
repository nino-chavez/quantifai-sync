//go:build windows

package updater

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// replaceExecutable installs src as the executable at dst. Windows will not
// overwrite or delete a running .exe, but it will rename one. So the new
// binary is copied beside dst, dst is moved aside to dst.old, and the copy
// is moved into place; if that last step fails, dst.old is moved back. This
// is minio/selfupdate's CommitBinary sequence.
//
// The running process keeps dst.old open, so it stays until the next update
// removes it. While that process still runs, a second update fails cleanly:
// dst.old cannot be replaced, and dst is left as it was.
func replaceExecutable(src, dst string) error {
	// After a swap the running process may report its own path as dst.old.
	dst = strings.TrimSuffix(dst, ".old")
	newPath := dst + ".new"
	oldPath := dst + ".old"

	// Stage beside dst so both renames stay on one volume.
	if err := copyExecutable(src, newPath); err != nil {
		os.Remove(newPath)
		return fmt.Errorf("stage new binary: %w", err)
	}
	os.Remove(oldPath) // left by the previous update; fails only if still running

	if err := os.Rename(dst, oldPath); err != nil {
		os.Remove(newPath)
		return fmt.Errorf("move running binary aside: %w", err)
	}
	if err := os.Rename(newPath, dst); err != nil {
		if rerr := os.Rename(oldPath, dst); rerr != nil {
			return fmt.Errorf("install new binary: %w; restoring old binary also failed: %v", err, rerr)
		}
		os.Remove(newPath)
		return fmt.Errorf("install new binary: %w", err)
	}
	os.Remove(oldPath) // fails while the old process runs; the next update retries
	return nil
}

func copyExecutable(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
