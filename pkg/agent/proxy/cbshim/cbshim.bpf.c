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

char LICENSE[] SEC("license") = "GPL";

#define HASH_LEN           32      // SHA-256
#define MAX_RANGES_PER_PID  4

// target_namespace_pids — Stage-1 PID allowlist. Externally maintained
// by the agent's existing PID discovery loop (in OSS standalone mode
// by cbshim's own /proc walker; in enterprise by the shared
// ensureAppPIDsRegistered ticker that already feeds the proxyless and
// SSL/GoTLS uprobe filters). Single source of truth: if a tgid is not
// in this map, NONE of cbshim's logic runs for it.
//
// Sized for K8s DaemonSet — ~500 pods × ~30 PIDs/pod + headroom. Same
// shape and limits the enterprise proxyless code uses, so the map can
// be unified later if we want a single shared instance.
struct {
    __uint(type, BPF_MAP_TYPE_HASH);
    __uint(max_entries, 262144);
    __type(key, __u32);    // tgid
    __type(value, __u8);   // flag
} target_namespace_pids SEC(".maps");

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

    // Stage 1: cheap TGID allowlist gate. ~99% of host X509_digest
    // calls (other tenants, system services, even keploy itself when
    // it does telemetry TLS) get rejected here.
    if (!bpf_map_lookup_elem(&target_namespace_pids, &tgid))
        return 0;
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
