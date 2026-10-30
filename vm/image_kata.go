package vm

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bubunyo/monolith"
	"github.com/klauspost/compress/zstd"
)

// Kata Containers guest assets. Kata publishes per-component release builds
// as OCI artifacts in its CI cache on GHCR, tagged <version>-<arch>. Each
// artifact has one layer, a zstd tarball laid out under /opt/kata/share/
// kata-containers. These are ~65 MB together, versus ~680 MB for the
// all-in-one kata-static release tarball.
const (
	kataVersion  = "4.3.1"
	kataArch     = "aarch64"
	kataRegistry = "https://ghcr.io"
	kataRepoBase = "kata-containers/cached-artefacts/"

	kataKernelFile   = "vmlinux"
	kataRootfsFile   = "rootfs.img"
	kataRootHashFile = "root_hash.txt"
)

// kataImage boots Kata's guest: its kernel (a raw ARM64 Image with virtio,
// vsock and dm-verity built in, no modules) and its Ubuntu rootfs image,
// whose root partition is protected by dm-verity. The rootfs is read-only
// and shared by every Kata VM; guests are ephemeral. systemd starts
// kata-agent, whose debug console provides the interactive root shell on
// hvc0, and the kernel configures eth0 by DHCP.
type kataImage struct{}

func (kataImage) Name() string { return ImageKata }

func (kataImage) Prepare(name string) (*BootSpec, error) {
	dir, err := kataCacheDir()
	if err != nil {
		return nil, err
	}
	kernel := filepath.Join(dir, kataKernelFile)
	rootfs := filepath.Join(dir, kataRootfsFile)
	rootHash := filepath.Join(dir, kataRootHashFile)

	if !exists(kernel) {
		if err := fetchKataArtifact("kernel", "kata-static-kernel.tar.zst", map[string]string{
			kataKernelFile: "vmlinux-",
		}, dir); err != nil {
			return nil, fmt.Errorf("fetching Kata kernel: %w", err)
		}
	}
	if !exists(rootfs) || !exists(rootHash) {
		if err := fetchKataArtifact("rootfs-image", "kata-static-rootfs-image.tar.zst", map[string]string{
			kataRootfsFile:   ".image",
			kataRootHashFile: "root_hash_base.txt",
		}, dir); err != nil {
			return nil, fmt.Errorf("fetching Kata rootfs image: %w", err)
		}
	}

	data, err := os.ReadFile(rootHash)
	if err != nil {
		return nil, err
	}
	verity, err := parseVerityParams(string(data))
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", rootHash, err)
	}

	// Kata VMs keep no disk of their own, but config, state and logs live
	// in the VM directory.
	vmDir, err := VMDir(name)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(vmDir, 0755); err != nil {
		return nil, fmt.Errorf("creating VM directory: %w", err)
	}

	return &BootSpec{
		KernelPath:   kernel,
		CommandLine:  kataCommandLine(name, verity),
		DiskPath:     rootfs,
		DiskReadOnly: true,
	}, nil
}

func kataCacheDir() (string, error) {
	dir, err := monolith.CachePath("kata", kataVersion)
	if err != nil {
		return "", err
	}
	return dir, os.MkdirAll(dir, 0755)
}

// verityParams is the dm-verity metadata Kata ships next to the rootfs image
// in root_hash_base.txt, e.g.
// "root_hash=<hex>,salt=<hex>,data_blocks=64000,data_block_size=4096,hash_block_size=4096".
type verityParams struct {
	rootHash      string
	salt          string
	dataBlocks    int
	dataBlockSize int
	hashBlockSize int
}

func parseVerityParams(s string) (verityParams, error) {
	kv := map[string]string{}
	for _, f := range strings.Split(strings.TrimSpace(s), ",") {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			return verityParams{}, fmt.Errorf("malformed field %q", f)
		}
		kv[k] = v
	}
	var p verityParams
	p.rootHash, p.salt = kv["root_hash"], kv["salt"]
	ints := map[string]*int{"data_blocks": &p.dataBlocks, "data_block_size": &p.dataBlockSize, "hash_block_size": &p.hashBlockSize}
	for k, dst := range ints {
		n, err := strconv.Atoi(kv[k])
		if err != nil || n <= 0 {
			return verityParams{}, fmt.Errorf("invalid %s %q", k, kv[k])
		}
		*dst = n
	}
	if p.rootHash == "" || p.salt == "" {
		return verityParams{}, fmt.Errorf("missing root_hash or salt")
	}
	return p, nil
}

// kataCommandLine builds the kernel command line for the Kata guest. The
// root partition (vda1) is mapped through dm-verity with its hash tree on
// vda2, the same way the Kata runtime boots measured rootfs images.
func kataCommandLine(name string, v verityParams) string {
	dataSectors := v.dataBlocks * v.dataBlockSize / 512
	return strings.Join([]string{
		"console=hvc0",
		fmt.Sprintf(`dm-mod.create="dm-verity,,,ro,0 %d verity 1 /dev/vda1 /dev/vda2 %d %d %d 0 sha256 %s %s"`,
			dataSectors, v.dataBlockSize, v.hashBlockSize, v.dataBlocks, v.rootHash, v.salt),
		"root=/dev/dm-0",
		"rootflags=data=ordered,errors=remount-ro",
		"ro",
		"rootfstype=ext4",
		"ip=dhcp", // kernel IP autoconfig; the image has no DHCP client
		"systemd.hostname=" + name,
		"systemd.unit=kata-containers.target",
		"agent.debug_console", // kata-agent runs a root shell on the console
		"agent.log=warn",      // agent logs go to the console too
	}, " ")
}

// fetchKataArtifact downloads the single-layer OCI artifact
// kata-containers/cached-artefacts/<component>:<version>-<arch> from GHCR,
// verifies the layer digest, and extracts files from its zstd tarball into
// dir. want maps destination file name to a match on the tar entry's base
// name: a suffix if it starts with ".", otherwise a prefix.
func fetchKataArtifact(component, layerTitle string, want map[string]string, dir string) error {
	repo := kataRepoBase + component
	tag := kataVersion + "-" + kataArch

	token, err := ghcrToken(repo)
	if err != nil {
		return err
	}

	var manifest struct {
		Layers []struct {
			Digest      string            `json:"digest"`
			Size        int64             `json:"size"`
			Annotations map[string]string `json:"annotations"`
		} `json:"layers"`
	}
	if err := ghcrGetJSON(fmt.Sprintf("%s/v2/%s/manifests/%s", kataRegistry, repo, tag), token,
		"application/vnd.oci.image.manifest.v1+json", &manifest); err != nil {
		return fmt.Errorf("manifest %s:%s: %w", repo, tag, err)
	}
	var digest string
	for _, l := range manifest.Layers {
		if l.Annotations["org.opencontainers.image.title"] == layerTitle {
			digest = l.Digest
		}
	}
	if !strings.HasPrefix(digest, "sha256:") {
		return fmt.Errorf("manifest %s:%s has no sha256 layer %q", repo, tag, layerTitle)
	}

	resp, err := ghcrGet(fmt.Sprintf("%s/v2/%s/blobs/%s", kataRegistry, repo, digest), token, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Hash the compressed stream as it is decoded; files are only moved
	// into place once the digest matches.
	hasher := sha256.New()
	zr, err := zstd.NewReader(io.TeeReader(resp.Body, hasher))
	if err != nil {
		return err
	}
	defer zr.Close()

	staged := map[string]string{} // destination name -> temp path
	defer func() {
		for _, tmp := range staged {
			os.Remove(tmp)
		}
	}()

	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("reading %s: %w", layerTitle, err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue // skip dirs and the vmlinux.container-style symlinks
		}
		base := path.Base(hdr.Name)
		for dst, match := range want {
			if _, done := staged[dst]; done || !matchKataEntry(base, match) {
				continue
			}
			tmp, err := os.CreateTemp(dir, dst+".*.tmp")
			if err != nil {
				return err
			}
			if _, err := io.Copy(tmp, tr); err != nil {
				tmp.Close()
				os.Remove(tmp.Name())
				return fmt.Errorf("extracting %s: %w", hdr.Name, err)
			}
			tmp.Close()
			staged[dst] = tmp.Name()
			break
		}
	}
	// Drain so the digest covers the whole blob.
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return err
	}

	if got := "sha256:" + hex.EncodeToString(hasher.Sum(nil)); got != digest {
		return fmt.Errorf("digest mismatch for %s: got %s, want %s", layerTitle, got, digest)
	}
	for dst := range want {
		tmp, ok := staged[dst]
		if !ok {
			return fmt.Errorf("%s does not contain a file matching %q", layerTitle, want[dst])
		}
		if err := os.Rename(tmp, filepath.Join(dir, dst)); err != nil {
			return err
		}
		delete(staged, dst)
	}
	return nil
}

func matchKataEntry(base, match string) bool {
	if strings.HasPrefix(match, ".") {
		return strings.HasSuffix(base, match)
	}
	return strings.HasPrefix(base, match)
}

// ghcrToken gets an anonymous pull token for a public GHCR repository.
func ghcrToken(repo string) (string, error) {
	var tok struct {
		Token string `json:"token"`
	}
	if err := ghcrGetJSON(fmt.Sprintf("%s/token?scope=repository:%s:pull", kataRegistry, repo), "", "", &tok); err != nil {
		return "", fmt.Errorf("GHCR token for %s: %w", repo, err)
	}
	if tok.Token == "" {
		return "", fmt.Errorf("GHCR returned an empty token for %s", repo)
	}
	return tok.Token, nil
}

func ghcrGet(url, token, accept string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp, nil
}

func ghcrGetJSON(url, token, accept string, v any) error {
	resp, err := ghcrGet(url, token, accept)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}
