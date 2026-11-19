package cluster

import (
	"testing"

	"github.com/stretchr/testify/require"
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
