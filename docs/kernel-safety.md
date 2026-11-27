# Linux Kernel Safety (eBPF Trampoline Detach Protection)

Keploy's eBPF capture paths (DaemonSet, `--low-latency` sidecar, Docker/Compose `--low-latency`, standalone `--network-io-metering`, scoped classic replay lifecycle, and OSS `unixconn` `fexit` detection) attach eBPF `fentry`/`fexit` trampoline programs. This document explains how Keploy protects its own trampoline detaches across Linux kernels `5.10+` against upstream **CVE-2026-98800** and kernel `-ENOMEM` unlink failures, the trampoline anchor links Keploy keeps while installed, kernel memory bounds, how to inspect nodes, and how to clean up or handle a stale-trampoline node alert.

## Background: Upstream Linux Trampoline Detach Hazards

### 1. Preempted-task UAF (CVE-2026-98800)

In Linux kernels `5.10.28` through `7.2.9` (including all current LTS kernels `5.10`, `5.15`, `6.1`, `6.6`, `6.12`, `6.18`, and `7.2.x`), detaching a BPF trampoline program (`fentry`, `fexit`, `fmod_ret`, or BPF LSM) while another program remains attached to the same kernel function frees the detached `struct bpf_prog` after a single normal RCU grace period (`call_rcu`), without waiting for an RCU-tasks grace period.

On a preemptible kernel, if a task was preempted in the old trampoline image between image entry (or the previous program's `rcu_read_unlock()`) and the detached program's `__bpf_prog_enter_recur()` / `prog->bpf_func`, the task resumes after the program's vmalloc/JIT memory has already been freed and triggers a kernel panic:

- **6.x / 7.x kernels:** `BUG: unable to handle page fault` in `__bpf_prog_enter_recur` reading `prog->active` (or executing freed JIT code).
- **5.10 kernels (`CONFIG_PREEMPT` / `PREEMPT_RT`):** page fault jumping into freed `prog->bpf_func`.

Linux `7.x` kernels default to `PREEMPT_LAZY` on stock `x86_64` boots (without passing `preempt=full`). Upstream Linux fixed CVE-2026-98800 in commit `0fb5751a6e41` ("bpf: Wait for an RCU tasks grace period before freeing trampoline progs") in **Linux 7.3** and **7.2.10** (and companion commit `17637e1a581a`, CVE-2026-98799, for sleepable trampoline programs). Keploy ships zero sleepable trampoline programs (`fentry.s`/`fexit.s`/`lsm.s`), so Keploy is not exposed to CVE-2026-98799.

### 2. Failed Unlink (`-ENOMEM`) and `SIGKILL` During Link Teardown

When `bpf_trampoline_unlink_prog` fails with `-ENOMEM` inside `bpf_tracing_link_release` — either under memory pressure or when `SIGKILL` is delivered mid-teardown and interrupts `pcpu_alloc`'s `mutex_lock_killable` under contention — the kernel logs `WARNING at kernel/bpf/syscall.c` (`RAX=0xfffffff4`) and leaves the old trampoline image installed and still calling the detached program, while releasing the link's reference to `struct bpf_prog`:

- **Non-last link (`tr->refcnt >= 1`):** If the program is unpinned before the stale trampoline image is replaced, the very next call to that kernel function jumps into freed `struct bpf_prog` memory (`prog+0x28` in `__bpf_prog_enter_recur`) and panics the node.
- **Last link (`tr->refcnt == 0` on Linux 6.12):** `bpf_trampoline_put` frees `tr` and `tr->fops` while the ftrace direct call still points at the stale image (a kernel ftrace UAF), and every subsequent `fentry`/`fexit` attach to that function fails permanently with `EBUSY` until the node reboots.

## How to Check Your Node's Kernel and Preemption Mode

Run on any Linux node:

```sh
uname -r
uname -v
```

- If `uname -v` contains **`PREEMPT`**, **`PREEMPT_RT`**, or **`PREEMPT_LAZY`**, or the kernel boot cmdline selects `preempt=full` or `preempt=lazy`, preempted-task trampoline UAF is reachable.
- The `-ENOMEM` / `SIGKILL`-during-teardown path is reachable on any kernel `5.10+` regardless of preemption mode.
- Keploy's host-bpffs protection runs on every Linux `5.10+` kernel (detecting dynamic `struct bpf_tramp_image` support via vmlinux BTF so `5.12+` uses `/proc/kallsyms` image-liveness tracking and `5.10–5.11` uses synchronous trampoline rebuild + `BPF_ENABLE_STATS` verification).

## How Keploy Protects Customer Nodes

Both OSS `keploy` and `enterprise` share a single implementation in `go.keploy.io/server/v3/pkg/agent/bpfsafety`:

1. **Dedicated host-bpffs grace root (`<host-bpffs>/keploy-grace/grace2-<nonce>/`):**
   - Before every `link.AttachTracing` call (primary hooks, backup hooks, lifecycle, kTLS, standalone netio, classic scope lifecycle, and OSS `unixconn`), `bpfsafety.PinBeforeAttach` pins the `bpf_prog` into a per-process directory under **`keploy-grace/`** on the **host** BPF filesystem: `/run/keploy/host-bpffs/keploy-grace/grace2-<nonce>/` in containers (mounted outside `/sys` by the Helm chart, webhook, replay pod spec, Docker launcher, and Docker Compose hook so private `/sys/fs/bpf` overlays and containerd AppArmor `/sys/fs/[^c]*/**` rules never interfere), or `/sys/fs/bpf/keploy-grace/grace2-<nonce>/` for bare native Linux hosts (auto-mounting `/sys/fs/bpf` only on bare hosts, never inside containers and never remounting read-only mounts).
   - Placing the grace root at `<host-bpffs>/keploy-grace/` keeps grace directories **completely outside** `<host-bpffs>/keploy/`, so `selfupdate.WipeBPFPins` and legacy map-pin cleanups never touch live or cooling grace directories. Using the `grace2-` prefix isolates image-liveness directories from older timer-only `grace-*` reapers during rolling upgrades or mixed-version runner nodes, while the current reaper safely reaps both `grace2-*` and legacy `grace-*` directories.
   - Each pinned program's kernel target function (`AttachTo`) is persisted per pin (`<pinName>=<targetFunc>`) in a `target_funcs` pinned BPF array map inside the grace directory so Linux `5.10–5.19` kernels (where `bpf_prog_info` does not populate `attach_btf_id`) and `CAP_BPF`-only module targets resolve targets per program via `/sys/kernel/btf/*` identically to `6.0+` kernels. `bpfsafety.PinBeforeAttach` refuses (`ErrUnresolvedTarget`) any Tracing/LSM program whose target cannot be resolved, and the reaper fails closed per program (keeping the whole directory pinned if any Tracing/LSM program in it is unresolved).
   - Each live agent holds an exclusive `flock(LOCK_EX)` on its `grace2-<nonce>` directory for its lifetime.
   - `bpfsafety.PinBeforeAttach` **fails closed by construction** (`ErrGraceNotArmed`) if `bpfsafety.Init` was not called.

2. **Pinned Trampoline Anchor Links (`<host-bpffs>/keploy-grace/anchors/<func>-link`):**
   - While Keploy is installed on a node with host bpffs, Keploy pins one tiny 2-instruction no-op `fentry` link (`mov r0, 0; exit`) per hooked kernel function (`vmlinux` and `tls.ko`, e.g., `tls_sw_sendmsg` / `tls_sw_recvmsg`) under `<host-bpffs>/keploy-grace/anchors/<func>-link` (alongside `<func>-prog`) **before** the first real attach on that function.
   - **Why:** On a DaemonSet or sidecar node, Keploy is usually the only BPF trampoline user on those functions. Keeping one no-op anchor link pinned guarantees the kernel trampoline's refcount never drops to 0 when an agent exits, rolls, self-updates, or is `SIGKILL`ed — preventing Linux 6.0+'s unrecoverable last-link `ftrace_ops_trampoline` UAF and permanent `EBUSY` until reboot, preventing Linux 5.10–5.11's static trampoline page free under `-ENOMEM`, and ensuring any failed `-ENOMEM` unlink is cleanly repaired by the next trampoline update.
   - **Measured idle overhead & memory footprint:**
     - Idle per-call overhead is **tens of nanoseconds per call** (within normal host/VM jitter). In an interleaved 21×200,000-call pinned-CPU KVM measurement on non-debug Linux `6.12.11`:
       - `unix_dgram_sendmsg` paired-delta median: `+72.6 ns` (IQR `+27.8..+117.9 ns`) and `+78.0 ns` (IQR `-87..+187 ns`).
       - `unix_dgram_connect` paired-delta median: `-1.0 ns` (IQR `-148..+43 ns`) and `+64.4 ns` (IQR `-14..+115 ns`).
       - The 2-instruction no-op program itself measures `24.5–26.1 ns/run` via `BPF_ENABLE_STATS`.
       - Reproducible benchmark source: `pkg/agent/bpfsafety/anchor_bench_linux_test.go` (`KEPLOY_RUN_ANCHOR_BENCH=1 go test -v -tags=integration -run TestAnchorIdleOverheadBenchmark ./pkg/agent/bpfsafety` inside a disposable VM).
     - Memory footprint: anchors hold **no BPF maps**; each anchored function occupies one BPF trampoline image (`~64–256 B` sub-page slot in `bpf_prog_pack` on **Linux `6.7+`** — `<8 KiB` for up to ~21 functions across full enterprise capture — or one `4 KiB` page per anchored function on **Linux `5.10–6.6`**, including Debian 12 / AL2023 `6.1`, Ubuntu 22.04 HWE `6.5`, and all `5.x` = `~84 KiB` for up to ~21 functions).
     - Note for kTLS: anchoring `tls_sw_sendmsg`/`tls_sw_recvmsg` holds a kernel module reference on `tls.ko` until reboot.
     - **Note for kernel livepatching on Linux `5.x` (`< 6.0`):** Prior to Linux `6.0` (which lifted the restriction via `host klp and bpf trampoline together`), ftrace direct calls carry `FTRACE_OPS_FL_IPMODIFY`. On Linux `5.x` nodes (such as Ubuntu 22.04 `5.15` with Canonical Livepatch or RHEL 8/9 with `kpatch`), a livepatch touching any anchored function (`tcp_sendmsg`, `tcp_recvmsg`, `udp_*`, `unix_*`, `do_splice`, etc.) will return `EBUSY` until the node reboots — including after Keploy is uninstalled. Leaving anchors pinned until reboot is deliberate: deferring a livepatch until reboot is strictly preferable to a node kernel panic.
   - **Lifecycle & removal (same on every Linux kernel):** **No Keploy path ever removes an anchor's last link on any kernel** — not the reaper, not `CleanupAnchors`, not `keploy agent cleanup`, not an uninstall hook, and not a failed `l.Pin()` under memory pressure. If `l.Pin` fails in `ensureAnchor` (for example under memcg or global `-ENOMEM`), Keploy retains the live link FD in-process and retries pinning on every `15s` reaper round and at `GracefulShutdown` (keeping still-unpinned FDs referenced across shutdown and Go GC until process exit). **Remaining unpinned-anchor window:** once `l.Pin` fails, the anchor remains unpinned until a retry succeeds (at least one `15s` reaper round, and for as long as the memory pressure persists). Any process exit during that unpinned window — `SIGKILL`, OOM kill, a crash, or a graceful exit after `SIGTERM` while retries are still failing — causes the kernel to close the unpinned link FD (dropping that function's last link if no other link is attached; on graceful exit under memcg pressure alone, kernel memcg charges are forced for exiting tasks, so hitting `-ENOMEM` on that close requires global memory pressure). Running `keploy agent cleanup --host-bpffs /sys/fs/bpf` after stopping all Keploy agents safely reaps dead map-bearing agent grace directories (on `5.12+` with visible kallsyms it also reaps its temporary rebuild directory once `/proc/kallsyms` clears; on `5.10–5.11`, hidden kallsyms, or `-ENOMEM` during cleanup, it never reaps on `BPF_ENABLE_STATS` alone and leaves at most one tiny mapless rebuild no-op directory pinned for the next agent's reaper or reboot) and prints:
     ```text
     N idle keploy anchors stay until the next reboot (safe; idle overhead tens of ns/call; on Linux 5.x livepatches touching anchored functions wait for a reboot)
     ```

3. **Pre-Detach Disarm + First-in-Teardown Fast Link Close:**
   - On shutdown, `bpfsafety.GracefulShutdown` (and enterprise's `GracefulShutdownProgGrace`) runs **first** in process teardown — before TLS uprobe removal, proxyless map cleanup, coverage flushes, Podman shutdown, or noise learning — so tracing links detach in milliseconds, well inside supervisor (`25s`), kubelet (`30s`), and Docker (`10s`) stop deadlines.
   - The background reaper is cancelled and drained **before** any detach hook runs, preventing concurrent rebuilds mid-teardown.
   - Registered disarm hooks (`bpfsafety.RegisterDisarmHook`) clear targeting/filter maps (`target_namespace_pids`, `keploy_client_kernel_pid_map`, `keploy_agent_kernel_pid_map`, `keploy_client_registration_map`, `keploy_agent_registration_map`, `target_cgroup_ids`, `netio_target_cgroups`) synchronously **before** any tracing link closes, ensuring that even a program momentarily left in a stale trampoline image immediately returns without doing work.
   - Process exit is **never blocked**: after all detach hooks finish, the agent persists the **union of pre-detach and post-detach** `/proc/kallsyms` trampoline image snapshots and leaves its `grace2-<nonce>` directory pinned on host bpffs (releasing `flock` only when the process terminates) for the cross-process image-liveness reaper.

4. **Zero-Timer `/proc/kallsyms` Image-Liveness Reaper (No Blind Timers):**
   - Keploy **never** unpins a BPF program based on a blind elapsed-time timer. A dead grace directory is unpinned **only** once the reaper proves no trampoline image that could call its programs still exists in the kernel:
     - **Clean exit:** Before closing links, the exiting agent snapshots the exact kernel addresses of `bpf_trampoline_<key>` in `/proc/kallsyms` into a pinned BPF array map (`kallsyms_snapshot`) in its grace directory.
     - **Crash / `SIGKILL`:** A dead directory with no clean snapshot is marked on pass 1 (`stateDeadPass1`), then on pass 2 the reaper snapshots live `/proc/kallsyms` addresses and immediately forces a trampoline rebuild for every target function (deduplicated per round).
     - **Reaping rule:** On subsequent passes, the reaper re-reads `/proc/kallsyms`. As soon as **none** of the snapshotted trampoline image addresses remain (typically within ~1–15 seconds after the kernel's `bpf_tramp_image_free` RCU-tasks callback completes), the reaper safely unpins and removes the dead directory.
     - **Stuck image recovery (`-ENOMEM` unlink):** If a snapshotted trampoline address is still present in `/proc/kallsyms`, the reaper forces a trampoline rebuild by attaching and immediately detaching a cached 2-instruction no-op `fentry` program (itself pinned inside the reaper's own active grace directory before its first attach). That replaces the stuck stale image, after which `/proc/kallsyms` clears and the dead directory is reaped safely. If any Tracing/LSM program's target cannot be resolved, the reaper **fails closed** and never unpins it.
     - **Fallback when `/proc/kallsyms` addresses are hidden** (`bpf_jit_kallsyms=0`, `bpf_jit_harden!=0`, or `kptr_restrict` hides addresses): the reaper forces a rebuild on every resolved target, enables `BPF_ENABLE_STATS(BPF_STATS_RUN_TIME)` for ~2 seconds, and verifies cumulative `run_cnt` across all pinned programs stays flat. If `BPF_ENABLE_STATS` is unavailable, no target was resolved, or `run_cnt` increases, the reaper **fails closed** and keeps the pins indefinitely rather than risking a UAF.

5. **Permanent Last-Link `EBUSY` Stale-Trampoline Alert & Drain-and-Reboot Procedure:**
   - If any `fentry`/`fexit` attach (agent hook, anchor, or reaper rebuild) returns `EBUSY`, that indicates a permanent last-link stale trampoline on the node.
   - Keploy keeps the affected program pinned until reboot, degrades the affected hook cleanly without crash-looping, sets Prometheus gauge `keploy_bpf_stale_trampoline=1`, logs a loud `WARN` (`"drain and reboot this node"`), and sets `bpf_stale_trampoline: true` in CLI telemetry.
   - **Operator action when `keploy_bpf_stale_trampoline == 1`:** cordon, drain, and reboot the affected node. Do **not** manually delete pinned programs from `/sys/fs/bpf/keploy-grace/` before rebooting.

6. **Honest Unprotected Mode (When No Host bpffs Is Reachable):**
   - If no writable host bpffs mount is reachable (for example, a customer-managed pod manifest or an older Helm chart missing `/run/keploy/host-bpffs`), Keploy **never** refuses to start or aborts recording (no exit code `6`).
   - Instead, Keploy enters honest unprotected mode, logs one clear `WARN`:
     ```text
     eBPF crash protection is OFF: mount the host's /sys/fs/bpf at /run/keploy/host-bpffs (see docs/kernel-safety.md)
     ```
     sets `keploy_bpf_unprotected_trampolines=1` and `keploy_bpf_grace_mode{mode="unprotected"}=1`, and flags `bpf_unprotected_trampolines: true` in CLI telemetry.
   - No anchor links can be pinned in unprotected mode. Mount the host's `/sys/fs/bpf` at `/run/keploy/host-bpffs` to re-enable full host-bpffs protection.

7. **Observability (Prometheus Metrics & Telemetry):**
   - `keploy_bpf_prog_grace_dirs`: number of `grace2-*` / `grace-*` directories currently under `<host-bpffs>/keploy-grace/`.
   - `keploy_bpf_prog_grace_pinned_progs`: total pinned BPF programs held across grace directories.
   - `keploy_bpf_anchor_links`: number of pinned no-op trampoline anchor links under `<host-bpffs>/keploy-grace/anchors/`.
   - `keploy_bpf_unprotected_trampolines`: `1` when running without host-bpffs protection, `0` otherwise.
   - `keploy_bpf_stale_trampoline`: `1` when a permanent stale trampoline (`EBUSY`) requires draining and rebooting the node, `0` otherwise.
   - `keploy_bpf_grace_mode{mode="host-bpffs"|"unprotected"}`: `1` on the active grace mode label, `0` on the inactive label.
   - CLI telemetry super-properties: `bpf_unprotected_trampolines: true` and `bpf_stale_trampoline: true` (omitted when false).

8. **Kernel Memory Cost and Steady-State Bounds:**
   - A pinned `bpf_prog` holds references to its `aux->used_maps` (`~185 MiB` at default 1 GiB container / 64 MiB ringbuf; `~377 MiB` at 256 MiB ringbuf; `~4–5 MiB` for OSS `unixconn` alone) only until its trampoline image disappears from `/proc/kallsyms`.
   - Because `/proc/kallsyms` image-liveness reaping runs every `15s` and kernel trampoline images normally free within `~1s` after link close, a dead directory from a rollout, self-update swap, or crash is typically reaped within **15–30 seconds** (rather than sitting for 15+ minutes).
   - Anchor links hold **no BPF maps** (`~64–256 B` sub-page slot per anchored function on Linux `6.7+`, or one `4 KiB` page per anchored function on Linux `5.10–6.6`) and remain pinned until the next node reboot.
   - On an uninstalled node with no successor agent, the last exited agent's map-bearing `grace2-*` directory is safely reaped by `keploy agent cleanup` (which runs the zero-timer `/proc/kallsyms` image-liveness reaper, leaves idle anchors untouched until reboot, and on `5.10–5.11` / hidden kallsyms / `-ENOMEM` keeps at most one tiny mapless rebuild no-op directory pinned safely until the next agent or reboot rather than reaping on stats alone).

9. **Cold Start-Up Probe (`pkg/kernelcap`):**
   - `ProbeTracingAttach` attaches to `bpf_fentry_test1` (invoked only by `BPF_PROG_TEST_RUN`, never by live workload traffic) instead of `tcp_close`, eliminating the start-time detach race on `tcp_close`.

## Upgrade Note & Safe Cleanup on Uninstall

- **Upgrade coupling:** Upgrade the `k8s-proxy` chart and the fixed enterprise agent **together** (re-rendering GitOps manifests if using `helm template`). Replacing a pre-fix agent performs one final pre-fix detach per node because the running pre-fix binary did not pin its programs or anchors.
- **Policy preconditions:** DaemonSet pods, webhook sidecars, and `replay.stackMode` DinD stack pods mount hostPath `/sys/fs/bpf` at `/run/keploy/host-bpffs` (clusters with Gatekeeper/Kyverno `allowedHostPaths` must permit `/sys/fs/bpf`), and nodes must have `bpffs` mounted at `/sys/fs/bpf`.
- **NEVER `rm -rf` `/sys/fs/bpf/keploy-grace/` while any Keploy agent is running (or if `keploy_bpf_stale_trampoline=1`):** Unpinning a live program or a program referenced by a stale trampoline image exposes the node to a kernel panic.
- **Cleaning up exited grace directories on uninstall:** Stop all Keploy agents on the node, then run:

```sh
keploy agent cleanup --host-bpffs /sys/fs/bpf
```

`keploy agent cleanup` refuses to run while any live Keploy agent holds a grace lock, safely reaps exited map-bearing `grace2-*`/`grace-*` directories (never unpinning a rebuild no-op on stats alone), never detaches any anchor link, and prints how many idle anchors remain until the next node reboot (`N idle keploy anchors stay until the next reboot (safe; idle overhead tens of ns/call; on Linux 5.x livepatches touching anchored functions wait for a reboot)`).

## Third-Party eBPF Tools Sharing Trampolines

BPF trampolines are node-wide per kernel function. Keploy's grace pins and anchor links protect **Keploy's** own detached programs on `5.10+` kernels. If a third-party eBPF agent (such as Cilium, Tetragon, Datadog system-probe, or Pixie) attaches and detaches its own `fentry`/`fexit` programs on the same kernel functions, Keploy cannot hold references to another tool's programs from userspace. Full protection for third-party trampoline detaches requires upgrading the host kernel to **Linux 7.2.10+** or **7.3+** (or a distro kernel carrying upstream commit `0fb5751a6e41`).
