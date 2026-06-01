# `scram-channel-binding` — e2e test for the LD_PRELOAD channel-binding workaround

End-to-end regression test for the `cbmap` + `cbshim` pair that lets
`SCRAM-SHA-256-PLUS` Postgres auth succeed across keploy's TLS MITM.

See `../../pkg/agent/proxy/cbmap/README.md` for the design.

## What it verifies

Three assertions, executed in sequence inside the `app` container:

| Phase | LD_PRELOAD | `channel_binding` | Expected | Why |
|---|---|---|---|---|
| 1 | (unset) | `require` | FATAL: SCRAM channel binding check failed | Baseline — proves channel binding is actually in effect (test would be meaningless if it weren't) |
| 2 | `cbshim.so` | `prefer` (default) | Auth OK | Shim wired correctly, libpq picks `-PLUS` over TLS, shim swaps the hash |
| 3 | `cbshim.so` | `require` | Auth OK | Forces `-PLUS` — no possibility of silent downgrade to plain SCRAM. Proves the swapped hash produces a valid proof |

If all three pass, the channel-binding workaround is sound end-to-end.

## Run

```sh
./run.sh
```

Exits 0 on success. Output ends with `PASS: SCRAM-SHA-256-PLUS auth succeeded across keploy MITM`. On failure the mitm container's logs are dumped to stderr to aid diagnosis.

## What's inside

```
scram-channel-binding/
├── docker-compose.yml          # 3 services, 1 named volume, 1 network
├── run.sh                      # build → up → assert
├── postgres/
│   ├── Dockerfile              # postgres:16 + TLS + hostssl scram-sha-256
│   ├── pg_hba.conf
│   ├── server.crt, server.key  # PG's TLS cert (self-signed)
├── mitm/
│   ├── Dockerfile              # multi-stage; copies whole keploy repo to link cbmap
│   ├── main.go                 # tiny MITM that calls real keploy cbmap.Publish
│   ├── mitm.crt, mitm.key      # MITM's TLS cert (different from PG's — that's the whole point)
└── app/
    ├── Dockerfile              # debian + libpq + compiles cbshim.so from source
    ├── client.c                # libpq probe
    ├── cbshim.c                # the LD_PRELOAD shim source (mirrors pkg/agent/proxy/tls/asset/cbshim.c)
    └── wait-and-run.sh         # the 3-phase test driver
```

## Service topology

```
   ┌─app──────┐    ┌─mitm──────────┐    ┌─postgres──┐
   │ libpq    │───►│ keploy/cbmap  │───►│ TLS+SCRAM │
   │ + cbshim │    │ + Go MITM     │    │           │
   └──────────┘    └───────────────┘    └───────────┘
        │                  │
        │                  │ writes /keploy-tls/cbmap.txt
        │ reads /keploy-tls/cbmap.txt (via shim)
        │                  │
        └────── named volume: keploy_tls ──────┘
```

The `keploy_tls` named volume is the docker-compose analogue of the Kubernetes EmptyDir mounted between `keploy-agent` and the app container in production. In real deployments the shim itself also lives on this volume (written by `setupSharedVolume` from the embedded `//go:embed asset/cbshim.so`). For this test, the shim is baked into the app image at build time instead — it makes the harness self-contained and lets the shim be recompiled against the app's exact OpenSSL ABI, which is what we'd recommend for any production deployment using a non-glibc / non-OpenSSL-3.x app stack anyway.

## Sample run output

```
============================================================
Phase 1: WITHOUT shim — expect FATAL channel binding
============================================================
[client] CONNECT FAILED: ... FATAL: SCRAM channel binding check failed
PASS: phase 1 failed as expected

============================================================
Phase 2: WITH shim, channel_binding=prefer — expect success
============================================================
[cbshim] real X509_digest resolved at 0x...
[cbshim] loaded 1 entries from /keploy-tls/cbmap.txt
[cbshim] swap: digest of len=32 matched a known MITM cert — substituting real hash
[client] CONNECTED, ssl=1, server_version=16.14
[client] result: status=auth-ok user=postgres
PASS: phase 2 succeeded with shim

============================================================
Phase 3: WITH shim, channel_binding=require — expect success
============================================================
[cbshim] swap: digest of len=32 matched a known MITM cert — substituting real hash
[client] CONNECTED, ssl=1, server_version=16.14
[client] result: status=auth-ok user=postgres
PASS: phase 3 succeeded with shim under require

ALL PHASES PASSED
```

And on the mitm side:

```
mitm  | MITM proxy (via keploy/cbmap) listening on :5432, forwarding to postgres:5432
mitm  | cbmap will be published to /keploy-tls/cbmap.txt
mitm  | cbmap: published  path=/keploy-tls/cbmap.txt  mitm_hash=2e5db2b9...  real_hash=65f8536b...
mitm  | cbmap published to /keploy-tls/cbmap.txt (subject=CN=postgres)
```

Two distinct hashes (`2e5db2b9...` for the MITM cert, `65f8536b...` for the real PG cert) — proof that channel binding really would have failed without the swap.

## Regenerating certs

Both certs are checked into the test for reproducibility. If they expire:

```sh
openssl req -new -x509 -days 365 -nodes \
  -out postgres/server.crt -keyout postgres/server.key -subj "/CN=postgres"
openssl req -new -x509 -days 365 -nodes \
  -out mitm/mitm.crt -keyout mitm/mitm.key -subj "/CN=mitm"
chmod 644 postgres/server.key mitm/mitm.key
```

The CNs are unimportant — neither client uses hostname verification (the libpq probe runs with `sslmode=require`, not `verify-full`, and the MITM dials upstream with `InsecureSkipVerify: true`). What matters is that the two certs are **different**, so their hashes differ and channel binding would fail without the swap.

## CI integration

Drop a new entry into `.github/workflows/test_workflow_scripts/` mirroring the existing pattern. The test is self-contained — needs only Docker and ~30s of CI runtime.
