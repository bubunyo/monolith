// Package update finds monolith's latest GitHub release and installs a
// release over the running binary, with the checks install.sh makes: the
// archive's sha256 against the release's checksums.txt, then the binary's
// code signature and virtualization entitlement.
package update

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

const repo = "bubunyo/monolith"

// Environment overrides, for mirrors and tests. MONOLITH_BASE_URL is the
// same variable install.sh reads.
const (
	envReleasesURL = "MONOLITH_RELEASES_URL" // the "latest release" API URL
	envBaseURL     = "MONOLITH_BASE_URL"     // download base; <base>/<tag>/<file>
	// EnvNoCheck turns off the release check in 'monolith --version'.
	EnvNoCheck = "MONOLITH_NO_UPDATE_CHECK"
)

// maxBinary bounds the binary read from a release archive.
const maxBinary = 512 << 20

// releasesURL is the GitHub API URL of the latest release.
func releasesURL() string {
	if u := os.Getenv(envReleasesURL); u != "" {
		return u
	}
	return "https://api.github.com/repos/" + repo + "/releases/latest"
}

// baseURL is where release files are downloaded from.
func baseURL() string {
	if u := os.Getenv(envBaseURL); u != "" {
		return strings.TrimSuffix(u, "/")
	}
	return "https://github.com/" + repo + "/releases/download"
}

// Latest returns the tag of the latest release, e.g. "v0.1.0".
func Latest(ctx context.Context, client *http.Client) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesURL(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("looking up the latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("looking up the latest release: %s", resp.Status)
	}
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return "", fmt.Errorf("reading the latest release: %w", err)
	}
	tag := Canonical(rel.TagName)
	if !semver.IsValid(tag) {
		return "", fmt.Errorf("the latest release has an unexpected tag %q", rel.TagName)
	}
	return tag, nil
}

// CheckLatest is Latest with a short timeout, for 'monolith --version'.
func CheckLatest() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return Latest(ctx, http.DefaultClient)
}

// Canonical adds the "v" release tags have, so "0.1.0" is "v0.1.0".
func Canonical(v string) string {
	v = strings.TrimSpace(v)
	if v != "" && !strings.HasPrefix(v, "v") {
		return "v" + v
	}
	return v
}

// describeRE matches what git describe adds past a tag: commits since it
// and the commit, then -dirty for uncommitted changes.
var describeRE = regexp.MustCompile(`-\d+-g[0-9a-f]+(-dirty)?$|-dirty$`)

// IsRelease reports whether v is a release build's version: a semver tag,
// not "dev" or a git describe of a commit past one.
func IsRelease(v string) bool {
	return semver.IsValid(v) && !describeRE.MatchString(v)
}

// Compare compares two release versions like semver: -1, 0 or +1.
func Compare(a, b string) int {
	return semver.Compare(a, b)
}

// archiveName is the release archive for version, as the Makefile's dist
// target names it.
func archiveName(version string) string {
	return "monolith_" + strings.TrimPrefix(version, "v") + "_darwin_arm64"
}

// Download fetches version's release archive and checksums into dir,
// checks the archive's sha256 and returns the path of the binary it
// extracted. log gets a line per stage.
func Download(ctx context.Context, client *http.Client, version, dir string, log io.Writer) (string, error) {
	name := archiveName(version)
	archive := name + ".tar.gz"
	url := baseURL() + "/" + version

	sums, err := fetch(ctx, client, url+"/checksums.txt", 1<<20)
	if err != nil {
		return "", err
	}
	want, err := checksumFor(sums, archive)
	if err != nil {
		return "", err
	}

	fmt.Fprintf(log, "Downloading monolith %s...\n", version)
	data, err := fetch(ctx, client, url+"/"+archive, maxBinary)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		return "", fmt.Errorf("checksum mismatch for %s: got %s, want %s", archive, got, want)
	}

	bin, err := extract(data, name+"/monolith")
	if err != nil {
		return "", fmt.Errorf("%s: %w", archive, err)
	}
	path := filepath.Join(dir, "monolith")
	if err := os.WriteFile(path, bin, 0o755); err != nil {
		return "", err
	}
	return path, nil
}

// fetch GETs url, reading at most limit bytes.
func fetch(ctx context.Context, client *http.Client, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", url, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("downloading %s: larger than %d bytes", url, limit)
	}
	return data, nil
}

// checksumFor finds file's sha256 in a checksums.txt ("<sum>  <file>").
func checksumFor(sums []byte, file string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == file {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("checksums.txt has no entry for %s", file)
}

// extract returns the file at path in a .tar.gz.
func extract(data []byte, path string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("no monolith binary in the archive")
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && strings.TrimPrefix(h.Name, "./") == path {
			if h.Size > maxBinary {
				return nil, errors.New("the monolith binary is too large")
			}
			return io.ReadAll(io.LimitReader(tr, maxBinary))
		}
	}
}

// VerifySignature checks that the binary at path has a valid code
// signature and the virtualization entitlement, which monolith needs.
func VerifySignature(path string) error {
	if out, err := exec.Command("codesign", "--verify", path).CombinedOutput(); err != nil {
		return fmt.Errorf("the downloaded binary's signature is invalid: %s", strings.TrimSpace(string(out)))
	}
	out, err := exec.Command("codesign", "-d", "--entitlements", "-", "--xml", path).Output()
	if err != nil {
		return fmt.Errorf("reading the downloaded binary's entitlements: %w", err)
	}
	if !bytes.Contains(out, []byte("com.apple.security.virtualization")) {
		return errors.New("the downloaded binary lacks the virtualization entitlement")
	}
	return nil
}

// Target is the binary to replace: the running executable, symlinks
// resolved.
func Target() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// Replace puts the binary at src in dst's place. If dst's directory is
// writable it's a rename of a copy made next to dst, so dst is never half
// written; otherwise it runs sudo install, which asks for a password on
// the terminal. It reports whether sudo was used.
func Replace(src, dst string) (sudo bool, err error) {
	dir := filepath.Dir(dst)
	if !writable(dir) {
		cmd := exec.Command("sudo", "install", "-m", "0755", src, dst)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			return true, fmt.Errorf("sudo install %s: %w", dst, err)
		}
		return true, nil
	}

	tmp, err := os.CreateTemp(dir, ".monolith-update-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	in, err := os.Open(src)
	if err != nil {
		tmp.Close()
		return false, err
	}
	defer in.Close()
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Chmod(0o755); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	return false, os.Rename(tmp.Name(), dst)
}

// writable reports whether files can be created in dir.
func writable(dir string) bool {
	f, err := os.CreateTemp(dir, ".monolith-write-test-*")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}
