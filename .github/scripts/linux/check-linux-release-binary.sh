#!/bin/sh
# check-linux-release-binary.sh BINARY ARCH — refuse a linux release binary
# that would not run on every Linux we support.
#
# build-linux-release.sh runs it on every linux binary it builds, so a binary
# that fails it is never published. ARCH is amd64 or arm64.
#
# Why: v3.6.164 and the releases before it were linked by the runner's gcc
# against its glibc and needed GLIBC_2.34; on Debian 11, Ubuntu 20.04, RHEL 8
# and Amazon Linux 2 they exited before main ("version `GLIBC_2.34' not
# found"). CI never noticed: it runs keploy on new distributions. The linux
# builds are now linked static against musl (zig cc, see
# build-linux-release.sh), and this holds them to it:
#
#   * ELF for ARCH, an executable;
#   * no program interpreter (PT_INTERP) and no dynamic section: nothing of
#     the host's libc, or any other library, is loaded at run time;
#   * a PT_GNU_STACK of at least 8 MiB: musl sizes every thread's stack from
#     it (128 KiB without one, at most 8 MiB), and cgo's C code — the
#     Postgres parser — runs on the calling thread's stack. zig's musl target
#     writes 16 MiB by default and build-linux-release.sh pins 8 MiB: both
#     give a thread 8 MiB. Measured, a 128 KiB header crashes the parser at
#     300 levels of nesting and 512 KiB at 3000 (see build-linux-release.sh);
#   * stripped: no .symtab and no .debug_* sections. zig cc does not act on
#     the -s Go's linker passes it, so build-linux-release.sh adds
#     -Wl,--strip-all; without it the binary carries its symbol table and the
#     C code's DWARF, which the gcc-linked releases (stripped) never shipped;
#   * when the host runs ARCH, the binary runs: `--version` exits 0 and
#     prints a Keploy version line.
#
# The checks read readelf's output (binutils: the GitHub runners carry it); a
# host without readelf fails the check rather than passing it unread.
set -eu

[ $# -eq 2 ] || { echo "usage: check-linux-release-binary.sh BINARY amd64|arm64" >&2; exit 2; }
bin="$1"; arch="$2"
case "$arch" in
  amd64) machine='Advanced Micro Devices X86-64'; host_arch=x86_64 ;;
  arm64) machine='AArch64'; host_arch=aarch64 ;;
  *) echo "check-linux-release-binary: unknown arch '$arch' (amd64 or arm64)" >&2; exit 2 ;;
esac
[ -f "$bin" ] || { echo "check-linux-release-binary: no file $bin" >&2; exit 2; }
command -v readelf >/dev/null 2>&1 || { echo "check-linux-release-binary: readelf not found; cannot check $bin" >&2; exit 2; }

fail=0
bad() { echo "check-linux-release-binary: $bin: $*" >&2; fail=1; }

hdr="$(readelf -hW "$bin")" || { echo "check-linux-release-binary: $bin is not an ELF file" >&2; exit 1; }
printf '%s\n' "$hdr" | grep -q "Machine: *$machine\$" \
  || bad "is not a linux/$arch binary ($(printf '%s\n' "$hdr" | sed -n 's/^ *Machine: *//p'))"
printf '%s\n' "$hdr" | grep -q 'Type: *EXEC ' \
  || bad "is not a static executable ($(printf '%s\n' "$hdr" | sed -n 's/^ *Type: *//p'))"

phdrs="$(readelf -lW "$bin")"
if printf '%s\n' "$phdrs" | grep -q '^ *INTERP '; then
  bad "asks for a program interpreter ($(printf '%s\n' "$phdrs" | sed -n 's/.*Requesting program interpreter: \(.*\)]/\1/p')): it is dynamically linked"
fi
if printf '%s\n' "$phdrs" | grep -q '^ *DYNAMIC '; then
  bad "has a dynamic section, needing: $(readelf -dW "$bin" | sed -n 's/.*(NEEDED).*\[\(.*\)\]/\1/p' | tr '\n' ' ')"
fi
# readelf -SW prints a section as "[Nr] Name Type ...", and pads Nr ("[ 1]"),
# so the name is the first field after the "]".
unstripped="$(readelf -SW "$bin" | awk '/^ *\[ *[0-9]+\]/ { sub(/^ *\[ *[0-9]+\] */, ""); if ($1 ~ /^(\.symtab|\.debug_)/) print $1 }' | tr '\n' ' ')"
[ -z "$unstripped" ] || bad "is not stripped (it has $unstripped); the linux builds link with -Wl,--strip-all (see build-linux-release.sh)"
# GNU_STACK's MemSiz is the 6th field: Type Offset VirtAddr PhysAddr FileSiz MemSiz Flg Align.
stack="$(printf '%s\n' "$phdrs" | awk '$1 == "GNU_STACK" { print $6 }')"
if [ -z "$stack" ]; then
  bad "has no PT_GNU_STACK header (musl would give its threads 128 KiB stacks)"
elif [ "$((stack))" -lt 8388608 ]; then
  bad "asks for $((stack)) bytes of thread stack in PT_GNU_STACK; at least 8388608 (8 MiB) wanted"
fi

if [ "$fail" -eq 0 ] && [ "$(uname -m)" = "$host_arch" ] && [ "$(uname -s)" = Linux ]; then
  out="$("$bin" --version 2>&1)" || { bad "--version failed: $out"; }
  case "$out" in
    *"Keploy "*) ;;
    *) bad "--version printed no Keploy version: $out" ;;
  esac
fi

if [ "$fail" -ne 0 ]; then
  echo "check-linux-release-binary: REFUSED $bin: a linux release binary must be static and stripped (musl, zig cc: see build-linux-release.sh)" >&2
  exit 1
fi
echo "check-linux-release-binary: $bin: static, stripped linux/$arch executable, thread stack $((stack)) bytes$( [ "$(uname -m)" = "$host_arch" ] && echo ', --version runs')"
