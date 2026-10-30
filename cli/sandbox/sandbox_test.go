package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bubunyo/monolith/vm"

	"github.com/stretchr/testify/require"
)

func TestNameFor(t *testing.T) {
	a := nameFor("/Users/me/src/My Project!")
	require.Regexp(t, `^sbx-my-project-[0-9a-f]{6}$`, a)
	require.NotEqual(t, a, nameFor("/Users/me/other/My Project!"), "same base name, different path")
	require.Equal(t, a, nameFor("/Users/me/src/My Project!"))
	require.LessOrEqual(t, len(nameFor("/x/"+strings.Repeat("a", 100))), 63)
}

func TestStripJSONC(t *testing.T) {
	in := `{
  // the image
  "image": "mcr.microsoft.com/devcontainers/go:1", /* inline */
  "url": "https://example.com/a//b",
  "list": [1, 2,],
}`
	out := string(stripJSONC([]byte(in)))
	require.Contains(t, out, `"https://example.com/a//b"`, "// inside strings is kept")
	require.NotContains(t, out, "the image")
	require.NotContains(t, out, "inline")
	require.NotContains(t, out, "2,]")
}

func TestShellImage(t *testing.T) {
	repo := t.TempDir()
	img, build, err := shellImage(repo, "")
	require.NoError(t, err)
	require.Equal(t, defaultImage, img)
	require.Nil(t, build)

	img, _, err = shellImage(repo, "node:22")
	require.NoError(t, err)
	require.Equal(t, "node:22", img)

	require.NoError(t, os.MkdirAll(filepath.Join(repo, ".devcontainer"), 0755))
	write := func(s string) {
		require.NoError(t, os.WriteFile(filepath.Join(repo, ".devcontainer", "devcontainer.json"), []byte(s), 0644))
	}
	write(`{ "image": "python:3.13", }`)
	img, build, err = shellImage(repo, "")
	require.NoError(t, err)
	require.Equal(t, "python:3.13", img)
	require.Nil(t, build)

	write(`{ "build": { "dockerfile": "Dockerfile", "context": ".." } }`)
	img, build, err = shellImage(repo, "")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(img, "monolith-devcontainer:"))
	require.Equal(t, []string{"-f", filepath.Join(repo, ".devcontainer", "Dockerfile"), repo}, build)

	write(`{ "build": { "dockerfile": "../../etc/Dockerfile" } }`)
	_, _, err = shellImage(repo, "")
	require.ErrorContains(t, err, "outside the repo")
}

func TestFirewallScript(t *testing.T) {
	out, err := firewallScript(&vm.SandboxConfig{})
	require.NoError(t, err)
	for _, n := range privateNets {
		require.Contains(t, out, "-d "+n+" -j REJECT")
	}
	require.Contains(t, out, "OUTPUT -o eth0 -m cgroup --path docker -j MONOLITH-SBX")
	require.Contains(t, out, "FORWARD -o eth0 -j MONOLITH-SBX")

	out, err = firewallScript(&vm.SandboxConfig{Network: vm.SandboxNetworkAllow, Allow: []string{"1.2.3.4:443"}})
	require.NoError(t, err)
	require.Contains(t, out, "-d 1.2.3.4 -p tcp --dport 443 -j RETURN")
	require.Contains(t, out, "iptables -A MONOLITH-SBX -j REJECT")
	require.NotContains(t, out, "10.0.0.0/8")

	_, err = firewallScript(&vm.SandboxConfig{Network: vm.SandboxNetworkAllow, Allow: []string{"nope"}})
	require.Error(t, err)
}

func TestWithin(t *testing.T) {
	require.True(t, within("/r/a/b", "/r"))
	require.False(t, within("/r", "/r/a"))
	require.False(t, within("/rx/a", "/r"))
	require.False(t, within("/r/../etc", "/r"))
}
