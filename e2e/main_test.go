//go:build e2e

// Package e2e boots real VMs through the monolith CLI. Run it with
// `make test-e2e`; it needs an unlocked Mac, internet, the docker CLI and
// kubectl.
package e2e

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// binary is the monolith under test: MONOLITH_BIN, else ../bin/monolith.
var binary string

// sharedCaches are the ~/.monolith downloads the suite reuses instead of
// fetching again: Alpine, Kata, k3s, container images and the init.
var sharedCaches = []string{"alpine", "kata", "k3s", "images", "container"}

// TestMain points HOME at its own directory (MONOLITH_E2E_HOME, else
// ~/.cache/monolith-e2e) so runs never touch the user's VMs, docker contexts
// or kubeconfig, and links in the user's ~/.monolith download caches.
func TestMain(m *testing.M) {
	os.Exit(setup(m))
}

func setup(m *testing.M) int {
	bin := os.Getenv("MONOLITH_BIN")
	if bin == "" {
		bin = "../bin/monolith"
	}
	abs, err := filepath.Abs(bin)
	if err == nil {
		_, err = os.Stat(abs)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: no monolith binary at %s; run 'make build' or set MONOLITH_BIN\n", bin)
		return 1
	}
	binary = abs

	real, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		return 1
	}
	home := os.Getenv("MONOLITH_E2E_HOME")
	if home == "" {
		home = filepath.Join(real, ".cache", "monolith-e2e")
	}
	// VM sockets live under HOME, and macOS caps socket paths at 104 bytes.
	if len(home) > 60 {
		fmt.Fprintf(os.Stderr, "e2e: %s is too long for VM socket paths; set a shorter MONOLITH_E2E_HOME\n", home)
		return 1
	}
	if err := linkCaches(filepath.Join(real, ".monolith"), filepath.Join(home, ".monolith")); err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		return 1
	}
	os.Setenv("HOME", home)
	for _, k := range []string{"DOCKER_HOST", "DOCKER_CONTEXT", "KUBECONFIG"} {
		os.Unsetenv(k)
	}
	return m.Run()
}

// linkCaches symlinks each shared cache from src (the user's ~/.monolith)
// into dst (the e2e one). A cache already linked is left alone; a real
// directory left by an older run is replaced, since it only held downloads.
func linkCaches(src, dst string) error {
	if err := os.MkdirAll(dst, 0755); err != nil {
		return err
	}
	for _, name := range sharedCaches {
		from, to := filepath.Join(src, name), filepath.Join(dst, name)
		if err := os.MkdirAll(from, 0755); err != nil {
			return err
		}
		if target, err := os.Readlink(to); err == nil && target == from {
			continue
		}
		if err := os.RemoveAll(to); err != nil {
			return err
		}
		if err := os.Symlink(from, to); err != nil {
			return err
		}
	}
	return nil
}

// requireTool fails the test when a tool the suite drives isn't installed.
func requireTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Fatalf("%s not found on PATH; the e2e suite needs it", name)
	}
}

// run runs a command with extra env and returns its output and exit code.
func run(t *testing.T, env []string, name string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(out), exit.ExitCode()
	}
	require.NoError(t, err, "%s %s", name, strings.Join(args, " "))
	return string(out), 0
}

// must runs a command and fails the test unless it exits 0.
func must(t *testing.T, env []string, name string, args ...string) string {
	t.Helper()
	out, code := run(t, env, name, args...)
	require.Zero(t, code, "%s %s:\n%s", filepath.Base(name), strings.Join(args, " "), out)
	return out
}

func monolith(t *testing.T, args ...string) (string, int) {
	t.Helper()
	return run(t, nil, binary, args...)
}

func mustMonolith(t *testing.T, args ...string) string {
	t.Helper()
	return must(t, nil, binary, args...)
}

// freePort is a TCP port free on 127.0.0.1 right now.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// freePrivilegedPort is a port below 1024 free on the wildcard address,
// which macOS lets any user bind.
func freePrivilegedPort(t *testing.T) int {
	t.Helper()
	for p := 900; p < 1024; p++ {
		if l, err := net.Listen("tcp", ":"+strconv.Itoa(p)); err == nil {
			l.Close()
			return p
		}
	}
	t.Fatal("no free port in 900-1023")
	return 0
}

// serves reports whether localhost:port answers 200 with want in the body.
func serves(port int, want string) bool {
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode == http.StatusOK && strings.Contains(string(body), want)
}

func waitServes(t *testing.T, port int, want string, timeout time.Duration) {
	t.Helper()
	require.Eventually(t, func() bool { return serves(port, want) }, timeout, 500*time.Millisecond,
		"localhost:%d never served %q", port, want)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	return lines[len(lines)-1] + "\n"
}
