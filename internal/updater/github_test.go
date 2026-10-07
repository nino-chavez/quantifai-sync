package updater

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Semver comparison tests
// ---------------------------------------------------------------------------

func TestNormalizeVersion(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"v1.2.3", "1.2.3"},
		{"1.2.3", "1.2.3"},
		{"v0.1.0", "0.1.0"},
		{"dev", "dev"},
	}
	for _, tt := range tests {
		got := normalizeVersion(tt.input)
		if got != tt.want {
			t.Errorf("normalizeVersion(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestParseSemver(t *testing.T) {
	tests := []struct {
		input string
		want  [3]int
	}{
		{"1.2.3", [3]int{1, 2, 3}},
		{"0.1.0", [3]int{0, 1, 0}},
		{"10.20.30", [3]int{10, 20, 30}},
		{"1.0.0-beta", [3]int{1, 0, 0}},
		{"1.2.3+build", [3]int{1, 2, 3}},
		{"dev", [3]int{0, 0, 0}},
		{"", [3]int{0, 0, 0}},
		{"1", [3]int{1, 0, 0}},
		{"1.2", [3]int{1, 2, 0}},
	}
	for _, tt := range tests {
		got := parseSemver(tt.input)
		if got != tt.want {
			t.Errorf("parseSemver(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestIsNewer(t *testing.T) {
	tests := []struct {
		latest, current string
		want            bool
	}{
		{"1.1.0", "1.0.0", true},
		{"1.0.1", "1.0.0", true},
		{"2.0.0", "1.9.9", true},
		{"1.0.0", "1.0.0", false},
		{"0.9.0", "1.0.0", false},
		{"1.0.0", "dev", true},      // any real version > dev (0.0.0)
		{"0.0.1", "0.0.0", true},
		{"10.0.0", "9.9.9", true},
	}
	for _, tt := range tests {
		got := isNewer(tt.latest, tt.current)
		if got != tt.want {
			t.Errorf("isNewer(%q, %q) = %v, want %v", tt.latest, tt.current, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Asset name matching tests
// ---------------------------------------------------------------------------

// The names below are copied from the v0.3.0 release, not derived from
// expectedAssetName: the old test checked the function against its own
// convention and passed while every real release used another one.
func TestExpectedAssetName(t *testing.T) {
	tests := []struct {
		goos, goarch string
		asset, bin   string
	}{
		{"darwin", "arm64", "quantifai-sync-darwin-arm64.tar.gz", "quantifai-sync-darwin-arm64"},
		{"darwin", "amd64", "quantifai-sync-darwin-amd64.tar.gz", "quantifai-sync-darwin-amd64"},
		{"linux", "amd64", "quantifai-sync-linux-amd64.tar.gz", "quantifai-sync-linux-amd64"},
		{"linux", "arm64", "quantifai-sync-linux-arm64.tar.gz", "quantifai-sync-linux-arm64"},
		{"windows", "amd64", "quantifai-sync-windows-amd64.zip", "quantifai-sync-windows-amd64.exe"},
	}
	for _, tt := range tests {
		if got := expectedAssetName(tt.goos, tt.goarch); got != tt.asset {
			t.Errorf("expectedAssetName(%q, %q) = %q, want %q", tt.goos, tt.goarch, got, tt.asset)
		}
		if got := binaryName(tt.goos, tt.goarch); got != tt.bin {
			t.Errorf("binaryName(%q, %q) = %q, want %q", tt.goos, tt.goarch, got, tt.bin)
		}
	}
}

// ---------------------------------------------------------------------------
// Atomic replace tests
// ---------------------------------------------------------------------------

func TestAtomicReplace(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "new-binary")
	dst := filepath.Join(dir, "old-binary")

	os.WriteFile(src, []byte("new content"), 0755)
	os.WriteFile(dst, []byte("old content"), 0755)

	if err := atomicReplace(src, dst); err != nil {
		t.Fatalf("atomicReplace: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != "new content" {
		t.Errorf("dst content = %q, want %q", string(got), "new content")
	}

	// Source should no longer exist (was renamed)
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("expected src to be gone after rename")
	}
}

// ---------------------------------------------------------------------------
// Mock HTTP version check tests
// ---------------------------------------------------------------------------

func TestFetchLatestRelease(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"tag_name": "v1.2.0",
			"assets": [
				{"name": "quantifai-sync-darwin-arm64", "browser_download_url": "https://example.com/darwin-arm64"},
				{"name": "quantifai-sync-darwin-arm64.sha256", "browser_download_url": "https://example.com/darwin-arm64.sha256"},
				{"name": "quantifai-sync-linux-amd64", "browser_download_url": "https://example.com/linux-amd64"}
			]
		}`)
	}))
	defer server.Close()

	g := &GithubUpdater{
		log:     newDiscardLogger(),
		version: "1.0.0",
		repo:    "test/repo",
		client:  server.Client(),
	}

	// Override the URL by injecting a custom fetch
	// We need to test the parsing, so use the server directly
	origURL := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", g.repo)
	_ = origURL // unused, we'll test via a round-trip test below

	// Test by using a custom transport
	g.client = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &rewriteTransport{
			base:   http.DefaultTransport,
			target: server.URL,
		},
	}

	release, err := g.fetchLatestRelease(context.Background())
	if err != nil {
		t.Fatalf("fetchLatestRelease: %v", err)
	}

	if release.TagName != "v1.2.0" {
		t.Errorf("tag_name: got %q, want %q", release.TagName, "v1.2.0")
	}
	if len(release.Assets) != 3 {
		t.Errorf("assets count: got %d, want 3", len(release.Assets))
	}
}

// rewriteTransport rewrites all requests to point at a local test server.
type rewriteTransport struct {
	base   http.RoundTripper
	target string
}

func (t *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.URL.Scheme = "http"
	req.URL.Host = t.target[len("http://"):]
	return t.base.RoundTrip(req)
}

// ---------------------------------------------------------------------------
// Checksum verification test
// ---------------------------------------------------------------------------

func TestVerifyChecksum(t *testing.T) {
	// Create a temp file with known content
	dir := t.TempDir()
	filePath := filepath.Join(dir, "binary")
	content := []byte("hello world binary content")
	os.WriteFile(filePath, content, 0644)

	// Compute expected hash
	h := sha256.Sum256(content)
	expectedHash := hex.EncodeToString(h[:])

	// Serve the checksum
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  quantifai-sync-darwin-arm64\n", expectedHash)
	}))
	defer server.Close()

	g := &GithubUpdater{
		log:     newDiscardLogger(),
		version: "1.0.0",
		client:  server.Client(),
	}

	err := g.verifyChecksum(context.Background(), server.URL+"/checksum", filePath)
	if err != nil {
		t.Fatalf("verifyChecksum should pass: %v", err)
	}

	// Now test with wrong checksum
	badServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "0000000000000000000000000000000000000000000000000000000000000000  file")
	}))
	defer badServer.Close()

	err = g.verifyChecksum(context.Background(), badServer.URL+"/checksum", filePath)
	if err == nil {
		t.Fatal("verifyChecksum should fail with wrong hash")
	}
}
