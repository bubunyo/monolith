package container

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	clivm "github.com/bubunyo/monolith/cli/vm"
	"github.com/stretchr/testify/require"
)

func TestGenerateName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for ref, prefix := range map[string]string{
		"nginx:1.29":                    "nginx",
		"ghcr.io/org/My_App@sha256:abc": "my-app",
		"docker.io/library/alpine":      "alpine",
		"___":                           "c",
	} {
		name := generateName(ref)
		require.Regexp(t, regexp.MustCompile("^"+prefix+"-[0-9a-f]{4}$"), name, ref)
		require.NoError(t, clivm.ValidateName(name), ref)
	}
}

func TestSplitRef(t *testing.T) {
	for ref, want := range map[string][2]string{
		"index.docker.io/library/nginx:1.29": {"nginx", "1.29"},
		"localhost:5000/app:dev":             {"localhost:5000/app", "dev"},
		"localhost:5000/app":                 {"localhost:5000/app", "latest"},
		"ghcr.io/o/r@sha256:abc":             {"ghcr.io/o/r", "<none>"},
	} {
		repo, tag := splitRef(ref)
		require.Equal(t, want, [2]string{repo, tag}, ref)
	}
}

func TestUserEnv(t *testing.T) {
	file := filepath.Join(t.TempDir(), "env")
	require.NoError(t, os.WriteFile(file, []byte("# comment\nFROM_FILE=1\n\nCOPIED\n"), 0644))
	t.Setenv("COPIED", "from-mac")
	t.Setenv("ALSO", "yes")
	flagEnvFile = []string{file}
	flagEnv = []string{"A=b=c", "ALSO", "UNSET_ON_MAC"}
	defer func() { flagEnvFile, flagEnv = nil, nil }()
	env, err := userEnv()
	require.NoError(t, err)
	require.Equal(t, []string{"FROM_FILE=1", "COPIED=from-mac", "A=b=c", "ALSO=yes"}, env)
}

func TestAgo(t *testing.T) {
	require.Equal(t, "1 second", ago(time.Now().Add(-time.Second-time.Millisecond)))
	require.Equal(t, "5 minutes", ago(time.Now().Add(-5*time.Minute-time.Second)))
	require.Equal(t, "3 days", ago(time.Now().Add(-73*time.Hour)))
	require.Equal(t, `"nginx -g daemon off;"`, quoteCommand([]string{"nginx", "-g", "daemon off;"}))
}
