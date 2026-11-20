package vm

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

const (
	// GuestLayoutTempShare is the first guest layout that mounts the Mac's
	// /var/folders.
	GuestLayoutTempShare = 5

	usersShareTag = "users"
	// macOS keeps per-user temp dirs ($TMPDIR, `mktemp -d`) under
	// /var/folders, which is /private/var/folders on disk. Docker Desktop,
	// Colima and OrbStack share it too.
	tmpShareTag      = "varfolders"
	tmpShareHostPath = "/private/var/folders"
)

// hostShares returns the Mac directories a docker host (not a sandbox)
// shares into the guest at the same path.
func hostShares(cfg VMConfig) []Share {
	shares := []Share{{Tag: usersShareTag, HostPath: "/Users"}}
	if cfg.GuestLayout >= GuestLayoutTempShare {
		shares = append(shares, Share{Tag: tmpShareTag, HostPath: tmpShareHostPath})
	}
	return shares
}

// sharedRoots returns the Mac paths a docker host's guest sees at the same
// path, in both spellings where macOS has a /private symlink.
func sharedRoots(cfg VMConfig) []string {
	roots := []string{"/Users"}
	if cfg.GuestLayout >= GuestLayoutTempShare {
		roots = append(roots, "/var/folders", tmpShareHostPath)
	}
	return roots
}

// guestPathPrefixes are bind sources that mean the guest's own files
// (`-v /var/run/docker.sock:…`, `-v /lib/modules:…`). Many also exist on the
// Mac, so they must not be mistaken for unshared Mac paths.
var guestPathPrefixes = []string{
	"/proc", "/sys", "/dev", "/run", "/var/run", "/var/lib/docker",
	"/lib/modules", "/etc", "/mnt/rosetta",
}

func underPath(p, root string) bool {
	return p == root || strings.HasPrefix(p, root+"/")
}

// unsharedBindSource reports whether src names a Mac path the guest can't
// see: it exists on the Mac (exists), isn't under a shared root and isn't
// one of the guest's own paths. Docker would mount an empty directory the
// guest creates instead, which is what makes the mistake so confusing.
func unsharedBindSource(src string, roots []string, exists func(string) bool) bool {
	if !filepath.IsAbs(src) {
		return false // a named volume
	}
	p := filepath.Clean(src)
	for _, r := range roots {
		if underPath(p, r) {
			return false
		}
	}
	for _, g := range guestPathPrefixes {
		if underPath(p, g) {
			return false
		}
	}
	return exists(p)
}

// containerCreateBody is the part of POST /containers/create that names
// bind mount sources.
type containerCreateBody struct {
	HostConfig struct {
		Binds  []string
		Mounts []struct {
			Type   string
			Source string
		}
	}
}

// checkBindSources returns an error naming the first bind source in a
// container create request that the guest can't see.
func checkBindSources(body []byte, vmName string, roots []string, exists func(string) bool) error {
	var req containerCreateBody
	if err := json.Unmarshal(body, &req); err != nil {
		return nil // let the engine report it
	}
	var sources []string
	for _, b := range req.HostConfig.Binds {
		sources = append(sources, strings.SplitN(b, ":", 2)[0])
	}
	for _, m := range req.HostConfig.Mounts {
		if m.Type == "bind" {
			sources = append(sources, m.Source)
		}
	}
	for _, src := range sources {
		if !unsharedBindSource(src, roots, exists) {
			continue
		}
		msg := fmt.Sprintf("bind mount source %s is not shared with monolith's docker host %q, so the container would see an empty directory; shared paths: %s",
			src, vmName, strings.Join(roots, ", "))
		if underPath(filepath.Clean(src), "/var/folders") || underPath(filepath.Clean(src), tmpShareHostPath) {
			msg += " (this VM was created before /var/folders was shared; recreate it to share temp dirs)"
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

var (
	containerCreatePath = regexp.MustCompile(`^(/v[0-9.]+)?/containers/create$`)
	// hijackPath matches endpoints whose connection turns into a raw stream
	// even when the client sends no Upgrade header.
	hijackPath = regexp.MustCompile(`^(/v[0-9.]+)?/(containers/[^/]+/attach|exec/[^/]+/start|session|grpc)$`)
)

// relayDockerAPI relays one docker API connection like relay, but reads the
// client's requests so it can refuse a container create whose bind mounts
// the guest can't see (check returns the reason) with a 400, as dockerd
// does for a bad mount. Responses are copied untouched. Once a request
// upgrades the connection (attach, exec), the rest is relayed raw, with the
// same half-close handling as relay so stdin EOF reaches the container.
func relayDockerAPI(client, guest net.Conn, check func(body []byte) error) {
	out := &lockedWriter{w: client}
	done := make(chan struct{})
	go func() {
		io.Copy(out, guest)
		closeWrite(client)
		close(done)
	}()

	br := bufio.NewReader(client)
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			break // client done (or not speaking HTTP)
		}
		if req.Method == http.MethodPost && containerCreatePath.MatchString(req.URL.Path) {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				break
			}
			if reason := check(body); reason != nil {
				out.Write(errorResponse(http.StatusBadRequest, reason.Error()))
				if req.Close {
					break
				}
				continue
			}
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
			req.TransferEncoding = nil
		}
		if err := req.Write(guest); err != nil {
			break
		}
		if req.Header.Get("Upgrade") != "" || hijackPath.MatchString(req.URL.Path) {
			io.Copy(guest, br)
			break
		}
		if req.Close {
			break
		}
	}
	closeWrite(guest)
	<-done
	client.Close()
	guest.Close()
}

// errorResponse is a docker API error response ({"message": …}, which the
// CLI prints as "Error response from daemon: …").
func errorResponse(status int, message string) []byte {
	body, _ := json.Marshal(map[string]string{"message": message})
	return []byte(fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		status, http.StatusText(status), len(body), body))
}

// lockedWriter serializes writes from the guest relay and locally written
// error responses.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
