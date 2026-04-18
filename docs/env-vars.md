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
