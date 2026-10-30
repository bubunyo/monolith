package docker

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

	"github.com/bubunyo/monolith/vm"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// kubeReadyTimeout covers the first k3s start, which pulls its system images.
const kubeReadyTimeout = 10 * time.Minute

var kubeconfigCmd = &cobra.Command{
	Use:   "kubeconfig",
	Short: "Print the kubeconfig for a Docker host's Kubernetes cluster",
	Long: `Print a standalone kubeconfig for the host's k3s cluster (context k3s-<name>,
API on localhost), e.g. for KUBECONFIG=<file> in scripts. 'start' already merges
the same entries into your kubeconfig.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := vm.ReadConfig(flagName)
		if err != nil {
			return err
		}
		kc, err := fetchKubeconfig(flagName, cfg)
		if err != nil {
			return err
		}
		out, err := yaml.Marshal(kc)
		if err != nil {
			return err
		}
		os.Stdout.Write(out)
		return nil
	},
}

// kubeContextName is the kubeconfig cluster, user and context name for a VM.
func kubeContextName(vmName string) string {
	return "k3s-" + vmName
}

// kubeconfig is the part of the kubeconfig format monolith reads and writes.
type kubeconfig struct {
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

// fetchKubeconfig gets k3s's kubeconfig from the VM daemon and rewrites it
// for the Mac: entries named k3s-<name>, server on the forwarded port.
func fetchKubeconfig(name string, cfg vm.VMConfig) (*kubeconfig, error) {
	if !cfg.Kubernetes {
		return nil, fmt.Errorf("Kubernetes is not enabled on %q; start it with --kubernetes", name)
	}
	resp, err := vm.SendControlCommand(name, vm.ControlRequest{Cmd: vm.CmdKubeconfig})
	if err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("%s", resp.Error)
	}
	var kc kubeconfig
	if err := yaml.Unmarshal([]byte(resp.Data), &kc); err != nil {
		return nil, fmt.Errorf("parsing k3s kubeconfig: %w", err)
	}
	if len(kc.Clusters) != 1 || len(kc.Users) != 1 || len(kc.Contexts) != 1 {
		return nil, fmt.Errorf("unexpected k3s kubeconfig (%d clusters, %d users, %d contexts)", len(kc.Clusters), len(kc.Users), len(kc.Contexts))
	}
	n := kubeContextName(name)
	kc.Clusters[0].Name = n
	kc.Clusters[0].Cluster.Server = fmt.Sprintf("https://127.0.0.1:%d", cfg.KubePort)
	kc.Users[0].Name = n
	kc.Contexts[0].Name = n
	kc.Contexts[0].Context.Cluster = n
	kc.Contexts[0].Context.User = n
	kc.CurrentContext = n
	return &kc, nil
}

// kubeconfigPath is the file kubectl writes to: the first entry of
// $KUBECONFIG, else ~/.kube/config.
func kubeconfigPath() (string, error) {
	if env := os.Getenv("KUBECONFIG"); env != "" {
		return filepath.SplitList(env)[0], nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".kube", "config"), nil
}

// editKubeconfig loads the user's kubeconfig as a generic document (keeping
// every field monolith doesn't know about), applies edit, and writes it
// back, keeping the previous version as <file>.monolith.bak.
func editKubeconfig(edit func(doc map[string]any)) error {
	path, err := kubeconfigPath()
	if err != nil {
		return err
	}
	doc := map[string]any{}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := yaml.Unmarshal(data, &doc); err != nil {
			return fmt.Errorf("parsing %s: %w", path, err)
		}
		if doc == nil {
			doc = map[string]any{}
		}
		if err := os.WriteFile(path+".monolith.bak", data, 0600); err != nil {
			return err
		}
	case os.IsNotExist(err):
		doc["apiVersion"] = "v1"
		doc["kind"] = "Config"
	default:
		return err
	}

	edit(doc)

	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
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

// mergeKubeconfig adds or replaces the VM's cluster, user and context in the
// user's kubeconfig and makes the context current.
func mergeKubeconfig(kc *kubeconfig) error {
	return editKubeconfig(func(doc map[string]any) {
		upsertNamed(doc, "clusters", toGeneric(kc.Clusters[0]))
		upsertNamed(doc, "users", toGeneric(kc.Users[0]))
		upsertNamed(doc, "contexts", toGeneric(kc.Contexts[0]))
		doc["current-context"] = kc.CurrentContext
	})
}

// removeKubeContext deletes the VM's entries from the user's kubeconfig,
// clearing current-context if it was the VM's. A kubeconfig without them is
// left untouched.
func removeKubeContext(vmName string) error {
	path, err := kubeconfigPath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), kubeContextName(vmName)) {
		return nil
	}
	n := kubeContextName(vmName)
	return editKubeconfig(func(doc map[string]any) {
		for _, key := range []string{"clusters", "users", "contexts"} {
			removeNamed(doc, key, n)
		}
		if doc["current-context"] == n {
			doc["current-context"] = ""
		}
	})
}

func toGeneric(v any) map[string]any {
	data, _ := yaml.Marshal(v)
	m := map[string]any{}
	yaml.Unmarshal(data, &m)
	return m
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

// readyKubernetes waits for k3s, merges its context into the user's
// kubeconfig, and waits for the API and the node to be ready.
func readyKubernetes(cfg vm.VMConfig) error {
	fmt.Println("Waiting for Kubernetes (the first start pulls k3s's system images)...")
	deadline := time.Now().Add(kubeReadyTimeout)
	var kc *kubeconfig
	for {
		var err error
		if kc, err = fetchKubeconfig(flagName, cfg); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("k3s did not start within %s: %w; see /var/log/k3s.log in the VM", kubeReadyTimeout, err)
		}
		time.Sleep(2 * time.Second)
	}
	client, err := kubeClient(kc)
	if err != nil {
		return err
	}
	for !nodeReady(client, kc) {
		if time.Now().After(deadline) {
			return fmt.Errorf("the Kubernetes node was not ready within %s; see /var/log/k3s.log in the VM", kubeReadyTimeout)
		}
		time.Sleep(2 * time.Second)
	}
	if err := mergeKubeconfig(kc); err != nil {
		return fmt.Errorf("Kubernetes is ready, but updating your kubeconfig failed: %w", err)
	}
	path, _ := kubeconfigPath()
	fmt.Printf("Kubernetes %s is ready (kubectl context %s is now current in %s).\n", vm.K3sVersionOf(cfg), kc.CurrentContext, path)
	return nil
}

// kubeClient authenticates to the API with the kubeconfig's client
// certificate.
func kubeClient(kc *kubeconfig) (*http.Client, error) {
	decode := func(s string) []byte {
		b, _ := base64.StdEncoding.DecodeString(s)
		return b
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(decode(kc.Clusters[0].Cluster.CertificateAuthorityData)) {
		return nil, fmt.Errorf("k3s kubeconfig has no usable CA certificate")
	}
	cert, err := tls.X509KeyPair(decode(kc.Users[0].User.ClientCertificateData), decode(kc.Users[0].User.ClientKeyData))
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

// nodeReady reports whether the API is healthy and the node's Ready
// condition is True.
func nodeReady(client *http.Client, kc *kubeconfig) bool {
	server := kc.Clusters[0].Cluster.Server
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
	for _, c := range nodes.Items[0].Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}
