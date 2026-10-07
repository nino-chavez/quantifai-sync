//go:build !windows

package updater

// replaceExecutable installs src as the executable at dst. On unix a
// rename replaces even a running binary, so it is atomicReplace.
func replaceExecutable(src, dst string) error {
	return atomicReplace(src, dst)
}
