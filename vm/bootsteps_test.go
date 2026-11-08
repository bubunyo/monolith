package vm

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/require"
)

func marker(name string) string { return bootMarkerPrefix + name + "\x07" }

type bootLog struct {
	lines map[int][]string
	ended []int
}

func watch(t *testing.T, console string, steps []BootStep) (bootLog, string, error) {
	t.Helper()
	got := bootLog{lines: map[int][]string{}}
	// One byte at a time: markers and lines arrive split across reads.
	r := strings.NewReader(console)
	rest, err := WatchBoot(iotest.OneByteReader(r), steps,
		func(step int, text string) { got.lines[step] = append(got.lines[step], text) },
		func(step int) { got.ended = append(got.ended, step) })
	unread, _ := io.ReadAll(r) // the console relay reads these next
	return got, string(rest) + string(unread), err
}

func TestWatchBootMarkers(t *testing.T) {
	steps := BootSteps(VMConfig{Image: ImageDocker, GuestLayout: GuestLayout})
	require.Equal(t, []string{"Kernel", "System", "Network", "Engine"}, titles(steps))
	console := "[    0.1] virtio_blk\r\n" +
		"[    0.2] Run /init as init process\r\n" +
		"Mounting root: ok.\r\n" +
		marker("network") +
		"udhcpc: lease of 192.168.127.2 obtained\r\n" +
		"half a line " + marker("engine") + "\r\n" +
		"docker-setup: done\r\n" +
		marker("ready") +
		"\r\nbox · Alpine Linux\r\nmono@box:~$ "
	got, rest, err := watch(t, console, steps)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1, 2, 3}, got.ended)
	require.Len(t, got.lines[0], 2)
	require.Equal(t, []string{"Mounting root: ok.\r"}, got.lines[1])
	require.Equal(t, []string{"udhcpc: lease of 192.168.127.2 obtained\r", "half a line "}, got.lines[2])
	require.Equal(t, []string{"\r", "docker-setup: done\r"}, got.lines[3])
	require.Equal(t, "\r\nbox · Alpine Linux\r\nmono@box:~$ ", rest, "what follows the last marker is the console's")
}

func TestWatchBootSkipsAhead(t *testing.T) {
	steps := BootSteps(VMConfig{Image: ImageAlpine, GuestLayout: GuestLayout})
	// The kernel line never shows (say, it scrolled out of the replay).
	got, rest, err := watch(t, "init stuff\n"+marker("ready")+"motd", steps)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1, 2}, got.ended, "a later marker ends the steps before it")
	require.Equal(t, "motd", rest)
}

func TestWatchBootKata(t *testing.T) {
	steps := BootSteps(VMConfig{Image: ImageKata})
	console := "[    0.09] Run /sbin/init as init process\r\n" +
		"[\x1b[0;32m  OK  \x1b[0m] Started \x1b[0;1;39mkata-agent.service\x1b[0m\r\n" +
		"[\x1b[0;32m  OK  \x1b[0m] Reached target \x1b[0;1;39mkata-containers.target\x1b[0m - Kata Containers Agent Target.\r\n" +
		"bash: no job control in this shell\r\n"
	got, rest, err := watch(t, console, steps)
	require.NoError(t, err)
	require.Equal(t, []int{0, 1}, got.ended)
	require.Len(t, got.lines[1], 2)
	require.Equal(t, "bash: no job control in this shell\r\n", rest)
}

func TestWatchBootOldDisk(t *testing.T) {
	steps := BootSteps(VMConfig{Image: ImageAlpine, GuestLayout: 0})
	require.Equal(t, []string{"Kernel"}, titles(steps), "no markers on disks from before them")
	_, rest, err := watch(t, "Run /init as init process\nudhcpc: started\n", steps)
	require.NoError(t, err)
	require.Equal(t, "udhcpc: started\n", rest)
}

func TestWatchBootConsoleCloses(t *testing.T) {
	steps := BootSteps(VMConfig{Image: ImageAlpine, GuestLayout: GuestLayout})
	got, _, err := watch(t, "Run /init as init process\nKernel panic\n", steps)
	require.ErrorContains(t, err, "the console closed during system")
	require.Equal(t, []string{"Kernel panic"}, got.lines[1])
}

func TestBootMarkersInInittabs(t *testing.T) {
	for _, path := range []string{"embeddings/etc/inittab.tmpl", "embeddings/docker/etc/inittab.tmpl", "embeddings/containerd/etc/inittab.tmpl"} {
		data, err := embeddings.ReadFile(path)
		require.NoError(t, err)
		image := ImageDocker
		if path == "embeddings/etc/inittab.tmpl" {
			image = ImageAlpine
		}
		for _, s := range BootSteps(VMConfig{Image: image, GuestLayout: GuestLayout}) {
			if s.End == "" {
				continue
			}
			// printf in the guest turns \033 and \007 into the marker's bytes.
			want := fmt.Sprintf(`printf '\033]7777;monolith;%s\007'`, s.End)
			require.Contains(t, string(data), want, "%s: step %s", path, s.Title)
		}
	}
}

func titles(steps []BootStep) []string {
	var out []string
	for _, s := range steps {
		out = append(out, s.Title)
	}
	return out
}
