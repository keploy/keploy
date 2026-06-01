# `cbmap` — channel-binding hash publisher for SCRAM-SHA-256-PLUS under MITM

`cbmap` is keploy's side of the workaround that lets apps using **SCRAM-SHA-256-PLUS** auth keep working when keploy is doing TLS MITM in front of Postgres.

The companion piece, `cbshim.so`, lives in `../tls/asset/` and runs **inside the app process** via `LD_PRELOAD`.

## The problem

PostgreSQL's `SCRAM-SHA-256-PLUS` authentication binds the password-knowledge proof to the **hash of the TLS server certificate** the client sees (RFC 5929 `tls-server-end-point`). It is specifically designed to detect and refuse any TLS interposer that doesn't hold the database's private key.

Keploy is exactly such an interposer:

```
app ──TLS(keploy_mitm_cert)──► keploy ──TLS(real_pg_cert)──► postgres
```

Two TLS sessions. Two different leaf certs.

- App computes `H(keploy_mitm_cert)` and folds it into the SCRAM proof.
- Postgres computes `H(real_pg_cert)` and expects to see *that* in the proof.

The hashes diverge → Postgres rejects with:

```
FATAL: SCRAM channel binding check failed
```

Every request becomes a 500. Recordings capture nothing useful.

This is the failure mode previously documented in [`integrations/pkg/postgres/v3/replayer/session/startup.go:76-87`](../../../../integrations/pkg/postgres/v3/replayer/session/startup.go) ("Channel-binding limitation … unfixable at the proxy layer"). The `cbmap` + `cbshim` pair is the proxy-side workaround that **is** possible — by accepting that the fix has to land inside the app process.

## The design

Keploy already knows both leaf certs at the moment upstream TLS completes:

- Its own MITM cert (minted per-hostname, cached in `tls/ca.go`)
- The real upstream cert (returned by `tlsConn.ConnectionState().PeerCertificates[0]`)

The trick: instead of trying to forge the SCRAM proof (which would require the app's password — impossible by SCRAM's design), we **substitute the cert hash inside libpq before the proof is computed**. The app then signs a proof against the *real* cert hash, using its real password, and Postgres verifies it normally.

```
┌─────── keploy-agent ──────────────────────────────────────────┐
│  dialPostgresSSLUpstream:                                      │
│    tlsConn.HandshakeContext()           ← real upstream TLS    │
│    cbmap.Publish(mitm_DER, real_DER, sig_algo)                │
│            │                                                   │
│            └──────► writes /tmp/keploy-tls/cbmap.txt           │
│                                                                │
│  setupSharedVolume:                                            │
│    writes /tmp/keploy-tls/cbshim.so       (embedded via go:embed) │
└────────────────────────────────────────────────────────────────┘
                                ↓ shared EmptyDir
┌─────── app container ─────────────────────────────────────────┐
│  env LD_PRELOAD=/tmp/keploy-tls/cbshim.so                     │
│                                                                │
│  libpq → OpenSSL X509_digest()                                │
│                │                                               │
│                ▼                                               │
│  cbshim.so intercepts:                                         │
│    1. calls real X509_digest → buf = H(keploy_mitm_cert)       │
│    2. reads /tmp/keploy-tls/cbmap.txt                          │
│    3. finds row where col1 == buf                              │
│    4. overwrites buf with col2 (H(real_pg_cert))               │
│                                                                │
│  libpq builds SCRAM proof using H(real_pg_cert) → valid → ✅   │
└────────────────────────────────────────────────────────────────┘
```

The substitution lookup key is `H(keploy_mitm_cert)` — different upstreams get different MITM certs (keploy's existing per-hostname cert cache), so the table cleanly disambiguates multi-upstream apps without any per-connection state in the shim.

## File format

`cbmap.txt` is plain text. One entry per line, two hex tokens separated by whitespace:

```
<H(keploy_mitm_cert)_hex>  <H(real_upstream_cert)_hex>
```

Hash algorithm follows RFC 5929 §4.1:

| Cert signature algorithm | Channel binding hash |
|---|---|
| MD5* / SHA1* | SHA-256 (normalized) |
| SHA-256* | SHA-256 |
| SHA-384* | SHA-384 |
| SHA-512* | SHA-512 |
| Anything else (incl. Ed25519, unknown) | SHA-256 |

Writes are atomic (tmpfile + rename). Readers see either the previous table or the new one — never a partial line.

## Usage from keploy code

```go
import "go.keploy.io/server/v3/pkg/agent/proxy/cbmap"

// After upstream TLS handshake completes:
state := tlsConn.ConnectionState()
realLeaf := state.PeerCertificates[0]

_, err := cbmap.Publish(
    logger,
    mitmDER,                    // []byte — DER bytes of the cert keploy serves to the app
    realLeaf.Raw,               // []byte — DER bytes of the real upstream cert
    realLeaf.SignatureAlgorithm, // x509.SignatureAlgorithm — drives RFC 5929 hash selection
)
```

`Publish` is safe to call concurrently and repeatedly with the same inputs (duplicates are no-ops).

## Wiring points

Three changes hook this into the existing keploy/k8s-proxy stack:

| File | Change |
|---|---|
| `pkg/agent/proxy/proxy.go` | `publishCBMap(addr, tlsConn, logger)` call inside `dialPostgresSSLUpstream` after the upstream handshake completes |
| `pkg/agent/proxy/tls/ca.go` | Embeds `cbshim.so` via `//go:embed asset/cbshim.so`; `setupSharedVolume` writes it to `/tmp/keploy-tls/cbshim.so`; new `CachedLeafDER(host)` helper exposes the per-hostname MITM cert |
| `k8s-proxy/pkg/service/webhook/service.go` | Injects `LD_PRELOAD=/tmp/keploy-tls/cbshim.so` into app containers alongside the existing CA env vars |

## Configuration

| Env var | Default | Effect |
|---|---|---|
| `KEPLOY_CBMAP_PATH` | `/tmp/keploy-tls/cbmap.txt` | Where keploy writes the mapping (and where the shim reads it). |
| `CBSHIM_HASHMAP` *(read by the shim)* | `/tmp/keploy-tls/cbmap.txt` | Override the shim's read path. |
| `CBSHIM_DEBUG=1` *(read by the shim)* | unset | Enables stderr logging in the shim — useful when diagnosing why a swap didn't happen. |

There is no flag to enable/disable the publisher in keploy itself yet. Disable by removing `LD_PRELOAD` from the webhook's injected env, or `chmod 000 /tmp/keploy-tls/cbshim.so`.

## What's safe and what's not

**Safe to run globally** even on apps that don't use SCRAM-PLUS:

- The shim is a no-op for any `X509_digest` call whose result isn't in `cbmap.txt`. Default behavior is "call real function, return its result unchanged."
- Apps that don't reach OpenSSL (Java's JSSE, Go's native crypto, .NET's managed runtime, rustls-based Rust) never invoke `X509_digest` and the shim is never triggered.
- Apps without dynamic linking against `libcrypto.so` (statically linked binaries) silently ignore `LD_PRELOAD`.

**Will break** the following:

- **`setuid` / `setgid` binaries** — the dynamic linker strips `LD_PRELOAD` from privileged binaries as a security measure. Channel binding will still fail for these. Unusual in containerized apps but flag it.
- **Apps using `channel_binding=require`** with a **non-OpenSSL** TLS stack — the shim can't help, and the app will hard-fail. Workaround: set `channel_binding=disable` for these specifically.
- **OpenSSL major-version changes** — the embedded `cbshim.so` is built against OpenSSL 3.x. OpenSSL 1.1 / 1.0 binaries are still ABI-compatible at the `X509_digest` symbol level so it should work, but this hasn't been exhaustively tested.
- **Alternate libc** — the embedded shim is glibc-linked. Alpine/musl containers need a separate build.

## Limitations / future work

1. **Single-arch / single-libc shim embedded.** Currently only `linux/amd64 + glibc + OpenSSL 3.x`. Multi-arch (arm64) and multi-libc (musl) need separate embedded variants and runtime arch detection.

2. **No config flag to disable globally.** Should add `--enable-channel-binding-shim` (default on) to k8s-proxy config and gate the `LD_PRELOAD` injection on it. Allows operators to opt out per-deployment if they hit an edge case.

3. **No upstream cert rotation handling.** If the real Postgres rotates its cert, the cached `(H_mitm, H_real)` mapping goes stale and auth starts failing. Today this requires a keploy-agent restart. The cleaner fix: detect cert change on each upstream handshake and re-publish.

4. **No staleness GC on `cbmap.txt`.** The file only grows. For long-running agents talking to many ephemeral upstreams this could bloat over time. Cap at N entries or evict by LRU.

5. **Per-thread / async correlation if not lookup-by-hash.** The current design works because keploy mints per-hostname certs, so `H(mitm)` is a stable lookup key. If keploy ever moves to per-connection certs, the shim's correlation strategy will need rework (probably via libpq `PQconnectdb` hook + thread-local).

## Verifying locally

A self-contained PoC harness is provided at `../../../../../scram-poc/` (outside the keploy tree). It runs:

1. Two Postgres containers with TLS + SCRAM-SHA-256 forced (different certs).
2. A keploy-linked test MITM proxy that uses `cbmap.Publish`.
3. A libpq C client with `LD_PRELOAD=cbshim.so`.
4. A concurrent multi-thread client hitting both upstreams.

Without the shim, all clients fail with `FATAL: SCRAM channel binding check failed`. With the shim, all 160 concurrent queries across 2 upstreams succeed.

See `../../../../../scram-poc/README.md` for the full repro.

## Cryptographic safety note

This setup **does not weaken** the TLS encryption or the app's password authentication:

- TLS encryption is identical to current keploy MITM behavior.
- The SCRAM password proof is still computed by the real client using the real password, and verified by the real Postgres using the real stored credentials. The shim only adjusts a single 32-byte input — the cert hash — to match what Postgres expects.
- Without keploy proxying the connection, the shim has nothing in `cbmap.txt` for any cert it sees, so it's a no-op. It doesn't accept arbitrary hashes from anywhere — only what keploy's own publisher writes.

In production environments **not running keploy**, the shim should not be present (the webhook doesn't run, `LD_PRELOAD` isn't set). In production environments **running keploy**, the only thing being defeated is keploy's own MITM detection — which is the entire point.

## References

- RFC 5802 — Salted Challenge Response Authentication Mechanism (SCRAM)
- RFC 5929 §4.1 — `tls-server-end-point` channel binding type
- libpq source: `src/interfaces/libpq/fe-secure-openssl.c::pgtls_get_peer_certificate_hash`
- OpenSSL: `X509_digest(3)`
