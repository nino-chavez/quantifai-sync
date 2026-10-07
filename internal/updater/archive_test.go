package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// writeArchive builds a release-style archive at path holding the given
// files. The format follows the file name, as releases name them.
func writeArchive(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if strings.HasSuffix(path, ".zip") {
		zw := zip.NewWriter(f)
		for name, body := range files {
			w, err := zw.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			w.Write([]byte(body))
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExtractBinary(t *testing.T) {
	for _, asset := range []string{"quantifai-sync-linux-amd64.tar.gz", "quantifai-sync-windows-amd64.zip"} {
		t.Run(asset, func(t *testing.T) {
			dir := t.TempDir()
			archive := filepath.Join(dir, asset)
			writeArchive(t, archive, map[string]string{
				"README":            "decoy",
				"quantifai-sync-x":  "the binary",
				"quantifai-sync-xy": "decoy",
			})

			dest := filepath.Join(dir, "out")
			if err := extractBinary(archive, asset, "quantifai-sync-x", dest); err != nil {
				t.Fatal(err)
			}
			if got, _ := os.ReadFile(dest); string(got) != "the binary" {
				t.Fatalf("extracted %q, want %q", got, "the binary")
			}

			err := extractBinary(archive, asset, "quantifai-sync-missing", filepath.Join(dir, "none"))
			if err == nil || !strings.Contains(err.Error(), "not found") {
				t.Fatalf("missing binary: got err %v, want not found", err)
			}
		})
	}
}

func TestExtractBinaryRejectsUnknownFormat(t *testing.T) {
	if err := extractBinary("x", "quantifai-sync-linux-amd64", "x", "y"); err == nil {
		t.Fatal("expected an error for an asset that is not an archive")
	}
}

// releaseLayout is the v0.3.0 release, copied by hand: asset name and the
// binary inside it, per platform.
var releaseLayout = map[string][2]string{
	"darwin/amd64":  {"quantifai-sync-darwin-amd64.tar.gz", "quantifai-sync-darwin-amd64"},
	"darwin/arm64":  {"quantifai-sync-darwin-arm64.tar.gz", "quantifai-sync-darwin-arm64"},
	"linux/amd64":   {"quantifai-sync-linux-amd64.tar.gz", "quantifai-sync-linux-amd64"},
	"linux/arm64":   {"quantifai-sync-linux-arm64.tar.gz", "quantifai-sync-linux-arm64"},
	"windows/amd64": {"quantifai-sync-windows-amd64.zip", "quantifai-sync-windows-amd64.exe"},
}

// CheckAndApply against a fake release laid out like the real ones:
// find the archive, verify its .sha256, extract the binary, replace the
// executable, and do not re-apply the same release on the next check.
func TestCheckAndApplyInstallsFromReleaseArchive(t *testing.T) {
	layout, ok := releaseLayout[runtime.GOOS+"/"+runtime.GOARCH]
	if !ok {
		t.Skipf("no release asset for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	asset, bin := layout[0], layout[1]

	dir := t.TempDir()
	archive := filepath.Join(dir, asset)
	writeArchive(t, archive, map[string]string{bin: "new binary v9.9.9"})
	data, _ := os.ReadFile(archive)
	sum := sha256.Sum256(data)

	downloads := 0
	mux := http.NewServeMux()
	var srvURL string
	mux.HandleFunc("/repos/o/r/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		var assets []string
		for _, l := range releaseLayout {
			for _, name := range []string{l[0], l[0] + ".sha256"} {
				assets = append(assets, fmt.Sprintf(`{"name":%q,"browser_download_url":%q}`, name, srvURL+"/dl/"+name))
			}
		}
		fmt.Fprintf(w, `{"tag_name":"v9.9.9","assets":[%s]}`, strings.Join(assets, ","))
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		switch name := strings.TrimPrefix(r.URL.Path, "/dl/"); name {
		case asset:
			downloads++
			w.Write(data)
		case asset + ".sha256":
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), asset)
		default:
			http.Error(w, "wrong platform asset requested: "+name, http.StatusTeapot)
		}
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	srvURL = server.URL

	exe := filepath.Join(dir, "installed")
	os.WriteFile(exe, []byte("old binary"), 0755)

	g := &GithubUpdater{
		log:        newDiscardLogger(),
		version:    "v0.3.0",
		repo:       "o/r",
		client:     &http.Client{Transport: &rewriteTransport{base: http.DefaultTransport, target: server.URL}},
		executable: func() (string, error) { return exe, nil },
	}

	applied, err := g.CheckAndApply(context.Background())
	if err != nil || !applied {
		t.Fatalf("CheckAndApply = %v, %v; want true, nil", applied, err)
	}
	if got, _ := os.ReadFile(exe); string(got) != "new binary v9.9.9" {
		t.Fatalf("installed %q, want the binary from inside the archive", got)
	}

	applied, err = g.CheckAndApply(context.Background())
	if err != nil || applied {
		t.Fatalf("second CheckAndApply = %v, %v; want false, nil", applied, err)
	}
	if downloads != 1 {
		t.Fatalf("archive downloaded %d times, want 1", downloads)
	}
}
