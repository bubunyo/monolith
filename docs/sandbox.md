# Sandboxes

`monolith sandbox` runs code you don't trust (a repo you just cloned, a
download, a contractor's branch) in a lith instead of on your Mac, scoped to
one repo the way a virtualenv scopes a project.

```bash
cd ~/Downloads/some-repo
monolith sandbox activate      # a shell inside the sandbox, at the repo
npm install && npm test        # runs in the lith, not on your Mac
deactivate                     # back on the Mac; the sandbox pauses to disk
monolith sandbox diff          # what the code changed
monolith sandbox apply src/x.js # copy chosen changes back to the Mac
monolith sandbox rm            # throw it away
```

Needs the `docker` CLI on the Mac (it talks to the sandbox's own engine; your
docker context isn't changed).

## What the code can and can't do

| | Inside a sandbox |
|---|---|
| The repo | Visible at the same path as on your Mac, read-only underneath. Writes (installs, builds, edits, deletes) land in a layer inside the VM; your files change only when you `apply` them. Edits you make on the Mac show up in the sandbox. |
| The rest of your Mac | Nothing: no home directory, SSH keys, cloud credentials, browser data, environment variables or SSH agent. |
| Network (default `outbound`) | The internet, but not your Mac (its loopback services or `host.docker.internal`) and not your local network. |
| `--network none` | No network at all. |
| `--network allow --allow host:port` | Only the listed destinations (plus DNS). Names are resolved when you activate. |
| Root | The shell runs as root in a container, inside a VM: two walls between the code and your Mac. |

**The network policy limits where code connects, not what it sends.** With
internet access, code can upload anything inside the sandbox, including the
repo itself, or use your bandwidth and CPU. For code you actively suspect, use
`--network none` or an allowlist.

## Commands

| Command | |
|---|---|
| `activate` | Open a shell in the sandbox (creating or resuming it). `deactivate` or `exit` returns to the Mac and pauses the sandbox to disk, unless another session is open or `--keep-running`. |
| `exec -- CMD...` | Run one command and exit with its status (works in scripts). |
| `diff [-p] [PATH...]` | List what the sandbox added (A), modified (M) or deleted (D) relative to your Mac's copy; `-p` shows unified diffs. New directories are summarised (`A node_modules/ (5234 files)`). |
| `apply PATH... \| --all` | Copy those changes to your Mac (asks first; `--yes` skips). It refuses paths that would leave the repo, including through symlinked directories, and skips symlinks pointing outside it. |
| `reset` | Discard everything written in the sandbox. |
| `status`, `list` | The current repo's sandbox; all sandboxes. |
| `stop`, `rm` | Stop it (keeping the writable layer); delete it entirely. |

`activate` and `exec` take `--image` (default: the repo's
`.devcontainer/devcontainer.json` `image` or `build.dockerfile`, else
`debian:stable-slim`), `--network`, `--allow`, `--cpus`, `--memory` and
`--rosetta`. They're saved for the repo.

The repo is the current directory's git root, else a directory that already
has a sandbox above the current one, else the current directory. Monolith
looks for `.git` itself rather than running `git`, since an untrusted repo's
`.git/config` can make git run commands.

## Notes

- Servers the code starts on `0.0.0.0` are forwarded to `localhost` on the
  Mac. Servers bound to `127.0.0.1` inside the sandbox aren't reachable; start
  them with `--host 0.0.0.0` (or the tool's equivalent).
- File watchers inside the sandbox don't see edits you make on the Mac (the
  sandbox reads the new content, but no inotify event fires). Re-run the
  command, or edit inside the sandbox.
- Resuming a paused sandbox needs the Mac unlocked.
- `apply` brings the code's output onto your Mac. Review it with `diff -p`
  first, and be careful with anything executable or with build configuration.
