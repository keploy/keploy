---
name: keploy-e2e-test
description: INVOKE AUTOMATICALLY after implementing any non-trivial change to keploy/keploy — new/changed CLI flag or command, record/replay pipeline, proxy, agent hook, protocol handler, YAML format, matcher, coverage, or config. Unit tests and `go build` are NOT sufficient; this skill runs keploy's own record/replay against a real sample app, the same way CI does. Also invoke when the user asks to test, verify, prove, reproduce, or wire behavior into CI. Skip only with one of the reasons in the skill's skip list (pure refactor, enterprise-only, CLI-output-only, docs-only, missing infra, unclear scope) — state which one. Covers deciding whether e2e is the right signal, finding or extending an existing sample script, or (only when necessary) adding a new sample and matrix entry.
---

# keploy-e2e-test

End-to-end verification of a change in `keploy/keploy` by building the
binary from source and driving it through real record → replay against a
real sample app, exactly the way CI does.

## When to use

Trigger this skill whenever the user asks to:

- test / verify / prove a change works against a real app
- add e2e coverage for a fix, feature, or PR
- reproduce an issue against one of the sample apps
- wire a new behavior into the CI matrix

## When to decline (and say so explicitly)

Before doing any work, decide whether e2e record/replay is even the right
signal. Skip with a clear, short reason if:

- **Pure refactor / internal rename.** No observable behavior changed. Unit
  tests + `go build` + `golangci-lint run` is enough.
- **Enterprise-only code path.** The logic lives in the private
  `keploy/integrations` repo, not in this tree. You cannot fully exercise
  it from a fresh fork — CI skips those jobs on forks, and so should you.
  Say so and stop.
- **CLI-only / output-formatting changes.** If the change is in help text,
  flag parsing, or log formatting, a unit test or a manual `keploy --help`
  check is sufficient.
- **Docs / workflow YAML only.** No behavior change.
- **Infrastructure the script relies on is missing.** If the affected code
  path needs a dependency (eBPF kernel features, a managed DB, a paid
  service) that isn't available in the sandbox you're running in, say so
  and propose what would be needed instead of half-running.
- **The user hasn't told you what changed.** Ask. Don't guess an app.

If you skip, say *why* in one or two sentences and stop — do not silently
fall back to "just run the unit tests."

- Also, you can even skip unit tests if they dont make sense because our code keeps on changing.
- If you are unsure about some stuff make sure you ask for approval before doing so.

## Step 1 — Understand the change

Before touching anything, build a concrete picture of:

1. **What code changed.** Read the diff (`git diff main...HEAD`, or the
   files the user points at). For a PR, read the PR description and the
   changed files — not just the titles.
2. **Which code path it exercises.**
   - Is it HTTP request/response matching? (`pkg/matcher`, `pkg/service/replay`)
   - A protocol parser? (`pkg/core/proxy`, `pkg/models/<protocol>`, `pkg/agent/hooks`)
   - On-disk YAML format? (`pkg/platform/yaml`)
   - Mock correlation / ordering? (`pkg/service/orchestrator`)
   - Reports / coverage? (`pkg/service/report`, `pkg/platform/coverage`)
   - A CLI command or flag? (`cli/<cmd>.go`, `config/config.go`)
   - Memory / performance? (`pkg/agent/memoryguard`, `main.go`)
3. **What user-visible behavior should now change.** State it in one
   sentence before you start looking for an app.
4. **What the failure mode would look like** in a report — a missing mock?
   a status mismatch? a race in the record log? Knowing this up front tells
   you what to grep for after replay.

If the affected path is obvious from the diff, say so and move on. If it
isn't, say so and ask — do not guess.

## Step 2 — Find an existing sample app that exercises this path

Search broadly across the keploy organization for a sample that already
exercises the affected surface area. The sample repos CI pulls from are in
`AGENTS.md` under "Sample repos CI pulls from"; the complete list of sample
repos on the org is discoverable with `gh repo list keploy`. its usually samples-* which * can be go, java etc. 

How to search, in rough order of cost:

1. **Look at the existing script directory first.** The canonical index of
   what's already covered is `.github/workflows/test_workflow_scripts/`.
   Each subdirectory maps 1:1 to a sample app, and the shell script inside
   is what CI actually runs against that app. `ls` that tree — if one of
   the names obviously matches your changed path (e.g. you changed DNS
   parsing → check `golang/dns_mock/`, `golang/dns_dedup/`), read that
   script first.
2. **Search the matrix entries.** `grep -rn script_dir .github/workflows/*.yml`
   shows every app currently wired into CI with its `path` (directory in
   the samples repo) and `script_dir` (directory in `test_workflow_scripts`).
3. **Search the sample repos themselves.** For a protocol or stack not
   covered by an in-tree script:
   ```bash
   gh api repos/keploy/samples-go/contents/       --jq '.[] | select(.type == "dir") | .name'
   gh api repos/keploy/samples-python/contents/   --jq '.[] | select(.type == "dir") | .name'
   gh api repos/keploy/samples-typescript/contents/ --jq '.[] | select(.type == "dir") | .name'
   gh api repos/keploy/samples-java/contents/     --jq '.[] | select(.type == "dir") | .name'
   gh api repos/keploy/samples-rust/contents/     --jq '.[] | select(.type == "dir") | .name'
   gh api repos/keploy/samples-csharp/contents/   --jq '.[] | select(.type == "dir") | .name'
   ```
   Don't hard-code which repo to look in — the best match may be in a
   less-obvious place (e.g. a graphql-over-postgres change might be best
   exercised by `samples-go/graphql-sql` *or* by `samples-java/spring-boot-postgres-graphql`).
   Skim each candidate's README in the sample repo before committing.
4. **When nothing fits.** If no sample on the org exercises the affected
   path, you have two honest options:
   - Extend the closest existing sample (see Step 3b).
   - Add a new minimal sample (see Step 3c). This is the last resort — new
     samples are expensive to maintain.
   - dont be relunctant to do these steps if you think it is necessary.

## Step 3 — Decide: use, extend, or create

### 3a. Use as-is (preferred)

If an existing `test_workflow_scripts/<lang>/<script_dir>/<lang>-linux.sh`
already exercises the exact behavior your change touches — use it. Don't
modify the script. Just build the binary and run it (Step 4).

### 3b. Extend an existing script (second preference)

If the app is right but the script doesn't cover the new behavior, make
the *minimal* additive change:

- Add the new traffic call inside the existing `send_request()` (or
  whatever the script calls its traffic driver).
- If the change introduces a new report field or a new exit-code
  expectation, add a new check after the existing `check_test_report` /
  report-walking loop — do not rewrite the loop.
- Keep everything that already passes passing. If you must change an
  existing assertion, state *why* explicitly in a comment on that line.
- Preserve the script's prevailing style — `set -Eeuo pipefail` vs. plain,
  `section`/`endsec` GitHub grouping helpers, trap handlers, 1 vs. 2
  record iterations. Match what's already there.

When in doubt, look at how recent extensions were done by reading the git
history of the script you're editing.

Please dont break existing code while doing it. 

### 3c. Create a new sample + script (last resort)

Only if no existing app exercises the path. This has two sides:

**Sample-app side (in `keploy/samples-<lang>/`):**
- Keep it minimal: one binary / module, one HTTP surface, one dependency
  if required. Match the structure of the smallest existing samples in
  the same repo (e.g. `samples-go/http-pokeapi`, `samples-python/flask-secret`).
- You cannot merge a new sample into the org sample repos yourself without
  a PR there. If that's out of scope for this task, say so and propose the
  sample contents instead of silently trying to checkout a branch that
  doesn't exist. 
  Do create a PR in the sample repository while using the skill `keploy-pr-workflow` to create the PR.

**`keploy/keploy` side (this repo):**
- Create `.github/workflows/test_workflow_scripts/<lang>/<script_dir>/<lang>-linux.sh`.
  Name `<script_dir>` as `lower_snake_case` of the sample folder name
  (convention: see existing mappings — `echo-mysql` → `echo_mysql`,
  `http-pokeapi` → `http_pokeapi`, `go-grpc` → `go-grpc` (kept hyphenated
  here, so copy the convention of the closest existing neighbor rather
  than normalizing blindly)).
- Copy the boilerplate from the closest existing script in the same
  language. Canonical references:
  - Go + HTTP: `.github/workflows/test_workflow_scripts/golang/http_pokeapi/golang-linux.sh`
  - Go + SQL/MySQL: `.github/workflows/test_workflow_scripts/golang/echo_mysql/golang-linux.sh`
  - Node + Mongo: `.github/workflows/test_workflow_scripts/node/express_mongoose/node-linux.sh`
  - Python + Flask: `.github/workflows/test_workflow_scripts/python/flask-secret/python-linux.sh`
  - Java + Postgres: `.github/workflows/test_workflow_scripts/java/spring_petclinic/java-linux.sh`
  - gRPC (modes): `.github/workflows/test_workflow_scripts/golang/go-grpc/grpc-linux.sh`
- Add a matrix entry under the right per-language workflow:
  - Go native Linux → `.github/workflows/golang_linux.yml`
  - Go Docker → `.github/workflows/golang_docker.yml`
  - gRPC → `.github/workflows/grpc_linux.yml`
  - Python → `.github/workflows/python_linux.yml` (native) / `python_docker.yml`
  - Node → `.github/workflows/node_linux.yml` / `node_docker.yml`
  - Java → `.github/workflows/java_linux.yml`
  - Schema match → `.github/workflows/schema_match_linux.yml`
  The entry must have `name`, `path` (dir inside the samples repo),
  `script_dir` (dir inside `test_workflow_scripts/<lang>/`), plus any
  extra axes the workflow uses (e.g. `mode`, `enable_ssl`).
- Do **not** add a new top-level workflow file; reuse the existing one
  for the language. The gate job in `prepare_and_run.yml` depends on the
  existing workflow names — new ones won't be required checks. 
- If needed, you can create a new workflow file but try to add a new step in the existing workflow file if possible. that's much better than creating a new workflow file.
- If the sample needs a branch of the samples repo other than `main`,
  follow the existing pattern of `git fetch origin && git checkout
  origin/<branch>` at the top of the script (see `echo_mysql`,
  `risk_profile`, `sse_preflight`). Document that branch in the PR
  description on the samples-repo side.

## Step 4 — Run record / replay locally

Always build the binary the same way CI does before running:

```bash
# matches CI's "build-no-race" artifact
go build -tags=viper_bind_struct -o ./out/build-no-race/keploy .

# matches CI's "build" artifact (race-enabled; needs CGO)
CGO_ENABLED=1 go build -race -tags=viper_bind_struct -o ./out/build/keploy .
```

Then run the script the way CI does (from inside the sample app
directory), passing the binaries through the env:

```bash
# Checkout samples repo beside this one (once)
git clone https://github.com/keploy/samples-go   ../samples-go

cd ../samples-go/<matrix.app.path>

RECORD_BIN=/abs/path/to/keploy/out/build/keploy \
REPLAY_BIN=/abs/path/to/keploy/out/build/keploy \
GITHUB_WORKSPACE=/abs/path/to/keploy \
bash -x $GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/<lang>/<script_dir>/<lang>-linux.sh
```

`GITHUB_WORKSPACE` must point at the keploy repo root because scripts
source `test-iid.sh` and `update-java.sh` via that path.

On Linux, `keploy` needs root for eBPF — most scripts use `sudo` on
specific commands. Don't add `sudo` to the whole script invocation if the
script doesn't already do that; copy the pattern used by the existing
scripts (selective `sudo -E env PATH=$PATH "$RECORD_BIN" …`, sometimes
`sudo "$RECORD_BIN" config --generate`).

On macOS/Windows the same script pattern won't work unmodified — in those
environments either fall back to the `*_macos.yml` / `*_windows.yml` /
`.ps1` counterparts, or say you can't reproduce locally and run it via
CI. State that limitation explicitly rather than pretending to test.

If you can't run the script locally (no Docker, no eBPF support in a
container, missing dependency), **say so**. Do not claim success without
real evidence.

## Step 5 — Verify

The script's own success criterion is: all `./keploy/reports/test-run-*/
test-set-*-report.yaml` have `status: PASSED`, and no `"ERROR"` or
`"WARNING: DATA RACE"` lines appeared in the record or test logs.

You must independently verify the same:

```bash
# the newest test-run dir
RUN_DIR=$(ls -1dt ./keploy/reports/test-run-* | head -n1)

# every report should say PASSED
for rpt in "$RUN_DIR"/test-set-*-report.yaml; do
  awk '/^status:/{print FILENAME": "$2; exit}' "$rpt"
done

# coverage (when the sample enables it)
[ -f "$RUN_DIR/coverage.yaml" ] && cat "$RUN_DIR/coverage.yaml"
```

Additionally:

- Confirm record ran the expected number of iterations (most scripts loop
  1 or 2 times — check the `for i in 1 2; do` construct).
- If the change is behavior-additive, the new test case or new assertion
  you added must be present in a generated `test-set-<n>/tests/test-*.yaml`
  or in the report's per-test breakdown.
- If the change is a fix, run the script **against the previous `main`
  binary first** (use the `latest` artifact pattern from CI, or just
  build `main` into a separate path). Confirm the script fails before
  your change and passes after. That is the clean proof of a fix.
- Grep record and replay logs for the known-fatal markers: `grep -E
  "ERROR|WARNING: DATA RACE"`. An "ERROR" line in keploy output fails
  the script in CI; reproduce that gate locally.

## Step 6 — Report back

End with a short, factual summary:

- Which sample app was used (`samples-<lang>/<path>`), and whether it was
  used as-is, extended, or newly created.
- Which script ran (`test_workflow_scripts/<lang>/<script_dir>/<lang>-linux.sh`).
- What command produced the evidence (the exact `RECORD_BIN=… REPLAY_BIN=… bash …` line).
- What the reports said (how many test sets, all PASSED, coverage %).
- If a pre-change run showed the failure mode, say so and include the
  relevant diff in report output.
- If you skipped e2e, say **which** skip reason applied from the list at
  the top — don't invent a new reason.
- If something is failing with your changes or you found a bug in the implementation using this test then please try to fix it by finding the root cause. If you cant then report it to the user.

## What not to do

- Don't invent a new shell-script pattern. If the existing scripts use
  `set -Eeuo pipefail` + `::group::` sections + a `die` trap, match that;
  if they use a simpler linear style (`http_pokeapi`), match that. Pick
  one of the existing templates as a starting point.
- Don't commit `keploy/`, `keploy.yml`, `*_logs.txt`, or other run
  artifacts from a sample app. Those are generated per run. It might be needed 
  if the only thing you are checking is the test mode or other modes and 
  you know you havent made any changes to the record mode. We can save CI times
  by not running the record mode.
- Don't edit generated eBPF Go (`pkg/agent/hooks/bpf_*_bpfel.go`) as part
  of the e2e flow. If the eBPF programs need to change, that belongs in a
  separate, intentional step (edit the `.c` source and regenerate).
- Don't skip the cross-version matrix dimension. The three configs
  (`record_latest_replay_build`, `record_build_replay_latest`,
  `record_build_replay_build`) exist to catch mock-format incompatibility.
  If your change would be gated on "both sides are new," either add
  capability detection (see
  `.github/workflows/test_workflow_scripts/golang/risk_profile/golang-linux.sh`
  and `connect_tunnel/golang-linux.sh` for how the existing scripts
  distinguish `*/build/keploy` from `latest`), or flag the incompatibility
  to the user so they can decide.
- Don't claim "tests pass" from compilation alone. `go build` succeeding
  isn't a substitute for a real replay run. If you couldn't run, say so.

For anything in samples repo side changes, you must have created a branch. Use that branch has ref in keploy/keploy to test it. 