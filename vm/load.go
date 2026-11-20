package vm

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
)

// LoadImage writes an image tarball (`docker save` format) from r to cluster
// name's load socket, where the guest imports it into k3s's containerd, and
// waits for the guest's answer.
func LoadImage(name string, r io.Reader) error {
	path, err := VMLoadSockPath(name)
	if err != nil {
		return err
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		return fmt.Errorf("cluster %q isn't accepting images (is it running?): %w", name, err)
	}
	defer conn.Close()
	if _, err := io.Copy(conn, r); err != nil {
		var opErr *net.OpError
		if errors.As(err, &opErr) {
			// The guest went away mid-stream; its answer may say why.
			if reply, rerr := io.ReadAll(io.LimitReader(conn, 4096)); rerr == nil && len(reply) > 0 {
				return parseLoadReply(string(reply))
			}
			return fmt.Errorf("the cluster closed the connection; its load service may still be starting")
		}
		return err
	}
	if uc, ok := conn.(*net.UnixConn); ok {
		uc.CloseWrite()
	}
	reply, err := io.ReadAll(io.LimitReader(conn, 4096))
	if err != nil {
		return err
	}
	return parseLoadReply(string(reply))
}

// parseLoadReply turns cluster-load's answer ("ok", or "error <reason>")
// into an error.
func parseLoadReply(reply string) error {
	reply = strings.TrimSpace(reply)
	if i := strings.LastIndexByte(reply, '\n'); i >= 0 {
		reply = reply[i+1:]
	}
	switch {
	case reply == "ok":
		return nil
	case strings.HasPrefix(reply, "error"):
		msg := strings.TrimSpace(strings.TrimPrefix(reply, "error"))
		if msg == "" {
			msg = "unknown error"
		}
		return fmt.Errorf("importing into containerd: %s", msg)
	case reply == "":
		return fmt.Errorf("the cluster closed the connection without an answer; its load service may still be starting")
	}
	return fmt.Errorf("unexpected answer from the cluster: %q", reply)
}
