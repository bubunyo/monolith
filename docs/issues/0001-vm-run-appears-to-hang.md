# 0001 — `vm run` appears to hang at the boot banner

**Status**: Open (mitigated by `--log-console`; root cause not yet fixed)
**Date filed**: 2026-04-26
**Affected**: `monolith vm run`, `monolith vm attach`, anything that drives the guest serial console
**Severity**: High (the VM appears unresponsive to users; actual VM is running fine in the background)
**Workaround**: detach with `Ctrl+C` and inspect the VM via `monolith vm list`; tee console output via the new `--log-console PATH` flag

---

## 1. Report

The user ran `make run` (which expands to `./bin/monolith vm run`) and observed:

```
Booting VM "lith-68417" (1 vCPU, 512 MiB RAM, network: none)...
```

…after which the terminal sat with no further output for an arbitrarily long time. No Alpine kernel banner, no OpenRC boot lines, no MOTD, no `mono@<name>:~$` prompt. Pressing Enter produced nothing visible. The CLI never returned a prompt; the VM appeared dead but `monolith vm list` showed it as `running`.

Initial framing assumed an interactive autologin shell was reached but our pty wasn't echoing input back — i.e. a presentation problem in the client.

## 2. Environment

- macOS on Apple Silicon (the only platform monolith targets)
- Workspace binary: `/Users/bubunyo/workspace/bubunyo/monolith/bin/monolith` (codesigned, current branch)
- Stale installed binary: `/Users/bubunyo/go/bin/monolith` (older snapshot)
- Alpine assets in `~/.cache/monolith/alpine/`, last modified 2026-04-11:
  - `vmlinuz-virt` — `Linux kernel ARM64 boot executable Image, little-endian, 4K pages` (34 MiB)
  - `initramfs-virt` — `gzip compressed data` (9.4 MiB)
  - `modloop-virt` (15.9 MiB)
  - `minirootfs.tar.gz` (3.8 MiB)
- Six VMs already in `~/.cache/monolith/vms/`, three of them owned by `__vmd` processes spawned from the **stale installed binary**.

## 3. Tests run during diagnosis

### 3.1 Sanity: confirm the VM is actually running

```sh
ps aux | grep '__vmd lith-68417'      # daemon present, PID matches state.json
cat ~/.cache/monolith/vms/lith-68417/state.json
```

Result: PID alive, status `running`, but `network_mode: none` in `config.json`. So this VM was created with no NIC — uninteresting baseline. The "hang" was specific to a misconfigured cached VM.

### 3.2 Boot a fresh VM end-to-end

```sh
./bin/monolith vm run > /tmp/run.log 2>&1 &
sleep 15
kill -INT $!
cat /tmp/run.log
```

```
Booting VM "lith-85802" (1 vCPU, 512 MiB RAM, network: nat)...
Detached from lith-85802. VM is still running.
```

Daemon spawned, VM transitioned to `VirtualMachineStateRunning`, state file written. **`make run` itself works.** The original "hang" was attributable to attaching to `lith-68417` (the misconfigured VM) and never seeing console output because there was no console output to see.

This led to the deeper question: was console output ever appearing on this machine?

### 3.3 Capture the console of a freshly-booted VM

This was the pivotal test. The original `vm run` console relay only existed *while a client was attached* — there was no record of pre-attach guest output. To capture it, we built a daemon-side tee:

- `cli/vmd.go` — `--log-console PATH` flag on the hidden `__vmd` subcommand
- `vmd.go` — single always-on `runPtyReader` goroutine that drains the PTY slave and fans out to (a) the optional log file and (b) the optionally-attached console client
- `cli/vm/run.go` — `--log-console PATH` user-facing flag
- `client.go` — `SpawnVMDaemon` forwards the path to the daemon

After implementing and codesigning:

```sh
./bin/monolith __vmd test-console-debug --log-console /tmp/console-debug.log
# wait 16s
wc -c /tmp/console-debug.log
# 0 bytes
```

Zero bytes. Daemon log confirmed:

```
INF VM daemon "test-console-debug" started ...
INF tee'ing console for "test-console-debug" to /tmp/console-debug.log
INF runPtyReader started for "test-console-debug"
INF VM "test-console-debug" state changed: VirtualMachineStateStarting
INF VM "test-console-debug" state changed: VirtualMachineStateRunning
```

The reader was alive, the VM was running, but the slave PTY produced no bytes.

### 3.4 Cross-check: attach a real client and dump everything

```sh
./bin/monolith __vmd lith-85275 --log-console /tmp/lith-console2.log &
SOCK=~/.cache/monolith/vms/lith-85275/daemon.sock
( printf '\x01'; sleep 12 ) | nc -U "$SOCK" > /tmp/attached.bin
wc -c /tmp/attached.bin   # 1 byte (just the MsgConsoleAttach ACK)
wc -c /tmp/lith-console2.log  # 0 bytes
```

**Both paths empty.** Even an interactive client receives nothing past the protocol ACK. So this is not a tee bug — it is a complete absence of guest-to-host console traffic.

### 3.5 Bisect against `master` to rule out regressions

```sh
git stash
make build
rm -rf ~/.cache/monolith/vms/baseline-test
./bin/monolith __vmd baseline-test &
( printf '\x01'; sleep 25 ) | nc -U .../daemon.sock > /tmp/baseline-attached.bin
wc -c /tmp/baseline-attached.bin   # 1 byte
git stash pop
```

**Baseline `master` (before the tee changes) reproduces zero console bytes.** This is a pre-existing condition on the dev machine, not introduced by the `--log-console` work.

## 4. Assumptions made and how they were validated or refuted

| # | Assumption | Validation method | Result |
|---|------------|-------------------|--------|
| 1 | The VM is hung in early boot | `monolith vm list` plus `state.json` inspection | **Refuted** — VM is `running`, kernel is alive (state callback fires), `__vmd` PID is healthy |
| 2 | The console is producing output but nothing is reading it, so the kernel TTY buffer is full and the guest is blocked | Added always-on `runPtyReader` to drain the slave continuously | **Refuted** — even with a continuous reader, zero bytes flow |
| 3 | The CLI is dropping bytes between the daemon and the user terminal | Bypassed CLI: connected directly via `nc -U` and dumped raw bytes | **Refuted** — raw socket gets only the protocol ACK, no console bytes |
| 4 | The previous attach-time relay worked, so something in the new code path broke it | `git stash` to revert and rerun | **Refuted** — `master` reproduces the same emptiness, so the new code is not the regression source |
| 5 | The kernel command line accidentally suppresses output (`quiet` etc.) | Read `vmd.go:84`: `console=hvc0 root=/dev/vda rootfstype=ext4 rw modules=squashfs,loop` | **Inconclusive** — cmdline looks correct; `console=hvc0` should route output to the Virtio console |
| 6 | Alpine assets (kernel/initramfs/minirootfs) are corrupt or stale | `file` and `ls -la ~/.cache/monolith/alpine/` | **Plausible** — assets last touched 2026-04-11; structurally valid (kernel is a recognised ARM64 Image, initramfs is gzip), but content correctness not verified end-to-end |
| 7 | The misconfigured `lith-68417` (network: none) and other stale VMs were causing UI confusion | Inspected `config.json` of each | **Confirmed contributory** — the original "hang" report was specifically against a `network_mode: none` VM that auto-attaches with no useful prompt |

## 5. Diagnosis

There are **two distinct problems** that combined into the user-visible symptom:

### 5.1 UX problem (resolved)

`monolith vm run` prints `Booting VM ...` and then attaches to the guest console with **no further user feedback** until either (a) the guest emits something or (b) the user types something. When the guest is silent (e.g. `network_mode: none` plus a kernel that already reached the autologin shell on a previous boot, or a guest that isn't producing console output for any reason), the CLI looks indistinguishable from a hang.

The user had no way to confirm the VM was actually alive without leaving the attached session. There was also no record of any console output that occurred *before* a client attached.

### 5.2 Platform problem (still open)

On this machine, the guest serial console produces **zero bytes** from cold boot onward, even when a client attaches before the VM enters `VirtualMachineStateRunning`. This was verified by:

- Continuous reads of the PTY slave returning nothing
- Raw client attaches receiving only the protocol ACK byte
- Reproducing the same emptiness on `master` (pre-tee), ruling out our changes

The kernel must be either (a) not initialising the `hvc0` console driver, (b) panicking before any printk, (c) having its output routed to a different device, or (d) the Virtio console attachment is misconfigured between the master and slave PTY ends. Possibilities not yet eliminated:

1. **PTY raw-mode flags too aggressive.** `vmd.go:65–73` strips `ICRNL`, `IXON/IXOFF`, `ISTRIP`, `INLCR`, `IGNCR`, `OPOST`, `ICANON`, `ECHO*`, `ISIG`, `IEXTEN` and forces `CS8`, `VMIN=1`, `VTIME=0`. On macOS, certain combinations of these on the slave can cause reads to block forever even when bytes are available on the master side.
2. **Stale Alpine assets.** `vmlinuz-virt`, `initramfs-virt`, and `modloop-virt` were last refreshed 2026-04-11. A corrupted decompression in `decompressKernel()` could produce a kernel that fails to initialise its console early, with no visible error.
3. **Boot loader argument silently ignored.** `vz.WithCommandLine(...)` is passed `console=hvc0`, but if the kernel was built without virtio_console support (unlikely for `vmlinuz-virt`), output goes nowhere.
4. **`NewFileHandleSerialPortAttachment(ptm, ptm)`** uses the master PTY for both read and write. If on this machine the master end is in a state where the kernel TTY layer never delivers writes to the slave (e.g. line discipline mode mismatch between master open and slave open), bytes are silently lost.

None of these has been individually proven yet.

## 6. Solution / mitigation shipped

We added an opt-in `--log-console PATH` flag that **decouples observability from console attachment**. Even though the flag does not (currently) capture any bytes on this machine, the plumbing is in place and will work the moment the platform issue is fixed, with no further user changes.

### 6.1 Changes landed

- `vmd.go` — `RunDaemon(name, restore, consoleLogPath string)` opens the log file (`O_CREATE|O_APPEND|O_WRONLY`) and starts an always-on `runPtyReader` goroutine that fans out to the file and any attached client. Replaces the previous attach-time goroutine that started/stopped per client.
- `vmd.go` — `vmDaemon.consoleLog *os.File` field; `consoleCancel` is now nilable so re-attach after a forcibly-cancelled previous client doesn't panic.
- `cli/vmd.go` — `--log-console` flag on the hidden `__vmd` command.
- `cli/vm/run.go` — user-facing `--log-console` flag on `monolith vm run`.
- `cli/vm/resume.go` — passes `""` to `SpawnVMDaemon` (resume path doesn't tee).
- `client.go` — `SpawnVMDaemon(name, restore, consoleLogPath string)` forwards `--log-console` to the spawned daemon.
- `state.go` — `VMConsoleLogPath(name)` helper returns the conventional `~/.cache/monolith/vms/<name>/console.log` (not used by default; available to callers that want a stable path).
- `vmd_console_log_test.go` — new test verifies the plumbing (file created, daemon stays healthy, VM reaches running). Content assertion gated on `MONOLITH_TEST_CONSOLE_OUTPUT=1` to avoid flaking while the platform issue persists.

### 6.2 Side-benefit: the PTY drain itself

Even on systems where the kernel currently emits no bytes, the always-on drain prevents the long-term failure mode where a guest *does* eventually write a lot of data with no client attached and blocks on a full kernel TTY buffer. The previous design only drained while a client was attached.

### 6.3 Usage

```sh
monolith vm run --log-console /tmp/myvm.log
tail -f /tmp/myvm.log    # see kernel + boot output as it happens

# In tests, opt in to the strict content check:
MONOLITH_TEST_CONSOLE_OUTPUT=1 go test -run TestConsoleLogTee -v
```

## 7. Caveats

1. **The platform issue is unresolved.** `--log-console` is a diagnostic lever; it does not fix the underlying cause of console silence. Until that is fixed, the log file will exist but stay at 0 bytes on this machine.
2. **Strict content assertions are off by default.** `TestConsoleLogTee` deliberately does not fail when no bytes arrive, because that would mask unrelated bugs as console-tee bugs. Set `MONOLITH_TEST_CONSOLE_OUTPUT=1` to require boot markers (`Linux version`, `OpenRC`, `Welcome to Alpine`, or the `mono@<name>` prompt).
3. **Stale `__vmd` processes from a different binary location will reproduce confusing UI.** If an existing daemon was spawned from `~/go/bin/monolith` and you reattach from `./bin/monolith`, vmnet's same-executable check (CDHash on macOS <26.2, path on 26.2+) breaks named-network restoration — but the per-VM console pipeline is unaffected. Cleaning up old daemons is still good hygiene.
4. **The `consoleCancel` channel is now created lazily and nil-checked.** Forced detaches in rapid succession (e.g. test teardowns) used to be safe because `close()` happened exactly once per attach; with the always-on reader we may forcibly null the field from `runPtyReader`'s error path, so callers must always nil-check before `close()`.
5. **`runPtyReader` does not currently survive a transient PTY read error.** If `Read()` returns any error it logs and exits, leaving subsequent client attaches receiving nothing. This is consistent with the previous behaviour (the old goroutine also returned on error) but worth tightening before relying on this in production.
6. **The new tee writes raw bytes, including ANSI escape sequences and control codes.** Tools that read the file should be prepared for that; for human inspection a pipe through `cat -v` or `less -R` is useful.

## 8. Follow-up tasks

- [ ] **Find the actual cause of console silence.** Suggested path:
  1. Force a fresh download of Alpine assets (`rm -rf ~/.cache/monolith/alpine/`).
  2. Boot the VM with `console=hvc0 earlyprintk=hvc0 loglevel=8` and verify *anything* arrives.
  3. Try `console=ttyAMA0` as an alternative (Virtualization.framework also exposes a UART on aarch64 in some configs).
  4. Bisect: revert the PTY raw-mode termios call in `vmd.go:65-73` to default cooked mode and re-test.
  5. As a last resort, swap `NewFileHandleSerialPortAttachment(ptm, ptm)` for separate read/write file handles (the same `os.File` for both can theoretically deadlock if the kernel TTY enforces single-direction blocking).
- [ ] Decide whether `monolith vm run` should default `--log-console` to `~/.cache/monolith/vms/<name>/console.log` so post-mortem debugging requires no flag.
- [ ] Tighten `runPtyReader` to retry on transient errors, not exit on first failure.
- [ ] Add a `monolith vm console-log <name>` CLI command (or extend `vm logs` with a `--console` flag) so users don't need to know the on-disk path.
- [ ] Document in `docs/vm.md` the difference between `daemon.log` (Go logger output from the daemon process) and `console.log` (raw guest serial output).

## 9. References

- Affected files: `vmd.go`, `cli/vmd.go`, `cli/vm/run.go`, `cli/vm/resume.go`, `client.go`, `state.go`, `vmd_console_log_test.go`
- Related: `AGENTS.md` § "Boot Flow", "VM Configuration"
- Test: `go test -run TestConsoleLogTee -v -timeout 90s ./`
