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

| Platform                  | Native binary (app runs on host)                                                                             | Keploy-in-Docker (app runs in Docker) |
| ------------------------- | ------------------------------------------------------------------------------------------------------------ | ------------------------------------- |
| **Linux** (x86_64, arm64) | ✅ Supported — uses eBPF (`pkg/agent/hooks/linux/`). Requires root.                                          | ✅ Supported                          |
| **Windows** (amd64)       | ✅ Supported — uses the WinDivert redirector (`pkg/agent/hooks/windows/`, `libwindows_redirector.a`).        | ✅ Supported                          |
| **Windows** (arm64)       | ❌ Falls through to the `others` stub — `Load()` / `Record()` return "not supported on non-Linux platforms". | ✅ Supported                          |
| **macOS** (amd64, arm64)  | ❌ Same `others` stub — there is **no** native interception path on macOS.                                   | ✅ Supported (only option)            |

- On **macOS** you _cannot_ use keploy natively. You must:
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

Linter is `golangci-lint` with config schema v2, at `.golangci.yml`:

- Enabled linters: `govet`, `staticcheck`, `errcheck`, `ineffassign`, `unused`
- Formatters: `gofmt`, `goimports`
- Paths excluded from linters: the generated eBPF Go files (currently `pkg/agent/hooks/bpf_arm64_bpfel.go` and `pkg/agent/hooks/bpf_x86_bpfel.go`) and `pkg/service/utgen`

```bash
golangci-lint run
```

### Commit hygiene

- `.pre-commit-config.yaml` wires `commitizen` (Conventional Commits).
- `.cz.toml` pins the convention to `cz_conventional_commits`. Use types
  like `feat:`, `fix:`, `chore:`, `refactor:`, `test:`, `docs:`.
- Every commit needs a description body (blank line, then a paragraph
  explaining what changed and why).
- Sign off every commit with `git commit -s` — this appends a
  `Signed-off-by: <user.name> <user.email>` trailer using the values from
  the effective git config (system → `~/.gitconfig` → `.git/config`). Do
  not hand-construct the trailer; let git read the identity from config so
  it matches the author.

When opening PRs or issues — customer-data hygiene, PR body template, which
files not to touch — see the **`keploy-pr-workflow` skill**
(`.claude/skills/keploy-pr-workflow/SKILL.md`). Don't paste real traces,
tokens, internal hostnames, or production logs into PRs, issues, tests, or
committed fixtures.

## Key commands (quick user-facing recap)

| Command                      | Package                          | What it does                                                                     |
| ---------------------------- | -------------------------------- | -------------------------------------------------------------------------------- |
| `keploy record -c "<cmd>"`   | `pkg/service/record`             | Runs the app, captures dependency traffic into `./keploy/test-set-*`             |
| `keploy test -c "<cmd>"`     | `pkg/service/replay`             | Replays recorded calls, mocks dependencies, writes `./keploy/reports/test-run-*` |
| `keploy rerecord -c "<cmd>"` | `pkg/service/orchestrator`       | Re-records against new code to pick up accepted changes                          |
| `keploy normalize`           | `pkg/service/tools`              | Accepts newly-observed responses into the golden test cases                      |
| `keploy sanitize`            | `pkg/service/tools`              | Scrubs secrets using `custom_gitleaks_rules.toml` + built-in rules               |
| `keploy templatize`          | `pkg/service/tools`              | Replaces dynamic values with templates in test sets                              |
| `keploy config --generate`   | `cli/config.go`                  | Writes a default `keploy.yml`                                                    |
| `keploy contract ...`        | `pkg/service/contract`           | OpenAPI contract generation / testing                                            |
| `keploy diff <r1> <r2>`      | `pkg/service/diff`               | Diff two test runs                                                               |
| `keploy report`              | `pkg/service/report`             | Summarize a previous test run                                                    |
| `keploy export` / `import`   | `cli/export.go`, `cli/import.go` | Move test-sets between repos                                                     |
| `keploy update`              | `cli/update.go`                  | Self-update the binary                                                           |
| `keploy agent`               | `cli/agent.go`                   | Internal — used by the Docker image entrypoint                                   |

Note: `keploy gen` (`pkg/service/utgen`, LLM unit-test generation) has its CLI
registration commented out in `cli/utgen.go` — the implementation exists but
the command isn't wired. Don't document it as user-facing.

`keploy --help` is authoritative; if you add, rename, or disable a command,
update this table in the same commit. Spin up a subagent to walk the `cli/`
directory if you need the full flag list for a specific command.

When a change alters user-visible behavior (new flag, changed default, new
command, new config field), the docs at `keploy/docs` may need updating — see
the **`keploy-docs` skill** (`.claude/skills/keploy-docs/SKILL.md`) for where
to put the edit and which CI checks must pass.

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

### Keploy-specific

- **Package doc comments** — every `package foo` opens with `// Package foo ...`.
- **Root context** — get the cancellable root from `utils.NewCtx()` (`utils/ctx.go`). It installs SIGINT/SIGTERM handlers that call `cancel`. Don't call `context.Background()` in service code.
- **Goroutine lifecycle** — use `errgroup.WithContext(ctx)` for any goroutine, not bare `go func()` or `sync.WaitGroup`. Split work across per-phase errgroups (setup / run-app / req) with their own cancel funcs so one phase can tear down without killing the others — `pkg/service/record/record.go:78-88` is the canonical layout.
- **Logging** — thread `*zap.Logger` explicitly (no globals); build once via `utils/log.New()`. Use `utils.LogError(logger, err, "msg", ...fields)` in place of `logger.Error(...)` — it drops `context.Canceled` so expected shutdown paths don't spam the log. (It does **not** set `ErrCode`; that's separate.). If something is failing, the logs should also tell the user the next step on what to do.
- **Exit code** — `utils.ErrCode` is a package-level `int` that `main.go` passes to `os.Exit`. Set it to `1` when you want the process to exit non-zero; today only `pkg/service/replay/replay.go` flips it (on failed test runs).
- **Errors** — wrap with `fmt.Errorf("...: %w", err)`. Classify app-lifecycle failures with `models.AppError` + `models.AppErrorType` (string enum in `pkg/models/errors.go`). Prefer `errors.Is` / `errors.As` over string matching; custom errors carrying a diagnostic payload (e.g. `mockMismatchError`) must implement `Unwrap()`.
- **Config access** — services read from the `*config.Config` wired in `cli/provider/`. Don't call `os.Getenv` from `pkg/service/` or `pkg/core/`; add a field to `config.Config`, parse it in `cmdConfigurator` / `main.go`, thread it in.
- **Interfaces live where they're consumed** — each `pkg/service/<name>/service.go` defines the small interfaces that package depends on (`TestDB`, `MockDB`, `Telemetry`, `Instrumentation`, ...). Keep them 1–10 methods and role-shaped; concrete implementations live in separate packages (e.g. `pkg/platform/yaml/testdb/`) and are wired in `cli/provider/core_service.go`.
- **Context-aware I/O** — long-lived flows should use `ctxReader` / `ctxWriter` from `pkg/platform/yaml/yaml.go` so file I/O honors cancellation.
- **Generated code** — the eBPF Go files `pkg/agent/hooks/bpf_*_bpfel.go` (today: `bpf_arm64_bpfel.go`, `bpf_x86_bpfel.go`) are generated from `.c` sources. Never edit by hand; they're linter-excluded in `.golangci.yml`. Regenerate via the eBPF toolchain if probe behavior must change.

### General Go hygiene (universal rules this codebase follows)

- **Accept interfaces, return structs.** Define an interface at the point of _use_, not the point of _implementation_. If there's a single impl and a single consumer, you probably don't need the interface yet.
- **`context.Context` is the first parameter** on every exported method — after the receiver, before everything else. Never store a context in a struct.
- **Don't thread state through `context.WithValue`.** It's for request-scoped values (trace IDs, auth), not for dependency injection — pass dependencies as struct fields or function args. The one tolerated exception here is `models.ErrGroupKey`, which carries the parent errgroup.
- **Panic at boundaries only.** Library and service code returns errors. `recover` lives at top-level goroutine entry points and `main` (see `utils.Recover`). Don't panic to signal expected failure.
- **Table-driven tests with `testify`.** `require` for fail-fast assertions, `assert` when the test should continue. Unit coverage is sparse in this repo — protocol-level behavior is verified via the `keploy-e2e-test` skill, so write table-driven tests for pure logic and reach for e2e when behavior crosses a boundary.
- **Doc comments on every exported symbol.** `<Name> does X.` form — godoc is the public contract.
- **Keep functions short and intention-revealing.** If a function has more than ~3 nested levels or ~50 logical lines, split it before adding more branches. Naming earns its keep: a function named `parseRecordFrame` beats a comment above a block called `doStep`.
- **Minimize exported surface.** Only capitalize identifiers that actually need to leave the package. Shrinking the public API is the cheapest refactor available.

Note: All of the things mentioned in Conventions can be ignored if you are not sure about it and you are not fully confident. 

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
   - `build` — `go build -race -tags=viper_bind_struct` (CGO on)
   - `latest` — downloads the most recent GitHub release
     Each is uploaded as an artifact with that name.
2. **Builds a Docker image** and pushes it to `ttl.sh` with a unique per-run tag.
3. **Fans out** to per-language workflows via `workflow_call`:
   - `golang_linux.yml` — `samples-go` apps, native Linux
   - `golang_docker.yml` — `samples-go` apps, through the Docker image
   - `golang_wsl.yml` — `samples-go` apps, WSL
   - `python_linux.yml`, `python_docker.yml` — `samples-python`
   - `node_linux.yml`, `node_docker.yml` — `samples-typescript`
   - `java_linux.yml` — `samples-java`
   - `grpc_linux.yml` — gRPC-specific go matrix
   - `schema_match_linux.yml` — schema-match matrix (python)
   - `fuzzer_linux.yml`, `node_mapping.yml` — specialised
4. **Gates on a single job `gate`**. That's the only required status check — re-running the `gate` alone is pointless; it just re-checks upstream results.

### Matrix structure (the pattern every language workflow follows)

```yaml
matrix:
  app:
    - name: <display-name>
      path: <directory in samples-<lang> repo>
      script_dir: <directory in .github/workflows/test_workflow_scripts/<lang>/>
  config:
    - job: record_latest_replay_build # record with released binary, replay with this PR's build
      record_src: latest
      replay_src: build
    - job: record_build_replay_latest # record with this PR's build, replay with released binary
      record_src: build
      replay_src: latest
    - job: record_build_replay_build # both this PR's build — exercises same-version behavior
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

You can add your own way if you are confident about it. 

### Sample repos CI pulls from

| CI workflow                                                                                                     | Sample repo                    | Where samples live          |
| --------------------------------------------------------------------------------------------------------------- | ------------------------------ | --------------------------- |
| `golang_linux.yml`, `golang_docker.yml`, `golang_wsl.yml`, `grpc_linux.yml`, `schema_match_linux.yml` (go side) | `keploy/samples-go`            | `samples-go/<path>`         |
| `python_linux.yml`, `python_docker.yml`, `schema_match_linux.yml` (python side)                                 | `keploy/samples-python`        | `samples-python/<path>`     |
| `node_linux.yml`, `node_docker.yml`, `node_mapping.yml`                                                         | `keploy/samples-typescript`    | `samples-typescript/<path>` |
| `java_linux.yml`                                                                                                | `keploy/samples-java`          | `samples-java/<path>`       |


## Where to look first for common changes

| If you're changing...                | Start here                                                                                                                              |
| ------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------- |
| Record/replay behavior of a protocol | `pkg/core/proxy/` + `pkg/models/<protocol>`                                                                                             |
| Mock matching logic                  | `pkg/matcher/` + `pkg/service/replay/`                                                                                                  |
| On-disk YAML format                  | `pkg/platform/yaml/` + `pkg/models/mock.go`, `testcase.go`                                                                              |
| CLI flags                            | `cli/<command>.go` + `cli/provider/`                                                                                                    |
| Config defaults                      | `config/default.go` + `config/config.go`                                                                                                |
| Test reports                         | `pkg/service/report/`                                                                                                                   |
| Coverage                             | `pkg/platform/coverage/`                                                                                                                |
| eBPF probe behavior                  | `pkg/agent/hooks/` (C sources — do not edit generated Go)                                                                               |
| Adding a sample to CI                | a sample repo (`samples-go`, …) + `.github/workflows/test_workflow_scripts/<lang>/<script_dir>/` + a matrix entry in the right workflow |

When adding end-to-end coverage for a behavior change, prefer extending an
existing sample + its script over creating a new one. See the
`keploy-e2e-test` skill for the full decision tree.
