#!/usr/bin/env bash
#
# Guards the committed Windows interception shim against drifting from its
# source.
#
# pkg/agent/hooks/winshim/assets/keploy_winshim.dll is a prebuilt binary — the
# Windows release binaries are cross-compiled and the shim links MinHook, which
# is C, so the asset is committed and embedded with go:embed. That leaves an
# obvious failure mode: someone edits keploy_winshim.c, does not re-run
# build.sh, and every unprivileged Windows run silently uses the old DLL.
#
# Hashing the DLL itself does not work — the linker stamps a fresh timestamp on
# every link, so two builds of identical source differ. build.sh instead
# compiles the sha256 of keploy_winshim.c INTO the DLL, and this script checks
# that recorded hash against the source. That makes the check runnable anywhere,
# including the Linux CI runners, which cannot build a PE DLL at all.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
src="${here}/pkg/agent/hooks/winshim/shim/keploy_winshim.c"
asset="${here}/pkg/agent/hooks/winshim/assets/keploy_winshim.dll"

for f in "$src" "$asset"; do
  if [[ ! -f "$f" ]]; then
    echo "error: missing $f" >&2
    exit 1
  fi
done

if command -v sha256sum >/dev/null 2>&1; then
  actual="$(sha256sum "$src" | cut -d' ' -f1)"
else
  actual="$(shasum -a 256 "$src" | cut -d' ' -f1)"
fi

# build.sh embeds this as: keploy_winshim_source_sha256=<64 hex chars>
recorded="$(LC_ALL=C strings -a "$asset" | grep -o 'keploy_winshim_source_sha256=[0-9a-f]\{64\}' | head -1 | cut -d= -f2 || true)"

if [[ -z "$recorded" ]]; then
  echo "error: $asset carries no source hash." >&2
  echo "       Rebuild it with pkg/agent/hooks/winshim/shim/build.sh." >&2
  exit 1
fi

if [[ "$recorded" != "$actual" ]]; then
  echo "error: the committed Windows shim was built from different source than keploy_winshim.c." >&2
  echo "  keploy_winshim.c sha256: $actual" >&2
  echo "       DLL records sha256: $recorded" >&2
  echo >&2
  echo "Rebuild it and commit the result:" >&2
  echo "  ./pkg/agent/hooks/winshim/shim/build.sh" >&2
  exit 1
fi

echo "Windows shim asset matches keploy_winshim.c (sha256 ${actual:0:12}…)"

# The asset must also still be a 64-bit PE DLL; a 32-bit build could not be
# injected into the 64-bit applications keploy supports.
if command -v file >/dev/null 2>&1; then
  if ! file "$asset" | grep -qi "PE32+"; then
    echo "error: $asset is not a 64-bit PE DLL; rebuild with build.sh" >&2
    exit 1
  fi
  echo "Windows shim asset is a 64-bit PE DLL"
fi
