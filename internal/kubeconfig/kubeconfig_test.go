package kubeconfig

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

const k3sConfig = `apiVersion: v1
kind: Config
clusters:
- name: default
  cluster:
    certificate-authority-data: Q0E=
    server: https://127.0.0.1:6443
users:
- name: default
  user:
    client-certificate-data: Q0VSVA==
    client-key-data: S0VZ
contexts:
- name: default
  context:
    cluster: default
    user: default
current-context: default
`

// userConfig is a kubeconfig with a context of the user's own, and fields
// monolith doesn't model.
const userConfig = `apiVersion: v1
kind: Config
preferences:
  colors: true
clusters:
- name: gke-prod
  cluster:
    server: https://203.0.113.10
    insecure-skip-tls-verify: true
users:
- name: gke-prod
  user:
    exec:
      command: gke-gcloud-auth-plugin
contexts:
- name: gke-prod
  context:
    cluster: gke-prod
    user: gke-prod
    namespace: api
current-context: gke-prod
`

func setup(t *testing.T, content string) (path, prev string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "config")
	if content != "" {
		require.NoError(t, os.WriteFile(path, []byte(content), 0600))
	}
	t.Setenv("KUBECONFIG", path)
	return path, filepath.Join(dir, "prev-context")
}

func load(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	doc := map[string]any{}
	require.NoError(t, yaml.Unmarshal(data, &doc))
	return doc
}

func names(doc map[string]any, key string) []string {
	var out []string
	list, _ := doc[key].([]any)
	for _, item := range list {
		out = append(out, item.(map[string]any)["name"].(string))
	}
	return out
}

func parse(t *testing.T, vmName string) *Config {
	t.Helper()
	c, err := Parse([]byte(k3sConfig), vmName, 6550)
	require.NoError(t, err)
	return c
}

func TestContextName(t *testing.T) {
	require.Equal(t, "cluster-dev", ContextName("dev"))
	require.Equal(t, "cluster-default", ContextName("cluster-default"), "no doubled prefix")
	require.Equal(t, "cluster-prod", ContextName("prod"), "never a bare name that could be a user's own context")
}

func TestParseRewritesForTheMac(t *testing.T) {
	c := parse(t, "dev")
	require.Equal(t, "cluster-dev", c.Clusters[0].Name)
	require.Equal(t, "cluster-dev", c.Users[0].Name)
	require.Equal(t, "cluster-dev", c.Contexts[0].Name)
	require.Equal(t, "cluster-dev", c.Contexts[0].Context.Cluster)
	require.Equal(t, "cluster-dev", c.Contexts[0].Context.User)
	require.Equal(t, "cluster-dev", c.CurrentContext)
	require.Equal(t, "https://127.0.0.1:6550", c.Clusters[0].Cluster.Server)
	require.Equal(t, "Q0E=", c.Clusters[0].Cluster.CertificateAuthorityData)

	out, err := c.Marshal()
	require.NoError(t, err)
	again, err := Parse(out, "dev", 6550)
	require.NoError(t, err)
	require.Equal(t, c, again)
}

func TestParseRejectsOtherShapes(t *testing.T) {
	_, err := Parse([]byte("apiVersion: v1\nkind: Config\n"), "dev", 1)
	require.ErrorContains(t, err, "unexpected k3s kubeconfig")
	_, err = Parse([]byte("{not yaml"), "dev", 1)
	require.Error(t, err)
}

func TestMergeKeepsTheUsersOwnEntries(t *testing.T) {
	path, prev := setup(t, userConfig)
	require.NoError(t, Merge(parse(t, "dev"), prev))

	doc := load(t, path)
	require.Equal(t, []string{"gke-prod", "cluster-dev"}, names(doc, "clusters"))
	require.Equal(t, []string{"gke-prod", "cluster-dev"}, names(doc, "users"))
	require.Equal(t, []string{"gke-prod", "cluster-dev"}, names(doc, "contexts"))
	require.Equal(t, "cluster-dev", doc["current-context"])
	require.Equal(t, map[string]any{"colors": true}, doc["preferences"], "fields monolith doesn't know are kept")
	gke := doc["contexts"].([]any)[0].(map[string]any)["context"].(map[string]any)
	require.Equal(t, "api", gke["namespace"])
}

func TestMergeIsIdempotent(t *testing.T) {
	path, prev := setup(t, userConfig)
	require.NoError(t, Merge(parse(t, "dev"), prev))
	once := load(t, path)
	require.NoError(t, Merge(parse(t, "dev"), prev))
	require.Equal(t, once, load(t, path))
}

// A second start must not replace the backup of the user's file with the
// already-merged one.
func TestMergeKeepsTheFirstBackup(t *testing.T) {
	path, prev := setup(t, userConfig)
	require.NoError(t, Merge(parse(t, "dev"), prev))
	bak, err := os.ReadFile(path + ".monolith.bak")
	require.NoError(t, err)
	require.Equal(t, userConfig, string(bak))

	require.NoError(t, Merge(parse(t, "dev"), prev))
	require.NoError(t, Merge(parse(t, "dev"), prev))
	bak, err = os.ReadFile(path + ".monolith.bak")
	require.NoError(t, err)
	require.Equal(t, userConfig, string(bak), "the backup is still the file as it was before monolith")
}

func TestMergeIntoMissingFile(t *testing.T) {
	path, prev := setup(t, "")
	require.NoError(t, Merge(parse(t, "dev"), prev))
	doc := load(t, path)
	require.Equal(t, "v1", doc["apiVersion"])
	require.Equal(t, "Config", doc["kind"])
	require.Equal(t, []string{"cluster-dev"}, names(doc, "contexts"))
	require.NoFileExists(t, path+".monolith.bak")
	require.NoFileExists(t, prev, "there was no previous context to note")
}

func TestRemoveRestoresThePreviousContext(t *testing.T) {
	path, prev := setup(t, userConfig)
	require.NoError(t, Merge(parse(t, "dev"), prev))
	require.NoError(t, Remove("dev", prev))

	doc := load(t, path)
	require.Equal(t, []string{"gke-prod"}, names(doc, "clusters"))
	require.Equal(t, []string{"gke-prod"}, names(doc, "users"))
	require.Equal(t, []string{"gke-prod"}, names(doc, "contexts"))
	require.Equal(t, "gke-prod", doc["current-context"], "back to the context that was current before")
	require.NoFileExists(t, prev)
}

func TestRemoveKeepsAnotherCurrentContext(t *testing.T) {
	path, prev := setup(t, userConfig)
	require.NoError(t, Merge(parse(t, "dev"), prev))
	// The user switched elsewhere afterwards.
	doc := load(t, path)
	doc["current-context"] = "gke-prod"
	out, err := yaml.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, out, 0600))

	require.NoError(t, Remove("dev", prev))
	require.Equal(t, "gke-prod", load(t, path)["current-context"])
}

func TestRemoveWithoutAPreviousContextClearsIt(t *testing.T) {
	path, prev := setup(t, "")
	require.NoError(t, Merge(parse(t, "dev"), prev))
	require.NoError(t, Remove("dev", prev))
	require.Equal(t, "", load(t, path)["current-context"])
}

func TestRemoveDoesNotRestoreAContextThatIsGone(t *testing.T) {
	path, prev := setup(t, userConfig)
	require.NoError(t, Merge(parse(t, "dev"), prev))
	doc := load(t, path)
	doc["contexts"] = doc["contexts"].([]any)[1:] // the user deleted gke-prod
	out, err := yaml.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, out, 0600))

	require.NoError(t, Remove("dev", prev))
	require.Equal(t, "", load(t, path)["current-context"])
}

func TestRemoveLeavesAFileWithoutTheClusterUntouched(t *testing.T) {
	path, prev := setup(t, userConfig)
	require.NoError(t, Remove("dev", prev))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, userConfig, string(data), "not even reformatted")
	require.NoFileExists(t, path+".monolith.bak")
}

func TestRemoveFromMissingFile(t *testing.T) {
	path, prev := setup(t, "")
	require.NoError(t, Remove("dev", prev))
	require.NoFileExists(t, path)
}

func TestCurrent(t *testing.T) {
	setup(t, userConfig)
	require.Equal(t, "gke-prod", Current())
	setup(t, "")
	require.Equal(t, "", Current())
}

func TestPathUsesTheFirstKubeconfigEntry(t *testing.T) {
	t.Setenv("KUBECONFIG", "/a/one"+string(os.PathListSeparator)+"/b/two")
	path, err := Path()
	require.NoError(t, err)
	require.Equal(t, "/a/one", path)
}

func TestUseSwitchesBetweenClusters(t *testing.T) {
	path, prev := setup(t, userConfig)
	require.NoError(t, Merge(parse(t, "dev"), prev))
	require.NoError(t, Merge(parse(t, "prod"), prev))
	require.Equal(t, "cluster-prod", load(t, path)["current-context"])

	require.NoError(t, Use("dev", prev))
	require.Equal(t, "cluster-dev", load(t, path)["current-context"])
	note, err := os.ReadFile(prev)
	require.NoError(t, err)
	require.Equal(t, "cluster-prod", string(note), "the context it replaced is noted for Remove")

	before, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, Use("dev", prev))
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after, "already current: nothing is rewritten")
}

func TestUseWithoutTheContext(t *testing.T) {
	path, prev := setup(t, userConfig)
	require.ErrorIs(t, Use("dev", prev), ErrNoContext)
	require.Equal(t, "gke-prod", load(t, path)["current-context"])
	require.NoFileExists(t, prev)
}

// RemoveContext takes the exact name: Docker hosts used to merge k3s-<name>.
func TestRemoveContextByExactName(t *testing.T) {
	path, _ := setup(t, userConfig)
	doc := load(t, path)
	for _, key := range []string{"clusters", "users", "contexts"} {
		doc[key] = append(doc[key].([]any), map[string]any{"name": "k3s-docker-lith-default"})
	}
	doc["current-context"] = "k3s-docker-lith-default"
	out, err := yaml.Marshal(doc)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, out, 0600))

	require.NoError(t, RemoveContext("k3s-docker-lith-default", ""))
	doc = load(t, path)
	require.Equal(t, []string{"gke-prod"}, names(doc, "contexts"))
	require.Equal(t, []string{"gke-prod"}, names(doc, "clusters"))
	require.Equal(t, "", doc["current-context"])

	require.NoError(t, RemoveContext("k3s-docker-lith-default", ""), "already gone: no error")
}
