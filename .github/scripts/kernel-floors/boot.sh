#!/usr/bin/env bash
# Runs the agent's root hooks tests on keploy's kernel floors, each in a QEMU
# VM of its own: amd64 5.10 and arm64 6.6 (kernels.txt). The hooks have to work
# the same from those kernels on, and a PR's runners have neither, so this
# boots them (.github/workflows/kernel-floors.yml).
#
#   boot.sh build ARCH OUT     the VM's init and the test binaries tests.txt
#                              names, built for ARCH (go; any build host)
#   boot.sh userland ARCH OUT  this machine's sh, iptables and kmod with the
#                              libraries they load, for the VM; run on ARCH
#   boot.sh boot ARCH OUT      ARCH's floor kernel (fetched once, by its
#                              sha256), the initramfs, the VM; its console goes
#                              to OUT/console-ARCH.log as it runs
#   boot.sh check ARCH OUT     the verdict, from that console
#   boot.sh selftest ARCH OUT  check fails on copies of that console that
#                              lack what it requires or hold what it refuses
#
# ARCH is amd64 or arm64. OUT is a directory of this script's: build fills
# OUT/root-ARCH, userland OUT/userland-ARCH, and boot unpacks the kernel beside
# them. The kernel's package is cached in KF_CACHE (default OUT/cache).
#
# The VM's /init is init/main.go: it mounts proc, sysfs, devtmpfs, tmpfs,
# cgroup2, bpffs, tracefs and debugfs, brings lo up, prints "KF kernel RELEASE
# on MACHINE" from inside the VM, runs the /kf/run.sh this writes (each test
# binary as root, -test.v, no test timeout), prints the kernel's log and powers
# off. The kernel's own modules are in the initramfs, so iptables and the
# kernel load netfilter's as on a host (kmod's modprobe at /sbin/modprobe;
# depmod runs first).
#
# The VM runs under KVM where /dev/kvm exists and the VM's architecture is this
# machine's; otherwise QEMU emulates it (TCG). It says which, and why; neither
# skips anything. Nothing here has a clock: the VM runs until init powers it
# off (or the kernel panics, which ends QEMU: -no-reboot, panic=-1), each test
# until it ends.
#
# check passes only when the console has: the guest's kernel line naming
# kernels.txt's release; a --- PASS line for every test and subtest of
# tests.txt; every binary's exit 0 and the script's; no --- SKIP or --- FAIL;
# the AF_UNIX report attached the way kernels.txt says; no kernel WARN(),
# BUG, oops, panic or stall in the kernel's log; and every test of the
# integration tags' files listed in tests.txt.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo=$(cd "$here/../../.." && pwd)
# The tests every floor runs (selftest points check at copies of it).
tests=${KF_TESTS:-$here/tests.txt}

die() {
  echo "::error::$*" >&2
  exit 1
}

# machine ARCH: uname -m of ARCH.
machine() {
  case $1 in
    amd64) echo x86_64 ;;
    arm64) echo aarch64 ;;
    *) die "unknown arch $1: want amd64 or arm64" ;;
  esac
}

# kernel ARCH: kernels.txt's line for ARCH (ARCH RELEASE UNIXCONN SHA256 URL...).
kernel() {
  local line
  line=$(awk -v a="$1" '!/^#/ && $1 == a' "$here/kernels.txt")
  [ -n "$line" ] || die "kernels.txt has no $1 kernel"
  [ "$(printf '%s\n' "$line" | wc -l)" = 1 ] || die "kernels.txt has more than one $1 kernel"
  echo "$line"
}

# binaries: tests.txt's binaries, in the order it first names them.
binaries() {
  awk '!/^#/ && NF == 2 && !seen[$1]++ { print $1 }' "$tests"
}

# tags BINARY: the build tags BINARY is built with (tests.txt's header).
tags() {
  case $1 in
    hooks) echo "" ;;
    pidmap) echo "viper_bind_struct pidmap_integration" ;;
    proxyconns) echo "viper_bind_struct proxyconns_integration" ;;
    *) die "tests.txt names a binary boot.sh does not build: $1" ;;
  esac
}

# toplevel BINARY: the tests of BINARY, without subtests, one per line.
toplevel() {
  awk -v b="$1" '!/^#/ && $1 == b { sub(/\/.*/, "", $2); if (!seen[$2]++) print $2 }' "$tests"
}

build() {
  local a=$1 out=$2 root b t
  machine "$a" > /dev/null
  root="$out/root-$a"
  rm -rf "$root"
  mkdir -p "$root/kf/bin"
  # Static (CGO_ENABLED=0): the binaries need no libc in the VM, and an arm64
  # build needs no cross compiler. The hooks' cgo is the standard library's
  # (net, os/user) alone.
  (cd "$here/init" && CGO_ENABLED=0 GOOS=linux GOARCH="$a" go build -trimpath -o "$root/init" .)
  for b in $(binaries); do
    t=$(tags "$b")
    echo "building $b.test for $a${t:+ (-tags '$t')}"
    (cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH="$a" go test -c ${t:+-tags "$t"} -o "$root/kf/bin/$b.test" ./pkg/agent/hooks/linux/)
  done
}

# copy_elf ROOT FILE: FILE, and the ELF interpreter and every library it loads
# (ldd), into ROOT at the paths this machine has them.
copy_elf() {
  local root=$1 f=$2 lib deps
  [ -e "$root$f" ] || { mkdir -p "$root$(dirname "$f")" && cp -L "$f" "$root$f"; }
  deps=$(ldd "$f" 2>&1 || true)
  if grep -q 'not found' <<< "$deps"; then
    echo "$deps" >&2
    die "$f needs a library this machine does not have"
  fi
  for lib in $(awk '$2 == "=>" && $3 ~ /^\// { print $3 } $1 ~ /^\// { print $1 }' <<< "$deps"); do
    [ -e "$root$lib" ] || { mkdir -p "$root$(dirname "$lib")" && cp -L "$lib" "$root$lib"; }
  done
}

userland() {
  local a=$1 out=$2 root p real x
  [ "$(uname -m)" = "$(machine "$a")" ] || die "userland copies this machine's programs into the VM: run it on $a, not $(uname -m)"
  root="$out/userland-$a"
  rm -rf "$root"
  mkdir -p "$root/bin" "$root/sbin" "$root/usr/bin" "$root/usr/sbin" "$root/etc" "$root/root"
  # /bin/sh: the tests run it (readywatch's restart is of /bin/sh), and so
  # does init, for run.sh.
  real=$(readlink -f /bin/sh)
  copy_elf "$root" "$real"
  [ "$real" = /bin/sh ] || cp -L "$real" "$root/bin/sh"
  # iptables, as this machine's default backend has it (the NAT tests add a
  # DNAT rule), and the extensions it loads by name.
  p=$(command -v iptables) || die "no iptables here"
  real=$(readlink -f "$p")
  copy_elf "$root" "$real"
  ln -sf "$real" "$root/usr/sbin/iptables"
  x=$(find /usr/lib /usr/lib64 -name libxt_standard.so -path '*xtables*' 2> /dev/null | head -1 || true)
  [ -n "$x" ] || die "no iptables extensions (libxt_standard.so) here"
  for p in "$(dirname "$x")"/*.so; do copy_elf "$root" "$p"; done
  # kmod: depmod for the kernel's modules, and modprobe, which the kernel runs
  # (as /sbin/modprobe) to load a module it is asked for, as iptables does.
  p=$(command -v modprobe) || die "no modprobe here"
  real=$(readlink -f "$p")
  copy_elf "$root" "$real"
  for x in modprobe depmod insmod lsmod; do ln -sf "$real" "$root/sbin/$x"; done
  for x in protocols services; do [ ! -e "/etc/$x" ] || cp -L "/etc/$x" "$root/etc/$x"; done
  printf 'root:x:0:0:root:/root:/bin/sh\n' > "$root/etc/passwd"
  printf 'root:x:0:\n' > "$root/etc/group"
  printf '127.0.0.1 localhost\n::1 localhost\n' > "$root/etc/hosts"
  printf 'hosts: files\n' > "$root/etc/nsswitch.conf"
  echo "the $a VM's userland: $(readlink -f /bin/sh), $(readlink -f "$(command -v iptables)") ($(iptables --version)), $(readlink -f "$(command -v modprobe)") ($(modprobe --version | head -1))"
}

# fetch SHA256 CACHE URL...: the package of that checksum, from CACHE, or else
# fetched into it from the first URL that serves it whole (each tried with
# curl's retries, resuming).
fetch() {
  local sum=$1 cache=$2 f u
  shift 2
  mkdir -p "$cache"
  f="$cache/$sum.deb"
  if [ -s "$f" ] && [ "$(sha256sum "$f" | cut -d' ' -f1)" = "$sum" ]; then
    echo "the kernel package, sha256 $sum, from the cache ($f)" >&2
    echo "$f"
    return
  fi
  rm -f "$f"
  for u in "$@"; do
    echo "fetching $u" >&2
    if curl -fL --no-progress-meter --retry 8 --retry-all-errors --retry-delay 5 -C - -o "$f.part" "$u" >&2 &&
      [ "$(sha256sum "$f.part" | cut -d' ' -f1)" = "$sum" ]; then
      mv "$f.part" "$f"
      echo "$f"
      return
    fi
    echo "$u did not serve the package of sha256 $sum" >&2
    rm -f "$f.part"
  done
  die "no URL served the package of sha256 $sum"
}

boot() {
  local a=$1 out=$2 root user line k rel sum urls pkg img mods run b names q con cpu accel how smp log rc
  root="$out/root-$a"
  user="$out/userland-$a"
  [ -x "$root/init" ] || die "no $root/init: run 'boot.sh build $a $out' first"
  [ -x "$user/bin/sh" ] || die "no $user/bin/sh: run 'boot.sh userland $a $out' on $a first"
  line=$(kernel "$a")
  read -r _ rel _ sum urls <<< "$line"
  # shellcheck disable=SC2086 # the URLs are words
  pkg=$(fetch "$sum" "${KF_CACHE:-$out/cache}" $urls)
  k="$out/kernel-$a"
  rm -rf "$k"
  mkdir -p "$k"
  (cd "$k" && ar x "$pkg" && tar -xf data.tar.* && rm -f data.tar.* control.tar.* debian-binary)
  img="$k/boot/vmlinuz-$rel"
  [ -s "$img" ] || die "the $a package has no boot/vmlinuz-$rel"
  # The modules, at /lib/modules/RELEASE in the VM, where kmod looks,
  # wherever the package has them.
  mods="$out/modules-$a"
  rm -rf "$mods"
  mkdir -p "$mods/lib/modules"
  if [ -d "$k/lib/modules/$rel" ]; then
    mv "$k/lib/modules/$rel" "$mods/lib/modules/"
  elif [ -d "$k/usr/lib/modules/$rel" ]; then
    mv "$k/usr/lib/modules/$rel" "$mods/lib/modules/"
  else
    die "the $a package has no modules for $rel"
  fi

  # run.sh: each binary of tests.txt, its tests by name (a -run that matches
  # nothing exits 0: check requires each PASS line), to its end.
  run="$out/run-$a"
  rm -rf "$run"
  mkdir -p "$run/kf"
  {
    echo '#!/bin/sh'
    echo '# written by .github/scripts/kernel-floors/boot.sh'
    echo 'rc=0'
    echo 'depmod -a || { echo "KF depmod failed"; rc=1; }'
    for b in $(binaries); do
      names=$(toplevel "$b" | paste -sd'|' -)
      echo "echo 'KF run $b'"
      echo "/kf/bin/$b.test -test.v -test.count=1 -test.timeout=0 -test.run '^($names)\$'"
      echo "s=\$?; echo \"KF exit $b \$s\"; [ \$s = 0 ] || rc=1"
    done
    echo 'exit $rc'
  } > "$run/kf/run.sh"

  # The initramfs: the root, the userland, the kernel's modules, run.sh. The
  # kernel unpacks concatenated archives in order. Uncompressed: an emulated
  # VM would spend minutes inflating it.
  {
    (cd "$root" && find . | cpio -o -H newc -R 0:0 --quiet)
    (cd "$user" && find . | cpio -o -H newc -R 0:0 --quiet)
    (cd "$mods" && find . | cpio -o -H newc -R 0:0 --quiet)
    (cd "$run" && find . | cpio -o -H newc -R 0:0 --quiet)
  } > "$out/initrd-$a.cpio"
  echo "initramfs: $(du -h "$out/initrd-$a.cpio" | cut -f1)"

  case $a in
    amd64) q="qemu-system-x86_64 -M q35"; con=ttyS0 ;;
    arm64) q="qemu-system-aarch64 -M virt,gic-version=max"; con=ttyAMA0 ;;
  esac
  if [ -e /dev/kvm ]; then echo "/dev/kvm: exists"; else echo "/dev/kvm: does not exist"; fi
  if [ -e /dev/kvm ] && [ "$(uname -m)" = "$(machine "$a")" ]; then
    [ -r /dev/kvm ] && [ -w /dev/kvm ] || die "/dev/kvm exists but $(id -un) cannot open it: give this user access (the kvm group, or a udev rule)"
    accel=kvm; cpu=host; how="KVM: /dev/kvm exists and the VM is $(uname -m), as this machine is"
  else
    accel=tcg; cpu=max
    # pointer authentication emulated as QEMU's own cipher, not the
    # architected QARMA: the same instructions, much faster to emulate.
    [ "$a" = arm64 ] && cpu=max,pauth-impdef=on
    if [ -e /dev/kvm ]; then
      how="TCG (emulated): the VM is $(machine "$a") and this machine $(uname -m), which KVM cannot run"
    else
      how="TCG (emulated): this machine has no /dev/kvm"
    fi
  fi
  smp=$(nproc)
  [ "$smp" -le 4 ] || smp=4
  echo "accel: $accel, $how ($(nproc) CPUs here, load $(cut -d' ' -f1-3 /proc/loadavg))"
  log="$out/console-$a.log"
  echo "booting $rel ($a) in $q -accel $accel -cpu $cpu -smp $smp; console: $log"
  rc=0
  # shellcheck disable=SC2086 # $q is a command and its words
  $q -accel "$accel" -cpu "$cpu" -smp "$smp" -m 4096 -display none -monitor none -serial stdio -no-reboot -nic none \
    -kernel "$img" -initrd "$out/initrd-$a.cpio" \
    -append "console=$con loglevel=1 panic=-1 rdinit=/init" < /dev/null | stdbuf -oL tr -d '\r' | tee "$log" || rc=$?
  echo "QEMU exited $rc"
  return "$rc"
}

check() {
  local a=$1 out=$2 log line rel way m fail=0 b t f n listed
  log="$out/console-$a.log"
  [ -s "$log" ] || die "no console $log: run 'boot.sh boot $a $out' first"
  line=$(kernel "$a")
  read -r _ rel way _ <<< "$line"
  m=$(machine "$a")
  bad() {
    echo "::error::$*" >&2
    fail=1
  }
  grep -qxF "KF kernel $rel on $m" "$log" || bad "the VM did not say it ran $rel on $m: $(grep '^KF kernel ' "$log" || echo 'no kernel line')"
  for b in $(binaries); do
    grep -qxF "KF exit $b 0" "$log" || bad "$b.test did not exit 0: $(grep "^KF exit $b " "$log" || echo 'it never ended')"
  done
  grep -qxF "KF run.sh exited 0" "$log" || bad "run.sh did not exit 0: $(grep '^KF run.sh \|^KF setup failed' "$log" || echo 'it never ended (a panic, or QEMU stopped)')"
  while read -r b t; do
    grep -qF -- "--- PASS: $t (" "$log" || bad "$t ($b.test) did not pass on $rel"
  done < <(awk '!/^#/ && NF == 2' "$tests")
  if grep -F -- "--- SKIP:" "$log" >&2; then bad "a test skipped on $rel (above): the floor runs every one"; fi
  if grep -F -- "--- FAIL:" "$log" >&2; then bad "a test failed on $rel (above)"; fi
  case $way in
    fexit) grep -qF "unixconn: attached by fexit on $m $rel" "$log" ||
      bad "unixconn did not attach by fexit on $rel: $(grep -o 'unixconn: attached .*' "$log" | head -1 || echo 'it never said how it attached')" ;;
    tracepoints) grep -qF "unixconn: attached at the syscall tracepoints on $m $rel" "$log" ||
      bad "unixconn did not attach at the syscall tracepoints on $rel: $(grep -o 'unixconn: attached .*' "$log" | head -1 || echo 'it never said how it attached')" ;;
    *) bad "kernels.txt says unixconn attaches by '$way' on $rel: want fexit or tracepoints" ;;
  esac
  grep -qxF "KF dmesg ends" "$log" || bad "the VM did not print the kernel's log to its end"
  # What the kernel says of its own faults, a BPF program's included: a
  # WARN() ("WARNING: CPU: N PID: N at FILE:LINE"), a BUG or oops, a panic, a
  # stall, and the stack dump each of them prints. (Not an advisory that
  # merely says WARNING, as a mitigation notice does.)
  if grep -E '^KF dmesg .*(WARNING: CPU:|BUG:|Oops|Kernel panic|general protection fault|detected stalls|blocked for more than|Call [Tt]race:)' "$log" >&2; then
    bad "the kernel reported a fault on $rel (above)"
  fi
  # Every test of the integration tags' files is run at the floors.
  for f in "$repo"/pkg/agent/hooks/linux/*_test.go; do
    b=""
    case $(grep -m1 '^//go:build' "$f" || true) in
      *pidmap_integration*) b=pidmap ;;
      *proxyconns_integration*) b=proxyconns ;;
    esac
    [ -n "$b" ] || continue
    for n in $(sed -n 's/^func \(Test[A-Za-z0-9_]*\)(t \*testing\.T).*/\1/p' "$f"); do
      listed=$(awk -v b="$b" -v n="$n" '!/^#/ && $1 == b && $2 == n' "$tests")
      [ -n "$listed" ] || bad "$n ($(basename "$f"), $b.test) is not in .github/scripts/kernel-floors/tests.txt: add it, so it runs at the kernel floors"
    done
  done
  if [ "$fail" = 0 ]; then
    echo "$a floor $rel: every test of tests.txt passed, unixconn attached by $way"
  fi
  return "$fail"
}

# selftest ARCH OUT: check passes on OUT's console, and fails on each copy of
# it that lacks one thing check requires or holds one thing it refuses, and
# on a tests.txt that leaves out a test of the integration tags' files.
selftest() {
  local a=$1 out=$2 log rel m tmp b t n=0 wrong=0 first=""
  log="$out/console-$a.log"
  read -r _ rel _ <<< "$(kernel "$a")"
  m=$(machine "$a")
  check "$a" "$out" > /dev/null 2>&1 || die "check fails on $log itself, so failing its copies proves nothing: run check"
  tmp=$(mktemp -d)
  # must_fail WHAT: check on $tmp's console (and $tmp/tests.txt, if there)
  # must fail.
  must_fail() {
    n=$((n + 1))
    if (if [ -s "$tmp/tests.txt" ]; then tests=$tmp/tests.txt; fi; check "$a" "$tmp") > /dev/null 2>&1; then
      echo "::error::check passed a console $1"
      wrong=1
    fi
    rm -f "$tmp/tests.txt"
  }
  while read -r b t; do
    grep -vF -- "--- PASS: $t (" "$log" > "$tmp/console-$a.log" || true
    must_fail "without $t's PASS line ($b.test)"
  done < <(awk '!/^#/ && NF == 2' "$tests")
  for b in $(binaries); do
    sed "s/^KF exit $b 0\$/KF exit $b 1/" "$log" > "$tmp/console-$a.log"
    must_fail "where $b.test exited 1"
  done
  grep -vxF "KF run.sh exited 0" "$log" > "$tmp/console-$a.log" || true
  must_fail "where run.sh never ended"
  sed "s/^KF kernel $rel on $m\$/KF kernel 0.0.0-not-the-floor on $m/" "$log" > "$tmp/console-$a.log"
  must_fail "from another kernel than $rel"
  grep -vF "unixconn: attached" "$log" > "$tmp/console-$a.log" || true
  must_fail "that does not say how the AF_UNIX report attached"
  sed "s/unixconn: attached by fexit on /unixconn: SWAPPED /; s/unixconn: attached at the syscall tracepoints on /unixconn: attached by fexit on /; s/unixconn: SWAPPED /unixconn: attached at the syscall tracepoints on /" "$log" > "$tmp/console-$a.log"
  must_fail "where the AF_UNIX report attached the other way"
  { cat "$log"; echo "    --- SKIP: TestTheHooksLoadOnThisKernel/sub (0.00s)"; } > "$tmp/console-$a.log"
  must_fail "with a skipped subtest"
  { cat "$log"; echo "--- FAIL: TestTheHooksLoadOnThisKernel/sub (0.00s)"; } > "$tmp/console-$a.log"
  must_fail "with a failed subtest"
  grep -vxF "KF dmesg ends" "$log" > "$tmp/console-$a.log" || true
  must_fail "whose kernel log did not end"
  sed "s/^KF dmesg ends\$/KF dmesg [   1.000000] WARNING: CPU: 0 PID: 1 at kernel\/bpf\/verifier.c:1 x\nKF dmesg ends/" "$log" > "$tmp/console-$a.log"
  must_fail "with a kernel WARNING"
  # A test of the integration tags' files left out of tests.txt.
  cp "$log" "$tmp/console-$a.log"
  first=$(awk '!/^#/ && NF == 2 && $1 != "hooks" { print $2; exit }' "$tests")
  awk -v t="$first" '!(!/^#/ && $2 == t)' "$tests" > "$tmp/tests.txt"
  must_fail "with $first left out of tests.txt"
  rm -rf "$tmp"
  [ "$wrong" = 0 ] || die "check passed what it must fail (above)"
  echo "selftest: check passed $log and failed each of its $n broken copies"
}

case ${1:-} in
  build | userland | boot | check | selftest)
    [ $# = 3 ] || die "usage: boot.sh $1 ARCH OUT"
    mkdir -p "$3"
    "$1" "$2" "$(cd "$3" && pwd)"
    ;;
  *) die "usage: boot.sh build|userland|boot|check|selftest ARCH OUT" ;;
esac
