package vm

import (
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseLoadReply(t *testing.T) {
	require.NoError(t, parseLoadReply("ok\n"))
	require.NoError(t, parseLoadReply("unpacking...\nok\n"), "the last line counts")
	require.ErrorContains(t, parseLoadReply("error ctr: content digest mismatch \n"), "content digest mismatch")
	require.ErrorContains(t, parseLoadReply(""), "may still be starting")
	require.ErrorContains(t, parseLoadReply("huh"), "unexpected answer")
}

// LoadImage writes the whole tarball, half-closes, and reports the answer.
func TestLoadImage(t *testing.T) {
	home, err := os.MkdirTemp("", "ml") // short: unix socket paths are limited
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	path, err := VMLoadSockPath("c")
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0755))
	l, err := net.Listen("unix", path)
	require.NoError(t, err)
	defer l.Close()

	got := make(chan string, 1)
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			data, _ := io.ReadAll(conn) // until the client half-closes
			got <- string(data)
			if string(data) == "bad" {
				io.WriteString(conn, "error no such image\n")
			} else {
				io.WriteString(conn, "ok\n")
			}
			conn.Close()
		}
	}()

	require.NoError(t, LoadImage("c", stringsReader("tarball bytes")))
	require.Equal(t, "tarball bytes", <-got)
	require.ErrorContains(t, LoadImage("c", stringsReader("bad")), "no such image")
	<-got

	require.ErrorContains(t, LoadImage("missing", stringsReader("x")), "isn't accepting images")
}

func stringsReader(s string) io.Reader { return &onceReader{s: s} }

type onceReader struct{ s string }

func (r *onceReader) Read(p []byte) (int, error) {
	if r.s == "" {
		return 0, io.EOF
	}
	n := copy(p, r.s)
	r.s = r.s[n:]
	return n, nil
}
