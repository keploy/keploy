// Package cbshim provides an eBPF-backed channel-binding shim that
// rewrites the cert hash libpq uses for SCRAM-SHA-256-PLUS auth, so
// keploy's TLS MITM stays invisible to the SCRAM proof check on the
// postgres server.
//
// # Why it exists
//
// Postgres SCRAM-SHA-256-PLUS authentication (RFC 5802 + RFC 5929)
// computes a "channel binding" value from the server cert's hash and
// folds it into the SCRAM proof. When keploy sits between client and
// server as a TLS MITM, the client sees keploy's freshly-minted cert
// while the server sees its own cert — the hashes differ, the proof
// fails, and the connection is rejected with
// "SCRAM channel binding check failed".
//
// # How it fixes that
//
// At upstream-TLS-handshake time, the proxy captures the real cert
// from postgres and the MITM cert it gave the client, hashes both,
// and writes the pair into a BPF map (cbmap). An eBPF uprobe attached
// to X509_digest in the target process's libcrypto fires on every
// channel-binding hash computation. On each fire:
//
//  1. Selectivity filter — only proceed if the call's return address
//     is inside libpq's mapped executable range. Internal libcrypto
//     callers (cert chain verify, fingerprint caches) often use
//     small (16- or 20-byte) md buffers, and writing 32 bytes to
//     those overflows the stack and crashes libcrypto. Restricting
//     to libpq's pgtls_get_peer_certificate_hash — which uses a
//     64-byte EVP_MAX_MD_SIZE buffer — keeps writes safe.
//
//  2. Look up the cert hash X509_digest just wrote into the cbmap.
//
//  3. If found (it was the MITM cert), overwrite the 32-byte output
//     buffer with the real server cert's hash before libpq's caller
//     reads it for the SCRAM proof.
//
// Internal libcrypto callers and connections going to non-MITMed hosts
// never have their hashes in the cbmap and silently pass through.
//
// # Compared to the previous LD_PRELOAD cbshim
//
// The old design shipped a libpq-compatible shim (cbshim.so) that
// the user app loaded via LD_PRELOAD. That approach broke for clients
// that bundle their own libcrypto under mangled SONAMEs — most notably
// psycopg2-binary, which auditwheel ships with libcrypto-XXXX.so.1.1
// under RTLD_LOCAL. dlsym(RTLD_NEXT, "X509_digest") returns NULL
// inside such a wheel, leaving the shim a silent no-op.
//
// eBPF uprobes attach at the ELF-offset level via the kernel's perf
// subsystem; they don't care about RTLD scopes, SONAME mangling, or
// whether the symbol is in the global resolution table. That makes
// this approach reach into auditwheel-bundled wheels where LD_PRELOAD
// cannot.
//
// # Threat model
//
// This is not a security mechanism; it's a compatibility shim. The
// cbmap is populated only with cert hashes the keploy proxy already
// has (it terminated the upstream TLS itself). The probe only writes
// when it sees a hash it knows came from its own MITM cert. The user
// app cannot be tricked into accepting an arbitrary upstream — keploy
// itself decides which upstream to dial, and only certs from that dial
// are eligible for substitution.
//
// # Operational requirements
//
// The keploy agent process needs CAP_BPF + CAP_PERFMON (or
// CAP_SYS_ADMIN on older kernels) to load the BPF program. The kernel
// must be >= 5.5 for the uprobe-on-perf path used here, and
// bpf_probe_write_user must not be disabled (it isn't by default in
// stock distributions; some hardened images restrict it).
package cbshim

//go:generate sh -c "GOPACKAGE=cbshim $(go env GOPATH)/bin/bpf2go -cc clang -target amd64,arm64 -type libpq_range_key -type libpq_range_val cbshim cbshim.bpf.c"
