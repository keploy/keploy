# AGENTS.md

Practical reference for agents working in `keploy/keploy`. Everything here is
grounded in the current state of the repo — if something below conflicts with
what you see on disk, trust the disk and update this file.

## What this project is

Keploy is a backend testing tool that records real API + dependency traffic
from a running application and replays it as deterministic tests with mocks.
It intercepts traffic at the network layer using eBPF (Linux) and a userspace
proxy (macOS/Windows), so apps don't need an SDK or code changes. The binary
is a single Go CLI: `keploy`.

- Go module: `go.keploy.io/server/v3`
- Go toolchain: Go 1.26 (`go.mod`)
- Primary entry point: `main.go` → `cli.Root(...)` in `cli/root.go`

## Build, run, lint, test

### Build the binary

```bash
# Standard build (what CI calls "build-no-race")
go build -tags=viper_bind_struct -o keploy .

# Race-enabled build (what CI calls "build" — used as the default in CI matrices)
CGO_ENABLED=1 go build -race -tags=viper_bind_struct -o keploy .
```

The `viper_bind_struct` build tag is required — leaving it off will cause
config fields not to bind correctly at runtime.

Version / Sentry DSN / server URL / GitHub client ID are injected via
`-ldflags` (see `Dockerfile` and `goreleaser.yaml`). For local development
the default `version` is `3-dev`.

### Run locally

Platform support is not uniform — it's gated by how the agent can
intercept traffic on each OS:

| Platform | Native binary (app runs on host) | Keploy-in-Docker (app runs in Docker) |
|----------|----------------------------------|---------------------------------------|
| **Linux** (x86_64, arm64) | ✅ Supported — uses eBPF (`pkg/agent/hooks/linux/`). Requires root. | ✅ Supported |
| **Windows** (amd64) | ✅ Supported — uses the WinDivert redirector (`pkg/agent/hooks/windows/`, `libwindows_redirector.a`). | ✅ Supported |
| **Windows** (arm64) | ❌ Falls through to the `others` stub — `Load()` / `Record()` return "not supported on non-Linux platforms". | ✅ Supported |
| **macOS** (amd64, arm64) | ❌ Same `others` stub — there is **no** native interception path on macOS. | ✅ Supported (only option) |

The `others` stub is `pkg/agent/hooks/others/hooks.go`; its build tag is
`(!windows && !linux) || (windows && arm64)` and every interface method
returns an error. Meaning:

- On **macOS** you *cannot* use keploy natively. You must:
  1. Build the keploy Docker image: `sudo docker image build -t ghcr.io/keploy/keploy:v3-dev .`
  2. Run your application inside Docker (usually via `docker compose`).
  3. Run keploy as that Docker image, which attaches to the app container.
  If the app isn't in Docker, keploy can't intercept its traffic on macOS.
  This is why `prepare_and_run_macos.yml` only calls `golang_docker_macos.yml`
  — there's no macOS-native equivalent.
- On **Linux** you can pick either — native (with `sudo`) or via the
  Docker image. CI exercises both (`golang_linux.yml` + `golang_docker.yml`).
- On **Windows** (amd64) native works without sudo; Docker mode also works.

**Native Linux run:**

```bash
sudo ./keploy record -c "<your app cmd>"
sudo ./keploy test   -c "<your app cmd>" --delay 10
```

If you pass a `docker`/`docker compose` command as `-c` without sudo on
Linux, `main.go` detects that in `utils.ShouldReexecWithSudo()` and
`syscall.Exec`s itself under `sudo -E` (see `utils/reexec_linux.go`). On
macOS and Windows the same helper is a no-op — `reexec_darwin.go` and
`reexec_windows.go` both short-circuit to `false`, since those platforms
rely on the active Docker context / Docker Desktop.

### Docker

```bash
sudo docker image build -t ghcr.io/keploy/keploy:v3-dev .
```

`ghcr.io/keploy/keploy:v3-dev` is the tag CI produces for dev builds and the
one the samples expect.

### Lint

Linter is `golangci-lint v2`, config at `.golangci.yml`:

- Enabled linters: `govet`, `staticcheck`, `errcheck`, `ineffassign`, `unused`
- Formatters: `gofmt`, `goimports`
- Paths excluded from linters: generated eBPF Go (`pkg/agent/hooks/bpf_*_bpfel.go`) and `pkg/service/utgen`

```bash
golangci-lint run
```

### Commit hygiene

- `.pre-commit-config.yaml` wires `commitizen` (Conventional Commits).
- `.cz.toml` pins the convention to `cz_conventional_commits`. Use types
like `feat:`, `fix:`, `chore:`, `refactor:`, `test:`, `docs:`.
- Commit messages should be very good and should also include a description of the changes made.
- Sign off every commit with `git commit -s` — this appends a
  `Signed-off-by: <user.name> <user.email>` trailer using the values from
  the effective git config (system → `~/.gitconfig` → `.git/config`). Do
  not hand-construct the trailer; let git read the identity from config so
  it matches the author.

## Key commands (quick user-facing recap)

| Command | Package | What it does |
|---------|---------|--------------|
| `keploy record -c "<cmd>"`  | `pkg/service/record`    | Runs the app, captures HTTP + dependency traffic into `./keploy/test-set-*` |
| `keploy test -c "<cmd>"`    | `pkg/service/replay`    | Replays recorded calls, mocks dependencies, writes `./keploy/reports/test-run-*` |
| `keploy rerecord -c "<cmd>"`| `pkg/service/orchestrator` | Re-records against new code to pick up accepted changes |
| `keploy normalize`          | `pkg/service/tools`     | Accepts newly-observed responses into the golden test cases |
| `keploy sanitize`           | `pkg/service/tools`     | Scrubs secrets using `custom_gitleaks_rules.toml` + built-in rules |
| `keploy templatize`         | `pkg/service/tools`     | Replaces dynamic values with templates in test sets |
| `keploy config --generate`  | `cli/config.go`         | Writes a default `keploy.yml` |
| `keploy gen`                | `pkg/service/utgen`     | LLM-driven unit-test generation |
| `keploy contract ...`       | `pkg/service/contract`  | OpenAPI contract generation / testing |
| `keploy mock {up,down}load` | `pkg/service/tools`     | Mock registry sync |
| `keploy diff <r1> <r2>`     | `pkg/service/diff`      | Diff two test runs |
| `keploy agent`              | `cli/agent.go`          | Internal — used by the Docker image entrypoint |

`keploy --help` is authoritative; update this table if you add a command.

These commands might change when you are reading this, so do spin up multiple agents to understand the commands and how they are used and all the flags that are possible. 

## The on-disk format (for matching against reports in scripts)

After `keploy record`:

```
keploy/
├── test-set-0/
│   ├── tests/
│   │   ├── test-1.yaml
│   │   └── test-2.yaml
│   └── mocks.yaml         (or mocks/ dir depending on version)
├── test-set-1/
│   └── ...
```

After `keploy test`:

```
keploy/
└── reports/
    └── test-run-0/                    (newest run has highest number)
        ├── test-set-0-report.yaml     (top-level field: `status: PASSED|FAILED`)
        ├── test-set-1-report.yaml
        └── coverage.yaml              (when coverage is enabled)
```

The `status:` line is the ground truth scripts grep for. See
`.github/workflows/test_workflow_scripts/golang/echo_mysql/golang-linux.sh`
for the canonical report-parsing loop.

## Conventions

- **Package doc comments** — every `package foo` has `// Package foo ...` per Go convention.
- **Logger** — always pass `*zap.Logger` through; construct via `utils/log.New()` at the top. Use `utils.LogError(logger, err, "message", zap.X(...))` instead of `logger.Error(...)` when you also want the error tracked in `utils.ErrCode`.
- **Context** — cancellable root comes from `utils.NewCtx()`; propagate it into goroutines.
- **Config access** — read from the `*config.Config` wired in `provider`. Don't read env vars deep in services; add a field to `Config` and parse it in `cmdConfigurator` / `main.go`.
- **Errors** — wrap with `fmt.Errorf("... : %w", err)` when adding context.
- **Generated code** — the eBPF Go files in `pkg/agent/hooks/bpf_*_bpfel.go` are generated; never edit by hand. They're linter-excluded in `.golangci.yml`.

## What NOT to touch

- **`pkg/agent/hooks/bpf_*_bpfel.go`** — generated from the eBPF C sources; edit the `.c` files if you need to change probe behavior, then regenerate.
- **`goreleaser.yaml`, `.releaserc.json`, `gon.json`** — release automation; changing these affects shipped artifacts. Coordinate with maintainers.
- **`artifacts/`, `adopters/`** — not source code; leave alone unless the task is explicitly about those.
- **`keploy.sh`, `entrypoint.sh`** — user-facing install / Docker bootstrap; small changes ripple to every release.
- **Private parsers** — the proprietary MySQL/Postgres/Mongo parser code is not in this repo. CI pulls it from `keploy/integrations` via the `setup-private-parsers` action (forks + non-main-branch runs skip it). Don't try to import paths that aren't present in a fresh fork clone.

## CI at a glance

### Entry points

- `.github/workflows/prepare_and_run.yml` — primary Linux CI. Runs on PRs to `main` and pushes to `main`. Everything below is downstream of this.
- `.github/workflows/prepare_and_run_macos.yml` — same idea for macOS (self-hosted).
- `.github/workflows/prepare_and_run_windows.yml` — Windows equivalents.
- `.github/workflows/prepare_and_run_integrations.yml` — runs the private-parser-only matrix subset for the integrations repo.
- `.github/workflows/manual-release.yml` — `workflow_dispatch` only; triggers the enterprise pipeline.

### What `prepare_and_run.yml` does

1. **Builds three binaries up front** so every matrix job can mix and match:
   - `build-no-race` — `go build -tags=viper_bind_struct`
   - `build`         — `go build -race -tags=viper_bind_struct` (CGO on)
   - `latest`        — downloads the most recent GitHub release
   Each is uploaded as an artifact with that name.
2. **Builds a Docker image** and pushes it to `ttl.sh` with a unique per-run tag.
3. **Fans out** to per-language workflows via `workflow_call`:
   - `golang_linux.yml`  — `samples-go` apps, native Linux
   - `golang_docker.yml` — `samples-go` apps, through the Docker image
   - `golang_wsl.yml`    — `samples-go` apps, WSL
   - `python_linux.yml`, `python_docker.yml` — `samples-python`
   - `node_linux.yml`, `node_docker.yml`     — `samples-typescript`
   - `java_linux.yml`                        — `samples-java`
   - `grpc_linux.yml`                        — gRPC-specific go matrix
   - `schema_match_linux.yml`                — schema-match matrix (python)
   - `fuzzer_linux.yml`, `node_mapping.yml`  — specialised
4. **Gates on a single job `gate`**. That's the only required status check — re-running the `gate` alone is pointless; it just re-checks upstream results.

### Matrix structure (the pattern every language workflow follows)

```yaml
matrix:
  app:
    - name: <display-name>
      path: <directory in samples-<lang> repo>
      script_dir: <directory in .github/workflows/test_workflow_scripts/<lang>/>
  config:
    - job: record_latest_replay_build   # record with released binary, replay with this PR's build
      record_src: latest
      replay_src: build
    - job: record_build_replay_latest   # record with this PR's build, replay with released binary
      record_src: build
      replay_src: latest
    - job: record_build_replay_build    # both this PR's build — exercises same-version behavior
      record_src: build
      replay_src: build
```

The three `config` entries are how CI guarantees **mock format backwards and
forwards compatibility**: any new change must interoperate with the last
released binary in both directions. If your change would break either,
it needs to be gated on something (capability detection, version check, or
feature flag) — see `risk_profile/golang-linux.sh` for a concrete example
that branches on `case "${REPLAY_BIN:-}" in */build/keploy) ...`.

Each matrix job ends with a step that:

```bash
cd samples-<lang>/${{ matrix.app.path }}
source $GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/<lang>/${{ matrix.app.script_dir }}/<lang>-linux.sh
```

The script receives `RECORD_BIN` and `REPLAY_BIN` in the environment, set by
the `./.github/actions/download-binary` composite action.

### Sample test script anatomy

Every script under `test_workflow_scripts/` follows broadly the same shape.
Canonical references:

- `.github/workflows/test_workflow_scripts/golang/echo_mysql/golang-linux.sh` — polished, with traps, log dumping, two record iterations, MySQL teardown before replay, latest test-run autodiscovery
- `.github/workflows/test_workflow_scripts/golang/http_pokeapi/golang-linux.sh` — minimal Go+HTTP pattern
- `.github/workflows/test_workflow_scripts/node/express_mongoose/node-linux.sh` — Node/Mongo with coverage threshold
- `.github/workflows/test_workflow_scripts/python/flask-secret/python-linux.sh` — Python with sanitize + normalize flow
- `.github/workflows/test_workflow_scripts/java/spring_petclinic/java-linux.sh` — Java/Spring/Postgres
- `.github/workflows/test_workflow_scripts/golang/risk_profile/golang-linux.sh` — demonstrates per-binary capability detection

All of them:

1. `source $GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/test-iid.sh` — writes a fake `~/.keploy/installation-id.yaml` so telemetry init doesn't prompt.
2. Clean `keploy/` and `keploy.yml` from any prior run.
3. `$RECORD_BIN config --generate` and optionally `sed` noise rules into `keploy.yml` (e.g. `global: {"body": {"updated_at":[]}}`).
4. Bring up any dependency containers (MySQL, Postgres, Mongo, Redis) and wait for readiness.
5. Build the sample app (`go build`, `mvn package`, `npm ci`, `pip install`, …).
6. Define `send_request()` — waits for app health, drives traffic, sleeps, kills keploy by PID (`pgrep keploy`).
7. Run **record** once or twice in the background with `tee`, then grep the log for `"ERROR"` and `"WARNING: DATA RACE"` (both are fatal).
8. Optionally stop the DB containers before replay (forces Keploy to use mocks — catches "mock missed" regressions).
9. Run **replay**: `"$REPLAY_BIN" test -c "./app" --delay N --generateGithubActions=false 2>&1 | tee test_logs.txt`.
10. Walk `./keploy/reports/test-run-*/test-set-*-report.yaml` (newest via `ls -1dt ... | head -n1`), grep each for `status:`, fail if any aren't `PASSED`.
11. Exit 0 on success, 1 on any failure.

### Sample repos CI pulls from

| CI workflow | Sample repo | Where samples live |
|-------------|-------------|--------------------|
| `golang_linux.yml`, `golang_docker.yml`, `golang_wsl.yml`, `grpc_linux.yml`, `schema_match_linux.yml` (go side) | `keploy/samples-go` | `samples-go/<path>` |
| `python_linux.yml`, `python_docker.yml`, `schema_match_linux.yml` (python side) | `keploy/samples-python` | `samples-python/<path>` |
| `node_linux.yml`, `node_docker.yml`, `node_mapping.yml` | `keploy/samples-typescript` | `samples-typescript/<path>` |
| `java_linux.yml` | `keploy/samples-java` | `samples-java/<path>` |
| `dns_mock_test` in `golang_linux.yml` | `akashkumar7902/dns-mock-test` | external mirror |

Other sample repos under the org that exist but are not currently wired into
`prepare_and_run.yml`: `samples-rust`, `samples-csharp`, `ecommerce_sample_app`,
`petclinic-hosted`, `orgChartApi`. Treat them as reference, not CI-backed.

## Debugging hung recordings

If `keploy` appears stuck, `SIGQUIT` dumps a goroutine trace to stderr —
see `DEBUG.md` for the exact PID-finding steps (it differs between native
runs and Docker with `--pid=host`).

## Where to look first for common changes

| If you're changing... | Start here |
|-----------------------|------------|
| Record/replay behavior of a protocol | `pkg/core/proxy/` + `pkg/models/<protocol>` |
| Mock matching logic                  | `pkg/matcher/` + `pkg/service/replay/` |
| On-disk YAML format                  | `pkg/platform/yaml/` + `pkg/models/mock.go`, `testcase.go` |
| CLI flags                            | `cli/<command>.go` + `cli/provider/` |
| Config defaults                      | `config/default.go` + `config/config.go` |
| Test reports                         | `pkg/service/report/` |
| Coverage                             | `pkg/platform/coverage/` |
| eBPF probe behavior                  | `pkg/agent/hooks/` (C sources — do not edit generated Go) |
| Adding a sample to CI                | a sample repo (`samples-go`, …) + `.github/workflows/test_workflow_scripts/<lang>/<script_dir>/` + a matrix entry in the right workflow |

When adding end-to-end coverage for a behavior change, prefer extending an
existing sample + its script over creating a new one. See the
`keploy-e2e-test` skill for the full decision tree.
