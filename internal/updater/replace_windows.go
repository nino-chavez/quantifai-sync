//go:build windows

package updater

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// oldSuffix matches the names replaceExecutable moves a binary aside to.
var oldSuffix = regexp.MustCompile(`\.old(-\d+)?$`)

// replaceExecutable installs src as the executable at dst. Windows will not
// overwrite or delete a running .exe, but it will rename one. So the new
// binary is copied beside dst, dst is moved aside to dst.old, and the copy
// is moved into place; if that last step fails, dst.old is moved back. This
// is minio/selfupdate's CommitBinary sequence.
//
// A process still running an earlier binary (the supervisor, which keeps
// running until the next logon) holds that file open, so it cannot be
// removed or replaced. Each update therefore moves dst aside under a name
// no running process holds, and removes every leftover it can.
func replaceExecutable(src, dst string) error {
	// After a swap the running process may report its own path as dst.old.
	dst = oldSuffix.ReplaceAllString(dst, "")
	newPath := dst + ".new"

	// Stage beside dst so both renames stay on one volume.
	if err := copyExecutable(src, newPath); err != nil {
		os.Remove(newPath)
		return fmt.Errorf("stage new binary: %w", err)
	}
	// Left by earlier updates; those still running stay.
	if leftovers, _ := filepath.Glob(dst + ".old*"); leftovers != nil {
		for _, f := range leftovers {
			os.Remove(f)
		}
	}
	oldPath := dst + ".old"
	if _, err := os.Lstat(oldPath); err == nil {
		oldPath = fmt.Sprintf("%s.old-%d", dst, time.Now().UnixNano())
	}

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
