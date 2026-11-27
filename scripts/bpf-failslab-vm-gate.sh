#!/usr/bin/env bash
# Run bpfsafety CONFIG_FAILSLAB=y -ENOMEM unlink regression tests (including
# clean-exit snapshot race, two-prog same-func race, pre-6.0 unresolved target
# fail-closed, kernel-module tls.ko target, last-link anchor protection, and
# calm anchor stale detection) inside QEMU against Linux debug kernels
# (default: 5.10, 5.15, 5.19, 6.12).
#
# Usage:
#   KERNEL_DIR=/tmp/debug-kernels scripts/bpf-failslab-vm-gate.sh [5.10 5.15 5.19 6.12]
#
# Auto-detects /dev/kvm when writable (falls back to tcg,thread=single).
# If $KERNEL_DIR/tls-${K}.ko exists, it is bundled as /tls.ko inside that
# kernel's initramfs so TestOnKernelModuleTargetDeadOwnerENOMEM runs on real
# module BTF. Any test failure, KASAN report, Oops, or kernel panic fails the gate.
set -euo pipefail

KERNEL_DIR="${KERNEL_DIR:-/tmp/debug-kernels}"
if [ -z "${QEMU_ACCEL:-}" ]; then
  if [ -r /dev/kvm ] && [ -w /dev/kvm ]; then
    QEMU_ACCEL="kvm"
  else
    QEMU_ACCEL="tcg,thread=single"
  fi
fi
QEMU_MEM="${QEMU_MEM:-1536}"
VM_TIMEOUT="${VM_TIMEOUT:-300}"
TEST_REGEX="${TEST_REGEX:-^TestOnKernel(ReaperSurvivesENOMEMUnlinkViaForceRebuild|AnchorPreventsLastLinkENOMEM|AnchorCleanupNeverDetachesLastLinkUnderFailslab|AnchorPinFailNeverClosesLastLink|AnchorPinRetryFailKeepsLink|AnchorEMFILEKeepsLink|ShutdownRetainsUnpinnedAnchor|StaticTrampolineUnpinnedAnchorRefusesFexitAndExitCloseSurvives|PersistentAnchorPinFailure|RootStatfsFaultNeverFailsOpen|InitUnprotectedStaticTrampolineRefusesFexit|IdleTmpNoopENOMEMNeverPanics|CleanSnapshotRace|CleanSnapshotTwoProgsSameFunc|UnresolvedNameFailsClosed|MixedDirUnresolvedProgDead|MixedDirUnresolvedProgClean|LegacyMixedDirUnresolvedProgDead|ModuleTargetDeadOwnerENOMEM|CleanupAnchorsIdleWithDeadDirAndOtherLink|SlotReuseConvergence)$}"
HOOKS_TEST_REGEX="${HOOKS_TEST_REGEX:-^TestOnKernelUnixconnRealObjectFexitPinsResolve$}"

if [ "$#" -gt 0 ]; then
  KERNELS=("$@")
else
  KERNELS=(5.10 5.15 5.19 6.12)
fi

WORK="$(mktemp -d /tmp/bpf-failslab-gate.XXXXXX)"
trap 'rm -rf "$WORK"' EXIT

echo "=== Building static bpfsafety & hooks/linux integration test binaries (accel=${QEMU_ACCEL}) ==="
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -tags=integration -c -o "$WORK/bpfsafety.test" ./pkg/agent/bpfsafety
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go test -tags=integration -c -o "$WORK/hooks_linux.test" ./pkg/agent/hooks/linux

echo "=== Building minimal QEMU init binary ==="
cat >"$WORK/init.go" <<'EOF'
package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

func mustMount(src, dst, fstype string) {
	_ = os.MkdirAll(dst, 0o755)
	if err := syscall.Mount(src, dst, fstype, 0, ""); err != nil {
		fmt.Printf("INIT_MOUNT_ERR %s (%s): %v\n", dst, fstype, err)
	}
}

func main() {
	mustMount("proc", "/proc", "proc")
	mustMount("sysfs", "/sys", "sysfs")
	mustMount("devtmpfs", "/dev", "devtmpfs")
	mustMount("tmpfs", "/tmp", "tmpfs")
	mustMount("debugfs", "/sys/kernel/debug", "debugfs")
	mustMount("bpf", "/sys/fs/bpf", "bpf")

	_ = os.WriteFile("/proc/sys/net/core/bpf_jit_enable", []byte("1\n"), 0o644)
	_ = os.WriteFile("/proc/sys/net/core/bpf_jit_kallsyms", []byte("1\n"), 0o644)
	_ = os.WriteFile("/proc/sys/net/core/bpf_jit_harden", []byte("0\n"), 0o644)
	_ = os.WriteFile("/proc/sys/kernel/kptr_restrict", []byte("0\n"), 0o644)

	if u, err := os.ReadFile("/proc/version"); err == nil {
		fmt.Printf("VM_KERNEL: %s", string(u))
	}

	regex := os.Getenv("TEST_REGEX")
	if regex == "" {
		regex = "^TestOnKernel(ReaperSurvivesENOMEMUnlinkViaForceRebuild|AnchorPreventsLastLinkENOMEM|AnchorCleanupNeverDetachesLastLinkUnderFailslab|AnchorPinFailNeverClosesLastLink|AnchorPinRetryFailKeepsLink|AnchorEMFILEKeepsLink|ShutdownRetainsUnpinnedAnchor|StaticTrampolineUnpinnedAnchorRefusesFexitAndExitCloseSurvives|PersistentAnchorPinFailure|RootStatfsFaultNeverFailsOpen|InitUnprotectedStaticTrampolineRefusesFexit|IdleTmpNoopENOMEMNeverPanics|CleanSnapshotRace|CleanSnapshotTwoProgsSameFunc|UnresolvedNameFailsClosed|MixedDirUnresolvedProgDead|MixedDirUnresolvedProgClean|LegacyMixedDirUnresolvedProgDead|ModuleTargetDeadOwnerENOMEM|CleanupAnchorsIdleWithDeadDirAndOtherLink|SlotReuseConvergence)$"
	}
	cmd := exec.Command("/bpfsafety.test", "-test.v", "-test.count=1", "-test.timeout=240s", "-test.run="+regex)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err == nil {
		hregex := os.Getenv("HOOKS_TEST_REGEX")
		if hregex == "" {
			hregex = "^TestOnKernelUnixconnRealObjectFexitPinsResolve$"
		}
		hcmd := exec.Command("/hooks_linux.test", "-test.v", "-test.count=1", "-test.timeout=60s", "-test.run="+hregex)
		hcmd.Stdout = os.Stdout
		hcmd.Stderr = os.Stderr
		err = hcmd.Run()
	}
	if err != nil {
		fmt.Printf("VM_TESTS_RESULT=FAIL err=%v\n", err)
	} else {
		fmt.Printf("VM_TESTS_RESULT=PASS\n")
	}
	_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
}
EOF
(cd "$WORK" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$WORK/init" "$WORK/init.go")

build_initrd() {
  local k="$1"
  local rdir="$WORK/rootfs-${k}"
  mkdir -p "$rdir/proc" "$rdir/sys" "$rdir/dev" "$rdir/tmp"
  cp "$WORK/init" "$rdir/init"
  cp "$WORK/bpfsafety.test" "$rdir/bpfsafety.test"
  cp "$WORK/hooks_linux.test" "$rdir/hooks_linux.test"
  if [ -f "$KERNEL_DIR/tls-${k}.ko" ]; then
    cp "$KERNEL_DIR/tls-${k}.ko" "$rdir/tls.ko"
  fi
  chmod +x "$rdir/init" "$rdir/bpfsafety.test" "$rdir/hooks_linux.test"
  (cd "$rdir" && find . -print0 | cpio --null -ov --format=newc 2>/dev/null | gzip -1 >"$WORK/initramfs-${k}.cpio.gz")
}

FAILURES=0
for K in "${KERNELS[@]}"; do
  VMLINUZ="$KERNEL_DIR/vmlinuz-${K}-debug"
  if [ ! -f "$VMLINUZ" ]; then
    echo "ERROR: missing debug kernel $VMLINUZ"
    FAILURES=$((FAILURES + 1))
    continue
  fi
  build_initrd "$K"
  LOG="$WORK/vm-${K}.log"
  echo "=== Booting debug kernel ${K} ($VMLINUZ) ==="
  set +e
  timeout -k 10 "$VM_TIMEOUT" qemu-system-x86_64 \
    -machine q35 -accel "$QEMU_ACCEL" -cpu max -smp 2 -m "$QEMU_MEM" \
    -kernel "$VMLINUZ" -initrd "$WORK/initramfs-${K}.cpio.gz" \
    -nographic -no-reboot \
    -append "console=ttyS0 panic=-1 oops=panic nokaslr quiet TEST_REGEX=${TEST_REGEX}" \
    >"$LOG" 2>&1
  RC=$?
  set -e

  echo "--- Console summary for kernel ${K} (qemu rc=${RC}) ---"
  CLEAN_LOG="$WORK/vm-${K}.clean.log"
  tr -d '\r' < "$LOG" > "$CLEAN_LOG"
  grep -E 'VM_KERNEL:|=== RUN|--- PASS:|--- FAIL:|--- SKIP:|FAIL|PASS|VM_TESTS_RESULT=|BUG:|KASAN:|Kernel panic|Oops:' "$CLEAN_LOG" || true

  BAD=0
  if ! grep -q '^VM_TESTS_RESULT=PASS$' "$CLEAN_LOG"; then
    echo "FAIL [${K}]: VM_TESTS_RESULT=PASS not observed (qemu rc=${RC})"
    BAD=1
  fi
  if grep -Eq 'BUG: KASAN:|BUG: unable to handle|Kernel panic|Oops:|general protection fault' "$CLEAN_LOG"; then
    echo "FAIL [${K}]: kernel fault/KASAN/panic observed in console log!"
    BAD=1
  fi
  # Enforce that every required test for this kernel actually PASSED and NONE skipped.
  REQ_TESTS=(
    TestOnKernelReaperSurvivesENOMEMUnlinkViaForceRebuild
    TestOnKernelAnchorPreventsLastLinkENOMEM
    TestOnKernelAnchorCleanupNeverDetachesLastLinkUnderFailslab
    TestOnKernelAnchorPinFailNeverClosesLastLink
    TestOnKernelAnchorPinRetryFailKeepsLink
    TestOnKernelAnchorEMFILEKeepsLink
    TestOnKernelShutdownRetainsUnpinnedAnchor
    TestOnKernelStaticTrampolineUnpinnedAnchorRefusesFexitAndExitCloseSurvives
    TestOnKernelPersistentAnchorPinFailure
    TestOnKernelRootStatfsFaultNeverFailsOpen
    TestOnKernelInitUnprotectedStaticTrampolineRefusesFexit
    TestOnKernelIdleTmpNoopENOMEMNeverPanics
    TestOnKernelCleanSnapshotRace
    TestOnKernelCleanSnapshotTwoProgsSameFunc
    TestOnKernelCleanupAnchorsIdleWithDeadDirAndOtherLink
    TestOnKernelSlotReuseConvergence
    TestOnKernelUnixconnRealObjectFexitPinsResolve
  )
  case "$K" in
    5.10|5.15|5.19)
      REQ_TESTS+=(
        TestOnKernelUnresolvedNameFailsClosed
        TestOnKernelMixedDirUnresolvedProgDead
        TestOnKernelMixedDirUnresolvedProgClean
        TestOnKernelLegacyMixedDirUnresolvedProgDead
      )
      ;;
  esac
  if [ -f "$KERNEL_DIR/tls-${K}.ko" ]; then
    REQ_TESTS+=(TestOnKernelModuleTargetDeadOwnerENOMEM)
  fi
  for T in "${REQ_TESTS[@]}"; do
    if grep -q "^--- SKIP: ${T} " "$CLEAN_LOG"; then
      echo "FAIL [${K}]: required test ${T} was SKIPPED!"
      BAD=1
    elif ! grep -q "^--- PASS: ${T} " "$CLEAN_LOG"; then
      echo "FAIL [${K}]: required test ${T} did not PASS!"
      BAD=1
    fi
  done
  if [ "$BAD" -ne 0 ]; then
    echo "--- Tail of ${K} log ---"
    tail -n 80 "$CLEAN_LOG"
    FAILURES=$((FAILURES + 1))
  else
    echo "PASS [${K}]: all failslab tests green with 0 KASAN/Oops/panic"
  fi
done

if [ "$FAILURES" -ne 0 ]; then
  echo "FAILSLAB 4-KERNEL GATE FAILED (${FAILURES} kernel(s) failed)"
  exit 1
fi
echo "FAILSLAB GATE PASSED FOR ALL KERNELS: ${KERNELS[*]}"
