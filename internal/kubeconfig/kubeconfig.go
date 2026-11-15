// Package kubeconfig reads the kubeconfig k3s writes in a cluster VM,
// rewrites it for the Mac, and merges it into (and removes it from) the
// user's own kubeconfig without disturbing anything else in that file.
package kubeconfig

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the part of the kubeconfig format monolith reads and writes.
type Config struct {
	APIVersion     string         `yaml:"apiVersion"`
	Kind           string         `yaml:"kind"`
	Clusters       []namedCluster `yaml:"clusters"`
	Users          []namedUser    `yaml:"users"`
	Contexts       []namedContext `yaml:"contexts"`
	CurrentContext string         `yaml:"current-context"`
}

type namedCluster struct {
	Name    string `yaml:"name"`
	Cluster struct {
		CertificateAuthorityData string `yaml:"certificate-authority-data"`
		Server                   string `yaml:"server"`
	} `yaml:"cluster"`
}

type namedUser struct {
	Name string `yaml:"name"`
	User struct {
		ClientCertificateData string `yaml:"client-certificate-data"`
		ClientKeyData         string `yaml:"client-key-data"`
	} `yaml:"user"`
}

type namedContext struct {
	Name    string `yaml:"name"`
	Context struct {
		Cluster string `yaml:"cluster"`
		User    string `yaml:"user"`
	} `yaml:"context"`
}

// ContextName is the kubeconfig cluster, user and context name for the
// cluster VM called vmName. It always starts with "cluster-", so a cluster
// can never replace one of the user's own contexts by name.
func ContextName(vmName string) string {
	if strings.HasPrefix(vmName, "cluster-") {
		return vmName
	}
	return "cluster-" + vmName
}

// Parse reads the kubeconfig k3s wrote for VM vmName and points it at
// https://127.0.0.1:port, with every entry named ContextName(vmName).
func Parse(data []byte, vmName string, port int) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parsing the k3s kubeconfig: %w", err)
	}
	if len(c.Clusters) != 1 || len(c.Users) != 1 || len(c.Contexts) != 1 {
		return nil, fmt.Errorf("unexpected k3s kubeconfig (%d clusters, %d users, %d contexts)", len(c.Clusters), len(c.Users), len(c.Contexts))
	}
	n := ContextName(vmName)
	c.Clusters[0].Name = n
	c.Clusters[0].Cluster.Server = fmt.Sprintf("https://127.0.0.1:%d", port)
	c.Users[0].Name = n
	c.Contexts[0].Name = n
	c.Contexts[0].Context.Cluster = n
	c.Contexts[0].Context.User = n
	c.CurrentContext = n
	return &c, nil
}

// Marshal returns c as a standalone kubeconfig file.
func (c *Config) Marshal() ([]byte, error) {
	return yaml.Marshal(c)
}

// Path is the file kubectl writes to: the first entry of $KUBECONFIG, else
// ~/.kube/config.
func Path() (string, error) {
	if env := os.Getenv("KUBECONFIG"); env != "" {
		return filepath.SplitList(env)[0], nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".kube", "config"), nil
}

// Current returns the user's current context, or "" if there is none or the
// kubeconfig can't be read.
func Current() string {
	path, err := Path()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var doc struct {
		Current string `yaml:"current-context"`
	}
	yaml.Unmarshal(data, &doc)
	return doc.Current
}

// Merge adds or replaces the cluster, user and context of c in the user's
// kubeconfig and makes the context current.
//
// Before the first merge of a cluster the previous file is kept as
// <file>.monolith.bak; later merges leave that backup alone, so it stays the
// file as it was before monolith touched it. The context that was current is
// noted in prevFile, for Remove to switch back to.
func Merge(c *Config, prevFile string) error {
	name := c.Contexts[0].Name
	return edit(func(doc map[string]any) (changed, backup bool) {
		had := hasNamed(doc, "contexts", name)
		if cur, _ := doc["current-context"].(string); cur != "" && cur != name {
			os.WriteFile(prevFile, []byte(cur), 0600)
		}
		upsertNamed(doc, "clusters", toGeneric(c.Clusters[0]))
		upsertNamed(doc, "users", toGeneric(c.Users[0]))
		upsertNamed(doc, "contexts", toGeneric(c.Contexts[0]))
		doc["current-context"] = c.CurrentContext
		return true, !had
	})
}

// Remove deletes the cluster VM's entries from the user's kubeconfig. If its
// context was current, the context that was current before (noted in
// prevFile) becomes current again if it still exists, else none is. A
// kubeconfig without the entries is left untouched.
func Remove(vmName, prevFile string) error {
	name := ContextName(vmName)
	err := edit(func(doc map[string]any) (changed, backup bool) {
		if !hasNamed(doc, "contexts", name) {
			return false, false
		}
		for _, key := range []string{"clusters", "users", "contexts"} {
			removeNamed(doc, key, name)
		}
		if doc["current-context"] == name {
			doc["current-context"] = ""
			if prev, err := os.ReadFile(prevFile); err == nil {
				if p := strings.TrimSpace(string(prev)); hasNamed(doc, "contexts", p) {
					doc["current-context"] = p
				}
			}
		}
		return true, false
	})
	if err == nil {
		os.Remove(prevFile)
	}
	return err
}

// edit loads the user's kubeconfig as a generic document (keeping every
// field monolith doesn't know about), applies fn, and writes it back. If fn
// reports no change, nothing is written, not even a missing file. If it
// asks for a backup and the file exists, the file as it was goes to
// <file>.monolith.bak first.
func edit(fn func(doc map[string]any) (changed, backup bool)) error {
	path, err := Path()
	if err != nil {
		return err
	}
	doc := map[string]any{}
	data, err := os.ReadFile(path)
	exists := err == nil
	switch {
	case exists:
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}
		if doc == nil {
			doc = map[string]any{}
		}
	case os.IsNotExist(err):
		doc["apiVersion"] = "v1"
		doc["kind"] = "Config"
	default:
		return err
	}

	changed, backup := fn(doc)
	if !changed {
		return nil
	}

	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	if exists && backup {
		if err := os.WriteFile(path+".monolith.bak", data, 0600); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp := path + ".monolith.tmp"
	if err := os.WriteFile(tmp, out, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func toGeneric(v any) map[string]any {
	data, _ := yaml.Marshal(v)
	m := map[string]any{}
	yaml.Unmarshal(data, &m)
	return m
}

func hasNamed(doc map[string]any, key, name string) bool {
	list, _ := doc[key].([]any)
	for _, item := range list {
		if m, ok := item.(map[string]any); ok && m["name"] == name {
			return true
		}
	}
	return false
}

func upsertNamed(doc map[string]any, key string, entry map[string]any) {
	list, _ := doc[key].([]any)
	for i, item := range list {
		if m, ok := item.(map[string]any); ok && m["name"] == entry["name"] {
			list[i] = entry
			doc[key] = list
			return
		}
	}
	doc[key] = append(list, entry)
}

func removeNamed(doc map[string]any, key, name string) {
	list, _ := doc[key].([]any)
	kept := list[:0]
	for _, item := range list {
		if m, ok := item.(map[string]any); ok && m["name"] == name {
			continue
		}
		kept = append(kept, item)
	}
	doc[key] = kept
}

// Client authenticates to the API with the kubeconfig's client certificate.
func (c *Config) Client() (*http.Client, error) {
	decode := func(s string) []byte {
		b, _ := base64.StdEncoding.DecodeString(s)
		return b
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(decode(c.Clusters[0].Cluster.CertificateAuthorityData)) {
		return nil, fmt.Errorf("the k3s kubeconfig has no usable CA certificate")
	}
	cert, err := tls.X509KeyPair(decode(c.Users[0].User.ClientCertificateData), decode(c.Users[0].User.ClientKeyData))
	if err != nil {
		return nil, fmt.Errorf("k3s client certificate: %w", err)
	}
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, Certificates: []tls.Certificate{cert}},
		},
	}, nil
}

// NodeReady reports whether the API is healthy and the node's Ready
// condition is True.
func (c *Config) NodeReady(client *http.Client) bool {
	server := c.Clusters[0].Cluster.Server
	resp, err := client.Get(server + "/readyz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	resp, err = client.Get(server + "/api/v1/nodes")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var nodes struct {
		Items []struct {
			Status struct {
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"status"`
		} `json:"items"`
	}
	if json.NewDecoder(resp.Body).Decode(&nodes) != nil || len(nodes.Items) == 0 {
		return false
	}
	for _, cond := range nodes.Items[0].Status.Conditions {
		if cond.Type == "Ready" {
			return cond.Status == "True"
		}
	}
	return false
}
