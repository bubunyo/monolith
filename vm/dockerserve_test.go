package vm

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLoopbackConnectRequest runs docker-serve's loopback-connect with a stub
// socat that prints its arguments, to check which requests are accepted and
// what they connect to. It runs under the host's sh, not busybox ash; the
// handler only uses POSIX features.
func TestLoopbackConnectRequest(t *testing.T) {
	bin := t.TempDir()
	stub := "#!/bin/sh\necho \"socat $*\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "socat"), []byte(stub), 0o755))
	script, err := filepath.Abs("embeddings/docker/usr/local/bin/docker-serve")
	require.NoError(t, err)

	tests := []struct {
		name    string
		request string
		want    string // socat's arguments; "" means rejected
	}{
		{"v4 port", "52093\n", "socat -t 86400 STDIO TCP:127.0.0.1:52093"},
		{"v4 lowest", "1\n", "socat -t 86400 STDIO TCP:127.0.0.1:1"},
		{"v4 highest", "65535\n", "socat -t 86400 STDIO TCP:127.0.0.1:65535"},
		{"v6 port", "6 9000\n", "socat -t 86400 STDIO TCP6:[::1]:9000"},
		{"zero", "0\n", ""},
		{"out of range", "65536\n", ""},
		{"too long", "123456\n", ""},
		{"leading zero", "0080\n", ""},
		{"not a number", "abc\n", ""},
		{"shell syntax", "80;id\n", ""},
		{"trailing word", "80 extra\n", ""},
		{"v6 missing port", "6\n", ""},
		{"v6 bad port", "6 x\n", ""},
		{"v6 extra", "6 80 1\n", ""},
		{"other family", "4 80\n", ""},
		{"empty", "\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command("sh", script, "loopback-connect")
			cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
			cmd.Stdin = strings.NewReader(tt.request)
			out, err := cmd.Output()
			if tt.want == "" {
				require.Error(t, err)
				require.Empty(t, string(out))
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, strings.TrimSpace(string(out)))
		})
	}
}
