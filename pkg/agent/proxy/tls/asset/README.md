# `asset/` — channel-binding shim build output

`cbshim.c` is the LD_PRELOAD shim source. The compiled `.so` files
(`cbshim_amd64.so`, `cbshim_arm64.so`) are **not committed**; they get
built fresh inside the enterprise Docker image's `build` stage and
embedded into the keploy binary via `//go:embed asset/*` in `../ca.go`.

This mirrors how `pkg/util/time/assets/` works for the time-freezing
feature — same gitignore strategy, same cross-compile-via-clang
pattern, same CI orchestration script.

## OSS builds

`go build` succeeds without any `.so` files present — `//go:embed
asset/*` is satisfied by this README, `build.sh`, and `cbshim.c`. The
runtime helper (`getCBShim()` in `../ca.go`) returns `nil` when no
shim variant is embedded, and every caller downgrades gracefully:
`LD_PRELOAD` is never set, the cbmap publishing keeps working but has
no consumer, and SCRAM-SHA-256-PLUS clients fall back to today's
"FATAL channel binding check failed" behaviour. No new failure modes.

## Enterprise / Pro builds

`.ci/scripts/prepare-dockerfile.sh` (in the enterprise repo) injects
a `RUN clang --target=...` block right after `COPY . /app` that
cross-compiles `cbshim.c` to both `amd64` and `arm64` `.so`s and
writes them to this directory. The subsequent `go build` then embeds
them and the runtime selector picks the matching one per
`runtime.GOARCH`.

## Local development

Run `./build.sh` from anywhere in the repo. Requires `clang-14` and
`gcc-aarch64-linux-gnu` (Debian/Ubuntu: `apt-get install clang-14
gcc-aarch64-linux-gnu libssl-dev`). Drops `cbshim_amd64.so` and
`cbshim_arm64.so` into this directory for a local `go build`.

`./build.sh` output is gitignored (see `.gitignore` here) so accidental
commits aren't possible.

## Why not commit the binaries?

- **No binary blobs in source control** — reviewable diffs only.
- **Single source of truth** — bumping `cbshim.c` automatically
  produces fresh artifacts on the next build, no out-of-band rebuild
  step.
- **Same trust model as everything else in the binary** — the .so
  comes from the same supply chain as the rest of keploy.
- **Same pattern keploy already uses** — see `pkg/util/time/assets/`
  and its sibling `pkg/util/time/build.sh` for the time-freezing
  equivalent.
