package cluster

import (
	"os"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/bubunyo/monolith/vm"
)

func TestUseRejectsBadName(t *testing.T) {
	require.Error(t, use("Bad_Name"))
}

func TestUseRejectsMissingCluster(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	require.ErrorContains(t, use("no-such-cluster"), "does not exist")
}

func TestClusterOfContextIgnoresOtherContexts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	require.Equal(t, "", clusterOfContext("gke-prod"))
	require.Equal(t, "", clusterOfContext("cluster-gone"), "a context whose VM is gone isn't a cluster")
}

// startCommand parses args as cluster start's flags into a fresh command,
// which also resets the flag variables to their defaults.
func startCommand(t *testing.T, name string, args ...string) *cobra.Command {
	t.Helper()
	c := &cobra.Command{}
	addStartFlags(c)
	require.NoError(t, c.ParseFlags(args))
	old := flagName
	flagName = name
	t.Cleanup(func() { flagName = old })
	return c
}

func writeCluster(t *testing.T, name string, rosetta bool) {
	t.Helper()
	cfg := vm.DefaultConfig()
	cfg.Image = vm.ImageCluster
	cfg.Rosetta = rosetta
	require.NoError(t, vm.WriteConfig(name, cfg))
}

func TestStartConfigRosetta(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cfg, err := startConfig(startCommand(t, "new-cluster"))
	require.NoError(t, err)
	require.False(t, cfg.Rosetta, "off by default")

	cfg, err = startConfig(startCommand(t, "new-cluster", "--rosetta"))
	require.NoError(t, err)
	require.True(t, cfg.Rosetta)

	writeCluster(t, "saved", true)
	cfg, err = startConfig(startCommand(t, "saved"))
	require.NoError(t, err)
	require.True(t, cfg.Rosetta, "kept from config.json without the flag")

	cfg, err = startConfig(startCommand(t, "saved", "--rosetta=false"))
	require.NoError(t, err)
	require.False(t, cfg.Rosetta)
}

func TestStartRefusesRosettaChangeWhileRunning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	writeCluster(t, "busy", false)
	require.NoError(t, vm.WriteState("busy", os.Getpid(), "", vm.StatusRunning))

	err := start(startCommand(t, "busy", "--rosetta"))
	require.ErrorContains(t, err, "stop it to change --rosetta")
}
