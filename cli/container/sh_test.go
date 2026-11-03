package container

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bubunyo/monolith/oci"
	"github.com/stretchr/testify/require"
)

// fakeImage is an in-memory image for the shell's tests.
type fakeImage struct {
	files map[string]string // path -> content ("" for directories ending in /)
	links map[string]string // path -> symlink target
}

func (f *fakeImage) Lstat(p string) (oci.Entry, error) {
	p = path.Clean("/" + p)
	now := time.Date(2026, 1, 2, 3, 4, 0, 0, time.UTC)
	if p == "/" {
		return oci.Entry{Name: "/", Path: "/", Mode: fs.ModeDir | 0755}, nil
	}
	if t, ok := f.links[p]; ok {
		return oci.Entry{Name: path.Base(p), Path: p, Mode: fs.ModeSymlink | 0777, Target: t, ModTime: now}, nil
	}
	if c, ok := f.files[p]; ok {
		mode := fs.FileMode(0644)
		if strings.HasPrefix(c, "#!") {
			mode = 0755
		}
		return oci.Entry{Name: path.Base(p), Path: p, Mode: mode, Size: int64(len(c)), ModTime: now}, nil
	}
	for q := range f.files {
		if strings.HasPrefix(q, p+"/") {
			return oci.Entry{Name: path.Base(p), Path: p, Mode: fs.ModeDir | 0755, ModTime: now}, nil
		}
	}
	return oci.Entry{}, fmt.Errorf("%s: no such file or directory", p)
}

func (f *fakeImage) Resolve(p string) (string, error) {
	p = path.Clean("/" + p)
	if t, ok := f.links[p]; ok {
		return path.Clean(path.Join(path.Dir(p), t)), nil
	}
	if _, err := f.Lstat(p); err != nil {
		return "", err
	}
	return p, nil
}

func (f *fakeImage) Stat(p string) (oci.Entry, error) {
	r, err := f.Resolve(p)
	if err != nil {
		return oci.Entry{}, err
	}
	return f.Lstat(r)
}

func (f *fakeImage) ReadDir(p string) ([]oci.Entry, error) {
	r, err := f.Resolve(p)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []oci.Entry
	add := func(q string) {
		if !strings.HasPrefix(q, strings.TrimSuffix(r, "/")+"/") {
			return
		}
		name := strings.SplitN(strings.TrimPrefix(q, strings.TrimSuffix(r, "/")+"/"), "/", 2)[0]
		if !seen[name] {
			seen[name] = true
			e, _ := f.Lstat(path.Join(r, name))
			out = append(out, e)
		}
	}
	for q := range f.files {
		add(q)
	}
	for q := range f.links {
		add(q)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeImage) Open(p string) (io.ReadCloser, oci.Entry, error) {
	e, err := f.Stat(p)
	if err != nil {
		return nil, oci.Entry{}, err
	}
	if e.IsDir() {
		return nil, oci.Entry{}, fmt.Errorf("%s: is a directory", p)
	}
	return io.NopCloser(strings.NewReader(f.files[e.Path])), e, nil
}

func (f *fakeImage) Names() (map[uint32]string, map[uint32]string) {
	return map[uint32]string{0: "root"}, map[uint32]string{0: "root"}
}

func testShell() (*imageShell, *bytes.Buffer) {
	img := &fakeImage{
		files: map[string]string{
			"/etc/nginx/nginx.conf":          "worker_processes auto;\n",
			"/etc/nginx/conf.d/default.conf": "server {}\n",
			"/etc/hostname":                  "box\n",
			"/etc/.hidden":                   "secret\n",
			"/usr/bin/tool":                  "#!/bin/sh\n" + strings.Repeat("x", 990),
			"/usr/bin/blob":                  "ELF\x00\x01binary",
			"/var/log/app.log":               "1\n2\n3\n4\n5\n",
		},
		links: map[string]string{"/bin": "usr/bin"},
	}
	var out bytes.Buffer
	return &imageShell{ctx: context.Background(), full: img, name: "test:1", layer: -1, cwd: "/", out: &out, width: 60}, &out
}

func TestParseLine(t *testing.T) {
	cmds, err := parseLine(`ls -l "/etc/my dir" && cat 'a b' ; find / -name '*.conf' # comment`)
	require.NoError(t, err)
	require.Len(t, cmds, 3)
	require.Equal(t, []string{"ls", "-l", "/etc/my dir"}, cmds[0].args)
	require.Equal(t, []string{"cat", "a b"}, cmds[1].args)
	require.True(t, cmds[1].andPrev)
	require.Equal(t, []string{"find", "/", "-name", "*.conf"}, cmds[2].args)
	require.False(t, cmds[2].andPrev)

	cmds, err = parseLine(`cat a\ b "q\"uote"`)
	require.NoError(t, err)
	require.Equal(t, []string{"cat", "a b", `q"uote`}, cmds[0].args)

	_, err = parseLine(`cat "open`)
	require.ErrorContains(t, err, "unterminated")
	_, err = parseLine(`ls | head`)
	require.ErrorContains(t, err, "pipes and redirection")
	_, err = parseLine(`cat x > y`)
	require.ErrorContains(t, err, "pipes and redirection")
	cmds, err = parseLine(`cat "a|b"`)
	require.NoError(t, err, "quoted | is a character")
	require.Equal(t, []string{"cat", "a|b"}, cmds[0].args)
}

func TestShellBrowse(t *testing.T) {
	sh, out := testShell()
	run := func(line string) (string, bool) {
		out.Reset()
		ok := sh.exec(line)
		return out.String(), ok
	}

	got, ok := run("ls")
	require.True(t, ok)
	require.Equal(t, "bin  etc  usr  var\n", got, "columns, hidden files hidden")
	got, _ = run("ls -a /etc")
	require.Contains(t, got, ".hidden")
	got, _ = run("ls -l /etc/hostname")
	require.Regexp(t, `^-rw-r--r-- root root +4B Jan  2 .* hostname\n$`, got)

	_, ok = run("cd /bin && pwd")
	require.True(t, ok)
	require.Equal(t, "/usr/bin", sh.cwd, "cd follows the symlink")
	got, _ = run("ls")
	require.Equal(t, "blob  tool\n", got)
	got, ok = run("cd /nope")
	require.False(t, ok)
	require.Contains(t, got, "cd: /nope: no such file or directory")
	_, ok = run("cd /etc/hostname")
	require.False(t, ok)

	got, _ = run("cd /; find /etc -name '*.conf'")
	require.Equal(t, "/etc/nginx/conf.d/default.conf\n/etc/nginx/nginx.conf\n", got)
	got, _ = run("find / -type l")
	require.Equal(t, "/bin\n", got)

	got, _ = run("cat /etc/nginx/nginx.conf")
	require.Equal(t, "worker_processes auto;\n", got)
	got, ok = run("cat /usr/bin/blob")
	require.False(t, ok)
	require.Contains(t, got, "binary file (use --force")
	got, ok = run("cat --force /usr/bin/blob")
	require.True(t, ok)
	require.Contains(t, got, "ELF")
	got, _ = run("head -n 2 /var/log/app.log")
	require.Equal(t, "1\n2\n", got)

	got, _ = run("tree /etc")
	require.Contains(t, got, "├── hostname\n")
	require.Contains(t, got, "└── nginx\n")
	require.Contains(t, got, "    ├── conf.d\n")
	require.Contains(t, got, "2 directories, 4 files")

	got, _ = run("du /usr")
	require.Contains(t, got, "bin/")
	require.Contains(t, got, "total")

	got, _ = run("stat /etc/hostname")
	require.Contains(t, got, "Type: regular file")
	require.Contains(t, got, "Mode: -rw-r--r-- (0644)")

	got, ok = run("nope; pwd")
	require.Contains(t, got, "nope: unknown command")
	require.True(t, ok, "; runs on after a failure")
	got, _ = run("nope && pwd")
	require.NotContains(t, got, "/\n", "&& stops after a failure")

	got, _ = run("help")
	require.Contains(t, got, "blame PATH")
	run("exit")
	require.True(t, sh.done)
}

func TestShellComplete(t *testing.T) {
	sh, _ := testShell()
	complete := func(line string) string {
		got, pos, ok := sh.complete(line, len(line), '\t')
		if !ok {
			return line
		}
		require.Equal(t, len(got), pos)
		return got
	}
	require.Equal(t, "layers ", complete("layers"), "commands")
	require.Equal(t, "layer", complete("lay"), "ambiguous (layer, layers): the shared part")
	require.Equal(t, "cd /etc/nginx/", complete("cd /etc/ngi"))
	require.Equal(t, "cat /etc/hostname ", complete("cat /etc/ho"))
	require.Equal(t, "ls /usr/bin/", complete("ls /usr/b"))
	require.Equal(t, "ls && cat ", complete("ls && ca"), "a command after &&")
	require.Equal(t, "cat /etc/", complete("cat /etc/"), "hidden files aren't offered for an empty prefix")
	_, _, ok := sh.complete("ls", 2, 'x')
	require.False(t, ok, "only Tab completes")
}

func TestColumns(t *testing.T) {
	names := []string{"a", "bb", "ccc", "dddd", "e"}
	require.Equal(t, "a     ccc   e\nbb    dddd\n", columns(names, names, 18))
	require.Equal(t, "a\nbb\nccc\ndddd\ne\n", columns(names, names, 3))
	require.Empty(t, columns(nil, nil, 80))
}
