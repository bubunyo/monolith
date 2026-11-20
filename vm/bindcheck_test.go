package vm

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHostSharesByGuestLayout(t *testing.T) {
	old := hostShares(VMConfig{GuestLayout: GuestLayoutTempShare - 1})
	require.Equal(t, []Share{{Tag: usersShareTag, HostPath: "/Users"}}, old)
	require.Equal(t, []string{"/Users"}, sharedRoots(VMConfig{GuestLayout: GuestLayoutTempShare - 1}))

	cur := hostShares(VMConfig{GuestLayout: GuestLayout})
	require.Contains(t, cur, Share{Tag: tmpShareTag, HostPath: "/private/var/folders"})
	require.Equal(t, []string{"/Users", "/var/folders", "/private/var/folders"}, sharedRoots(VMConfig{GuestLayout: GuestLayout}))
}

func TestCheckBindSources(t *testing.T) {
	everything := func(string) bool { return true }
	roots := sharedRoots(VMConfig{GuestLayout: GuestLayout})
	body := func(binds []string, mounts string) []byte {
		b := `{"HostConfig":{"Binds":[` + quoteAll(binds) + `],"Mounts":[` + mounts + `]}}`
		return []byte(b)
	}

	for _, ok := range []string{
		"/Users/me/src:/src",
		"/Users/me/src:/src:ro",
		"/var/folders/xy/abc/T/tmp.1:/data",
		"/private/var/folders/xy/abc/T/tmp.1:/data",
		"/var/run/docker.sock:/var/run/docker.sock",
		"/proc:/host/proc:ro",
		"/etc/hosts:/etc/hosts",
		"myvolume:/data",
	} {
		require.NoError(t, checkBindSources(body([]string{ok}, ""), "docker", roots, everything), ok)
	}

	for _, bad := range []string{"/tmp/x:/x", "/private/tmp/x:/x", "/opt/data:/d", "/Volumes/ext/p:/p", "/Users/../tmp/x:/x"} {
		err := checkBindSources(body([]string{bad}, ""), "docker", roots, everything)
		require.Error(t, err, bad)
		require.Contains(t, err.Error(), "not shared")
		require.Contains(t, err.Error(), strings.SplitN(bad, ":", 2)[0])
	}

	// --mount type=bind is checked; volumes aren't.
	require.Error(t, checkBindSources(body(nil, `{"Type":"bind","Source":"/tmp/x","Target":"/x"}`), "docker", roots, everything))
	require.NoError(t, checkBindSources(body(nil, `{"Type":"volume","Source":"/tmp/x","Target":"/x"}`), "docker", roots, everything))

	// A path that doesn't exist on the Mac is left to the engine (it's the
	// guest's own, or dockerd creates it).
	nothing := func(string) bool { return false }
	require.NoError(t, checkBindSources(body([]string{"/opt/data:/d"}, ""), "docker", roots, nothing))

	// VMs created before the temp share: /var/folders is refused, with a hint.
	err := checkBindSources(body([]string{"/var/folders/xy/abc/T/tmp.1:/data"}, ""), "docker", sharedRoots(VMConfig{GuestLayout: 4}), everything)
	require.Error(t, err)
	require.Contains(t, err.Error(), "recreate")

	// Malformed JSON is left to the engine.
	require.NoError(t, checkBindSources([]byte("{"), "docker", roots, everything))
}

func quoteAll(ss []string) string {
	var q []string
	for _, s := range ss {
		q = append(q, fmt.Sprintf("%q", s))
	}
	return strings.Join(q, ",")
}

// unixPair returns two connected unix sockets (which, unlike net.Pipe, can
// half-close).
func unixPair(t *testing.T) (net.Conn, net.Conn) {
	path := filepath.Join(t.TempDir(), "s")
	l, err := net.Listen("unix", path)
	require.NoError(t, err)
	defer l.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := l.Accept()
		accepted <- c
	}()
	a, err := net.Dial("unix", path)
	require.NoError(t, err)
	return a, <-accepted
}

// TestRelayDockerAPI runs the relay between a client and a fake engine: a
// refused create never reaches the engine and gets a 400, later requests on
// the same connection still go through, and an upgraded attach relays raw
// with stdin EOF reaching the engine.
func TestRelayDockerAPI(t *testing.T) {
	client, relayClient := unixPair(t)
	relayGuest, engine := unixPair(t)
	go relayDockerAPI(relayClient, relayGuest, func(body []byte) error {
		if strings.Contains(string(body), "/tmp/") {
			return fmt.Errorf("bind mount source /tmp/x is not shared")
		}
		return nil
	})

	// The fake engine answers each request with its path; attach echoes
	// stdin until EOF, then says "eof".
	engineSaw := make(chan string, 10)
	go func() {
		br := bufio.NewReader(engine)
		for {
			req, err := http.ReadRequest(br)
			if err != nil {
				return
			}
			body, _ := io.ReadAll(req.Body)
			engineSaw <- req.URL.Path + " " + string(body)
			if strings.HasSuffix(req.URL.Path, "/attach") {
				io.WriteString(engine, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
				data, _ := io.ReadAll(br)
				io.WriteString(engine, string(data)+"eof")
				engine.(*net.UnixConn).CloseWrite()
				return
			}
			fmt.Fprintf(engine, "HTTP/1.1 201 Created\r\nContent-Length: %d\r\n\r\n%s", len(req.URL.Path), req.URL.Path)
		}
	}()

	cr := bufio.NewReader(client)
	send := func(method, path, body string) *http.Response {
		req, err := http.NewRequest(method, "http://docker"+path, strings.NewReader(body))
		require.NoError(t, err)
		require.NoError(t, req.Write(client))
		resp, err := http.ReadResponse(cr, req)
		require.NoError(t, err)
		return resp
	}

	resp := send("POST", "/v1.47/containers/create", `{"HostConfig":{"Binds":["/tmp/x:/x"]}}`)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	msg, _ := io.ReadAll(resp.Body)
	require.Contains(t, string(msg), `"message":"bind mount source /tmp/x is not shared"`)

	resp = send("POST", "/v1.47/containers/create", `{"HostConfig":{"Binds":["/Users/me:/me"]}}`)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	io.ReadAll(resp.Body)
	require.Equal(t, `/v1.47/containers/create {"HostConfig":{"Binds":["/Users/me:/me"]}}`, <-engineSaw)

	resp = send("GET", "/_ping", "")
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	io.ReadAll(resp.Body)
	require.Equal(t, "/_ping ", <-engineSaw)

	req, _ := http.NewRequest("POST", "http://docker/v1.47/containers/abc/attach?stream=1&stdin=1", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	require.NoError(t, req.Write(client))
	resp, err := http.ReadResponse(cr, req)
	require.NoError(t, err)
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	io.WriteString(client, "hello ")
	client.(*net.UnixConn).CloseWrite()
	rest, err := io.ReadAll(cr)
	require.NoError(t, err)
	require.Equal(t, "hello eof", string(rest))
	require.Len(t, engineSaw, 1) // only the attach; the refused create never arrived
}
