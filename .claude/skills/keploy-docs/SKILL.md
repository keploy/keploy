---
name: keploy-docs
description: Guide for contributing to the Keploy documentation site at github.com/keploy/docs. Invoke when a change in keploy/keploy introduces, removes, or alters user-visible behavior (new CLI flag, changed default, new command, new configuration field, new on-disk format) and the docs need to catch up — or when the user asks to document a feature, write a quickstart, or fix a docs bug. Covers where to put the edit, every CI check that must pass, and how to reproduce each check locally before the PR.
---

# keploy-docs

## When to use

- A change in `keploy/keploy` alters user-visible behavior: a new or renamed CLI flag, a changed default, a new command, a new configuration field, a new on-disk file format.
- The user reports a docs bug, confusing passage, broken example, or stale screenshot.
- You are about to claim "feature X is documented" without having checked.

**Do not** touch docs for internal refactors, CI changes, performance work, or bug fixes that don't change user-visible behavior. The docs are a contract with users — edit only when the contract changes.

## Repository layout

- Upstream: `github.com/keploy/docs` — Docusaurus 2 site, published at `https://docs.keploy.io`.
- Node version: `.nvmrc` pins the project; the CI workflows use Node 20. Use `nvm use` before `yarn`.
- Package manager: `yarn` (classic — `yarn.lock` present). `npm install` also works and is what CI runs.

```
keploy-docs/
├── docs/                          # Shared content (components, GSoC, Hacktoberfest). NOT feature docs.
├── versioned_docs/
│   ├── version-4.0.0/             # ← LATEST VERSION. All feature-doc edits go here.
│   ├── version-3.0.0/             # Archived. Touch only for explicit back-ports.
│   ├── version-2.0.0/             # Archived.
│   └── version-1.0.0/             # Archived.
├── versioned_sidebars/
│   ├── version-4.0.0-sidebars.json  # Sidebar for v4 — update when adding a new page.
│   └── ...
├── versions.json                  # Ordered list; first entry ("4.0.0") is current.
├── src/                           # Site React code (theme, components). Infrastructure, not content.
├── plugins/                       # Docusaurus plugins. Infrastructure.
├── static/                        # Images, binary assets.
├── STYLE.md                       # Keploy-specific prose rules (see §Style).
├── CONTRIBUTING.md                # Contribution flow.
├── ADDING_A_QUICKSTART.md         # Checklist for new quickstart guides.
└── .vale.ini, vale_styles/        # Vale prose linter config.
```

## **Rule: edits go in `versioned_docs/version-4.0.0/`**

This is non-negotiable. The current version listed in `versions.json` is `4.0.0`, and the first entry in that file is always the live one. Editing `version-3.0.0/` (or older) only changes archived pages that users see under a banner explaining they are out of date.

Before editing, run `head -n1 versions.json` (or open the file) and confirm the current version. If it has rolled over to `5.0.0` by the time you read this, edit `versioned_docs/version-5.0.0/` instead.

Do **not** put new feature docs under the top-level `docs/` directory — that folder holds components and programme pages (GSoC, Hacktoberfest), not product content.

## Decision: edit existing page vs. add new page

Grep `versioned_docs/version-4.0.0/` for the feature name (and its synonyms) before creating a new file. Most features have a home.

**Add a new page only when:**
- The feature is a distinct workflow with multiple steps (a quickstart, an integration guide).
- No existing page covers the surface, and shoehorning it in would hurt the existing narrative.
- Ask for approval before doing this.

**When you add a page you must also:**
1. Register it in `versioned_sidebars/version-4.0.0-sidebars.json` under the correct section.
2. Match the frontmatter shape of nearby pages. Required keys: `id`, `title`, `sidebar_label`, `description`, `tags`, `keywords`.
3. Keep the URL slug kebab-cased (`my-new-feature`, not `myNewFeature`).
4. For a new quickstart specifically, follow `ADDING_A_QUICKSTART.md` step by step.

## Style rules (from `STYLE.md`)

Docs follow the [Google Developer Documentation Style Guide](https://developers.google.com/style) and the Microsoft Writing Style Guide for anything Google doesn't cover. On top of that:

- **Capitalize Keploy-specific terms.** `Test-Case`, `Test-Set`, `Test-Run`, `Mock`, `Normalise` when referring to the feature. Generic usage stays lowercase and should be rare.
- **Sentence case in headings.** `How to record your first test`, not `How To Record Your First Test`.
- **Infinitive verbs in titles.** `Install Keploy`, not `Installing Keploy`.
- **Active voice.** `Keploy records the calls`, not `The calls are recorded by Keploy`.
- **En dashes (`–`) for numeric ranges.** `5–10 GB`, not `5-10 GB`.
- **Code formatting.** Backticks for inline code. Fenced code blocks with a language hint (`bash`, `go`, `yaml`, `json`).
- **Inclusive, jargon-free prose.** Define acronyms on first use. Prefer short, direct sentences.

Vale enforces a subset of this (see §CI).

## CI pipelines — all must pass 


NOTE: ONLY REPRODUE THESE CI PIPELINES IF YOU ARE NOT CONFIDENT THAT IT WILL PASS IN CI/CD. 

Every PR against `keploy/docs` runs the following GitHub Actions. **All must be green before merge.** Each one is listed below with what it checks and how to reproduce it locally so you fix problems before the CI cycle.

### 1. `Deploy PR Build and Check` (`.github/workflows/build_and_check.yml`)

- Runs: `npm install && npm run build`.
- Fails if Docusaurus can't build the site — broken frontmatter, dead sidebar references, malformed MDX, missing image paths, invalid admonitions.

Reproduce locally:

```bash
yarn         # or: npm install
yarn build   # same as CI's `npm run build`
```

If `yarn build` is slow, `yarn start` (dev server at http://localhost:3000) catches most issues faster and lets you visually verify the page renders.

### 2. `Check Docusaurus docs with Vale linter` (`.github/workflows/vale-lint-action.yml`)

- Runs Vale 3.0.3 over `versioned_docs/` with `filter_mode: diff_context` and `fail_on_error: true`. It only flags lines you added or modified, not pre-existing issues.
- Config lives in `.vale.ini` and `vale_styles/`. Uses the Google style pack plus a custom vocabulary (`vale_styles/config/Vocab/Base/`).
- Alert level is `error` — warnings do not fail the build, but fix them anyway.

Reproduce locally (Vale via Homebrew: `brew install vale`):

```bash
vale versioned_docs/version-4.0.0/<path-to-your-edit>.md
```

Common failures: unspelled Keploy-specific terms (add them to `vale_styles/config/Vocab/Base/accept.txt`), capitalization of Keploy terms, passive voice in Google-pack rules.

### 3. `Lint Codebase` (`.github/workflows/lint.yml`)

- Runs `wagoid/commitlint-github-action@v5` on every commit in the PR.
- Enforces Conventional Commits: `<type>(<scope>): <subject>` with type in `feat | fix | docs | style | refactor | test | chore | build | ci | perf | revert`.
- For docs changes, the type is almost always `docs:`.

just be careful writing the commit messages — the rules are not surprising.

### 4. `Prettify Code` (`.github/workflows/prettify_code.yml`)

- Runs Prettier 2.8.8 in `--check` mode on changed `.js` and `.md` files.
- Config in `.prettierrc.json`: `printWidth: 80`, `proseWrap: preserve`, `tabWidth: 2`, `semi: true`, no `bracketSpacing`, double quotes, trailing commas (`es5`), `arrowParens: always`.

Reproduce locally:

```bash
npx prettier@2.8.8 --check versioned_docs/version-4.0.0/<path-to-your-edit>.md

# To auto-fix:
npx prettier@2.8.8 --write versioned_docs/version-4.0.0/<path-to-your-edit>.md
```

Pin Prettier to `2.8.8` — a newer version will reformat differently and the CI diff will fail.

### 5. `CLA` (`.github/workflows/cla.yml`)

- Requires the contributor to have signed the Keploy CLA. The bot posts a comment on the PR with a signing link if needed. First-time contributors will need to sign once.

### 6. `CodeQL` (`.github/workflows/codeql.yml`)

- Security scanning. Prose edits won't fail this; JS changes might.

### 7. `greetings.yml`

- Welcome bot on first-time contributors. Not a blocking check.

### One-shot pre-push checklist

Run all of these from the docs repo root before pushing:

```bash
yarn                                                      # install
yarn build                                                # pipeline 1
vale versioned_docs/version-4.0.0/<your-file>.md          # pipeline 2
git log origin/main..HEAD --format='%s'                   # eyeball pipeline 3
npx prettier@2.8.8 --check versioned_docs/version-4.0.0/<your-file>.md  # pipeline 4
```

Only push when all four are clean.

## Commits and branches

- Branch name: `docs/<short-kebab>` — e.g. `docs/rename-test-sets-clarify`.
- Commit subject: `docs: <what changed>` in present-tense imperative, under ~70 chars. Use `docs(<scope>):` if a scope helps (`docs(quickstart): add flask-redis sample`).
- Commit body: required. Blank line after the subject, then a paragraph explaining the user-facing gap this closes and any code/behavior reference.
- Sign-off: every commit. `git commit -s`. Do not hand-construct the `Signed-off-by` trailer.
- Do not `--amend` past a failing hook — fix and create a new commit.
- Never `--no-verify`.

## PR template

Follow the PR template that is followed in docs repository.

## Customer-data hygiene

Same rules as `keploy-pr-workflow`. Docs are read by everyone — even the most innocent-looking example can leak a real customer. Before committing:

- No real hostnames; use `example.com`, `app.example.com`, `localhost:8080`.
- No real auth tokens, API keys, JWTs. Use placeholders: `sk-xxxxxxxx`, `Bearer <token>`.
- No real customer/user IDs, emails, order IDs. Use `user@example.com`, `user-123`.
- No copy-pasted production logs, stack traces, or request IDs.
- No screenshots with real PII — scrub or regenerate from a sample app.
- IP addresses must be RFC1918 (`10.*`, `192.168.*`), loopback, or TEST-NET (`192.0.2.1`).

If you're unsure whether something is customer-derived, it is. Replace it.

## Scope discipline

- Only the file(s) required for the change. Don't "clean up" unrelated prose in the same PR — it muddles review and inflates the Vale diff surface.
- Don't reformat surrounding lines. Prettier's `proseWrap: preserve` deliberately keeps hand-chosen line breaks; changing them adds noise.
- Don't add screenshots unless the user explicitly asked for them — they age fast. If you want to add a screenshot, ask for approval before doing so. And also them to provide you with the screenshot. dont leave it blank while creating a PR. just tell at the end that a screenshot would look great, you can add it to the user. 

## Related skills

- `keploy-pr-workflow` — commit format, sign-off, customer-data hygiene. Same rules apply here; this skill specializes them for the docs pipeline.
- `keploy-e2e-test` — if you are documenting a behavior change, verify the behavior against a real sample before writing the doc. Don't document from source-reading alone.
