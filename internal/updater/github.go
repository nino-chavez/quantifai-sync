package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

// UpdatedToEnv names the release this process just installed. The restart
// after an update keeps the environment (exec on macOS and Linux; the
// supervisor passes it on Windows), so the new process sees it.
const UpdatedToEnv = "QUANTIFAI_UPDATED_TO"

// GithubUpdater checks GitHub Releases for newer versions and performs
// atomic binary replacement when an update is found.
type GithubUpdater struct {
	log           *logger.Logger
	version       string
	updateChannel string
	repo          string // "owner/repo"
	interval      time.Duration
	// client's Timeout bounds the small requests (release lookup, checksum
	// files). The archive download replaces it with a stall timeout.
	client *http.Client

	// executable locates the binary to replace; nil means os.Executable.
	executable func() (string, error)

	// applied is signalled once Run installs an update.
	applied chan struct{}

	// retryBase overrides defaultRetryBase (tests).
	retryBase time.Duration

	// stallTimeout overrides defaultStallTimeout (tests).
	stallTimeout time.Duration
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

	tag, err := g.latestTag(ctx)
	if err != nil {
		return false, fmt.Errorf("find latest release: %w", err)
	}

	latestVersion := normalizeVersion(tag)
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
	if os.Getenv(UpdatedToEnv) == tag {
		return false, fmt.Errorf("release %s was just installed but this binary reports %s; not installing it again (check the release binary's version stamp)", tag, g.version)
	}

	g.log.Info("update available", map[string]any{
		"current": currentVersion,
		"latest":  latestVersion,
	})

	assetName := expectedAssetName(runtime.GOOS, runtime.GOARCH)
	checksumName := assetName + ".sha256"
	// Release files download from github.com directly by name; the REST
	// API, rate-limited to 60 unauthenticated requests an hour per IP, is
	// not involved.
	base := fmt.Sprintf("https://github.com/%s/releases/download/%s/", g.repo, tag)

	// Download the archive to a temp file
	tmpFile, err := tempPath("quantifai-sync-update-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmpFile)

	if err := g.downloadFile(ctx, base+assetName, tmpFile); err != nil {
		if notFound(err) {
			return false, fmt.Errorf("no asset %s in release %s: %w", assetName, tag, err)
		}
		return false, fmt.Errorf("download archive: %w", err)
	}

	// Verify the checksum: the per-file .sha256, else a consolidated
	// checksums.txt or SHA256SUMS. Refuse the update if none exists.
	err = g.verifyChecksum(ctx, base+checksumName, tmpFile)
	if notFound(err) {
		for _, name := range []string{"checksums.txt", "SHA256SUMS"} {
			err = g.verifyConsolidatedChecksum(ctx, base+name, assetName, tmpFile)
			if !notFound(err) {
				break
			}
		}
		if notFound(err) {
			g.log.Warn("no checksum file found in release — refusing unverified update", map[string]any{
				"expected": checksumName,
				"release":  tag,
				"asset":    assetName,
			})
			return false, fmt.Errorf("no checksum file found for %s in release %s — refusing to apply unverified update", assetName, tag)
		}
	}
	if err != nil {
		g.log.Error("checksum verification failed", map[string]any{
			"asset": assetName,
			"error": err.Error(),
		})
		return false, fmt.Errorf("checksum verification failed for %s: %w", assetName, err)
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
	g.version = tag
	os.Setenv(UpdatedToEnv, tag)

	g.log.Info("update applied successfully", map[string]any{
		"from": currentVersion,
		"to":   latestVersion,
	})

	return true, nil
}

// Run starts the background update loop. It checks on startup, then
// every interval. A check that fails for a reason that may clear up on its
// own (see retryable) is retried after a minute, then two, four and so on,
// up to the interval, instead of waiting the whole interval. Blocks until
// ctx is cancelled, or returns once an update is installed, after
// signalling Applied.
func (g *GithubUpdater) Run(ctx context.Context) {
	base := g.retryBase
	if base <= 0 {
		base = defaultRetryBase
	}
	when := "startup"
	var retry time.Duration
	for {
		applied, err := g.check(ctx, when)
		if applied {
			return
		}
		wait := g.interval
		if err != nil && retryable(err) {
			retry = nextRetry(retry, base, g.interval)
			wait = retry
			g.log.Info("update check will be retried soon", map[string]any{"in": wait.String()})
		} else {
			retry = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		when = "periodic"
	}
}

// check runs one update check. When it installs an update it signals
// Applied and returns true.
func (g *GithubUpdater) check(ctx context.Context, when string) (bool, error) {
	applied, err := g.CheckAndApply(ctx)
	if err != nil {
		g.log.Warn(when+" update check failed", map[string]any{
			"error": err.Error(),
		})
		return false, err
	}
	if !applied {
		return false, nil
	}
	select {
	case g.applied <- struct{}{}:
	default:
	}
	return true, nil
}

// latestTag returns the tag of the repository's latest release, read
// from the redirect github.com/<repo>/releases/latest answers with
// (Location: .../releases/tag/<tag>). GitHub documents that URL ("Linking
// to releases"), and unlike the REST API it carries no 60-an-hour limit
// for unauthenticated clients.
func (g *GithubUpdater) latestTag(ctx context.Context) (string, error) {
	url := fmt.Sprintf("https://github.com/%s/releases/latest", g.repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "quantifai-sync/"+g.version)

	client := *g.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	resp.Body.Close()

	if resp.StatusCode < 300 || resp.StatusCode > 399 {
		return "", newStatusError(resp, "latest release lookup returned HTTP %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	const marker = "/releases/tag/"
	i := strings.LastIndex(loc, marker)
	if i < 0 || i+len(marker) == len(loc) {
		// A repository with no release redirects to its releases page.
		return "", fmt.Errorf("no published release (latest redirects to %s)", loc)
	}
	return loc[i+len(marker):], nil
}

// downloadFile downloads a URL to a local file path. The download has no
// overall time limit, so a slow link can finish it; it is abandoned only
// when no data arrives for the stall timeout, including while waiting for
// the response to start.
func (g *GithubUpdater) downloadFile(ctx context.Context, url, destPath string) error {
	stall := g.stallTimeout
	if stall <= 0 {
		stall = defaultStallTimeout
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	timer := time.AfterFunc(stall, func() { cancel(&stalledError{after: stall}) })
	defer timer.Stop()
	// stalled reports a stall as the error. Go before 1.23 surfaces it from
	// the body read as a bare context.Canceled, which is not retryable. A
	// shutdown keeps its own error.
	stalled := func(err error) error {
		var se *stalledError
		if errors.As(context.Cause(ctx), &se) {
			return se
		}
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "quantifai-sync/"+g.version)

	client := *g.client
	client.Timeout = 0
	resp, err := client.Do(req)
	if err != nil {
		return stalled(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return newStatusError(resp, "download returned HTTP %d", resp.StatusCode)
	}

	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()

	body := &progressReader{r: resp.Body, progress: func() { timer.Reset(stall) }}
	if _, err := io.Copy(f, body); err != nil {
		return stalled(err)
	}
	return f.Close()
}

// progressReader calls progress whenever a read returns data.
type progressReader struct {
	r        io.Reader
	progress func()
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.progress()
	}
	return n, err
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
		return newStatusError(resp, "checksum download returned HTTP %d", resp.StatusCode)
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
		return newStatusError(resp, "checksum download returned HTTP %d", resp.StatusCode)
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
