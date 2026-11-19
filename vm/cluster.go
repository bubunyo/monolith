package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bubunyo/monolith"
	"github.com/bubunyo/monolith/internal/kubeconfig"
	mnet "github.com/bubunyo/monolith/net"
)

// clusterServices exposes a cluster VM on localhost through the user-mode
// network: 127.0.0.1:<kube_port> relays to the k3s API server in the guest,
// and every LoadBalancer and NodePort service gets a forward on its port, as
// a docker host does for published container ports. Connections fail while
// k3s is still starting (nothing is listening yet).
type clusterServices struct {
	name     string
	user     *mnet.UserNetwork
	listener net.Listener
	cancel   context.CancelFunc
	fwd      *portForwarder

	mu      sync.Mutex
	client  *http.Client // k3s API client, rebuilt when the kubeconfig changes
	kcMtime time.Time
}

// startClusterServices starts the API forward and the service forwarder for
// cluster VM name.
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
	s := &clusterServices{name: name, user: user, listener: l, cancel: cancel}
	s.fwd = newPortForwarder(user, logger, s.servicePorts)
	go serveGuest(ctx, user, l, KubeAPIPort)
	go s.fwd.run(ctx)
	logger.Info("Kubernetes API for %q on localhost:%d", name, cfg.KubePort)
	return s, nil
}

// forwards lists the service ports and their forwarding errors.
func (s *clusterServices) forwards() []PortForward {
	return s.fwd.forwards()
}

// apiClient returns a client for the guest's k3s API, authenticated with the
// kubeconfig k3s wrote into the kube share. It is rebuilt when that file
// changes (each boot rewrites it).
func (s *clusterServices) apiClient() (*http.Client, error) {
	path, err := VMKubeconfigPath(s.name)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err // k3s hasn't written it yet
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil && fi.ModTime().Equal(s.kcMtime) {
		return s.client, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	kc, err := kubeconfig.Parse(data, s.name, KubeAPIPort)
	if err != nil {
		return nil, err
	}
	tlsCfg, err := kc.TLSConfig()
	if err != nil {
		return nil, err
	}
	s.client = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: tlsCfg,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialGuest(ctx, s.user, KubeAPIPort)
			},
		},
	}
	s.kcMtime = fi.ModTime()
	return s.client, nil
}

// servicePorts returns the ports the cluster's services expose on the node.
func (s *clusterServices) servicePorts() (map[portKey]bool, error) {
	client, err := s.apiClient()
	if err != nil {
		return nil, err
	}
	// The URL's host only names the certificate to verify (k3s's serving
	// certificate covers 127.0.0.1); the dialer goes to the guest.
	resp, err := client.Get(fmt.Sprintf("https://127.0.0.1:%d/api/v1/services", KubeAPIPort))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing services: %s", resp.Status)
	}
	var data json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, err
	}
	return parseKubeServicePorts(data)
}

// parseKubeServicePorts returns the ports a ServiceList exposes on the node:
// LoadBalancer ports (served by k3s's servicelb on the node) and NodePorts.
func parseKubeServicePorts(data []byte) (map[portKey]bool, error) {
	var list struct {
		Items []struct {
			Spec struct {
				Type  string `json:"type"`
				Ports []struct {
					Protocol string `json:"protocol"`
					Port     int    `json:"port"`
					NodePort int    `json:"nodePort"`
				} `json:"ports"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("services: %w", err)
	}
	ports := map[portKey]bool{}
	for _, svc := range list.Items {
		for _, p := range svc.Spec.Ports {
			proto := strings.ToLower(p.Protocol)
			if proto == "" {
				proto = "tcp"
			}
			if proto != "tcp" && proto != "udp" {
				continue
			}
			switch svc.Spec.Type {
			case "LoadBalancer":
				ports[portKey{proto, p.Port, ""}] = true
				if p.NodePort > 0 {
					ports[portKey{proto, p.NodePort, ""}] = true
				}
			case "NodePort":
				if p.NodePort > 0 {
					ports[portKey{proto, p.NodePort, ""}] = true
				}
			}
		}
	}
	return ports, nil
}

func (s *clusterServices) close() {
	s.cancel()
	s.listener.Close()
	s.fwd.stop()
}
