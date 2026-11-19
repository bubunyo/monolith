# Clusters

`monolith cluster` runs a single-node [k3s](https://k3s.io) Kubernetes cluster
in a VM and points `kubectl` at it.

```bash
monolith cluster start              # seconds once k3s and Alpine are cached; the first run downloads them
kubectl get nodes                   # context cluster-default is now current
kubectl create deployment web --image=nginx
monolith cluster pause              # whole VM to disk, pods and all
monolith cluster resume
monolith cluster stop               # state stays on the cluster's data disk
monolith cluster rm                 # delete it; your previous kubectl context comes back
```

The VM is an ordinary monolith VM: `monolith vm attach cluster-default` opens
its console (Ctrl+] then q to detach), and `monolith vm logs` shows its daemon
log. k3s's own log is `/var/log/k3s.log` in the VM.

## Commands

| Command | |
|---|---|
| `cluster start` | Create and boot the cluster, or resume a paused one. Waits until the node is Ready, merges the context into your kubeconfig and makes it current. |
| `cluster pause` / `resume` | Save the VM's memory and devices to disk and stop it; restore it exactly. Needs the Mac unlocked to resume. |
| `cluster stop` | Stop the VM. On a paused cluster it discards the saved state (after asking, unless `--force`). |
| `cluster kubeconfig` | Print a standalone kubeconfig, for `KUBECONFIG=<file>` in scripts. |
| `cluster list` | State, k3s version, context (`*` = current), CPUs, memory, data disk use and API address. |
| `cluster rm` | Delete the VM and its context. |

Every command except `list` takes `--name` (default `cluster-default`), so
several clusters can run at once. `start` also takes `--cpus` (default 2),
`--memory` (default 4G) and `--k3s-version`; they are saved for later starts,
and the k3s version only changes while the cluster is stopped.

## Your kubeconfig

`start` merges the cluster, user and context `cluster-<name>` into the first
file in `$KUBECONFIG`, else `~/.kube/config`, and makes the context current.
Everything else in the file is kept as it is. The name always starts with
`cluster-`, so a cluster never replaces a context of yours.

- The file as it was before the first merge is kept as `<file>.monolith.bak`.
  Later starts leave that backup alone.
- `rm` removes the entries. If the cluster's context was current, the one that
  was current before becomes current again (if it still exists).
- The API is on `127.0.0.1` at a port chosen once per cluster, shown by
  `cluster list`.

## What's inside

Alpine with k3s running on its own bundled containerd; no Docker. The VM uses
user-mode networking, so image pulls work wherever the Mac is online. The k3s
binary is downloaded once to `~/.monolith/k3s/<version>/` and shared read-only
into the VM, so the first start of a new version needs internet. Images, the
datastore and pods live on a sparse 64 GiB data disk.

## Limits

- **Multi-arch images only.** Pods can't pull images that only exist for amd64:
  containerd's CRI pulls the node's platform (arm64), and Rosetta in the guest
  doesn't change that. The error is `no match for platform in manifest`.
- **No local image builds into pods.** Pull from a registry, or use
  `monolith docker start --kubernetes`, which shares its engine with k3s.
- **No service ports on localhost.** LoadBalancer and NodePort services aren't
  published; use `kubectl port-forward`.
- A cluster can't be created with `monolith vm run`; `cluster start` creates it.
