# Keploy Record/Replay CI Guide

This document describes the **one‑time build / one‑time download** strategy we use in all CI jobs that exercise Keploy, and how you can plug new sample workflows into the same system.

---

## Table of contents

1. Why we build the PR binary **once** and download the latest release **once**.
2. Why we also **build *one* Docker image** and reuse it via a temporary registry.
3. Key files that implement the mechanism.
4. Checklist – add a new language / sample workflow in < 2 mins.
5. Troubleshooting tips.

---

## 1. Why one‑time *build + download*

* **Speed** – compiling and downloading just once keeps the fan‑out jobs lean.
* **Consistency** – every downstream job gets the *exact same* PR binary and the *exact same* "latest" binary (avoids the "a new patch went live mid‑run" race).
* **Flexibility** – the 3‑row test‑matrix lets us cover all flows:
  * record **latest** → replay **build**
  * record **build**  → replay **latest**
  * record **build**  → replay **build** *(the legacy flow)*

---

## 2. Why one‑time *build & push Docker image*
Some samples (e.g. `gin‑mongo` in Docker‑mode) need a container image. Building it repeatedly inside every matrix row is wasteful, so we:

1. **Build once** in `prepare_and_run.yml` → job `build‑docker‑image`.
2. **Push** the result to [`ttl.sh`](https://ttl.sh) with a 1‑hour TTL (`ttl.sh/keploy/keploy:1h`).
3. **Pull & re‑tag** it inside downstream jobs via the composite action `download‑image` so that the image name matches what the samples expect (`ghcr.io/keploy/keploy:v2‑dev`).

Advantages are identical to the binary‑artifact strategy – plus we keep our public registries clean because the image auto‑expires.

---

## 3. Key files

| File / dir                                         | Purpose                                                                                                                                                                             |
| -------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `.github/workflows/prepare_and_run.yml`            | The *aggregator* – builds the PR binary, downloads `latest`, uploads both as artifacts **and** builds + pushes the one Docker image. Then it fans‑out to language/sample workflows. |
| `.github/actions/download-binary/action.yml`       | Composite action – downloads **one** of those two binary artifacts and outputs its absolute path.                                                                                   |
| `.github/actions/download-image/action.yml`        | Composite action – pulls the temporary image from `ttl.sh`, re‑tags it to `ghcr.io/keploy/keploy:v2-dev`, and makes it available for the sample.                                    |
| `.github/workflows/*_linux.yml`, `*_docker.yml`, … | Language/sample workflows. They declare the 3‑row matrix and obtain the two binaries (and, for Docker flows, the image) via the composite actions.                                  |
| `.github/workflows/test_workflow_scripts/*.sh`     | Bash helpers that run the sample under record / replay. All scripts use the two env vars **`$RECORD_BIN`** / **`$REPLAY_BIN`** that the workflow passes in.                         |

### Why fail-fast: false in every matrix?

We set fail-fast: false inside each strategy.matrix to ensure that all matrix permutations run to completion even if one of them fails. This gives us full visibility into the different record/replay combinations, helps surface flaky paths, and prevents a single early failure from masking other regressions.

---

## 4. Adding a new workflow – checklist

1. **Copy an existing sibling** (e.g. `golang_linux.yml`) ➜ rename it.
2. Keep the **matrix** block *as is* unless you truly need fewer combinations.
3. Add the two `download-binary` steps:

   ```yaml
   - id: record
     uses: ./.github/actions/download-binary
     with: { src: ${{ matrix.record_src }} }

   - id: replay
     uses: ./.github/actions/download-binary
     with: { src: ${{ matrix.replay_src }} }
   ```
4. **Docker‑based sample?**  Insert the `download-image` step *before* you start the app:

   ```yaml
   - id: image
     uses: ./.github/actions/download-image
   ```
5. Pass the paths into your run step:

   ```yaml
   env:
     RECORD_BIN: ${{ steps.record.outputs.path }}
     REPLAY_BIN: ${{ steps.replay.outputs.path }}
   ```
6. In the helper script you invoke, ensure every `keploy record|test` call uses `$RECORD_BIN` / `$REPLAY_BIN`.
7. **Wire the workflow** into `prepare_and_run.yml`:

   ```yaml
   run_<new_workflow_name>:
     needs: [build-and-upload, upload-latest]
     uses: ./.github/workflows/my_<new_workflow_name>.yml
   ```
   Replace `<new_workflow_name>` with the name of your workflow.
   **Note:** *If your sample needs the pre‑built Docker image add `build-docker-image` to the `needs` list.*

---

## 5. Troubleshooting

When a workflow fails, and log isn't visible, then it might be due to the error happening not in record/replay step, but somewhere else, could be that some referenced binary was not found (exit code 127), could be some permission issues, could be file not found. Look for the exit code to determine what failed (https://tldp.org/LDP/abs/html/exitcodes.html), and check the previous successful step to pipe out the line in bash script related to the workflow run.

---

