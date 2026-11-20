//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/stretchr/testify/require"
)

// TestClusterLoad loads images into a cluster with Rosetta through
// `monolith cluster load`: an amd64-only image (relabelled), multi-arch ones,
// a `docker save` archive, and images from a monolith docker host (one built
// there, one amd64), then runs a pod from each with imagePullPolicy: Never.
func TestClusterLoad(t *testing.T) {
	requireTool(t, "kubectl")
	requireTool(t, "docker")
	const clusterName, host = "e2e-load", "e2e-load-docker"
	env := []string{"KUBECONFIG=" + filepath.Join(t.TempDir(), "config")}
	cluster := func(args ...string) string {
		t.Helper()
		return must(t, env, binary, append([]string{"cluster"}, args...)...)
	}
	kubectl := func(args ...string) (string, int) {
		t.Helper()
		out, code := run(t, env, "kubectl", append([]string{"--context", "cluster-" + clusterName}, args...)...)
		return strings.TrimSpace(out), code
	}
	t.Cleanup(func() {
		run(t, env, binary, "cluster", "rm", "--force", "--name", clusterName)
		run(t, nil, binary, "docker", "rm", "--force", "--name", host)
	})

	cluster("start", "--name", clusterName, "--rosetta")

	// An arm64 image written as `docker save` would.
	archive := filepath.Join(t.TempDir(), "image.tar")
	src, err := name.NewTag("alpine:3.21")
	require.NoError(t, err)
	img, err := remote.Image(src, remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "arm64"}),
		remote.WithAuthFromKeychain(authn.DefaultKeychain))
	require.NoError(t, err)
	local, err := name.NewTag("monolith.test/local:1")
	require.NoError(t, err)
	require.NoError(t, tarball.WriteToFile(archive, local, img))

	// nginx is big enough that unpacking outlasts socat's default 0.5s
	// close after the host half-closes.
	out := cluster("load", "--name", clusterName, "amd64/alpine:3.21", "busybox:1.37", "nginx:1.27-alpine", "-i", archive)
	require.Contains(t, out, "amd64, relabelled for Rosetta")
	require.Contains(t, out, "Loaded 4 of 4 images")
	out = cluster("load", "--name", clusterName, "amd64/alpine:3.21")
	require.Contains(t, out, "Loaded 1 of 1 image", "a second load reads the cached layers")

	// A docker host with an image built there and an amd64 one.
	mustMonolith(t, "docker", "start", "--name", host, "--rosetta")
	sock, err := filepath.Abs(filepath.Join(os.Getenv("HOME"), ".monolith", "vms", host, "docker.sock"))
	require.NoError(t, err)
	denv := []string{"DOCKER_HOST=unix://" + sock}
	ctx := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(ctx, "Dockerfile"),
		[]byte("FROM alpine:3.21\nRUN echo built-on-docker-host > /hi\nCMD [\"cat\", \"/hi\"]\n"), 0644))
	must(t, denv, "docker", "build", "-q", "-t", "myapp:dev", ctx)
	must(t, denv, "docker", "pull", "-q", "--platform", "linux/amd64", "amd64/alpine:3.20")
	out = cluster("load", "--name", clusterName, "--from", "docker:"+host, "myapp:dev", "amd64/alpine:3.20")
	require.Contains(t, out, "Loaded 2 of 2 images")

	require.Eventually(t, func() bool {
		_, code := kubectl("get", "serviceaccount", "default")
		return code == 0
	}, 2*time.Minute, time.Second, "the default service account never appeared")

	for pod, c := range map[string]struct{ image, cmd, want string }{
		"x86":      {"amd64/alpine:3.21", "uname -m", "x86_64"},
		"multi":    {"busybox:1.37", "uname -m", "aarch64"},
		"nginx":    {"nginx:1.27-alpine", "nginx -v", "nginx/1.27"},
		"archive":  {"monolith.test/local:1", "cat /etc/alpine-release", "3.21"},
		"built":    {"myapp:dev", "cat /hi", "built-on-docker-host"},
		"host-x86": {"amd64/alpine:3.20", "uname -m", "x86_64"},
	} {
		o, code := kubectl("run", pod, "--image="+c.image, "--image-pull-policy=Never", "--restart=Never", "--", "sh", "-c", c.cmd)
		require.Zero(t, code, o)
		require.Eventually(t, func() bool {
			phase, _ := kubectl("get", "pod", pod, "-o", "jsonpath={.status.phase}")
			return phase == "Succeeded" || phase == "Failed"
		}, 2*time.Minute, time.Second, "pod %s never finished", pod)
		logs, code := kubectl("logs", pod)
		require.Zero(t, code, logs)
		require.Contains(t, logs, c.want, pod)
	}
}
