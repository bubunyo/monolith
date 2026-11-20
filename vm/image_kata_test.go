package vm

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseVerityParams(t *testing.T) {
	p, err := parseVerityParams("root_hash=abc,salt=def,data_blocks=64000,data_block_size=4096,hash_block_size=4096\n")
	require.NoError(t, err)
	require.Equal(t, verityParams{rootHash: "abc", salt: "def", dataBlocks: 64000, dataBlockSize: 4096, hashBlockSize: 4096}, p)

	for _, bad := range []string{
		"",
		"root_hash=abc,salt=def,data_blocks=x,data_block_size=4096,hash_block_size=4096",
		"salt=def,data_blocks=1,data_block_size=4096,hash_block_size=4096",
		"root_hash=abc,salt=def,data_blocks=1,data_block_size=4096",
	} {
		_, err := parseVerityParams(bad)
		require.Error(t, err, "input %q", bad)
	}
}

func TestKataCommandLine(t *testing.T) {
	cl := kataCommandLine("my-vm", verityParams{rootHash: "abc", salt: "def", dataBlocks: 64000, dataBlockSize: 4096, hashBlockSize: 4096})
	// 64000 blocks of 4 KiB = 512000 sectors; hash tree on vda2.
	require.Contains(t, cl, `dm-mod.create="dm-verity,,,ro,0 512000 verity 1 /dev/vda1 /dev/vda2 4096 4096 64000 0 sha256 abc def"`)
	for _, want := range []string{"console=hvc0", "root=/dev/dm-0", "ip=dhcp", "systemd.hostname=my-vm", "agent.debug_console"} {
		require.Contains(t, strings.Fields(cl), want)
	}
}
