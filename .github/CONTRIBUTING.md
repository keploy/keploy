# Keploy Record/Replay CI Guide

This document explains:

1.  **Why** we build the PR binary + fetch the latest release once and share them across jobs.
2.  **Which files** implement the mechanism.
3.  **How to create a new sample workflow** that plugs into the system.

---
## 1. Why one‑time *build + download*

* **Speed** – compile and fetch just once, then every job reuses the two artifacts
* **Consistency** – all jobs get the exact same *PR binary* and the same *latest release* (avoid “new patch went live mid‑run”).
* **Flexibility** – lets the matrix try the two new flows
  *record latest → replay build*, and, *record build → replay latest* —
  whereas before we only had, *record build → replay build*.


## 2. Key files

| File | Purpose |
|------|---------|
| `.github/workflows/prepare_and_run.yml` | Main aggregator. Builds `keploy` (PR), downloads `latest`, uploads both as artifacts, then fans out to language workflows. |
| `.github/actions/download-binary/action.yml` | Composite action that downloads exactly one of those two artifacts and exposes its absolute path as an output. |
| `.github/workflows/*_linux.yml`, `*_docker.yml`, … | Language/sample‑specific jobs that: <br>• declare the 3‑row matrix (`record_src`, `replay_src`) <br>• call the composite action twice to get the path of binary referenced in record/replay.<br>• pass `RECORD_BIN` / `REPLAY_BIN` as env into their bash helper. |
| `.github/workflows/test_workflow_scripts/*.sh` | use `$RECORD_BIN` / `$REPLAY_BIN` instead of `../../keployv2`. |

---

## 3. Adding a new workflow (check‑list)

1. **Copy a sibling sample** (e.g. `golang_linux.yml`) and rename it.
2. Keep the **matrix** block; change nothing unless you truly need fewer rows.
3. **Checkout your sample repo** (or build your app) as usual.
4. Add the two `uses: ./.github/actions/download-binary` steps:  
   ```yaml
   - id: record
     uses: ./.github/actions/download-binary
     with: { src: ${{ matrix.record_src }} }

   - id: replay
     uses: ./.github/actions/download-binary
     with: { src: ${{ matrix.replay_src }} }
  ```
5. Pass the paths into your run step:

   ```yaml
   env:
     RECORD_BIN: ${{ steps.record.outputs.path }}
     REPLAY_BIN: ${{ steps.replay.outputs.path }}
   ```
6. In the **helper script** you invoke, make sure every `keploy record|test` call uses the 2 env variables passed to it.
7. Finally, **wire the new workflow** into `prepare_and_run.yml` by adding one line:

   ```yaml
   run_my_new_workflow:
     needs: [build-and-upload, upload-latest]
     uses: ./.github/workflows/my_new_workflow.yml
   ```

---
