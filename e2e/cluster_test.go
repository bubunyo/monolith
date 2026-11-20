//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// userKubeconfig stands in for a kubeconfig with a context of the user's own.
const userKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: mine
  cluster:
    server: https://203.0.113.1
users:
- name: mine
  user:
    token: x
contexts:
- name: mine
  context:
    cluster: mine
    user: mine
current-context: mine
`

// TestCluster drives a k3s cluster through `monolith cluster`: start, a
// LoadBalancer service on localhost, use, pause/resume and rm.
func TestCluster(t *testing.T) {
	requireTool(t, "kubectl")
	const name = "e2e-k8s"
	kc := filepath.Join(t.TempDir(), "config")
	require.NoError(t, os.WriteFile(kc, []byte(userKubeconfig), 0600))
	env := []string{"KUBECONFIG=" + kc}
	cluster := func(args ...string) string {
		t.Helper()
		return must(t, env, binary, append([]string{"cluster"}, args...)...)
	}
	kubectl := func(args ...string) string {
		t.Helper()
		return strings.TrimSpace(must(t, env, "kubectl", args...))
	}
	t.Cleanup(func() { run(t, env, binary, "cluster", "rm", "--force", "--name", name) })

	cluster("start", "--name", name)
	require.Equal(t, "cluster-"+name, kubectl("config", "current-context"))

	port := freePort(t)
	kubectl("create", "deployment", "web", "--image=nginx:alpine")
	kubectl("expose", "deployment", "web", "--type=LoadBalancer", "--port="+strconv.Itoa(port), "--target-port=80")
	kubectl("rollout", "status", "deployment/web", "--timeout=5m")
	waitServes(t, port, "nginx", 3*time.Minute)
	require.Regexp(t, name+`\s.*`+strconv.Itoa(port), cluster("list"))

	kubectl("config", "use-context", "mine")
	cluster("use", name)
	require.Equal(t, "cluster-"+name, kubectl("config", "current-context"))
	require.Equal(t, name, strings.TrimSpace(cluster("use")))

	pod := func() string {
		return kubectl("get", "pod", "-l", "app=web", "-o", "jsonpath={.items[0].metadata.uid}")
	}
	before := pod()
	cluster("pause", "--name", name)
	cluster("resume", "--name", name)
	require.Equal(t, before, pod(), "the pod carried on across pause/resume")
	waitServes(t, port, "nginx", 3*time.Minute)

	cluster("rm", "--force", "--name", name)
	require.Equal(t, "mine", kubectl("config", "current-context"), "rm brings the previous context back")
	require.NotContains(t, kubectl("config", "get-contexts", "-o", "name"), "cluster-"+name)
}
