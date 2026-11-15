package vm

import (
	"context"
	"fmt"
	"net"
	"strconv"

	"github.com/bubunyo/monolith"
	mnet "github.com/bubunyo/monolith/net"
)

// clusterServices forwards a cluster VM's Kubernetes API to localhost: every
// connection to 127.0.0.1:<kube_port> is relayed to the k3s API server in the
// guest through the user-mode network. Connections fail while k3s is still
// starting (nothing is listening yet).
type clusterServices struct {
	user     *mnet.UserNetwork
	listener net.Listener
	cancel   context.CancelFunc
}

// startClusterServices starts the API forward for cluster VM name.
func startClusterServices(name string, cfg VMConfig, user *mnet.UserNetwork, logger *monolith.Logger) (*clusterServices, error) {
	if user == nil {
		return nil, fmt.Errorf("the cluster image requires user-mode networking")
	}
	if cfg.KubePort == 0 {
		return nil, fmt.Errorf("no kube_port is configured")
	}
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.KubePort)))
	if err != nil {
		return nil, fmt.Errorf("forwarding the Kubernetes API on localhost:%d: %w", cfg.KubePort, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &clusterServices{user: user, listener: l, cancel: cancel}
	go s.serve(ctx)
	logger.Info("Kubernetes API for %q on localhost:%d", name, cfg.KubePort)
	return s, nil
}

func (s *clusterServices) serve(ctx context.Context) {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return // listener closed
		}
		go func() {
			dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
			defer cancel()
			guest, err := s.user.DialGuest(dialCtx, KubeAPIPort)
			if err != nil {
				conn.Close()
				return
			}
			relay(conn, guest)
		}()
	}
}

func (s *clusterServices) close() {
	s.cancel()
	s.listener.Close()
}
