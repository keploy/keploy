#!/usr/bin/env bash
#
# Builds the Keploy Windows interception shim into the embedded asset that
# pkg/agent/hooks/winshim serves to instrumented applications.
#
# The Windows release binaries are cross-compiled and the shim links MinHook,
# which is C, so the DLL cannot be produced as part of `go build`. It is built
# here, committed as a binary asset, and pulled in with go:embed. Run this
# whenever keploy_winshim.c changes:
#
#     ./pkg/agent/hooks/winshim/shim/build.sh
#
# It works on Windows (mingw gcc) and on Linux/macOS with a mingw-w64
# cross-compiler installed:
#
#     apt install gcc-mingw-w64-x86-64      # Debian/Ubuntu
#     brew install mingw-w64                # macOS
#
# CI verifies the committed asset still matches the source, by reading back the
# source hash this compiles in — see .ci/scripts/check-windows-shim-asset.sh.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
out_dir="${here}/../assets"
out="${out_dir}/keploy_winshim.dll"

# MinHook provides the inline hooking (trampolines over ws2_32/kernel32 entry
# points). It is pinned by tag and fetched rather than vendored: it is only
# needed to REBUILD the shim, never to build or run keploy itself, so vendoring
# ~20 files of third-party C into the tree would be pure carrying cost.
minhook_repo="https://github.com/TsudaKageyu/minhook.git"
minhook_tag="v1.3.3"

# Pick a compiler: a native mingw gcc on Windows, a cross-compiler elsewhere.
cc=""
for candidate in x86_64-w64-mingw32-gcc gcc; do
  if command -v "$candidate" >/dev/null 2>&1; then
    if [[ "$candidate" == "gcc" ]] && [[ "$(uname -s)" != MINGW* && "$(uname -s)" != MSYS* && "$(uname -s)" != CYGWIN* ]]; then
      continue # a Linux/macOS `gcc` cannot produce a PE DLL
    fi
    cc="$candidate"
    break
  fi
done
if [[ -z "$cc" ]]; then
  echo "error: no mingw-w64 compiler found." >&2
  echo "       Install one (apt install gcc-mingw-w64-x86-64, or brew install mingw-w64)," >&2
  echo "       or run this script on Windows in an MSYS2/MinGW shell." >&2
  exit 1
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

echo "fetching MinHook ${minhook_tag}"
git -c advice.detachedHead=false clone --depth 1 --branch "${minhook_tag}" "${minhook_repo}" "${work}/minhook" >/dev/null 2>&1

# MinHook ships no build system we want here; its four translation units compile
# straight into a static archive.
"$cc" -O2 -c -I"${work}/minhook/include" \
  "${work}/minhook/src/buffer.c" \
  "${work}/minhook/src/hook.c" \
  "${work}/minhook/src/trampoline.c" \
  "${work}/minhook/src/hde/hde64.c"
ar_bin="${cc%gcc}ar"
command -v "$ar_bin" >/dev/null 2>&1 || ar_bin="ar"
"$ar_bin" rcs "${work}/libMinHook.a" buffer.o hook.o trampoline.o hde64.o
rm -f buffer.o hook.o trampoline.o hde64.o

mkdir -p "${out_dir}"

# Compile the source hash into the DLL so CI can detect a committed asset that
# no longer matches keploy_winshim.c. Hashing the DLL itself would not work: the
# linker stamps a fresh timestamp on every link, so two builds of identical
# source differ.
if command -v sha256sum >/dev/null 2>&1; then
  src_sha="$(sha256sum "${here}/keploy_winshim.c" | cut -d' ' -f1)"
else
  src_sha="$(shasum -a 256 "${here}/keploy_winshim.c" | cut -d' ' -f1)"
fi

# -static-libgcc so the DLL carries no dependency on a mingw runtime that is not
# present on a user's machine. The shim is loaded into arbitrary applications,
# so a missing DLL dependency would fail the injection with nothing to explain
# it.
"$cc" \
  -O2 \
  -Wall \
  -Wextra \
  -Werror \
  -shared \
  -static-libgcc \
  -DKEPLOY_WINSHIM_SOURCE_SHA="\"${src_sha}\"" \
  -I"${work}/minhook/include" \
  -o "${out}" \
  "${here}/keploy_winshim.c" \
  "${work}/libMinHook.a" \
  -lws2_32 -lmswsock

echo "built ${out} (keploy_winshim.c sha256 ${src_sha})"
ls -l "${out}"
