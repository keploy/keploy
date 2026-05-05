# Refreshing the embedded Mozilla NSS roots

`mozilla_roots.pem` is compiled into the keploy agent binary via `go:embed`
([ca.go](../ca.go)). It's the trust-anchor source the agent uses to
populate `/tmp/keploy-tls/ca.crt` when the runtime image's own
`/etc/ssl/certs/ca-certificates.crt` is missing or shadowed. Without this
embedded fallback, an orphan-mutated app pod whose agent has no system
trust store would fail every public-endpoint HTTPS call with
`CERTIFICATE_VERIFY_FAILED` (the failure mode in keploy/k8s-proxy#375).

## Source

The bundle is curl's daily-refreshed extract of Mozilla's NSS
`certdata.txt`:

- Canonical URL: <https://curl.se/ca/cacert.pem>
- Canonical SHA-256: published at <https://curl.se/docs/caextract.html>
- Upstream Mozilla source: <https://hg.mozilla.org/mozilla-central/raw-file/tip/security/nss/lib/ckfw/builtins/certdata.txt>

curl's extract is the same data as Debian/Ubuntu's `ca-certificates`
package and the `golang.org/x/crypto/x509roots/fallback` Go package —
it's the de-facto "Mozilla trust store" for Linux/Unix.

## Refresh procedure

Run from the repo root:

```bash
curl -sSfL https://curl.se/ca/cacert.pem -o pkg/agent/proxy/tls/data/mozilla_roots.pem
go test ./pkg/agent/proxy/tls/... -run TestLoadSystemCABundle -v
```

The two relevant tests are:

- `TestLoadSystemCABundle_EmbeddedFallback_ContainsMozillaRoots` — fails
  if the file is empty, malformed PEM, or has fewer than 100 root certs
  (catches truncation and accidental empty-file commits).
- `TestLoadSystemCABundle_EmbeddedFallback_ReturnsBytes` — fails if the
  fallback path is wired wrong end-to-end.

Both run in CI on every PR.

## Refresh cadence

Mozilla updates NSS roots every 6-12 weeks (root additions, removals,
distrusted CAs after compromises). curl re-publishes the extract within
a day of each upstream change.

A stale bundle here means the agent's fallback won't trust newly-added
public CAs (a customer's app talking to a service whose cert chains to
a brand-new root would still see `CERTIFICATE_VERIFY_FAILED`). The
practical risk is low — most established services chain to roots that
have been in NSS for years — but quarterly refresh is the right cadence
to keep the lag bounded.

A `chore(agent): refresh embedded Mozilla CA roots` PR every quarter is
the lightweight maintenance path. The diff is just this one file.

## Why we vendor the bytes instead of importing `golang.org/x/crypto/x509roots/fallback`

The Go fallback package registers roots via `x509.SetFallbackRoots` for
in-process verification. It doesn't expose the underlying PEM bytes, and
the agent needs PEM bytes — the merged `ca.crt` is read by non-Go
clients (Python's `ssl`, libcurl, OpenJDK, Rust's `rustls`, ...) that
each parse PEM directly. Re-serializing certs through `x509.Certificate`
+ `pem.Encode` would work but loses curl's exact byte ordering and
header comments that operators sometimes diff against the upstream.
Vendoring the bytes is the simpler, more auditable path.
