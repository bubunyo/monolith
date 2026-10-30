package vm

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mnet "github.com/bubunyo/monolith/net"

	"github.com/stretchr/testify/require"
)

// TestKubernetesPauseResume boots a docker image VM with k3s on each engine,
// exposes nginx through a LoadBalancer service, checks it on localhost,
// pauses and resumes the VM, and checks that the same pod is still serving
// and the node is Ready.
//
// It downloads k3s and installs the engine on first boot (needs internet),
// needs kubectl and an unlocked Mac, so it only runs with
// MONOLITH_TEST_K8S=1. It uses a temporary KUBECONFIG and never touches
// ~/.kube/config.
func TestKubernetesPauseResume(t *testing.T) {
	if os.Getenv("MONOLITH_TEST_K8S") != "1" {
		t.Skip("set MONOLITH_TEST_K8S=1 to run (boots Kubernetes VMs)")
	}
	binary := "../bin/monolith"
	if _, err := os.Stat(binary); err != nil {
		t.Skipf("binary not found at %s — run 'make build' first", binary)
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl not found on PATH")
	}

	for i, engine := range ValidEngines {
		t.Run(engine, func(t *testing.T) {
			testKubernetes(t, binary, "test-k8s-"+engine, engine, 18090+i)
		})
	}
}

func testKubernetes(t *testing.T, binary, name, engine string, svcPort int) {
	dir, err := VMDir(name)
	require.NoError(t, err)
	t.Cleanup(func() {
		SendControlCommand(name, ControlRequest{Cmd: CmdStop, Force: true})
		os.RemoveAll(dir)
	})

	kubePort, err := FreeLocalPort()
	require.NoError(t, err)
	cfg := DefaultConfig()
	cfg.Image = ImageDocker
	cfg.Engine = engine
	cfg.NetworkMode = mnet.NetworkUser
	cfg.CPUs = 2
	cfg.Memory = 4 << 30
	cfg.Kubernetes = true
	cfg.KubePort = kubePort
	require.NoError(t, WriteConfig(name, cfg))
	_, err = dockerImage{}.Prepare(name)
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
			resp, err := SendControlCommand(name, ControlRequest{Cmd: CmdKubeconfig})
			if err != nil || !resp.OK {
				return false
			}
			data := strings.Replace(resp.Data, "https://127.0.0.1:6443", fmt.Sprintf("https://127.0.0.1:%d", kubePort), 1)
			require.NoError(t, os.WriteFile(kubeconfig, []byte(data), 0600))
			out, err := kubectl("get", "nodes", "--no-headers")
			return err == nil && strings.Contains(out, " Ready ")
		}, 10*time.Minute, 2*time.Second, "node never became Ready")
	}
	webPod := func() string {
		out, err := kubectl("get", "pod", "-l", "app=web", "-o", "jsonpath={.items[0].metadata.uid}")
		require.NoError(t, err, out)
		return out
	}
	serving := func() bool {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", svcPort))
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}

	spawn()
	waitReady()
	for _, args := range [][]string{
		{"create", "deployment", "web", "--image=nginx:alpine"},
		{"expose", "deployment", "web", "--type=LoadBalancer", fmt.Sprintf("--port=%d", svcPort), "--target-port=80"},
		{"rollout", "status", "deployment/web", "--timeout=5m"},
	} {
		out, err := kubectl(args...)
		require.NoError(t, err, out)
	}
	require.Eventually(t, serving, 2*time.Minute, 2*time.Second, "LoadBalancer service not reachable on localhost")
	before := webPod()

	resp, err := SendControlCommand(name, ControlRequest{Cmd: CmdSuspend})
	require.NoError(t, err)
	require.True(t, resp.OK, resp.Error)

	spawn("--restore")
	waitReady()
	require.Equal(t, before, webPod(), "the pod was replaced across pause/resume")
	require.Eventually(t, serving, time.Minute, time.Second, "service not reachable after resume")
}
