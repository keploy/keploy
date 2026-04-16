# CLAUDE.md — keploy repo conventions for Claude Code

This file is loaded automatically at the start of every Claude Code session
run from this repository. It carries **non-negotiable** rules for working
in `keploy/keploy`.

## Mandatory skill enforcement

Three repo-specific skills live under `.claude/skills/` and MUST be
invoked automatically — without waiting for the user to ask — whenever
their trigger conditions are met:

| Skill                 | Invoke automatically when…                                                                                             |
| --------------------- | ---------------------------------------------------------------------------------------------------------------------- |
| `keploy-e2e-test`     | After any non-trivial code change (CLI flags, record/replay, proxy, agent hooks, protocol handlers, YAML format, config). |
| `keploy-pr-workflow`  | Before opening/updating a PR or issue; before any push; before composing a commit that leaves the local machine.      |
| `keploy-docs`         | When a code change introduces/removes/alters user-visible behavior (new flag, command, default, config field, on-disk format). |

Unit tests passing and `go build` succeeding are **not** evidence that a
behavior change works. `keploy-e2e-test` describes the canonical
end-to-end signal (build the binary, drive it through record/replay
against a real sample app). Use it, or explicitly name one of the skip
reasons from the skill's own skip list. Do not invent new skip reasons.

Do not report a task complete until the applicable skills have either
been invoked or explicitly skipped with a reason from the skill's skip
list.

## Commit conventions

Every commit in this repo must follow:

- Subject line: `<type>(<scope>): <subject>` where `<type>` ∈
  `feat | fix | docs | style | refactor | test | chore`.
- Mandatory body (after a blank line) explaining **what** changed and
  **why**, no matter how small the change.
- Sign-off trailer: `Signed-off-by: <Name> <email>` (use
  `git commit -s`).

See `keploy-pr-workflow` for the full PR checklist including
customer-data hygiene and sample-repo branch refs.

## What not to commit

- Generated eBPF Go (`pkg/agent/hooks/bpf_*_bpfel.go`) outside an
  intentional regeneration step.
- Run artifacts from sample apps: `keploy/`, `keploy.yml`, `*_logs.txt`.
- Secrets, credentials, or customer data of any shape.
