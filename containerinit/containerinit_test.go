package containerinit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCommand(t *testing.T) {
	ep, cmd := []string{"/docker-entrypoint.sh"}, []string{"nginx", "-g", "daemon off;"}
	empty, sh := "", "sh"
	require.Equal(t, []string{"/docker-entrypoint.sh", "nginx", "-g", "daemon off;"}, Command(ep, cmd, nil, nil))
	require.Equal(t, []string{"/docker-entrypoint.sh", "nginx", "-t"}, Command(ep, cmd, nil, []string{"nginx", "-t"}))
	require.Equal(t, []string{"sh", "-c", "id"}, Command(ep, cmd, &sh, []string{"-c", "id"}))
	require.Equal(t, []string{"ls"}, Command(ep, cmd, &empty, []string{"ls"}))
	require.Empty(t, Command(ep, cmd, &empty, nil))
	require.Equal(t, []string{"/bin/sh"}, Command(nil, []string{"/bin/sh"}, nil, nil))
}

func TestEnv(t *testing.T) {
	env := MergeEnv([]string{"PATH=/a", "LANG=C"}, "LANG=en", "NEW=1", "BARE")
	require.Equal(t, []string{"PATH=/a", "LANG=en", "NEW=1"}, env)

	got := ProcessEnv(Spec{Env: env, Hostname: "web", TTY: true}, "/root")
	require.Equal(t, []string{"HOSTNAME=web", "HOME=/root", "TERM=xterm", "PATH=/a", "LANG=en", "NEW=1"}, got)
	got = ProcessEnv(Spec{}, "/")
	require.Equal(t, []string{"PATH=" + DefaultPath, "HOME=/"}, got)
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	for p, data := range files {
		full := filepath.Join(root, p)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0755))
		require.NoError(t, os.WriteFile(full, []byte(data), 0755))
	}
}

func TestResolveUser(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{
		"etc/passwd": "root:x:0:0:root:/root:/bin/sh\nnginx:x:101:101:nginx:/var/cache/nginx:/sbin/nologin\napp:x:1000:1000::/home/app:/bin/sh\n",
		"etc/group":  "root:x:0:root\nnginx:x:101:\napp:x:1000:\nwheel:x:10:app\naudio:x:29:app,nginx\n",
	})
	cases := map[string]Cred{
		"":            {UID: 0, GID: 0, Home: "/root", Groups: []uint32{0}},
		"root":        {UID: 0, GID: 0, Home: "/root", Groups: []uint32{0}},
		"nginx":       {UID: 101, GID: 101, Home: "/var/cache/nginx", Groups: []uint32{29}},
		"1000":        {UID: 1000, GID: 1000, Home: "/home/app", Groups: []uint32{10, 29}},
		"app:wheel":   {UID: 1000, GID: 10, Home: "/home/app", Groups: []uint32{10, 29}},
		"4242":        {UID: 4242, GID: 0, Home: "/"},
		"4242:4343":   {UID: 4242, GID: 4343, Home: "/"},
		"nginx:audio": {UID: 101, GID: 29, Home: "/var/cache/nginx", Groups: []uint32{29}},
	}
	for user, want := range cases {
		got, err := ResolveUser(root, user)
		require.NoError(t, err, user)
		require.Equal(t, want, got, user)
	}
	_, err := ResolveUser(root, "nobody")
	require.ErrorContains(t, err, "unable to find user nobody")
	_, err = ResolveUser(root, "app:nogroup")
	require.ErrorContains(t, err, "unable to find group nogroup")

	// An image without passwd (scratch) still runs as root or a numeric user.
	got, err := ResolveUser(t.TempDir(), "")
	require.NoError(t, err)
	require.Equal(t, Cred{Home: "/root"}, got)
}

func TestLookPath(t *testing.T) {
	root := t.TempDir()
	writeFiles(t, root, map[string]string{"usr/bin/tool": "x", "app/run.sh": "x"})
	require.NoError(t, os.WriteFile(filepath.Join(root, "usr/bin/data"), nil, 0644))

	p, code, err := LookPath(root, "/", "tool", "/usr/local/bin:/usr/bin")
	require.NoError(t, err)
	require.Equal(t, "/usr/bin/tool", p)
	require.Zero(t, code)

	p, _, err = LookPath(root, "/app", "./run.sh", "")
	require.NoError(t, err)
	require.Equal(t, "/app/run.sh", p)

	_, code, err = LookPath(root, "/", "missing", "/usr/bin")
	require.Equal(t, ExitNotFound, code)
	require.ErrorContains(t, err, "executable file not found in $PATH")

	_, code, _ = LookPath(root, "/", "data", "/usr/bin")
	require.Equal(t, ExitNotExecutable, code)
	_, code, _ = LookPath(root, "/", "/nope", "/usr/bin")
	require.Equal(t, ExitNotFound, code)
}

func TestLinuxSignal(t *testing.T) {
	for in, want := range map[string]int{"SIGQUIT": 3, "quit": 3, "SIGUSR1": 10, "15": 15, " SIGWINCH ": 28} {
		got, err := LinuxSignal(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
	}
	for _, bad := range []string{"", "SIGNOPE", "0", "99"} {
		_, err := LinuxSignal(bad)
		require.Error(t, err, bad)
	}
}

func TestResolvConf(t *testing.T) {
	pnp := "#PROTO: DHCP\nnameserver 192.168.127.1\nnameserver 0.0.0.0\nbootserver 0.0.0.0\ndomain example.test\n"
	require.Equal(t, "nameserver 192.168.127.1\ndomain example.test\n", ResolvConf(pnp))
}
