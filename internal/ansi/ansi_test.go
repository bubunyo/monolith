package ansi

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPlain(t *testing.T) {
	for in, want := range map[string]string{
		"[    0.1] virtio_blk: ok\r":                               "[    0.1] virtio_blk: ok",
		"[\x1b[0;32m  OK  \x1b[0m] Started \x1b[0;1;39mfoo\x1b[0m": "[  OK  ] Started foo",
		"\x1b]7777;monolith;ready\x07motd":                         "motd",
		"\x1b]11;?\x1b\\\x1b[6nshell":                              "shell",
		"progress 10%\rprogress 50%":                               "progress 50%",
		"a\tb\x01c":                                                "a    bc",
		"\x1b7 30% ####\x1b8(8/24) Installing iptables":            "(8/24) Installing iptables",
		"text \x1b7bar":                                            "text bar",
	} {
		require.Equal(t, want, Plain(in), "%q", in)
	}
}

func TestCut(t *testing.T) {
	require.Equal(t, "abc", Cut("abcdef", 3))
	require.Equal(t, "✔ é", Cut("✔ éx", 3))
	require.Equal(t, "ab", Cut("ab", 5))
	require.Empty(t, Cut("ab", 0))
}
