package guestinit

import (
	"bytes"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// readCpio parses a newc archive back into entries.
func readCpio(t *testing.T, data []byte) []CpioEntry {
	var out []CpioEntry
	field := func(b []byte, i int) int {
		n, err := strconv.ParseUint(string(b[6+8*i:6+8*i+8]), 16, 32)
		require.NoError(t, err)
		return int(n)
	}
	align := func(n int) int { return (n + 3) &^ 3 }
	for off := 0; off < len(data); {
		hdr := data[off : off+110]
		require.Equal(t, "070701", string(hdr[:6]))
		mode, size, namesize := field(hdr, 1), field(hdr, 6), field(hdr, 11)
		name := string(bytes.TrimRight(data[off+110:off+110+namesize], "\x00"))
		off = align(off + 110 + namesize)
		body := data[off : off+size]
		off = align(off + size)
		if name == "TRAILER!!!" {
			require.Equal(t, len(data), off, "nothing after the trailer")
			break
		}
		out = append(out, CpioEntry{Name: name, Mode: uint32(mode), Data: body})
	}
	return out
}

func TestCpio(t *testing.T) {
	in := []CpioEntry{
		{Name: "dev", Mode: 040755},
		{Name: "init", Mode: 0100755, Data: []byte("\x7fELF odd length")},
		{Name: "spec.json", Mode: 0100644, Data: []byte(`{"args":["sh"]}`)},
	}
	got := readCpio(t, Cpio(in))
	require.Len(t, got, 3)
	for i := range in {
		require.Equal(t, in[i].Name, got[i].Name)
		require.Equal(t, in[i].Mode, got[i].Mode)
		require.Equal(t, string(in[i].Data), string(got[i].Data))
	}
}
