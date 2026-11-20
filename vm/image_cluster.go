package vm

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	// ImageCluster is the base image of `monolith cluster` VMs: Alpine
	// running a single-node k3s server on its bundled containerd. It isn't
	// in ValidImages; `vm run --image` doesn't create clusters.
	ImageCluster = "cluster"

	clusterOverlay = "cluster/manifest.yaml"
	kubeShareTag   = "kube"

	// clusterDataDiskSize is a cluster's data disk (images, datastore,
	// pods): sparse, so a ceiling rather than space used. Docker hosts size
	// theirs with --disk (datadisk.go); clusters have no such flag yet.
	clusterDataDiskSize = 64 << 30
)

// clusterImage is a Kubernetes VM: the Alpine image plus the files in
// embeddings/cluster, which install what k3s needs on first boot and run it.
// The k3s binary is shared read-only from the host cache, and k3s writes its
// kubeconfig into a read-write share that the host reads it from. State
// (images, datastore, pods) lives on a separate sparse data disk. The daemon
// forwards the Kubernetes API to localhost through the user-mode network,
// which the image therefore requires.
type clusterImage struct{}

func (clusterImage) Name() string { return ImageCluster }

func (clusterImage) Prepare(name string, log io.Writer) (*BootSpec, error) {
	kernel, err := DefaultKernel(log)
	if err != nil {
		return nil, err
	}
	rootfs, err := DefaultRootFS(log)
	if err != nil {
		return nil, err
	}
	cfg, err := ReadConfig(name)
	if err != nil {
		return nil, err
	}
	k3sDir, err := ensureK3s(K3sVersionOf(cfg), log)
	if err != nil {
		return nil, err
	}
	vmDir, err := VMDir(name)
	if err != nil {
		return nil, err
	}
	kubeDir, err := VMKubeDir(name)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(kubeDir, 0755); err != nil {
		return nil, fmt.Errorf("creating the kubeconfig share: %w", err)
	}

	// A disk created now gets the current guest files.
	newDisk := !exists(filepath.Join(vmDir, alpineDiskName))
	if newDisk {
		cfg.GuestLayout = GuestLayout
	}
	disk, err := DefaultDisk(rootfs, kernel.ModloopPath, name, log, clusterOverlay)
	if err != nil {
		return nil, err
	}
	if newDisk {
		if err := WriteConfig(name, cfg); err != nil {
			return nil, err
		}
	}

	commandLine := alpineCommandLine
	if cfg.Rosetta {
		if cfg.GuestLayout < GuestLayoutRosetta {
			return nil, fmt.Errorf("VM %q was created before Rosetta support; recreate it to use --rosetta", name)
		}
		if err := CheckRosetta(); err != nil {
			return nil, err
		}
		commandLine += " " + rosettaCmdlineArg + "=1"
	}

	dataDisk := filepath.Join(vmDir, dockerDataDiskName)
	if !exists(dataDisk) {
		// Left unformatted; cluster-setup runs mkfs.ext4 on first boot.
		if err := createSparseFile(dataDisk, clusterDataDiskSize); err != nil {
			return nil, fmt.Errorf("creating the cluster data disk: %w", err)
		}
	}

	return &BootSpec{
		KernelPath:   kernel.KernelPath,
		InitrdPath:   kernel.InitrdPath,
		CommandLine:  commandLine,
		DiskPath:     disk,
		DataDiskPath: dataDisk,
		Shares: []Share{
			{Tag: k3sShareTag, HostPath: k3sDir, ReadOnly: true},
			{Tag: kubeShareTag, HostPath: kubeDir},
		},
		HandlesStop: true,
		Rosetta:     cfg.Rosetta,
	}, nil
}
