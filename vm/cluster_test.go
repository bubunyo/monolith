package vm

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestParseKubeServicePorts(t *testing.T) {
	out := `{"kind":"ServiceList","items":[
 {"spec":{"type":"ClusterIP","ports":[{"protocol":"TCP","port":443}]}},
 {"spec":{"type":"LoadBalancer","ports":[{"protocol":"TCP","port":8080,"nodePort":31000},{"protocol":"UDP","port":53,"nodePort":31001}]}},
 {"spec":{"type":"NodePort","ports":[{"protocol":"TCP","port":80,"nodePort":30080},{"protocol":"SCTP","port":9,"nodePort":30009}]}},
 {"spec":{"type":"LoadBalancer","ports":[{"port":9000}]}}
]}`
	ports, err := parseKubeServicePorts([]byte(out))
	require.NoError(t, err)
	require.Equal(t, map[portKey]bool{
		{"tcp", 8080, ""}: true, {"tcp", 31000, ""}: true, {"udp", 53, ""}: true, {"udp", 31001, ""}: true,
		{"tcp", 30080, ""}: true, {"tcp", 9000, ""}: true,
	}, ports)

	_, err = parseKubeServicePorts([]byte("not json"))
	require.Error(t, err)
}

// Without the kubeconfig k3s writes (k3s still starting), there are no
// service ports to forward, and the forwarder keeps what it has.
func TestClusterServicePortsBeforeK3s(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := &clusterServices{name: "c"}
	_, err := s.servicePorts()
	require.Error(t, err)
}

func TestFormatPorts(t *testing.T) {
	require.Equal(t, "-", FormatPorts(nil))
	require.Equal(t, "80,53/udp,8080!", FormatPorts([]PortForward{
		{Port: 80, Protocol: "tcp"},
		{Port: 53, Protocol: "udp"},
		{Port: 8080, Protocol: "tcp", Error: "address already in use"},
	}))
}

func TestClusterBootSteps(t *testing.T) {
	steps := BootSteps(VMConfig{Image: ImageCluster, GuestLayout: GuestLayout})
	require.Equal(t, []string{"Kernel", "System", "Network", "Cluster"}, titles(steps))
	require.Equal(t, []string{"Kernel"}, titles(BootSteps(VMConfig{Image: ImageCluster})), "no markers without a guest layout")
}

func TestClusterImageIsRegistered(t *testing.T) {
	image, err := LookupImage(ImageCluster)
	require.NoError(t, err)
	require.Equal(t, ImageCluster, image.Name())
	require.NotContains(t, ValidImages, ImageCluster, "`vm run --image` doesn't create clusters")
}

// The overlay's files must all exist, and the guest must write the
// kubeconfig where the host reads it from.
func TestClusterOverlay(t *testing.T) {
	data, err := embeddings.ReadFile("embeddings/" + clusterOverlay)
	require.NoError(t, err)
	var m manifest
	require.NoError(t, yaml.Unmarshal(data, &m))
	require.NotEmpty(t, m.Files)
	for _, f := range m.Files {
		_, err := embeddings.ReadFile("embeddings/" + f.Src)
		require.NoError(t, err, f.Src)
	}

	hostPath, err := VMKubeconfigPath("x")
	require.NoError(t, err)
	kubeDir, err := VMKubeDir("x")
	require.NoError(t, err)
	require.Equal(t, kubeDir, filepath.Dir(hostPath))

	serve, err := embeddings.ReadFile("embeddings/cluster/usr/local/bin/cluster-serve")
	require.NoError(t, err)
	require.Contains(t, string(serve), "KUBECONFIG_OUT=/opt/monolith/out/"+filepath.Base(hostPath))
	require.Contains(t, string(serve), fmt.Sprintf("socat -t 86400 TCP-LISTEN:%d,bind=192.168.127.2,range=192.168.127.1/32", ClusterLoadPort),
		"the load service: gateway only, and not closed 0.5s after the host half-closes")
	inittab, err := embeddings.ReadFile("embeddings/cluster/etc/inittab.tmpl")
	require.NoError(t, err)
	require.Contains(t, string(inittab), "::respawn:/usr/local/bin/cluster-serve load")

	setup, err := embeddings.ReadFile("embeddings/cluster/usr/local/bin/cluster-setup")
	require.NoError(t, err)
	require.Contains(t, string(setup), "virtiofs kube \"$OUT_DIR\"", "the kube share's tag")
	require.Equal(t, "kube", kubeShareTag)
	require.Contains(t, string(setup), "virtiofs -o ro k3s ", "the k3s share's tag")
	require.Equal(t, "k3s", k3sShareTag)
}
