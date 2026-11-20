# Clusters

`monolith cluster` runs a single-node [k3s](https://k3s.io) Kubernetes cluster
in a VM and points `kubectl` at it.

```bash
monolith cluster start              # seconds once k3s and Alpine are cached; the first run downloads them
kubectl get nodes                   # context cluster-default is now current
kubectl create deployment web --image=nginx
kubectl expose deployment web --type=LoadBalancer --port=8080 --target-port=80
curl localhost:8080                 # services are forwarded to localhost
monolith cluster use other          # switch kubectl to another cluster
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
| `cluster use [name]` | Make the cluster's context current without booting it (`--start` boots or resumes it first). With no name, print the current cluster. |
| `cluster pause` / `resume` | Save the VM's memory and devices to disk and stop it; restore it exactly. Needs the Mac unlocked to resume. |
| `cluster stop` | Stop the VM. On a paused cluster it discards the saved state (after asking, unless `--force`). |
| `cluster kubeconfig` | Print a standalone kubeconfig, for `KUBECONFIG=<file>` in scripts. |
| `cluster list` | State, k3s version, context (`*` = current), CPUs, memory, data disk use, API address and forwarded service ports. `-q` prints only names. |
| `cluster rm` | Delete the VM and its context. |

Every command except `list` takes `--name` (default `cluster-default`), so
several clusters can run at once. `start` also takes `--cpus` (default 2),
`--memory` (default 4G), `--k3s-version` and `--rosetta` (see
[amd64 images](#amd64-images-rosetta)); they are saved for later starts, and
the k3s version and Rosetta only change while the cluster is stopped.

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
- `cluster use <name>` switches between clusters; the context it replaces is
  remembered the same way, for `rm`.

## Services on localhost

LoadBalancer services and NodePorts (TCP and UDP) are forwarded to the same
port on `127.0.0.1`, as a Docker host does for `docker run -p`:

```bash
kubectl expose deployment web --type=LoadBalancer --port=8080 --target-port=80
curl localhost:8080
```

The forwards follow the services within a second. A port that another cluster
or Docker host already holds isn't forwarded; `cluster list` shows it with a
`!` (e.g. `8080!`) until it frees up. ClusterIP services stay inside the
cluster; use `kubectl port-forward` for those. Ports below 1024 work without
sudo. Traefik is disabled.

## amd64 images (Rosetta)

`cluster start --rosetta` lets pods run x86-64 (amd64) binaries through Rosetta.
It needs Rosetta on the Mac (`softwareupdate --install-rosetta`).

The node still only pulls images labelled for its own platform (arm64), so an
amd64-only image fails with `no match for platform in manifest` even with
Rosetta. Relabel the image as arm64 and push it under a tag of your own; the
binaries inside stay x86-64 and Rosetta runs them:

```bash
crane mutate vendor/app@$(crane digest --platform linux/amd64 vendor/app:1.2) \
  --set-platform linux/arm64 -t registry.example.com/vendor/app:1.2-rosetta
```

Then use `registry.example.com/vendor/app:1.2-rosetta` in the pod spec.
Multi-arch images need none of this; they run natively. Expect amd64 code to
run slower than native (around 1.4× in a CPU-bound test), and programs that
need CPU features Rosetta doesn't provide (such as AVX-512) won't run.

## What's inside

Alpine with k3s running on its own bundled containerd; no Docker. The VM uses
user-mode networking, so image pulls work wherever the Mac is online. The k3s
binary is downloaded once to `~/.monolith/k3s/<version>/` and shared read-only
into the VM, so the first start of a new version needs internet. Images, the
datastore and pods live on a sparse 64 GiB data disk.

## Limits

- **amd64-only images need relabelling.** Pods can't pull images that only
  exist for amd64: containerd's CRI pulls the node's platform (arm64). With
  `--rosetta` they run once relabelled, see [amd64 images](#amd64-images-rosetta).
- **No local image builds into pods.** Pods pull from a registry; push images
  you build there first.
- A cluster can't be created with `monolith vm run`; `cluster start` creates it.
