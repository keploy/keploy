// SPDX-License-Identifier: GPL-2.0
//
//go:build ignore
//
// cbshim.bpf.c — eBPF replacement for the LD_PRELOAD channel-binding
// shim. Solves the SCRAM-SHA-256-PLUS-across-MITM problem by hooking
// X509_digest in any process whose tgid the agent has allowlisted, and
// substituting the MITM cert's hash with the upstream cert's hash
// before libpq reads it into the SCRAM proof.
//
// Two probes, attached globally per libcrypto file (no kernel-side PID
// filter — the BPF program does its own two-stage filtering, which
// scales better than re-attaching per-PID for gunicorn-style apps with
// 10+ worker processes per master).
//
//   uprobe   on X509_digest entry — three-stage filter:
//     1) target_namespace_pids[tgid] match (the cheap allowlist the
//        agent maintains, shared with the rest of the eBPF tooling so
//        a single /proc walk feeds every probe).
//     2) caller's return address falls inside libpq's mapped range
//        for this tgid (libpq_ranges) — keeps internal libcrypto
//        callers from being clobbered when their smaller md buffers
//        would overflow under a 32-byte write.
//     3) record the md pointer for the uretprobe.
//
//   uretprobe on X509_digest return — reads the hash X509_digest wrote,
//   looks it up in the cbmap (mitm_hash → real_hash, populated by
//   keploy's proxy at upstream-dial time), and overwrites md with the
//   real hash on match.

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_tracing.h>
#include <bpf/bpf_core_read.h>

char LICENSE[] SEC("license") = "GPL";

#define HASH_LEN           32      // SHA-256
#define MAX_RANGES_PER_PID  4

// target_namespace_pids — Stage-1 PID allowlist, now a CACHE rather
// than the source of truth. Populated lazily by the uprobe itself on
// first X509_digest fire from a given tgid: if task_in_agent_ns()
// returns true (current task is in the agent's PID namespace), the
// tgid is added here so subsequent fires from the same tgid hit the
// cheap one-lookup fast path. Eliminates the userspace polling loop
// that previously had to /proc-walk descendants every 2s.
//
// Sized for K8s DaemonSet — ~500 pods × ~30 PIDs/pod + headroom.
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 262144);
    __type(key, __u32);    // tgid
    __type(value, __u8);   // flag
} target_namespace_pids SEC(".maps");

// cbshim_agent_info — single-entry map carrying the keploy agent's
// PID-namespace inode. Populated once at agent startup by reading
// stat("/proc/self/ns/pid").Ino. Read by task_in_agent_ns() below
// every time we need to classify a new tgid.
//
// Same shape and semantics as enterprise's
// keploy_agent_registration_map but cbshim-private — no cross-BPF-
// object coupling, no load-order dependency, no struct-version drift
// when OSS hooks evolve. Pays one stat() at startup; in exchange
// cbshim stays a self-contained BPF unit.
struct cbshim_agent_info {
    __u64 keploy_agent_inode;
};

struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 1);
    __type(key, __u32);
    __type(value, struct cbshim_agent_info);
} cbshim_agent_info_map SEC(".maps");

// task_in_agent_ns — mirrors the OSS hooks' check_and_register_agent
// approach (headers/k_helpers.h:97 in the keploy/ebpf repo) but reads
// our private cbshim_agent_info. Returns 1 if the calling task is in
// or below the keploy agent's PID namespace.
//
// bpf_get_ns_current_pid_tgid is a Linux 5.7+ kernel helper. It looks
// up the calling task's PID/TGID as seen FROM the specified PID
// namespace (identified by dev + inode). Returns non-zero on error —
// notably when the calling task is not in that namespace, which is
// our "filter this out" signal.
static __always_inline int task_in_agent_ns(void) {
    __u32 key = 0;
    struct cbshim_agent_info *info = bpf_map_lookup_elem(&cbshim_agent_info_map, &key);
    if (!info || info->keploy_agent_inode == 0)
        return 0;
    struct bpf_pidns_info ns = {};
    if (bpf_get_ns_current_pid_tgid(4, info->keploy_agent_inode,
                                     &ns, sizeof(ns)) != 0)
        return 0;
    return 1;
}

// libpq_ranges — Stage-2 filter. For tgids that pass Stage 1, this
// holds the libpq executable mapping ranges so we can verify the
// X509_digest call came FROM libpq (not from libcrypto-internal cert
// chain verification or similar). Composite key (tgid, idx) handles
// the common case of a process that loaded multiple libpq's — system
// libpq for python-ssl and a wheel-bundled libpq for psycopg2-binary,
// for example.
struct libpq_range_key {
    __u32 pid;
    __u32 idx;
};

struct libpq_range_val {
    __u64 start;
    __u64 end;
};

struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 1024);
    __type(key, struct libpq_range_key);
    __type(value, struct libpq_range_val);
} libpq_ranges SEC(".maps");

// cbmap — mitm_hash[32] → real_hash[32]. Populated by keploy's proxy
// every time it captures a real upstream cert from a TLS handshake.
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 4096);
    __type(key, __u8[HASH_LEN]);
    __type(value, __u8[HASH_LEN]);
} cbmap SEC(".maps");

// pending — per-thread state from uprobe to uretprobe.
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 4096);
    __type(key, __u64);
    __type(value, __u64);
} pending SEC(".maps");

// cbshim_proc_events — ringbuf the fentry/security_mmap_file program
// emits to whenever a process in the agent's PID namespace mmap()s a
// file with PROT_EXEC and a basename starting with libcrypto / libpq /
// libssl. The basename is read from file->f_path.dentry->d_name.name so
// userspace gets {tgid, basename} for free — no syscall-argument string
// reads, no TOCTOU, no ENOENT noise from glibc-hwcaps probes. Userspace
// then calls AttachToProcess(tgid) which scans /proc/<tgid>/maps for the
// canonical full paths and attaches uprobes idempotently.
//
// Sized 256 KiB to comfortably absorb startup bursts (a typical
// dynamic linker mmaps 10-20 .so files per process exec; on a busy host
// that can sum to a few thousand events/sec during startup).
struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 1 << 18);
} cbshim_proc_events SEC(".maps");

#define LIB_EVENT_PATH_LEN 256

struct cbshim_lib_event {
    __u32 tgid;
    __u32 _pad;
    char  path[LIB_EVENT_PATH_LEN];
};

// counters — observability. Indexed by C_* below.
struct {
    __uint(type, BPF_MAP_TYPE_ARRAY);
    __uint(max_entries, 8);
    __type(key, __u32);
    __type(value, __u64);
} counters SEC(".maps");

#define C_TOTAL_FIRES   0
#define C_TGID_MATCHED  1
#define C_LIBPQ_FIRES   2
#define C_LOOKUP_HIT    3
#define C_LOOKUP_MISS   4
#define C_WRITE_OK      5
#define C_WRITE_FAIL    6

static __always_inline void bump(__u32 i) {
    __u64 *c = bpf_map_lookup_elem(&counters, &i);
    if (c) __sync_fetch_and_add(c, 1);
}

static __always_inline int caller_in_libpq(__u32 pid, __u64 ret_addr) {
    #pragma unroll
    for (__u32 i = 0; i < MAX_RANGES_PER_PID; i++) {
        struct libpq_range_key k = { .pid = pid, .idx = i };
        struct libpq_range_val *r = bpf_map_lookup_elem(&libpq_ranges, &k);
        if (!r) continue;
        if (ret_addr >= r->start && ret_addr < r->end) return 1;
    }
    return 0;
}

SEC("uprobe/cb_x509_digest_entry")
int BPF_KPROBE(cb_x509_digest_entry,
               void *cert, void *type, void *md, void *len) {
    bump(C_TOTAL_FIRES);

    __u64 ptg = bpf_get_current_pid_tgid();
    __u32 tgid = ptg >> 32;

    // Stage 1: namespace membership gate with lazy classification.
    // Fast path — already-classified tgid hits target_namespace_pids
    // cache in ~30ns. Slow path — first fire from this tgid does the
    // namespace check via task_in_agent_ns() (~500ns once) and caches
    // the result. Rejects ~99% of host X509_digest calls (other
    // tenants, system services, keploy's own telemetry TLS) at the
    // namespace check — never poisons the cache for them.
    if (!bpf_map_lookup_elem(&target_namespace_pids, &tgid)) {
        if (!task_in_agent_ns())
            return 0;
        __u8 v = 1;
        bpf_map_update_elem(&target_namespace_pids, &tgid, &v, BPF_ANY);
    }
    bump(C_TGID_MATCHED);

    // Stage 2: is the caller actually libpq? Internal libcrypto
    // callers use smaller md buffers; clobbering them crashes
    // libcrypto. Read the saved return address from [rsp].
    __u64 rsp = PT_REGS_SP(ctx);
    __u64 ret_addr = 0;
    if (bpf_probe_read_user(&ret_addr, sizeof(ret_addr), (void *)rsp) != 0)
        return 0;
    if (!caller_in_libpq(tgid, ret_addr))
        return 0;

    bump(C_LIBPQ_FIRES);

    __u64 md_addr = (__u64)(unsigned long)md;
    bpf_map_update_elem(&pending, &ptg, &md_addr, BPF_ANY);
    return 0;
}

SEC("uretprobe/cb_x509_digest_return")
int BPF_KRETPROBE(cb_x509_digest_return) {
    __u64 ptg = bpf_get_current_pid_tgid();
    __u64 *md_addr_p = bpf_map_lookup_elem(&pending, &ptg);
    if (!md_addr_p) return 0;
    __u64 md = *md_addr_p;
    bpf_map_delete_elem(&pending, &ptg);

    __u8 mitm_hash[HASH_LEN] = {0};
    if (bpf_probe_read_user(&mitm_hash, HASH_LEN, (void *)md) != 0)
        return 0;

    __u8 *real_hash = bpf_map_lookup_elem(&cbmap, &mitm_hash);
    if (!real_hash) {
        bump(C_LOOKUP_MISS);
        return 0;
    }
    bump(C_LOOKUP_HIT);

    long rc = bpf_probe_write_user((void *)md, real_hash, HASH_LEN);
    if (rc == 0) bump(C_WRITE_OK);
    else         bump(C_WRITE_FAIL);
    return 0;
}

// PROT_EXEC value — fixed by Linux ABI on x86_64 and arm64. Defined
// here so we don't have to depend on any specific kernel header layout.
#define CB_PROT_EXEC 4

// cb_mmap_file_enter — fentry hook on security_mmap_file, the LSM hook
// the kernel invokes during every mmap() syscall (and brk-extension,
// MAP_ANONYMOUS, etc.). For us the interesting case is the PROT_EXEC
// mapping of a shared library by ld.so / __libc_dlopen_mode:
//
//   ld.so / dlopen path for a shared library:
//       openat(..., "<path>/lib*", O_RDONLY|O_CLOEXEC)
//       read(fd, ELF header)
//       mmap(fd, PROT_READ, ...)                         ← read-only header map
//       mmap(fd, PROT_READ|PROT_EXEC, ...)               ← we fire HERE
//       mmap(fd, PROT_READ|PROT_WRITE, ...)              ← data section
//       close(fd)
//       ld.so resolves relocations + runs library init
//       ... eventually ...
//       application calls a function in the library      ← uprobe must
//                                                          be attached
//                                                          BY THIS POINT
//
// Why security_mmap_file beats sys_enter_openat as the discovery hook:
//
//   - One event per library load, not one per openat: ENOENT probes
//     from glibc-hwcaps (3 misses per library, every time) don't make
//     it to userspace.
//   - PROT_EXEC bit filters out non-library opens (config files,
//     /proc reads, lock files) entirely in-kernel.
//   - Path comes from the kernel-resolved dentry (no TOCTOU on the
//     userspace string the syscall is about to dereference).
//   - By the time the EXEC mapping is being installed the file has
//     already been opened and the read-only header has been mapped,
//     so /proc/<tgid>/maps reliably shows the file when userspace
//     reads it.
//
// Filtering strategy (cheapest first; ~99% of mmap traffic system-wide
// is anonymous/data/etc., so PROT_EXEC is the dominant filter):
//   1. file != NULL (anonymous mappings have file==NULL)
//   2. prot & PROT_EXEC
//   3. task_in_agent_ns (PID-namespace membership)
//   4. basename starts with libcrypto / libpq / libssl
//
// Only the basename is captured — full-path extraction from kernel
// dentries requires walking d_parent and is verifier-tedious. Userspace
// has /proc/<tgid>/maps anyway, where it can resolve full paths once
// the event tells it which tgid to scan.
SEC("fentry/security_mmap_file")
int BPF_PROG(cb_mmap_file_enter, struct file *file, unsigned long prot, unsigned long flags) {
    if (!file) return 0;
    if ((prot & CB_PROT_EXEC) == 0) return 0;
    if (!task_in_agent_ns()) return 0;

    struct dentry *d = BPF_CORE_READ(file, f_path.dentry);
    if (!d) return 0;
    const unsigned char *name = BPF_CORE_READ(d, d_name.name);
    if (!name) return 0;

    struct cbshim_lib_event *evt = bpf_ringbuf_reserve(
        &cbshim_proc_events, sizeof(*evt), 0);
    if (!evt) return 0;

    long n = bpf_probe_read_kernel_str(evt->path, LIB_EVENT_PATH_LEN, name);
    if (n <= 1) {
        bpf_ringbuf_discard(evt, 0);
        return 0;
    }

    char *bn = evt->path;
    int is_libcrypto = (bn[0] == 'l' && bn[1] == 'i' && bn[2] == 'b' &&
                       bn[3] == 'c' && bn[4] == 'r' && bn[5] == 'y' &&
                       bn[6] == 'p' && bn[7] == 't' && bn[8] == 'o');
    int is_libpq = (bn[0] == 'l' && bn[1] == 'i' && bn[2] == 'b' &&
                   bn[3] == 'p' && bn[4] == 'q');
    int is_libssl = (bn[0] == 'l' && bn[1] == 'i' && bn[2] == 'b' &&
                    bn[3] == 's' && bn[4] == 's' && bn[5] == 'l');

    if (!is_libcrypto && !is_libpq && !is_libssl) {
        bpf_ringbuf_discard(evt, 0);
        return 0;
    }

    evt->tgid = bpf_get_current_pid_tgid() >> 32;
    evt->_pad = 0;
    bpf_ringbuf_submit(evt, 0);
    return 0;
}
