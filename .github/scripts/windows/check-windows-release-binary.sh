#!/bin/sh
# check-windows-release-binary.sh BINARY - refuse a windows release binary that
# needs a DLL Windows does not ship, carries gcc's runtime, is not stripped or
# has a stack reserve under 2 MiB.
#
# Why: v3.6.216 and the releases before it imported libwinpthread-1.dll, a
# MinGW DLL. Windows does not ship it, so on a machine without MinGW on its
# PATH keploy did not start (STATUS_DLL_NOT_FOUND, before main), and no lane
# noticed: the windows runners have MinGW on their PATH. They also carried
# gcc's runtime (libgcc, GPL-3.0 with the GCC runtime exception).
# build-windows-release.sh now links with llvm-mingw, -static; this holds the
# binary to what that gives:
#
#   * every DLL it imports (and delay-imports) is KERNEL32.dll or one of the
#     Universal CRT's api-ms-win-crt-*.dll sets, which are part of Windows 10
#     and later. Go loads every other DLL it calls itself at run time
#     (LoadLibraryEx, from System32), so they are not in the import table;
#   * no object in it was compiled by gcc (no "GCC: (" ident): gcc's runtime
#     is not linked;
#   * no symbol table (stripped, as -s asks);
#   * a stack reserve of at least 2 MiB (build-windows-release.sh's pin):
#     Windows sizes the main thread's stack, and every thread cgo starts, from
#     it, and the Postgres parser's C code recurses on that stack.
#
# Reads the binary with llvm-readobj: KEPLOY_LLVM_MINGW's, else the one on PATH.
set -eu

[ $# -eq 1 ] || { echo "usage: check-windows-release-binary.sh BINARY" >&2; exit 2; }
bin="$1"
[ -f "$bin" ] || { echo "check-windows-release-binary: $bin: no such file" >&2; exit 2; }
readobj=llvm-readobj
if [ -n "${KEPLOY_LLVM_MINGW:-}" ]; then readobj="$KEPLOY_LLVM_MINGW/bin/llvm-readobj"; fi

die() { echo "check-windows-release-binary: $bin: $*" >&2; exit 1; }

headers="$("$readobj" --file-headers "$bin")" || die "llvm-readobj cannot read it as a PE binary"
printf '%s\n' "$headers" | grep -q 'Machine: IMAGE_FILE_MACHINE_AMD64' || die "not a windows/amd64 PE binary"

imports="$("$readobj" --coff-imports "$bin")" || die "llvm-readobj cannot read its import table"
dlls="$(printf '%s\n' "$imports" | sed -n 's/^ *Name: *\(.*\.[dD][lL][lL]\) *$/\1/p' | LC_ALL=C sort -u)"
[ -n "$dlls" ] || die "imports no DLL at all: the import table was not read"
bad=""
for d in $dlls; do
  # DLL names are ASCII.
  # shellcheck disable=SC2018,SC2019
  case "$(printf '%s' "$d" | tr 'A-Z' 'a-z')" in
    kernel32.dll|api-ms-win-crt-*-l1-1-0.dll) ;;
    *) bad="$bad $d" ;;
  esac
done
[ -z "$bad" ] || die "imports DLLs Windows does not ship:$bad (linked against a MinGW or LLVM runtime DLL; build it with build-windows-release.sh)"

n="$(LC_ALL=C grep -a -c 'GCC: (' "$bin" || true)"
[ "$n" -eq 0 ] || die "carries objects compiled by gcc ($n 'GCC: (' idents): gcc's runtime (libgcc) is linked"

syms="$(printf '%s\n' "$headers" | sed -n 's/^ *SymbolCount: *//p' | head -n 1)"
[ "$syms" = 0 ] || die "is not stripped (SymbolCount: ${syms:-unread})"

stack="$(printf '%s\n' "$headers" | sed -n 's/^ *SizeOfStackReserve: *//p' | head -n 1)"
case "$stack" in ''|*[!0-9]*) die "its stack reserve cannot be read (SizeOfStackReserve: '$stack')" ;; esac
[ "$stack" -ge 2097152 ] || die "has a $stack-byte stack reserve, under the 2 MiB pin (-Wl,--stack,2097152)"

# shellcheck disable=SC2086 # a word a DLL
echo "check-windows-release-binary: $bin imports only$(printf ' %s' $dlls), no gcc runtime, stripped, stack reserve $stack"
