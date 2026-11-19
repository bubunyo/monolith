---
title: 'Initial RAM disk and PID 1 for monolith containers'
description: "How the Linux boot hands over to PID 1, and what it took to write my own init so each container runs in its own VM on a Mac."
comments: true
keywords: "pid 1, init, initrd, initramfs, linux boot, containers, virtualization.framework, apple silicon, macos, vsock, ttrpc, kata containers, apple container, golang, tech"
tags: [linux, containers, macos, golang]
published: true
---

[monolith](https://github.com/bubunyo/monolith) started as a way to run Linux
VMs on my Mac. It sits on top of Apple's
[Virtualization.framework](https://developer.apple.com/documentation/virtualization), and over time it grew three ways:

- `monolith vm` for a Linux machine to work in;
- `monolith docker` for a Colima-style Docker host, so the `docker` CLI on the
  Mac has somewhere to send its containers;
- `monolith sandbox` for running a repo I don't trust, where only the repo is
  visible and the code can reach the internet but not my Mac or my network (still WIP).

All of them work the same way underneath: one long-lived VM, and whatever you
run lives inside it. For Docker that means every container shares the VM's
kernel. That's how Docker works everywhere, and for the Docker ecosystem
(Compose, buildx, Kubernetes) it's what you want.

But most of the time I'm not using the ecosystem. I'm running one image to try
something: a Postgres database for some random app, a Redis container because
why not, a tool I don't want to install, an image someone sent me. For that,
the Docker host is a lot of machinery. A VM has to be up, a daemon has to be
running inside it, and all those containers share one kernel, so they're only
as isolated from each other as that kernel keeps them.

Then there's setting things aside. `docker pause` freezes a container, but its
memory stays allocated. The only way to really get the resources back is to
stop it, or to suspend the whole Docker VM along with everything else in it. I
wanted something simpler: run an image, put it away and free what it was
using, run something else, and come back to the first one later without losing
a beat.

So I started thinking: if starting a VM on Apple Silicon is already cheap, why
keep one big VM around at all? What if every container got its own small VM,
with its own kernel, that exists only as long as the container does?

My answer to that question became `monolith container`: a container engine where
`monolith container run nginx` pulls an OCI image, turns it into a disk, and
boots a VM whose only job is to run nginx. Making it work the way I wanted
turned out to be less trivial than I expected. It turns out that for it to
work exactly as I wanted, I had to write the program the kernel runs first,
from the initial RAM disk (initrd): `/init`, which becomes PID 1. This essay is
about how I built that piece for monolith's containers, and how the pieces fit
together.

## Why not just use something that exists?

"A VM per container" isn't a new idea. Two projects already do it, and I
looked hard at both and took inspiration from them.

[Kata Containers](https://github.com/kata-containers/kata-containers) is the
best-known answer to "what if each container had its own kernel?". It plugs
into containerd and Docker as a runtime: when you start a container, Kata boots
a small VM and runs the container inside it, with an agent in the guest taking
orders from the host.

monolith already knows about Kata. `monolith vm --image kata` boots Kata's guest
kernel and rootfs, mostly because they boot fast. But that rootfs isn't a
machine you work in. It's a minimal, read-only image built to host the Kata
agent, which made me question what the point of it was outside Kata.

The obvious next idea was to make Kata the runtime of `monolith docker`. That's
where it falls apart on a Mac. Kata's runtime expects a Linux host with KVM and
drives hypervisors like QEMU, Cloud Hypervisor or Firecracker. macOS has none
of those; it has Virtualization.framework. So Kata would have to run inside a VM running
Docker, and every container would become a VM nested inside that VM.
So you end up with two nested layers of virtualization just to run one container.
The isolation would be real, but nothing else about it would be cheap. Not good enough.

In 2025 Apple released [`container`](https://github.com/apple/container) and it looked
like most of what I wanted:

- every container gets its own lightweight VM on Virtualization.framework, with
  no shared Linux VM in the middle;
- the image's filesystem becomes an ext4 block device for that VM;
- inside the guest, PID 1 is Apple's own small init, `vminitd`, which the host
  talks to over [vsock](https://man7.org/linux/man-pages/man7/vsock.7.html).

I tried it, and I liked the experience a lot. `container run` feels like
`docker run`, but there's no big VM to start first and nothing to keep
running in the background. Each container just shows up in its own VM and goes
away when it's done. It proved the model works on a Mac, and that it can feel
fast.

That experience is what I wanted, so I set out to recreate it inside monolith.
That way a container would be just another monolith VM, sitting next to the
ones from `monolith vm`, `docker` and `sandbox`, and it would get what those
already have: a daemon per VM, named networks, a shared cache, `pause` and
`resume`, and one `vm list` showing everything that's running along with what runs on them.

To build it myself, I had to learn what it takes to boot a VM and turn it
into a container: everything between the kernel starting and nginx answering a
request. I had a working knowledge of the Linux boot process, but this project
crystallized it in a way I didn't expect. Happy accident.

## How the Linux boot process works and where monolith comes in

Before getting into the design, it helps to walk through what normally happens
when a Linux machine starts. On a typical PC or server:

1. The **firmware** (UEFI) starts and runs a **bootloader**, usually GRUB.
2. GRUB loads two files into memory: the **kernel**, and an [**initrd** (initial
   RAM disk)](https://docs.kernel.org/admin-guide/initrd.html). Then it starts the kernel with a command line, a string of
   options like `root=/dev/sda2 quiet`.
3. The kernel sets up memory, CPUs and its built-in drivers. Then it unpacks
   the initrd into a filesystem in RAM and runs the program at `/init` in
   it. That program is process 1, with **PID 1**.
4. The initrd's `/init` has one job: find the real root filesystem. It loads
   the storage drivers that weren't built into the kernel, unlocks encrypted
   disks, assembles RAID, waits for slow devices. Then it mounts the real root
   and switches into it with [`switch_root`](https://man7.org/linux/man-pages/man8/switch_root.8.html).
5. `switch_root` hands over to the distro's real init, typically systemd. It
   takes over as PID 1 and starts everything else: logging, networking,
   services, a login prompt.

The initrd itself is less magic than it sounds. It's a [cpio archive](https://docs.kernel.org/filesystems/ramfs-rootfs-initramfs.html) (the
`newc` format, an old Unix archive format a bit like tar) that the kernel
unpacks into RAM. A tiny, temporary filesystem whose only purpose is getting
from "the kernel is running" to "the real root disk is mounted".

### What the initrd's `/init` has to do

The `/init` sets up just enough to reach the real root filesystem. That comes
down to:

1. **Mount the kernel's pseudo-filesystems:** `/proc` (processes and kernel
   state), `/sys` (devices and drivers) and `/dev` (a `devtmpfs`, where the
   kernel creates a device file for each disk it finds). Without them it can't
   even see the disks.
2. **Make the root disk appear:** load storage drivers as modules, wait for
   slow devices, unlock encryption, assemble RAID or LVM. This is the part that
   varies most between machines, and the reason initrds exist at all.
3. **Mount the real root filesystem** somewhere temporary, like `/newroot`.
4. **Switch into it:** move `/proc`, `/sys` and `/dev` across, delete the
   initrd's files to give the RAM back, make the new root `/`, and `exec` the
   real init. `exec` replaces the program but keeps the process, which is how
   systemd starts life as PID 1. This step is what `switch_root` does.
5. **Never exit.** If PID 1 exits, the kernel panics. An initrd's `/init`
   avoids that by `exec`ing, so it never returns.

A VM on Virtualization.framework skips the firmware and GRUB entirely: for
container VMs, monolith hands the kernel, the initrd and the command
line straight to
[`VZLinuxBootLoader`](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/vm/vmd.go#L122-L127),
and the kernel starts directly. The command line a container VM boots with is
[`console=hvc0 quiet loglevel=1 ip=dhcp rdinit=/init panic=-1`](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/vm/container.go#L44-L47).

### Where monolith comes in

monolith builds the initrd itself, in a
[few lines](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/vm/guestinit/guestinit.go#L33-L72),
the first time a container starts. It holds three empty directories and one
file, `/init`, which is `monolith-init`.

From there, `monolith-init` works through the same five steps:

1. **Pseudo-filesystems:** it
   [mounts `/dev` and `/proc`](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/cmd/monolith-init/main_linux.go#L83-L90)
   in the initrd first, so it can see the disk and read what the kernel
   learned by DHCP.
2. **Making the disk appear** is free. The disk is always `/dev/vda`, and its
   driver is built into the kernel, so there's nothing to load or wait for.
3. **Mounting the container image file system as root:** it
   [mounts the image's filesystem](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/cmd/monolith-init/main_linux.go#L96),
   which monolith
   [converted to an ext4 disk](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/oci/convert.go#L60-L72)
   when it pulled the image, at `/newroot`, then mounts `/proc`, `/sys`, `/dev` and friends inside it.
4. **Switching** is only half done. It
   [moves into the disk](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/cmd/monolith-init/main_linux.go#L139-L147)
   the same way `switch_root` does, but it `exec`s nothing.

This is where it breaks from the usual sequence. A normal initrd would now
`exec` the real init and disappear. `monolith-init` doesn't hand over to
anyone. There's no systemd in an nginx image, and there shouldn't be. It stays
running as PID 1, now inside the image's filesystem, and the container's
process (nginx, postgres, your shell) becomes its child.

That leaves step 5, the one `monolith-init` keeps for good: it never exits.
When the container's process ends, it doesn't return. It reports the exit code
to the Mac and [powers the VM off](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/cmd/monolith-init/main_linux.go#L67-L73)
itself.

```
A typical distro                       A monolith container

firmware (UEFI)                        VZLinuxBootLoader
GRUB                                     │
kernel                                 kernel
initrd /init  ── finds the root disk   monolith-init (from the initrd)
switch_root                              ├─ mounts the image disk, moves into it
systemd (PID 1)                          └─ stays PID 1
services, login                              └─ nginx
```

This is also why the kernel matters. The initrd has no kernel modules in it,
so everything this boot needs has to be built into the kernel: virtio for the
disk and console, ext4, vsock to talk to the Mac, cgroup v2, and the in-kernel
DHCP behind `ip=dhcp`. Kata's guest kernel has all of that built in and boots
fast, so rather than configure my own, monolith reuses the Kata guest kernel
for every container.

Staying on as PID 1 sounds like a small detail, but it means `monolith-init`
inherits every duty the kernel gives process 1. That's where most of the
interesting work turned out to be.

## What else PID 1 has to do

By the end of the boot, `monolith-init` is PID 1 inside the image's
filesystem, ready to start the container's process. To make containers work
the way I wanted, it had to do a few other things too. The first is taking
orders from the Mac.

A container should be able to run in the background, outlive the terminal
that started it, and be attached to again later. A Virtualization.framework VM
can't do that on its own: it lives inside the process that created it, and
dies with it. So the CLI doesn't run the VM itself. For each VM, monolith
[spawns a daemon](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/cli/container/run.go#L171-L186),
`monolith __vmd <name>`, which owns the VM, stays up while it runs, and exits
when it stops. (How that daemon works is a post of its own.)

```
 The Mac                                  The container's VM

 monolith __vmd <name>  ── vsock ──────▶  monolith-init (PID 1)
 (the daemon)           ◀─ console ────     └─ your process
```

The daemon is what actually runs the VM, so the whole control plane comes down
to handing commands to the daemon and the daemon passing them on to the VM.
For a container, that means giving the init orders: run this process, tell me
when it exits, signal it, resize its terminal. monolith does this with RPC
over
[vsock](https://man7.org/linux/man-pages/man7/vsock.7.html), a socket family
designed for exactly this: it connects a VM to its host the way a unix socket
connects two processes, with no network involved. On top of it runs protobuf
over [ttRPC](https://github.com/containerd/ttrpc), containerd's lighter
version of gRPC. It keeps gRPC's service definitions and generated code but
drops HTTP/2, which suits a process that's the only thing running in its VM.
It's what containerd uses to talk to its shims, and what the Kata agent uses
over vsock.

The init process
[serves the API on vsock port 1025](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/cmd/monolith-init/agent_linux.go#L38-L55),
and the whole thing is
[one small service](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/containerinit/agentpb/agent.proto#L15-L34)
of five calls: `Start`, `Wait`, `Signal`, `ResizeTTY` and `Shutdown`.

The process spec arrives in `Start` rather than being baked into the initrd,
which is why one cached initrd serves every container. The spec is everything
`docker run` would work out: the command, environment, user and working
directory. One field is `tty`. With `-it`, the init runs the process on the
VM's console as its controlling terminal, with stdin attached, so a shell gets
job control and Ctrl+C. Without it, stdin is `/dev/null`. For a foreground
run, the daemon holds `Start` back until your terminal is attached, so the
first line of output isn't lost.

The rest of the job comes from being PID 1. On a normal Linux system you never
think about it: systemd is there, it works, and nobody writes their own. But
the kernel treats process 1 differently from every other process, and once
`monolith-init` stays on as PID 1, every one of those differences becomes its
problem.

There's a familiar version of this in Docker, where your process *is* PID 1
inside the container. That's why Ctrl+C sometimes does nothing there, and why
[tini](https://github.com/krallin/tini) and `docker run --init` exist. In
monolith the roles are split: `monolith-init` is PID 1, and nginx or postgres
runs as its child, as an ordinary process. So the init has to get the
following right, so that the container's process can just focus on what the container's authors
want it to do.

The first difference: the kernel ignores signals sent to PID 1 unless it has a
handler. For any other process, a signal it hasn't set up a handler for does its
default thing: `SIGTERM` and `SIGINT` end the process. PID 1 is the
exception. The kernel won't deliver a signal to it unless it has installed a
handler, so a `SIGTERM` to init simply does nothing. That protects the system
from someone accidentally killing init, and it means PID 1 has to choose which
signals it cares about.

The obvious move is to ignore them, but that is a wrong move. When a process
calls `exec` to become a new program, the kernel keeps some of its signal
settings and drops others. "Ignore this signal" is kept. A handler is dropped,
because the handler's code belonged to the old program. So if the init
*ignored* `SIGTERM`, every process it started would inherit that, and nginx
would never hear `monolith container stop`'s `SIGTERM` unless it set up a
handler of its own. A handler doesn't have that problem: after `exec`, the
child starts with normal behaviour. So
[the first thing `monolith-init` does](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/cmd/monolith-init/main_linux.go#L36-L38)
is install handlers that do nothing.

Getting signals *to* the container's process is then simple. When you run
`monolith container stop`, the daemon asks the init over vsock to
[deliver a signal](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/cmd/monolith-init/agent_linux.go#L101-L112),
and it calls `kill` on the process. The daemon picks the image's own
`STOPSIGNAL` if it has one, the way Docker does, so an image that wants
`SIGQUIT` to shut down gracefully gets `SIGQUIT`. If it hasn't exited after
`--time` seconds (10 by default), the daemon
[sends `SIGKILL`](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/vm/container_agent.go#L177-L193).
And if even that doesn't stop it, the daemon
[pulls the VM's power](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/vm/vmd.go#L718-L737).
A container can't outlive `stop`.

PID 1 also has to adopt every orphan.
When a process exits, it stays in the process table as a zombie until its
parent collects its exit status with `wait`. If the parent has already gone,
the kernel hands the orphan to PID 1, and PID 1 has to collect it. A zombie uses
almost no memory, but it holds on to its process ID, and process IDs are
limited (`pid_max`, typically 32,768). An init that doesn't collect them
slowly fills the process table, until `fork` fails and nothing in the
container can start a new process, not even a shell. A container running
a shell script that starts background jobs makes orphans all the time.

So `monolith-init` doesn't just wait for the container's process. It
[waits for *any* child](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/cmd/monolith-init/main_linux.go#L231-L250),
throws away the ones it doesn't care about, and stops only when the main one
exits.

The `128 + signal` is the shell's convention, which
[Docker follows too](https://github.com/opencontainers/runc/blob/v1.3.0/libcontainer/utils/utils.go#L19-L24),
through runc: a
process killed by `SIGKILL` (9) exits with 137.

If PID 1 exits, the kernel panics. In a container VM that's not a disaster,
since `panic=-1` ends the VM straight away, but it's a terrible way to
report a typo in your command. The person at the terminal would see a dead VM
and no error. So nothing in `monolith-init` is allowed to take it down.
[Setup recovers from panics](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/cmd/monolith-init/main_linux.go#L78-L82),
and every failure, from a disk that won't mount to a command that doesn't
exist, becomes an exit code the daemon can report:
[125, 126 or 127](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/containerinit/spec.go#L29-L34),
the same codes `docker run` uses for "the runtime failed", "not executable"
and "not found". The init then shuts down normally. A broken container looks
like a failed command, not a broken machine.

In Docker, a runtime called runc also sets up a lot of small things before a
container's process starts, and images quietly depend on all of them. With no
runc, the init does them itself: it
[creates the links in `/dev`](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/cmd/monolith-init/main_linux.go#L121-L137)
that images log to, like `/dev/stdout`; it
[writes the hostname, `/etc/hosts` and `/etc/resolv.conf`](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/cmd/monolith-init/main_linux.go#L151),
using the DNS servers the kernel learned from `ip=dhcp`; it
[resolves the image's `USER`](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/containerinit/user.go#L22-L26)
against the image's own `/etc/passwd` and `/etc/group`; it
[raises the open-file limit](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/cmd/monolith-init/agent_linux.go#L167-L172)
to Docker's; and it
[grows the filesystem](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/cmd/monolith-init/grow_linux.go#L14-L17)
to fill the container's `--disk` size. Most of these were bugs first, and the
fix was me finding out which detail Docker had been taking care of all along.

When the main process exits, the container is over, but there may still be
stragglers: daemons it started, background jobs. The init
[sends everything left a `SIGTERM`, then a `SIGKILL`](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/cmd/monolith-init/main_linux.go#L252-L267),
collects the zombies, flushes the disk and remounts the root filesystem
read-only, so the next `start` has no journal to replay. Then it
[powers the VM off](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/cmd/monolith-init/main_linux.go#L67-L73).
Not exits: powers off. PID 1 doesn't get to exit.

There's one catch in that last step. The exit code reaches the Mac as the
reply to the daemon's `Wait` call, and powering off is instant. If the init
powered off the moment the process exited, the reply could still be in flight
when the VM disappeared, and the CLI would never learn how the container
ended. So the init waits for permission.
[After cleaning up](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/cmd/monolith-init/main_linux.go#L52-L64),
it waits, for up to two seconds, for the daemon to call `Shutdown`. The daemon
only does that
[once it has the exit code and has written it to disk](https://github.com/bubunyo/monolith/blob/827f9fab90c6bba762680fecdca145ceda735590/vm/container_agent.go#L142-L147).
Then the VM goes away.

## Conclusion
So there. Everything it takes in PID 1 to run a container. Other init systems do more, because people
expect more of them. But for monolith containers, it has one job; here is an image, run it. It means
we get to skip a lot of the details that make most Linux systems work the way they should. For containers,
it turns out, this is quite enough.
