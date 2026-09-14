# Environment variables

Canonical listing of all env vars keploy reads that are not already
documented in CLI help or `keploy.yml`. If you add a new variable,
please document it here so reviewers and operators have one place
to find env-var knobs.

Config-file fields always win over their env-var counterpart — except
where noted (the env var is an ad-hoc override for power-user
experimentation). Env vars are process-global: exporting one in a
shell leaks it into every keploy invocation that shell spawns.

## Recording

### `KEPLOY_MOCK_FORMAT` — `yaml` \| `gob`

Selects the on-disk mock format written during `keploy record`. Reader
paths auto-detect by file extension; `mocks.gob` is preferred when
both exist in the same test-set directory.

| Value | On-disk file | Notes |
|---|---|---|
| unset / `yaml` | `mocks.yaml` | Default. Human-readable. Compatible with all existing keploy tooling, CI diffs, and PR review workflows. |
| `gob` | `mocks.gob` | Binary. ~28% CPU saving on the record client at high throughput. Not grep/diff-friendly. Has no cross-Go-version stability contract — a `gob.Register`-dependent struct change in `pkg/models/*` may break replay of older `mocks.gob` files. Magic-header versioned (`keploy-gob-v1`); breaking changes bump the suffix and old files fail fast at read time. |

**Config-file equivalent:** `record.mockFormat` in `keploy.yml`. Env
var takes precedence when set.

Example:

```bash
# Ad-hoc run with gob
KEPLOY_MOCK_FORMAT=gob keploy record -c "./my-app"

# Or via keploy.yml for a permanent team default
record:
  mockFormat: gob
```

### `GOMEMLIMIT_MB` — positive integer

Sets a soft GC memory limit on the keploy record agent. Upstream
memory-aware work; unchanged by this PR.

### `KEPLOY_UI_CAPTURE_ID`, `KEPLOY_UI_SESSION_NONCE`, `KEPLOY_APP_ORIGINS` — UI join

Set together by whatever starts BOTH halves of a UI capture — the browser
recorder and `keploy record`. They tell the recorder that this recording
belongs to a browser session, so it can write a join annotation onto the
test-set it produces.

| Variable | Required | Meaning |
|---|---|---|
| `KEPLOY_UI_CAPTURE_ID` | yes | Which browser capture session this test-set pairs with. |
| `KEPLOY_UI_SESSION_NONCE` | yes | Proves both halves came from the SAME run. Without it, replaying one scenario twice produces two test-sets that both "match" a single browser capture. |
| `KEPLOY_APP_ORIGINS` | no | Comma-separated origins the app is served from, e.g. `https://app.example.com,https://cdn.example.com`. Empty entries are dropped. |

Setting **either** of the first two switches the feature on. If only one
is set the recording still runs, and the annotation is refused with a
diagnostic that names the VARIABLE to go and look at, not the field it
maps to — a half-configured join is not silently treated as "no join
requested". For a refusal no single variable explains (an implausible
clock reading, an unknown key spec, a malformed origin) the diagnostic
lists all three rather than guessing.

`KEPLOY_APP_ORIGINS` carries the prefix deliberately. An unprefixed name
is one an application may well define for itself, and keploy reads its
environment from the same shell, CI job or pod spec that configures the
app — so a value meant for the application would be picked up as though
it were meant for us, and published in an annotation.

#### What lands on disk

The annotation is written into the test-set's `config.yaml`, under the
namespaced metadata key `io.keploy.ui-join/v1`:

```yaml
metadata:
    io.keploy.ui-join/v1:
        appOrigins:
            - https://app.example.com
        canonicalKeySpec: canonical-key.v1
        captureId: cap-0000000000000001
        ingressPorts:
            - 8080
        sessionNonce: nonce-0000000000000001
        specVersion: 1
        t0WallMs: 1700000000000
        t1WallMs: 1700000060000
```

That is the real on-disk shape, generated through the same encoder the
recorder writes with rather than hand-typed: keys in alphabetical order,
four-space indent, block sequences. Only the values differ from a real
run.

`t0WallMs` is when capture began and `t1WallMs` when it stopped, both in
epoch milliseconds; `ingressPorts` lists the ports ingress was OBSERVED
arriving on.

Three things worth knowing about those values:

- **`ingressPorts` is observed, not configured.** It lists ports traffic
  actually arrived on, so an exchange on a port nothing was seen on is
  classified `NO_INGRESS_OBSERVED` rather than silently failing to join.
- **`t0WallMs` is when CAPTURE began, not when the process started.** It
  excludes agent bring-up always. It excludes the app's own boot ONLY
  under docker-compose, where keploy does not start the app; for every
  other command type the app is started after this stamp, so its boot
  falls INSIDE the span. It exists for clock-skew estimation against the
  browser, so it is kept as milliseconds rather than reformatted through
  a date type.
- **An empty `appOrigins` is legal.** A page with no usable origin — a
  sandboxed iframe, a `data:` or `file:` URL — is *correctly recorded and
  cannot be joined*. That is a different thing from a broken record, and
  consumers must not treat it as one.

Nothing else reads these variables, and `keploy record` behaves exactly as
before when none is set.

## Profiling / diagnostics

### `PPROF_PORT` — positive integer (keploy OSS)

Starts `net/http/pprof` on `localhost:<port>`. Default: unset (no
server). Both the record client and its spawned agent share one
binary and would collide on a single port — on keploy OSS the same
`PPROF_PORT` drives whichever process reads it first; on enterprise
the agent uses `PPROF_AGENT_PORT` instead (see enterprise docs).

Bound to `localhost` only; do not rely on this for multi-host
profiling.

## What we deliberately did NOT add

### `GOMAXPROCS` cap as a keploy default

Considered: an automatic `runtime.GOMAXPROCS(2)` in low-latency mode
recovered ~30% throughput + 27% p50 in one benchmark. Rejected as a
default because the win comes from taking cores *away* from keploy on
saturated hosts — well-provisioned hosts lose throughput ceiling.
Users who need it should set Go's standard `GOMAXPROCS` env var
themselves, the same way they would for any Go process.

### Env-var-only mock format selection (no config field)

Initially the only switch. Upgraded to `record.mockFormat` in
`keploy.yml` because an env var is process-global: exporting it in
a shell leaks into standard-mode recordings too. The config field is
scoped to the specific run and visible at code-review time in the
keploy config.
