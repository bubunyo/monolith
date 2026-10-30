package docker

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// errNoDockerCLI is returned when the docker CLI isn't on PATH, so contexts
// can't be managed.
var errNoDockerCLI = errors.New("docker CLI not found on PATH")

// contextName is the docker context for a Docker host VM.
func contextName(vmName string) string {
	return "docker-" + vmName
}

// useContext creates or updates the VM's docker context to point at sock and
// makes it the current context.
func useContext(vmName, sock string) error {
	name := contextName(vmName)
	endpoint := "host=unix://" + sock
	if _, err := dockerCLI("context", "inspect", name); err == nil {
		if _, err := dockerCLI("context", "update", name, "--docker", endpoint); err != nil {
			return err
		}
	} else if errors.Is(err, errNoDockerCLI) {
		return err
	} else {
		desc := fmt.Sprintf("monolith Docker host VM %q", vmName)
		if _, err := dockerCLI("context", "create", name, "--description", desc, "--docker", endpoint); err != nil {
			return err
		}
	}
	_, err := dockerCLI("context", "use", name)
	return err
}

// removeContext deletes the VM's docker context, switching to "default"
// first if it is current (docker refuses to remove the current context).
// A missing context or docker CLI is not an error.
func removeContext(vmName string) error {
	name := contextName(vmName)
	if _, err := dockerCLI("context", "inspect", name); err != nil {
		return nil
	}
	if currentContext() == name {
		if _, err := dockerCLI("context", "use", "default"); err != nil {
			return err
		}
	}
	_, err := dockerCLI("context", "rm", name)
	return err
}

// currentContext returns the docker CLI's current context, or "" if unknown.
func currentContext() string {
	out, err := dockerCLI("context", "show")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// dockerCLI runs the docker CLI and returns its stdout. DOCKER_HOST and
// DOCKER_CONTEXT are dropped so they can't redirect context commands.
func dockerCLI(args ...string) (string, error) {
	path, err := exec.LookPath("docker")
	if err != nil {
		return "", errNoDockerCLI
	}
	cmd := exec.Command(path, args...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "DOCKER_HOST=") && !strings.HasPrefix(kv, "DOCKER_CONTEXT=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("docker %s: %s", strings.Join(args, " "), strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
