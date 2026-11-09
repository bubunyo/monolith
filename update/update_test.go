package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsRelease(t *testing.T) {
	for v, want := range map[string]bool{
		"v0.1.0":                  true,
		"v0.0.1-rc.4":             true,
		"dev":                     false,
		"34fa993":                 false,
		"v0.0.1-rc.3-5-g34fa993":  false,
		"v0.1.0-2-gabc1234-dirty": false,
		"v0.1.0-dirty":            false,
		"0.1.0":                   false,
	} {
		if got := IsRelease(v); got != want {
			t.Errorf("IsRelease(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestCompare(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"v0.0.1-rc.3", "v0.0.1-rc.4", -1},
		{"v0.0.1-rc.10", "v0.0.1-rc.9", 1},
		{"v0.0.1-rc.4", "v0.0.1", -1},
		{"v0.1.0", "v0.1.0", 0},
		{"v1.0.0", "v0.9.9", 1},
	} {
		if got := Compare(c.a, c.b); got != c.want {
			t.Errorf("Compare(%s, %s) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCanonical(t *testing.T) {
	if got := Canonical("0.1.0"); got != "v0.1.0" {
		t.Errorf("Canonical(0.1.0) = %q", got)
	}
	if got := Canonical("v0.1.0"); got != "v0.1.0" {
		t.Errorf("Canonical(v0.1.0) = %q", got)
	}
}

// release is a fake GitHub release: the latest tag and its files.
type release struct {
	tag   string
	files map[string][]byte // under /download/<tag>/
}

func (r release) serve(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/latest" {
			io.WriteString(w, `{"tag_name": "`+r.tag+`", "name": "x"}`)
			return
		}
		name, ok := strings.CutPrefix(req.URL.Path, "/download/"+r.tag+"/")
		if data, found := r.files[name]; ok && found {
			w.Write(data)
			return
		}
		http.NotFound(w, req)
	}))
	t.Cleanup(srv.Close)
	t.Setenv(envReleasesURL, srv.URL+"/latest")
	t.Setenv(envBaseURL, srv.URL+"/download")
}

// tarball builds a .tar.gz holding files.
func tarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func checksums(files map[string][]byte) []byte {
	var b strings.Builder
	for name, data := range files {
		sum := sha256.Sum256(data)
		b.WriteString(hex.EncodeToString(sum[:]) + "  " + name + "\n")
	}
	return []byte(b.String())
}

func TestLatest(t *testing.T) {
	release{tag: "v0.2.0"}.serve(t)
	tag, err := Latest(context.Background(), http.DefaultClient)
	if err != nil || tag != "v0.2.0" {
		t.Fatalf("Latest = %q, %v", tag, err)
	}
}

func TestDownload(t *testing.T) {
	const tag = "v0.2.0"
	archive := tarball(t, map[string]string{
		"monolith_0.2.0_darwin_arm64/LICENSE":  "license",
		"monolith_0.2.0_darwin_arm64/monolith": "new binary",
	})
	files := map[string][]byte{"monolith_0.2.0_darwin_arm64.tar.gz": archive}
	files["checksums.txt"] = checksums(files)
	release{tag: tag, files: files}.serve(t)

	var log bytes.Buffer
	bin, err := Download(context.Background(), http.DefaultClient, tag, t.TempDir(), &log)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(bin)
	if string(got) != "new binary" {
		t.Errorf("binary = %q", got)
	}
	if fi, _ := os.Stat(bin); fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v", fi.Mode())
	}
	if !strings.Contains(log.String(), "Downloading monolith v0.2.0") {
		t.Errorf("log = %q", log.String())
	}
}

func TestDownloadChecksumMismatch(t *testing.T) {
	const tag = "v0.2.0"
	files := map[string][]byte{"monolith_0.2.0_darwin_arm64.tar.gz": []byte("tampered")}
	files["checksums.txt"] = checksums(map[string][]byte{"monolith_0.2.0_darwin_arm64.tar.gz": []byte("original")})
	release{tag: tag, files: files}.serve(t)

	_, err := Download(context.Background(), http.DefaultClient, tag, t.TempDir(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err = %v, want a checksum mismatch", err)
	}
}

func TestDownloadMissingBinary(t *testing.T) {
	const tag = "v0.2.0"
	files := map[string][]byte{"monolith_0.2.0_darwin_arm64.tar.gz": tarball(t, map[string]string{
		"monolith_0.2.0_darwin_arm64/README.md": "readme",
	})}
	files["checksums.txt"] = checksums(files)
	release{tag: tag, files: files}.serve(t)

	_, err := Download(context.Background(), http.DefaultClient, tag, t.TempDir(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no monolith binary") {
		t.Fatalf("err = %v, want a missing binary", err)
	}
}

func TestDownloadNoChecksumEntry(t *testing.T) {
	const tag = "v0.2.0"
	release{tag: tag, files: map[string][]byte{"checksums.txt": []byte("abc  other.tar.gz\n")}}.serve(t)
	_, err := Download(context.Background(), http.DefaultClient, tag, t.TempDir(), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no entry") {
		t.Fatalf("err = %v, want no checksum entry", err)
	}
}

func TestReplace(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "new")
	dst := filepath.Join(dir, "monolith")
	os.WriteFile(src, []byte("new"), 0o644)
	os.WriteFile(dst, []byte("old"), 0o755)

	sudo, err := Replace(src, dst)
	if err != nil || sudo {
		t.Fatalf("Replace = %v, %v", sudo, err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "new" {
		t.Errorf("dst = %q", got)
	}
	if fi, _ := os.Stat(dst); fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v", fi.Mode())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Errorf("left behind: %v", entries)
	}
}
