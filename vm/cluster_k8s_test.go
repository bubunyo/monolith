package vm

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	mnet "github.com/bubunyo/monolith/net"

	"github.com/stretchr/testify/require"
)

// TestClusterPauseResume boots a cluster VM, runs nginx on it behind a
// LoadBalancer service, pauses and resumes the VM, and checks that the same
// pod is still running, the node is Ready, and the service answers on
// localhost both times.
//
// It downloads k3s and installs packages on first boot (needs internet),
// needs kubectl and an unlocked Mac, so it only runs with
// MONOLITH_TEST_K8S=1. It uses a temporary KUBECONFIG and never touches
// ~/.kube/config.
func TestClusterPauseResume(t *testing.T) {
	if os.Getenv("MONOLITH_TEST_K8S") != "1" {
		t.Skip("set MONOLITH_TEST_K8S=1 to run (boots a Kubernetes VM)")
	}
	binary := "../bin/monolith"
	if _, err := os.Stat(binary); err != nil {
		t.Skipf("binary not found at %s — run 'make build' first", binary)
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl not found on PATH")
	}

	const name = "test-cluster"
	dir, err := VMDir(name)
	require.NoError(t, err)
	t.Cleanup(func() {
		SendControlCommand(name, ControlRequest{Cmd: CmdStop, Force: true})
		os.RemoveAll(dir)
	})

	kubePort, err := FreeLocalPort()
	require.NoError(t, err)
	cfg := DefaultConfig()
	cfg.Image = ImageCluster
	cfg.NetworkMode = mnet.NetworkUser
	cfg.CPUs = 2
	cfg.Memory = 4 << 30
	cfg.KubePort = kubePort
	require.NoError(t, WriteConfig(name, cfg))
	_, err = clusterImage{}.Prepare(name, io.Discard)
	require.NoError(t, err)
	hostKubeconfig, err := VMKubeconfigPath(name)
	require.NoError(t, err)

	spawn := func(args ...string) {
		daemon := exec.Command(binary, append([]string{"__vmd", name}, args...)...)
		require.NoError(t, daemon.Start())
		go daemon.Wait()
	}

	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	kubectl := func(args ...string) (string, error) {
		cmd := exec.Command("kubectl", args...)
		cmd.Env = append(os.Environ(), "KUBECONFIG="+kubeconfig)
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	waitReady := func() {
		require.Eventually(t, func() bool {
			data, err := os.ReadFile(hostKubeconfig)
			if err != nil {
				return false
			}
			// k3s writes the server as the guest sees it.
			data = []byte(strings.Replace(string(data), "https://127.0.0.1:6443", "https://127.0.0.1:"+strconv.Itoa(kubePort), 1))
			require.NoError(t, os.WriteFile(kubeconfig, data, 0600))
			out, err := kubectl("get", "nodes", "--no-headers")
			return err == nil && strings.Contains(out, " Ready ")
		}, 10*time.Minute, 2*time.Second, "node never became Ready")
	}
	webPod := func() string {
		out, err := kubectl("get", "pod", "-l", "app=web", "-o", "jsonpath={.items[0].metadata.uid}")
		require.NoError(t, err, out)
		return out
	}

	// The LoadBalancer service is forwarded to this port on localhost.
	lbPort, err := FreeLocalPort()
	require.NoError(t, err)
	serviceAnswers := func() {
		url := "http://127.0.0.1:" + strconv.Itoa(lbPort) + "/"
		require.Eventually(t, func() bool {
			resp, err := http.Get(url)
			if err != nil {
				return false
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return resp.StatusCode == http.StatusOK && strings.Contains(string(body), "nginx")
		}, 3*time.Minute, time.Second, "the LoadBalancer service never answered on %s", url)
		resp, err := SendControlCommand(name, ControlRequest{Cmd: CmdStatus})
		require.NoError(t, err)
		require.Contains(t, resp.Ports, PortForward{Port: lbPort, Protocol: "tcp"}, "status lists the forward")
	}

	spawn()
	waitReady()
	for _, args := range [][]string{
		{"create", "deployment", "web", "--image=nginx:alpine"},
		{"expose", "deployment", "web", "--type=LoadBalancer", "--port=" + strconv.Itoa(lbPort), "--target-port=80"},
		{"rollout", "status", "deployment/web", "--timeout=5m"},
	} {
		out, err := kubectl(args...)
		require.NoError(t, err, out)
	}
	before := webPod()
	serviceAnswers()

	resp, err := SendControlCommand(name, ControlRequest{Cmd: CmdSuspend})
	require.NoError(t, err)
	require.True(t, resp.OK, resp.Error)

	spawn("--restore")
	waitReady()
	require.Equal(t, before, webPod(), "the pod was replaced across pause/resume")
	serviceAnswers()
}
