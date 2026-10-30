package vm

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/bubunyo/monolith"
)

// Kubernetes (k3s) on docker image VMs. The host downloads the k3s binary
// once per version and shares the directory read-only into the guest over
// virtio-fs (tag "k3s"); the guest runs it as a single-node server on the
// VM's engine.
const (
	// DefaultK3sVersion is the k3s release used when --k3s-version isn't given.
	DefaultK3sVersion = "v1.36.5+k3s1"

	// KubeAPIPort is the k3s API server port in the guest.
	KubeAPIPort = 6443

	// K8sInfoPort is the guest TCP port (gateway only, like DockerAPIPort)
	// that answers a one-line request, "kubeconfig" or "services", with
	// k3s's kubeconfig or `kubectl get svc -A -o json`.
	K8sInfoPort = 2378

	// GuestLayout is the version of the guest files written into a docker
	// image VM's disk when it's created. Layout 1 has the k3s services;
	// layout 2 adds clean shutdown, Rosetta and file-change events.
	GuestLayout = 2

	// Minimum GuestLayout for each feature that needs guest files.
	GuestLayoutKubernetes = 1
	GuestLayoutRosetta    = 2 // also clean shutdown and file-change events

	k3sShareTag       = "k3s"
	k3sCmdlineArg     = "monolith.k3s"
	k3sBinaryName     = "k3s"
	k3sReleaseBaseURL = "https://github.com/k3s-io/k3s/releases/download/"
)

// ensureK3s returns the cache directory holding the k3s binary for version,
// downloading and verifying it on first use.
func ensureK3s(version string) (string, error) {
	dir, err := monolith.CachePath("k3s", version)
	if err != nil {
		return "", err
	}
	if exists(filepath.Join(dir, k3sBinaryName)) {
		return dir, nil
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", err
	}

	base := k3sReleaseBaseURL + url.PathEscape(version) + "/"
	want, err := k3sChecksum(base+"sha256sum-arm64.txt", "k3s-arm64")
	if err != nil {
		return "", fmt.Errorf("k3s %s checksums: %w", version, err)
	}

	resp, err := http.Get(base + "k3s-arm64")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading k3s %s: %s", version, resp.Status)
	}
	tmp, err := os.CreateTemp(dir, "k3s.*.tmp")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, hasher), resp.Body); err != nil {
		tmp.Close()
		return "", fmt.Errorf("downloading k3s %s: %w", version, err)
	}
	tmp.Close()
	if got := hex.EncodeToString(hasher.Sum(nil)); got != want {
		return "", fmt.Errorf("k3s %s checksum mismatch: got %s, want %s", version, got, want)
	}
	if err := os.Chmod(tmp.Name(), 0755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(dir, k3sBinaryName)); err != nil {
		return "", err
	}
	return dir, nil
}

// k3sChecksum fetches a release's sha256sum file and returns the hash for
// file.
func k3sChecksum(sumsURL, file string) (string, error) {
	resp, err := http.Get(sumsURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", sumsURL, resp.Status)
	}
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) == 2 && f[1] == file {
			return f[0], nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("no checksum for %s", file)
}

// K3sVersionOf returns the VM's k3s version, defaulting when unset.
func K3sVersionOf(cfg VMConfig) string {
	if cfg.K3sVersion == "" {
		return DefaultK3sVersion
	}
	return cfg.K3sVersion
}

// FreeLocalPort returns a TCP port that is free on 127.0.0.1 right now,
// for the host end of a VM's Kubernetes API forward.
func FreeLocalPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
