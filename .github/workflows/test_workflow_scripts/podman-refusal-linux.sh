#!/usr/bin/env bash
# E2E: this build refuses an application that runs in Podman, at once, with
# exit code 4 (ExitUnsupportedPlatform) and a message saying where to get a
# keploy that records it, without asking for sudo first.
#
# Before, `keploy record -c "podman run ..."` was taken for a native command:
# keploy ran podman as a host process, the application ran in a container
# keploy never looked at, and the recording came back empty with no word
# about why. The refusal has to cover every spelling of a podman invocation,
# including those the docker-kind detection does not spell out (sudo -E, a
# full path, podman's own flags, a wrapper with arguments), and it must not
# fire where the command is set aside (a base path), so this runs the real
# binary through each.
#
# The same silence hit Docker, which this build does drive: a docker command
# in a form the kind detection does not take for one (sudo -E, a full path,
# docker's own flags, a wrapper) ran as a host process, and the container it
# started was out of keploy's sight. Those are refused too, saying how to
# write the command, and so is `docker start`, whose container cannot be
# given keploy's namespaces. What keploy does take (an application on the
# host with its dependencies in containers, --cmd-type native, an explicit
# container kind, a base path) is checked to get through.
#
# A `sudo` stand-in that fails loudly is first on PATH: keploy re-executes
# itself under sudo for container commands, and a refusal that comes after a
# password prompt is the experience this guards against. It also guarantees
# nothing here runs as root.
#
# Env: RECORD_BIN (the keploy binary under test). Needs a sudo that works
# without a password (a CI runner's) for one setup step, before the stand-in.
set -uo pipefail

BIN="${RECORD_BIN:-keploy}"
BIN="$(command -v "$BIN" || echo "$BIN")"
WORK="$(mktemp -d)"
FAIL=0
fail() { # FAIL counts failures, so each case can tell whether it added one
  echo "FAIL: $*"
  FAIL=$((FAIL + 1))
}

# A keploy folder that a docker-mode run left owned by root, as any such run
# does: fixing its permissions is what made `keploy mock` ask for sudo before
# it got to the refusal. Made with the real sudo, before the stand-in.
REAL_SUDO=$(command -v sudo)
mkdir -p "$WORK/mock-record-owned-folder/keploy"
"$REAL_SUDO" -n touch "$WORK/mock-record-owned-folder/keploy/config.yaml" ||
  { echo "setup: a passwordless sudo is needed to make a root-owned keploy folder"; exit 1; }
"$REAL_SUDO" -n chown root:root "$WORK/mock-record-owned-folder/keploy" "$WORK/mock-record-owned-folder/keploy/config.yaml"

mkdir -p "$WORK/bin"
cat > "$WORK/bin/sudo" <<EOF
#!/bin/sh
echo "sudo \$*" >> "$WORK/sudo.calls"
exit 97
EOF
chmod +x "$WORK/bin/sudo"
export PATH="$WORK/bin:$PATH"

# refusal WANT NAME ARGS...: keploy must refuse saying WANT (a grep
# pattern), exit 4, never call sudo, and write no keploy folder. Leaves the
# output in $out.
refusal() {
  local want=$1 name=$2 rc
  shift 2
  rm -f "$WORK/sudo.calls"
  mkdir -p "$WORK/$name" && cd "$WORK/$name" || exit 1
  out=$(timeout 60 "$BIN" "$@" --disable-tele 2>&1 </dev/null)
  rc=$?
  cd "$WORK" || exit 1
  [ "$rc" -eq 4 ] || fail "$name: exit $rc, want 4 (ExitUnsupportedPlatform)"
  # Here-strings, not `echo | grep -q`: under pipefail the echo dies of
  # SIGPIPE whenever grep -q exits on an early match.
  grep -q -- "$want" <<<"$out" || fail "$name: the refusal does not say: $want"
  [ ! -s "$WORK/sudo.calls" ] || fail "$name: asked for sudo before refusing: $(cat "$WORK/sudo.calls")"
  [ "$name" = mock-record-owned-folder ] || [ ! -d "$WORK/$name/keploy" ] ||
    fail "$name: wrote a keploy folder for a command it refused"
}

refused() { # NAME ARGS...: a Podman command, which this build cannot drive
  local before=$FAIL
  refusal 'cannot record or test applications that run in Podman' "$@"
  # The refusal itself, not the upgrade banner (which also carries the
  # install line).
  grep -q 'run in Podman.*https://keploy.io/install.sh' <<<"$out" ||
    fail "$1: the refusal does not say where to get a keploy that records Podman"
  [ "$FAIL" -ne "$before" ] || echo "ok: $1"
}

# not_refused NAME ARGS...: keploy takes the command; it then needs sudo (the
# stand-in refuses) for the agent or the re-exec, which proves it got past
# the checks. Leaves the output in $out.
not_refused() {
  local name=$1 rc before=$FAIL
  shift
  rm -f "$WORK/sudo.calls"
  mkdir -p "$WORK/$name" && cd "$WORK/$name" || exit 1
  out=$(timeout 60 "$BIN" "$@" --disable-tele 2>&1 </dev/null)
  rc=$?
  cd "$WORK" || exit 1
  [ "$rc" -ne 4 ] || fail "$name: exit 4, as a refusal"
  if grep -qE 'would run as a host process|cannot record or test applications that run in Podman|resumes: its container' <<<"$out"; then
    fail "$name: refused a command keploy takes"
  fi
  [ -s "$WORK/sudo.calls" ] || fail "$name: never got as far as asking for sudo: $(tail -3 <<<"$out")"
  [ "$FAIL" -ne "$before" ] || echo "ok: $name"
}

refused_as_host_process() { # NAME ARGS...: a docker command in a form keploy would run on the host
  local before=$FAIL
  refusal 'would run as a host process' "$@"
  # shellcheck disable=SC2016 # the backquotes are the message's own
  grep -q 'starts with `docker run`' <<<"$out" ||
    fail "$1: the refusal does not say how to write the command"
  [ "$FAIL" -ne "$before" ] || echo "ok: $1"
}

refused record-run record -c "podman run --rm --name app -p 8080:8080 img"
refused test-start test -c "sudo podman start -a app"
refused record-compose record -c "podman compose up"
refused record-podman-compose record -c "podman-compose -f compose.yml up"
refused record-sudo-E record -c "sudo -E podman run --name app img"
refused record-full-path record -c "/usr/bin/podman run --name app img"
refused record-engine-flags record -c "podman --remote run --name app img"
refused record-explicit-kind record --cmd-type docker-run -c "podman run --name app img"
refused mock-record mock record --local -c "sudo -E podman run --name app img"
# sudo -E: a spelling that resolves to native, the kind whose folder
# permissions get fixed (a docker kind's never do, so it would prove nothing).
refused mock-record-owned-folder mock record --local -c "sudo -E podman run --name app img"
refused record-explicit-kind-engine-flags record --cmd-type docker-run -c "podman --remote run --name app img"
refused record-connection-flag record -c "podman -c machine run --name app img"
refused record-empty-cmd-type record --cmd-type= -c "sudo -E podman run --name app img"
refused record-trailer record -c "podman run --rm --name app img || true"

refused_as_host_process docker-sudo-E record -c "sudo -E docker run --rm --name app img"
refused_as_host_process docker-full-path test -c "/usr/bin/docker run --rm --name app img"
refused_as_host_process docker-engine-flags record -c "docker --context default run --rm --name app img"
refused_as_host_process docker-wrapper record -c "timeout 600 docker compose up"
refused_as_host_process docker-mock-record mock record --local -c "sudo -E docker run --rm --name app img"
refused_as_host_process docker-built-first record -c "docker build -t app . && docker run --rm --name app app"
refused_as_host_process docker-trailer record -c "sudo -E docker compose up || true"

# A container `docker start` resumes cannot be given keploy's namespaces: it
# is refused before anything starts, and before any sudo prompt.
before=$FAIL
refusal 'resumes: its container' docker-start record -c "docker start -a app"
grep -q -- '--from-container' <<<"$out" || fail "docker-start: the refusal does not name --from-container"
[ "$FAIL" -ne "$before" ] || echo "ok: docker-start"

# What keploy takes: the application on the host with its dependencies in
# containers, a native command as given, and an explicit container kind.
# cd first: a command that starts with `docker compose` is the compose kind.
not_refused native-with-dependencies record -c "cd . && docker compose up -d db && sleep 1"
not_refused explicit-native record --cmd-type native -c "/usr/bin/docker run --rm --name app img"
not_refused explicit-docker-run record --cmd-type docker-run -c "sudo -E docker run --rm --name app img"

# A base path sets the command aside: nothing runs it, so nothing is refused,
# and the run goes on to look for test sets (there are none here).
rm -f "$WORK/sudo.calls"
mkdir -p "$WORK/base-path" && cd "$WORK/base-path" || exit 1
out=$(timeout 60 "$BIN" test --base-path http://127.0.0.1:1 -c "podman run --name app img" --disable-tele 2>&1 </dev/null)
cd "$WORK" || exit 1
before=$FAIL
if grep -q 'cannot record or test applications that run in Podman' <<<"$out"; then
  fail "base-path: refused a command the base path set aside"
fi
grep -q 'No test-sets found' <<<"$out" || fail "base-path: the run did not go on to look for test sets: $(tail -5 <<<"$out")"
[ ! -s "$WORK/sudo.calls" ] || fail "base-path: asked for sudo: $(cat "$WORK/sudo.calls")"
[ "$FAIL" -ne "$before" ] || echo "ok: base-path"

# A base path sets a docker command aside too.
rm -f "$WORK/sudo.calls"
mkdir -p "$WORK/base-path-docker" && cd "$WORK/base-path-docker" || exit 1
out=$(timeout 60 "$BIN" test --base-path http://127.0.0.1:1 -c "sudo -E docker run --name app img" --disable-tele 2>&1 </dev/null)
cd "$WORK" || exit 1
before=$FAIL
if grep -q 'would run as a host process' <<<"$out"; then
  fail "base-path-docker: refused a command the base path set aside"
fi
grep -q 'No test-sets found' <<<"$out" || fail "base-path-docker: the run did not go on to look for test sets: $(tail -5 <<<"$out")"
[ "$FAIL" -ne "$before" ] || echo "ok: base-path-docker"

"$REAL_SUDO" -n rm -rf "$WORK"
if [ "$FAIL" -ne 0 ]; then
  echo "podman-refusal e2e: FAILED"
  exit 1
fi
echo "podman-refusal e2e: all cases passed"
