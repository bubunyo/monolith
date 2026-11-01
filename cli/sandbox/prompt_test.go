package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/bubunyo/monolith/vm"
)

func TestShellRCLocation(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "it's a $(repo)")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "src", "api"), 0755))
	rc := filepath.Join(root, "rc")
	require.NoError(t, os.WriteFile(rc, []byte(shellRC(repo, vm.SandboxNetworkOutbound)), 0644))

	for dir, want := range map[string]string{
		repo:                              "lith:it-s-a---repo-",
		filepath.Join(repo, "src", "api"): "lith:it-s-a---repo-/src/api",
		root:                              "lith:" + root,
	} {
		cmd := exec.Command("bash", "-c", `. "$1"; cd "$2"; __lith_where`, "bash", rc, dir)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		require.Equal(t, want, string(out))
	}
}

func TestShellRCPrompt(t *testing.T) {
	render := func(network string, env ...string) string {
		rc := filepath.Join(t.TempDir(), "rc")
		require.NoError(t, os.WriteFile(rc, []byte(shellRC("/r/repo", network)), 0644))
		cmd := exec.Command("bash", "-c", `. "$1"; printf %s "$PS1"`, "bash", rc)
		cmd.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, string(out))
		return string(out)
	}

	got := render(vm.SandboxNetworkOutbound, "TERM=xterm")
	require.Contains(t, got, "\x1b[1;33m")
	require.NotContains(t, got, "net")

	require.Contains(t, render(vm.SandboxNetworkNone, "TERM=xterm"), "⊘net")
	require.Contains(t, render(vm.SandboxNetworkAllow, "TERM=xterm"), "allow")

	for _, env := range []string{"NO_COLOR=1", "TERM=dumb"} {
		got := render(vm.SandboxNetworkNone, "TERM=xterm", env)
		require.False(t, strings.Contains(got, "\x1b"), "%s: %q", env, got)
		require.Equal(t, `($(__lith_where)) ⊘net \$ `, got)
	}
}
