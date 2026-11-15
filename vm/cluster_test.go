package vm

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

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

	setup, err := embeddings.ReadFile("embeddings/cluster/usr/local/bin/cluster-setup")
	require.NoError(t, err)
	require.Contains(t, string(setup), "virtiofs kube \"$OUT_DIR\"", "the kube share's tag")
	require.Equal(t, "kube", kubeShareTag)
	require.Contains(t, string(setup), "virtiofs -o ro k3s ", "the k3s share's tag")
	require.Equal(t, "k3s", k3sShareTag)
}
