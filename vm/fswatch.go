package vm

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bubunyo/monolith"

	"github.com/fsnotify/fsevents"
)

const (
	// FileEventsPort is the guest TCP port (gateway only) that takes one
	// path per line and touches it, raising inotify events in the guest.
	FileEventsPort = 2379

	// fileEventsLatency is how long FSEvents coalesces before delivering.
	fileEventsLatency = 100 * time.Millisecond

	// echoWindow suppresses a path's events for this long after it was
	// forwarded: the guest's touch shows up on the Mac as a metadata change
	// and must not be sent back.
	echoWindow = 2 * time.Second
)

// forwardedFlags are the FSEvents changes worth telling the guest about.
// Metadata-only changes are left out, which is also what keeps the guest's
// own touches from echoing back.
const forwardedFlags = fsevents.ItemCreated | fsevents.ItemModified | fsevents.ItemRenamed

// relayFileEvents watches root (the Mac side of the /Users share) with
// FSEvents and sends changed paths to the guest's fsnotify relay.
// virtio-fs doesn't deliver host-side changes as inotify events, so without
// this, watchers in containers (hot reload, test runners) never fire for
// edits made on the Mac. It runs until ctx is done; errors are logged once
// and retried, since older guests have no relay.
func (s *dockerServices) relayFileEvents(ctx context.Context, root string) {
	skip := skippedPrefixes()
	stream := &fsevents.EventStream{
		Paths:   []string{root},
		Latency: fileEventsLatency,
		Flags:   fsevents.FileEvents | fsevents.WatchRoot,
	}
	if err := stream.Start(); err != nil {
		s.logger.Warn("watching %s for file changes: %v", root, err)
		return
	}
	defer stream.Stop()

	var conn net.Conn
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()
	recent := map[string]time.Time{}
	warned := false

	for {
		select {
		case <-ctx.Done():
			return
		case batch := <-stream.Events:
			now := time.Now()
			var lines strings.Builder
			for _, ev := range batch {
				path := ev.Path
				if !strings.HasPrefix(path, "/") {
					path = "/" + path
				}
				if ev.Flags&forwardedFlags == 0 || ev.Flags&fsevents.ItemRemoved != 0 || skipped(path, skip) || strings.ContainsRune(path, '\n') {
					continue
				}
				if t, ok := recent[path]; ok && now.Sub(t) < echoWindow {
					continue
				}
				recent[path] = now
				lines.WriteString(path)
				lines.WriteByte('\n')
			}
			for path, t := range recent {
				if now.Sub(t) >= echoWindow {
					delete(recent, path)
				}
			}
			if lines.Len() == 0 {
				continue
			}
			// One persistent connection; reconnect after a failure (the
			// relay restarts with the guest).
			for attempt := 0; attempt < 2; attempt++ {
				if conn == nil {
					c, err := s.dialGuest(ctx, FileEventsPort)
					if err != nil {
						if !warned {
							s.logger.Warn("file-change relay unavailable (guest created before it existed?): %v", err)
							warned = true
						}
						break
					}
					conn = c
				}
				conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
				if _, err := conn.Write([]byte(lines.String())); err == nil {
					break
				}
				conn.Close()
				conn = nil
			}
		}
	}
}

// skippedPrefixes are busy trees under /Users nobody bind-mounts: monolith's
// own cache (VM disks change constantly) and ~/Library.
func skippedPrefixes() []string {
	var out []string
	if dir, err := monolith.CachePath(); err == nil {
		out = append(out, dir+string(filepath.Separator))
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, "Library")+string(filepath.Separator))
	}
	return out
}

func skipped(path string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}
