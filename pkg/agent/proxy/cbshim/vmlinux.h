// SPDX-License-Identifier: GPL-2.0
//
// vmlinux.h — arch dispatcher. bpf2go passes -D__TARGET_ARCH_x86 or
// -D__TARGET_ARCH_arm64 for each target in the -target list; we include
// the matching per-arch BTF dump. Keeps cbshim.bpf.c arch-agnostic.

#if defined(__TARGET_ARCH_x86)
#include "vmlinux_amd64.h"
#elif defined(__TARGET_ARCH_arm64)
#include "vmlinux_arm64.h"
#else
#error "cbshim: unsupported target architecture (set __TARGET_ARCH_x86 or __TARGET_ARCH_arm64)"
#endif
