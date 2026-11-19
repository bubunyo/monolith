package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bubunyo/monolith/update"
)

func TestVersionNote(t *testing.T) {
	latest := func(tag string) func() (string, error) {
		return func() (string, error) { return tag, nil }
	}
	for _, c := range []struct {
		current, latest, want string
	}{
		{"v0.0.1-rc.4", "v0.0.1-rc.4", " (latest)\n"},
		{"v0.0.1-rc.3", "v0.0.1-rc.4", "\nUpdate available: v0.0.1-rc.4 (run 'monolith update')\n"},
		{"v0.0.2", "v0.0.1", " (newer than the latest release, v0.0.1)\n"},
		{"v0.0.1-rc.3-5-g34fa993", "v0.0.1-rc.4", " (development build; latest release is v0.0.1-rc.4)\n"},
		{"dev", "v0.0.1-rc.4", " (development build; latest release is v0.0.1-rc.4)\n"},
	} {
		if got := versionNote(c.current, latest(c.latest)); got != c.want {
			t.Errorf("versionNote(%s, %s) = %q, want %q", c.current, c.latest, got, c.want)
		}
	}

	failed := func() (string, error) { return "", errors.New("offline") }
	if got := versionNote("v0.0.1", failed); got != "\n" {
		t.Errorf("offline note = %q", got)
	}

	t.Setenv(update.EnvNoCheck, "1")
	called := false
	versionNote("v0.0.1", func() (string, error) { called = true; return "v0.0.2", nil })
	if called {
		t.Error("the check ran with " + update.EnvNoCheck + " set")
	}
}

func TestRunUpdate(t *testing.T) {
	// A fake release v0.2.0 whose binary is a script printing its version.
	const script = "#!/bin/sh\necho monolith version v0.2.0\n"
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "monolith_0.2.0_darwin_arm64/monolith", Mode: 0o755, Size: int64(len(script)), Typeflag: tar.TypeReg})
	tw.Write([]byte(script))
	tw.Close()
	gz.Close()
	archive := buf.Bytes()
	sum := sha256.Sum256(archive)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			io.WriteString(w, `{"tag_name": "v0.2.0"}`)
		case "/download/v0.2.0/checksums.txt":
			fmt.Fprintf(w, "%s  monolith_0.2.0_darwin_arm64.tar.gz\n", hex.EncodeToString(sum[:]))
		case "/download/v0.2.0/monolith_0.2.0_darwin_arm64.tar.gz":
			w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("MONOLITH_RELEASES_URL", srv.URL+"/latest")
	t.Setenv("MONOLITH_BASE_URL", srv.URL+"/download")
	t.Setenv("HOME", t.TempDir()) // no VMs or networks

	dst := filepath.Join(t.TempDir(), "monolith")
	os.WriteFile(dst, []byte("old"), 0o755)
	oldVerify, oldTarget, oldVersion := verifySignature, updateTarget, version
	defer func() { verifySignature, updateTarget, version = oldVerify, oldTarget, oldVersion }()
	verified := false
	verifySignature = func(string) error { verified = true; return nil }
	updateTarget = func() (string, error) { return dst, nil }

	// A development build isn't replaced without --force.
	version = "v0.1.0-3-gabc1234"
	if err := runUpdate(); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("dev build: err = %v, want a --force hint", err)
	}

	// Up to date: nothing is downloaded.
	version = "v0.2.0"
	if err := runUpdate(); err != nil || verified {
		t.Fatalf("up to date: err = %v, verified = %v", err, verified)
	}

	version = "v0.1.0"
	if err := runUpdate(); err != nil {
		t.Fatal(err)
	}
	if !verified {
		t.Error("the signature wasn't verified")
	}
	if got, _ := os.ReadFile(dst); string(got) != script {
		t.Errorf("dst = %q, want the new binary", got)
	}
	if got := installedVersion(dst, "?"); got != "v0.2.0" {
		t.Errorf("installed version = %q", got)
	}
}
