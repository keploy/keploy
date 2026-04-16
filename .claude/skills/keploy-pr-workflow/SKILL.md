---
name: keploy-pr-workflow
description: INVOKE AUTOMATICALLY before creating or updating a PR or issue on any keploy repository, before any `git push` to a shared branch, and before composing any commit that will leave the local machine. Carries required PR format, commit-message conventions, Signed-off-by enforcement, and customer-data hygiene checks. Also invoke when the user asks to open, update, or review a PR/issue. Do not draft a PR body, push to a remote branch, or commit-and-push without first consulting this skill.
---

# keploy-pr-workflow

## When to use

- About to run `gh pr create`, `gh pr edit`, or `gh issue create`.
- Writing a commit message that will land in `main`.
- Reviewing your own diff before pushing.
- Copying error output, logs, or sample data into a PR description, issue, test
  fixture, or README.

## 1. Customer-data hygiene (non-negotiable)

Keploy records real user applications. Traces, mocks, recordings, and logs
routinely carry customer data — headers with auth tokens, bodies with PII,
internal hostnames, request IDs that map back to users. Treat every fixture,
log snippet, and error dump as tainted until you've checked it.

Before anything leaves your machine, scrub for:

- **Credentials** — API keys, bearer tokens, JWTs, DB passwords, session
  cookies, OAuth client secrets, AWS/GCP/Azure keys. If a test needs one,
  read from env; use placeholders in docs (`sk-xxxxxxxx`, `Bearer <token>`).
- **Internal hostnames and URLs** — `*.internal`, `*.prod`, `*.corp`, real
  company domains. Use `example.com`, `httpbin.org`, or loopback in samples.
- **IP addresses that aren't RFC1918 / loopback / TEST-NET** — assume any
  public IP in a log is traceable. Replace with `192.0.2.1` (TEST-NET-1).
- **User identifiers** — emails, usernames, account IDs, order IDs, customer
  names. Substitute with `user@example.com`, `user-123`, etc.
- **Request/trace IDs** — these tie back to real traffic in observability
  systems. Redact them from pasted logs.
- **Real recorded traffic** — never commit a customer's `keploy/test-set-*`
  directory. Even anonymized ones tend to keep giveaways in paths or timings.
  If you need sample recordings, generate them against `samples-go`,
  `samples-python`, etc.
- **Stack traces from production runs** — they leak file paths, binary
  versions, and sometimes in-memory values.

If you're unsure whether something is customer-derived, it is. Err on the
side of redaction — you can always add detail back, you can't un-publish.

## 2. Commit messages

Format: `<type>(<scope>): <subject>` — Conventional Commits, enforced by
`commitizen` via `.pre-commit-config.yaml`.

Types: `feat`, `fix`, `docs`, `style`, `refactor`, `test`, `chore`.

Rules:

- Subject in present tense, imperative mood. `fix: resolve null pointer on
  test-set reset`, not `fixed` or `fixes`.
- Every commit includes a body — blank line after the subject, then a
  paragraph describing **what changed and why**. Mandatory even for
  one-liners.
- Sign off with `git commit -s`. Let git read identity from config; don't
  hand-construct the `Signed-off-by` trailer.

## 3. PR/issue title and body

The PR/issue template should match the template that is being used in the repository that we are working in. 

## 4. Destructive git operations

Never, without explicit user approval:

- `git push --force` to `main` (or any shared branch).
- `git reset --hard` / `git clean -fd` on a branch with uncommitted work.
- `git branch -D` on anything not your own local branch.
- Rebase across other contributors' published commits.

When in doubt, ask. Destructive ops are cheap to confirm and expensive to
undo.

## 5. Issues

When filing or commenting on an issue:

- Same customer-data rules apply — scrub logs before pasting.
- Include: keploy version (`keploy --version`), OS/arch, the exact command
  you ran, and whether it reproduces in Docker vs native.
- If you're attaching a recording to reproduce, regenerate it against a
  public sample app; never attach a customer's test-set.

## Related skills

- `keploy-e2e-test` — verify a behavior change against a real sample before
  opening the PR.
