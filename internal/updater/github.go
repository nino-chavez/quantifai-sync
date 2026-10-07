package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/quantifai/sync/internal/logger"
)

// updatedToEnv names the release this process just installed. The restart
// after an update keeps the environment, so the new process sees it.
const updatedToEnv = "QUANTIFAI_UPDATED_TO"

// GithubUpdater checks GitHub Releases for newer versions and performs
// atomic binary replacement when an update is found.
type GithubUpdater struct {
	log           *logger.Logger
	version       string
	updateChannel string
	repo          string // "owner/repo"
	interval      time.Duration
	client        *http.Client

	// executable locates the binary to replace; nil means os.Executable.
	executable func() (string, error)

	// applied is signalled once Run installs an update.
	applied chan struct{}
}

// githubRelease is the subset of the GitHub Releases API response we need.
type githubRelease struct {
	TagName string        `json:"tag_name"`
	Assets  []githubAsset `json:"assets"`
}

// githubAsset represents a single downloadable file attached to a release.
type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// NewGithubUpdater creates a GithubUpdater that checks the given repo
// for newer releases at the specified interval.
func NewGithubUpdater(version, updateChannel, repo string, interval time.Duration, log *logger.Logger) *GithubUpdater {
	return &GithubUpdater{
		log:           log,
		version:       version,
		updateChannel: updateChannel,
		repo:          repo,
		interval:      interval,
		client:        &http.Client{Timeout: 30 * time.Second},
		applied:       make(chan struct{}, 1),
	}
}

// Applied receives once when Run has installed an update.
func (g *GithubUpdater) Applied() <-chan struct{} {
	return g.applied
}

// CheckAndApply checks GitHub for a newer release. If found, it downloads
// the release archive, verifies its SHA256 checksum, extracts the binary,
// and atomically replaces the current executable.
func (g *GithubUpdater) CheckAndApply(ctx context.Context) (bool, error) {
	g.log.Info("checking for updates", map[string]any{
		"current_version": g.version,
		"channel":         g.updateChannel,
		"repo":            g.repo,
	})

	release, err := g.fetchLatestRelease(ctx)
	if err != nil {
		return false, fmt.Errorf("fetch latest release: %w", err)
	}

	latestVersion := normalizeVersion(release.TagName)
	currentVersion := normalizeVersion(g.version)

	if !isNewer(latestVersion, currentVersion) {
		g.log.Debug("already up to date", map[string]any{
			"current": currentVersion,
			"latest":  latestVersion,
		})
		return false, nil
	}

	// A release whose binary reports a version older than its tag (an
	// unstamped build, or one built before tagging) would otherwise be
	// installed again after every restart.
	if os.Getenv(updatedToEnv) == release.TagName {
		return false, fmt.Errorf("release %s was just installed but this binary reports %s; not installing it again (check the release binary's version stamp)", release.TagName, g.version)
	}

	g.log.Info("update available", map[string]any{
		"current": currentVersion,
		"latest":  latestVersion,
	})

	assetName := expectedAssetName(runtime.GOOS, runtime.GOARCH)
	checksumName := assetName + ".sha256"

	assetURL, checksumURL, consolidatedChecksumURL := "", "", ""
	for _, a := range release.Assets {
		if a.Name == assetName {
			assetURL = a.BrowserDownloadURL
		}
		if a.Name == checksumName {
			checksumURL = a.BrowserDownloadURL
		}
		// Fallback: consolidated checksum file (checksums.txt or SHA256SUMS)
		if a.Name == "checksums.txt" || a.Name == "SHA256SUMS" {
			consolidatedChecksumURL = a.BrowserDownloadURL
		}
	}

	if assetURL == "" {
		return false, fmt.Errorf("no asset found for %s/%s in release %s", runtime.GOOS, runtime.GOARCH, release.TagName)
	}

	// Download the archive to a temp file
	tmpFile, err := tempPath("quantifai-sync-update-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmpFile)

	if err := g.downloadFile(ctx, assetURL, tmpFile); err != nil {
		return false, fmt.Errorf("download archive: %w", err)
	}

	// Verify checksum — reject update if no checksum source is available
	if checksumURL != "" {
		// Preferred: per-file .sha256 checksum
		if err := g.verifyChecksum(ctx, checksumURL, tmpFile); err != nil {
			g.log.Error("checksum verification failed", map[string]any{
				"checksum_file": checksumName,
				"error":         err.Error(),
			})
			return false, fmt.Errorf("checksum verification failed for %s: %w", checksumName, err)
		}
	} else if consolidatedChecksumURL != "" {
		// Fallback: consolidated checksums.txt / SHA256SUMS
		g.log.Info("using consolidated checksum file", nil)
		if err := g.verifyConsolidatedChecksum(ctx, consolidatedChecksumURL, assetName, tmpFile); err != nil {
			g.log.Error("consolidated checksum verification failed", map[string]any{
				"asset": assetName,
				"error": err.Error(),
			})
			return false, fmt.Errorf("checksum verification failed for %s: %w", assetName, err)
		}
	} else {
		g.log.Warn("no checksum file found in release — refusing unverified update", map[string]any{
			"expected":  checksumName,
			"release":   release.TagName,
			"asset":     assetName,
			"num_assets": len(release.Assets),
		})
		return false, fmt.Errorf("no checksum file found for %s in release %s — refusing to apply unverified update", assetName, release.TagName)
	}
	g.log.Info("checksum verified", map[string]any{"asset": assetName})

	// The checksum covers the archive; the binary inside is what gets installed.
	binFile, err := tempPath("quantifai-sync-bin-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(binFile) // no-op after a successful replace
	if err := extractBinary(tmpFile, assetName, binaryName(runtime.GOOS, runtime.GOARCH), binFile); err != nil {
		return false, fmt.Errorf("extract %s: %w", assetName, err)
	}

	// Make the extracted binary executable
	if err := os.Chmod(binFile, 0755); err != nil {
		return false, fmt.Errorf("chmod: %w", err)
	}

	// Atomic replace: rename the extracted binary over the current one
	executable := g.executable
	if executable == nil {
		executable = os.Executable
	}
	currentBinary, err := executable()
	if err != nil {
		return false, fmt.Errorf("resolve current executable: %w", err)
	}
	currentBinary, err = filepath.EvalSymlinks(currentBinary)
	if err != nil {
		return false, fmt.Errorf("resolve symlinks: %w", err)
	}

	if err := replaceExecutable(binFile, currentBinary); err != nil {
		return false, fmt.Errorf("replace binary: %w", err)
	}
	// The running process is still the old binary until it restarts; record
	// the installed version so later checks do not re-apply the same release.
	g.version = release.TagName
	os.Setenv(updatedToEnv, release.TagName)

	g.log.Info("update applied successfully", map[string]any{
		"from": currentVersion,
		"to":   latestVersion,
	})

	return true, nil
}

// Run starts the background update loop. It checks on startup, then
// every interval. Blocks until ctx is cancelled, or returns once an update
// is installed, after signalling Applied.
func (g *GithubUpdater) Run(ctx context.Context) {
	if g.check(ctx, "startup") {
		return
	}

	ticker := time.NewTicker(g.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if g.check(ctx, "periodic") {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// check runs one update check. When it installs an update it signals
// Applied and returns true.
func (g *GithubUpdater) check(ctx context.Context, when string) bool {
	applied, err := g.CheckAndApply(ctx)
	if err != nil {
		g.log.Warn(when+" update check failed", map[string]any{
			"error": err.Error(),
		})
		return false
	}
	if !applied {
		return false
	}
	select {
	case g.applied <- struct{}{}:
	default:
	}
	return true
}

// fetchLatestRelease queries the GitHub API for the latest release.
func (g *GithubUpdater) fetchLatestRelease(ctx context.Context) (*githubRelease, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", g.repo)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github.v3+json")
	req.Header.Set("User-Agent", "quantifai-sync/"+g.version)

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(body))
	}

	var release githubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &release, nil
}

// downloadFile downloads a URL to a local file path.
func (g *GithubUpdater) downloadFile(ctx context.Context, url, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "quantifai-sync/"+g.version)

	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}

	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		return err
	}
	return f.Close()
}

// verifyChecksum downloads the .sha256 file and verifies the binary matches.
func (g *GithubUpdater) verifyChecksum(ctx context.Context, checksumURL, filePath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checksumURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "quantifai-sync/"+g.version)

	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("checksum download returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	// Parse expected hash (format: "hash  filename" or just "hash")
	expectedHash := strings.Fields(strings.TrimSpace(string(body)))[0]

	// Compute actual hash
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	actualHash := hex.EncodeToString(h.Sum(nil))

	if !strings.EqualFold(actualHash, expectedHash) {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedHash, actualHash)
	}
	return nil
}

// verifyConsolidatedChecksum downloads a multi-entry checksum file (checksums.txt
// or SHA256SUMS) and verifies the binary matches the entry for the given asset name.
// Each line is expected to be in the format: "hash  filename" or "hash filename".
func (g *GithubUpdater) verifyConsolidatedChecksum(ctx context.Context, checksumURL, assetName, filePath string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checksumURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "quantifai-sync/"+g.version)

	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("checksum download returned HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	// Find the line matching our asset name
	var expectedHash string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == assetName {
			expectedHash = fields[0]
			break
		}
	}

	if expectedHash == "" {
		return fmt.Errorf("no checksum entry found for %s in consolidated checksum file", assetName)
	}

	// Compute actual hash
	f, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	actualHash := hex.EncodeToString(h.Sum(nil))

	if !strings.EqualFold(actualHash, expectedHash) {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedHash, actualHash)
	}
	return nil
}

// atomicReplace moves src over dst using rename. On Unix this is atomic
// within the same filesystem. We first remove the old file because some
// OS require the destination to be absent for cross-device moves.
func atomicReplace(src, dst string) error {
	// Try direct rename first (same filesystem)
	if err := os.Rename(src, dst); err == nil {
		return nil
	}

	// Cross-filesystem fallback: copy + remove
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	info, err := srcFile.Stat()
	if err != nil {
		return err
	}

	// Write to dst.new, then rename over dst
	tmpDst := dst + ".new"
	dstFile, err := os.OpenFile(tmpDst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		dstFile.Close()
		os.Remove(tmpDst)
		return err
	}
	dstFile.Close()

	return os.Rename(tmpDst, dst)
}

// expectedAssetName returns the release archive for an OS and architecture:
// quantifai-sync-<os>-<arch>.tar.gz, or .zip on Windows. Every release
// since v0.1.0 publishes this layout, with a .sha256 file per archive.
func expectedAssetName(goos, goarch string) string {
	if goos == "windows" {
		return fmt.Sprintf("quantifai-sync-%s-%s.zip", goos, goarch)
	}
	return fmt.Sprintf("quantifai-sync-%s-%s.tar.gz", goos, goarch)
}

// binaryName is the executable inside a release archive, named as
// cross-build in the Makefile names it.
func binaryName(goos, goarch string) string {
	name := fmt.Sprintf("quantifai-sync-%s-%s", goos, goarch)
	if goos == "windows" {
		name += ".exe"
	}
	return name
}

// tempPath creates an empty temp file and returns its path.
func tempPath(pattern string) (string, error) {
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	f.Close()
	return f.Name(), nil
}

// extractBinary writes the regular file called name from the archive at
// archivePath to dest. The format comes from the asset name.
func extractBinary(archivePath, assetName, name, dest string) error {
	switch {
	case strings.HasSuffix(assetName, ".tar.gz"):
		return extractFromTarGz(archivePath, name, dest)
	case strings.HasSuffix(assetName, ".zip"):
		return extractFromZip(archivePath, name, dest)
	}
	return fmt.Errorf("unsupported archive type: %s", assetName)
}

func extractFromTarGz(archivePath, name, dest string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("%s not found in archive", name)
		}
		if err != nil {
			return err
		}
		if hdr.Name == name && hdr.Typeflag == tar.TypeReg {
			return writeFile(dest, tr)
		}
	}
}

func extractFromZip(archivePath, name, dest string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		if zf.Name == name && zf.Mode().IsRegular() {
			rc, err := zf.Open()
			if err != nil {
				return err
			}
			defer rc.Close()
			return writeFile(dest, rc)
		}
	}
	return fmt.Errorf("%s not found in archive", name)
}

func writeFile(dest string, r io.Reader) error {
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// normalizeVersion strips a leading "v" prefix from a version string.
func normalizeVersion(v string) string {
	return strings.TrimPrefix(v, "v")
}

// isNewer returns true if latest is newer than current using simple
// semver comparison (major.minor.patch). Non-numeric versions compare
// as "0.0.0" which means any real release is considered newer than "dev".
func isNewer(latest, current string) bool {
	lp := parseSemver(latest)
	cp := parseSemver(current)

	if lp[0] != cp[0] {
		return lp[0] > cp[0]
	}
	if lp[1] != cp[1] {
		return lp[1] > cp[1]
	}
	return lp[2] > cp[2]
}

// parseSemver splits a "major.minor.patch" string into three integers.
// Returns [0,0,0] for unparseable input.
func parseSemver(v string) [3]int {
	var result [3]int
	parts := strings.SplitN(v, ".", 3)
	for i, p := range parts {
		if i >= 3 {
			break
		}
		// Strip any pre-release suffix (e.g., "1-beta")
		if idx := strings.IndexAny(p, "-+"); idx >= 0 {
			p = p[:idx]
		}
		n := 0
		for _, c := range p {
			if c >= '0' && c <= '9' {
				n = n*10 + int(c-'0')
			} else {
				break
			}
		}
		result[i] = n
	}
	return result
}
