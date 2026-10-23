#!/usr/bin/env bash
# E2E: a native application's DNS is recorded, and replayed, on a host whose
# /etc/nsswitch.conf has glibc ask systemd-resolved, and the host's file is
# left byte for byte as it was, a killed run included.
#
# The host is Fedora's: "hosts: files myhostname resolve [!UNAVAIL=return]
# dns". There glibc sends a name lookup to systemd-resolved over its socket,
# and resolved queries its upstream server from its own process, which keploy
# does not see: none of the application's DNS reached keploy, so none was
# recorded. keploy now starts the application in a mount namespace of its own
# whose /etc/nsswitch.conf is a copy that names files, myhostname and dns, and
# so the application's queries reach keploy's DNS server; nothing else on the
# host sees the copy, and the host's file is never written (keploy used to
# rewrite it for every process on the host while keploy test ran, and a killed
# run left it rewritten).
#
# Each phase runs as root in a network namespace of its own (unshare -n) and a
# mount namespace of its own (unshare -m), whose /etc/nsswitch.conf is the
# Fedora file this lane writes (the lane's file, the one it judges), and where
# a systemd-resolved of the lane's own runs, with DEP_IP as its only server,
# which the phase's /etc/resolv.conf names the stub listener of:
#   record  DEP_IP is up, with a DNS server on it that answers
#           dep.keploy.test: a cgo application, which resolves names through
#           glibc, looks the name up for each request; keploy record must
#           record the lookup as a DNS mock, and resolved must not see it
#   test    DEP_IP and its DNS server are gone, so resolved cannot answer the
#           name (this is checked): keploy test passes only if the
#           application's lookups are answered from the recorded mock
#   killed  keploy test, killed with SIGKILL (keploy, its agent and the
#           application) once the application serves
#   leftover-record  keploy record on an authselect host's /etc (a copy of
#           the machine's, bound over /etc in the phase's namespace alone,
#           whose nsswitch.conf links to /etc/authselect/nsswitch.conf),
#           with the lane's file one authselect generated whose hosts line
#           is the one earlier keploy versions wrote while keploy test ran
#           (and a killed run left behind): keploy needs no view there, and
#           must say once, at INFO, that the line is likely an earlier
#           keploy's, naming authselect apply-changes, and leave that file
#           as it is too
# those four with keploy run as root, then with keploy run as the user who
# runs this lane, the way keploy's docs run it (keploy elevates only its
# agent, through sudo):
#   user-record  keploy record: a keploy run as a user has no CAP_SYS_ADMIN,
#           cannot make the application's view, and must say so once, at
#           INFO, naming the remedy (setcap on keploy's binary); the
#           application runs, and resolves its name live, through resolved
#   cap-record and cap-test  record and test again with that remedy (a copy
#           of each binary given cap_sys_admin+p): the lookup must be
#           recorded, and answered from the recording, as with root; and the
#           application must get TMPDIR and LD_LIBRARY_PATH as keploy was
#           started with them, though a file capability has glibc's loader
#           remove both from keploy's own environment
# and the lane's file must be byte for byte as it was while each phase runs
# and after it.
#
# Env: RECORD_BIN and REPLAY_BIN, the keploy binaries under test (both builds
# of this tree: the lane checks the behaviour this tree adds). Run it as a
# user with passwordless sudo, as a CI runner is.
set -uo pipefail

LANE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCRIPTS="$(cd "$LANE_DIR/../.." && pwd)"
RECORD_BIN="$(command -v "${RECORD_BIN:-keploy}" || echo "${RECORD_BIN:-keploy}")"
REPLAY_BIN="$(command -v "${REPLAY_BIN:-keploy}" || echo "${REPLAY_BIN:-keploy}")"
WORK="$(mktemp -d)"
FAIL=0
PORT=8086
DEP_IP=10.203.0.1
NAME=dep.keploy.test
FEDORA_HOSTS='hosts:      files myhostname resolve [!UNAVAIL=return] dns'

fail() {
  echo "::error::$*"
  FAIL=1
}

# --- systemd-resolved and nss-resolve -------------------------------------
# The runner's image has systemd-resolved; nss-resolve is libnss-resolve. The
# packages' scripts may point /etc/resolv.conf at resolved's stub and add
# resolve to /etc/nsswitch.conf: the machine's own files are put back as they
# were, as this lane's phases each have files of their own.
resolved_bin() {
  local b
  for b in /usr/lib/systemd/systemd-resolved /lib/systemd/systemd-resolved; do
    [ -x "$b" ] && { echo "$b"; return 0; }
  done
  return 1
}
nss_resolve_lib() {
  ldconfig -p 2>/dev/null | grep -q 'libnss_resolve\.so\.2'
}
# Only what is missing: Ubuntu 22.04 has resolved in systemd itself, and no
# systemd-resolved package.
setcap_bin() { PATH="$PATH:/usr/sbin:/sbin" command -v setcap; }
missing=()
resolved_bin >/dev/null || missing+=(systemd-resolved)
nss_resolve_lib || missing+=(libnss-resolve)
setcap_bin >/dev/null || missing+=(libcap2-bin)
if [ ${#missing[@]} -gt 0 ]; then
  for f in /etc/resolv.conf /etc/nsswitch.conf; do
    sudo cp -a "$f" "$WORK/machine.$(basename "$f")" 2>/dev/null || true
  done
  # apt-install.sh reads the Ubuntu archive alone unless told otherwise: a
  # machine of another distribution (a Debian CI image) installs from its own.
  sources=()
  [ "$(. /etc/os-release 2>/dev/null; echo "${ID:-}")" = ubuntu ] || sources=(--all-sources)
  bash "$SCRIPTS/apt-install.sh" "${sources[@]}" "${missing[@]}" || { echo "::error::could not install ${missing[*]}"; exit 1; }
  for f in /etc/resolv.conf /etc/nsswitch.conf; do
    b="$WORK/machine.$(basename "$f")"
    [ -e "$b" ] || [ -L "$b" ] || continue
    if [ -L "$b" ] || [ -L "$f" ]; then
      [ "$(readlink "$b")" = "$(readlink "$f")" ] || { sudo rm -f "$f" && sudo cp -a "$b" "$f"; }
    else
      # Its content, into the same file: one mounted over (a container's
      # resolv.conf) cannot be replaced.
      cmp -s "$b" "$f" || sudo cp "$b" "$f"
    fi
  done
fi
RESOLVED="$(resolved_bin)" || { echo "::error::no systemd-resolved binary"; exit 1; }
SETCAP="$(setcap_bin)" || { echo "::error::no setcap"; exit 1; }
nss_resolve_lib || { echo "::error::no libnss_resolve.so.2"; exit 1; }
id systemd-resolve >/dev/null 2>&1 || { echo "::error::no systemd-resolve user for resolved to run as"; exit 1; }

# --- the lane's files -------------------------------------------------------
# The lane's /etc/nsswitch.conf: the machine's, with Fedora's hosts line. The
# phases mount it over /etc/nsswitch.conf, so a write to theirs is a write to
# it; orig is what it must stay.
{ grep -Ev '^[[:space:]]*hosts[[:space:]:]' /etc/nsswitch.conf 2>/dev/null; echo "$FEDORA_HOSTS"; } > "$WORK/nsswitch.conf"
cp "$WORK/nsswitch.conf" "$WORK/nsswitch.orig"
# The leftover phase's: one authselect generated, whose hosts line is the one
# earlier keploy versions wrote over every hosts line.
{ echo "# Generated by authselect"; grep -Ev '^[[:space:]]*hosts[[:space:]:]' /etc/nsswitch.conf 2>/dev/null; echo "hosts: files dns"; } > "$WORK/nsswitch.leftover"
# The phases' /etc/resolv.conf names the lane's resolved's stub listener, as
# a resolved host's does: this file is mounted over wherever the machine's
# leads (a runner's links into resolved's runtime directory, which is the
# lane's in a phase), the same every run. keploy's DNS server then forwards
# what it has no answer for to the stub, which asks DEP_IP: the
# application's queries reach keploy, resolved's varlink requests are
# glibc's alone, and those are what is counted.
echo "nameserver 127.0.0.53" > "$WORK/resolv.conf"
cat > "$WORK/resolved.conf" <<EOF
[Resolve]
DNS=$DEP_IP
Domains=~.
FallbackDNS=
LLMNR=no
MulticastDNS=no
DNSSEC=no
DNSOverTLS=no
Cache=no
DNSStubListener=yes
EOF

# Answers A for dep.keploy.test with DEP_IP, NXDOMAIN for every other name.
# Says "ready" once bound.
cat > "$WORK/dnsserver.py" <<'PY'
import socket, struct, sys
ip, name = sys.argv[1], sys.argv[2]
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind((ip, 53))
print("ready", flush=True)
def answer(q):
    tid = struct.unpack(">H", q[:2])[0]
    i, labels = 12, []
    while q[i]:
        labels.append(q[i + 1:i + 1 + q[i]].decode())
        i += 1 + q[i]
    qtype = struct.unpack(">H", q[i + 1:i + 3])[0]
    question = q[12:i + 5]
    if ".".join(labels).lower() != name:
        return struct.pack(">HHHHHH", tid, 0x8183, 1, 0, 0, 0) + question
    if qtype != 1:
        return struct.pack(">HHHHHH", tid, 0x8180, 1, 0, 0, 0) + question
    return (struct.pack(">HHHHHH", tid, 0x8180, 1, 1, 0, 0) + question +
            struct.pack(">HHHIH4s", 0xC00C, 1, 1, 60, 4, socket.inet_aton(ip)))
while True:
    q, addr = s.recvfrom(512)
    try:
        s.sendto(answer(q), addr)
    except (IndexError, struct.error, UnicodeDecodeError) as e:
        print("unanswered query:", e, file=sys.stderr)
PY

# The application: a cgo build, made to resolve through glibc
# (GODEBUG=netdns=cgo, set where it runs), which looks NAME up (IPv4) for each
# request to /resolve. It writes the variables of its environment that the
# cap phases follow to APPENV_FILE as it starts.
mkdir -p "$WORK/app"
cat > "$WORK/app/go.mod" <<'EOF'
module nsapp

go 1.21
EOF
cat > "$WORK/app/main.go" <<'EOF'
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
)

func main() {
	name := os.Getenv("LOOKUP_NAME")
	if f := os.Getenv("APPENV_FILE"); f != "" {
		var b strings.Builder
		for _, k := range []string{"TMPDIR", "LD_LIBRARY_PATH"} {
			v, ok := os.LookupEnv(k)
			if !ok {
				v = "<unset>"
			}
			fmt.Fprintf(&b, "%s=%s\n", k, v)
		}
		if err := os.WriteFile(f, []byte(b.String()), 0o644); err != nil {
			panic(err)
		}
	}
	http.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	http.HandleFunc("/resolve", func(w http.ResponseWriter, _ *http.Request) {
		res := map[string]any{"name": name}
		ips, err := net.DefaultResolver.LookupIP(context.Background(), "ip4", name)
		if err != nil {
			res["error"] = err.Error()
		}
		addrs := []string{}
		for _, ip := range ips {
			addrs = append(addrs, ip.String())
		}
		sort.Strings(addrs)
		res["addrs"] = addrs
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	})
	if err := http.ListenAndServe(":"+os.Getenv("PORT"), nil); err != nil {
		panic(err)
	}
}
EOF
(cd "$WORK/app" && CGO_ENABLED=1 go build -o "$WORK/nsapp" .) || { echo "::error::could not build the application"; exit 1; }

# --- the user's phases ------------------------------------------------------
# keploy run as this lane's user, as keploy's docs run it: not root, its agent
# elevated through sudo, which must not prompt here.
LANE_UID="$(id -u)" LANE_GID="$(id -g)" LANE_USER="$(id -un)"
[ "$LANE_UID" != 0 ] || { echo "::error::run this lane as a user with passwordless sudo (a CI runner is one): its user phases run keploy as that user"; exit 1; }
sudo -n true || { echo "::error::$LANE_USER has no passwordless sudo, which keploy run as a user elevates its agent with"; exit 1; }
# Each its own working directory (the root phases' is root's) and home, and
# the cap phases a TMPDIR of the user's.
mkdir -p "$WORK/user/home" "$WORK/cap/home" "$WORK/cap/tmp" "$WORK/leftover" || exit 1
# The remedy the fallback names, on copies of the binaries under test: setcap
# cap_sys_admin+p. The kernel ignores file capabilities on a nosuid mount,
# where the cap phases' keploy would fall back like the user's; and a
# statically linked keploy has no loader to remove anything from its
# environment, where cap_env would pass with nothing restored: both are
# refused here, with what to do.
if findmnt -no OPTIONS --target "$WORK" 2>/dev/null | tr , '\n' | grep -qx nosuid; then
  echo "::error::$WORK is on a nosuid mount, where the kernel ignores file capabilities: run the lane with TMPDIR on another mount"
  exit 1
fi
for b in "$RECORD_BIN" "$REPLAY_BIN"; do
  ldd "$b" >/dev/null 2>&1 || { echo "::error::$b is not linked dynamically (keploy's release and CI builds are, with cgo): the cap phases would check nothing of its environment"; exit 1; }
done
cp "$RECORD_BIN" "$WORK/cap/keploy-record" && cp "$REPLAY_BIN" "$WORK/cap/keploy-replay" || exit 1
for b in "$WORK/cap/keploy-record" "$WORK/cap/keploy-replay"; do
  sudo "$SETCAP" cap_sys_admin+p "$b" || { echo "::error::could not give $b CAP_SYS_ADMIN"; exit 1; }
done

# --- one phase, inside its namespaces ---------------------------------------
# Runs as root: $1 is the phase. Leaves its results in $WORK/<phase>.*.
cat > "$WORK/inside.sh" <<'SH'
#!/bin/bash
PHASE=$1
# The phase's kind (record, test, killed), its working directory, and how
# keploy runs in it: as root, or as the lane's user (user-*, cap-*), with the
# environment that user's own shell would give it (no sudo's variables), and
# for cap-* the binaries given CAP_SYS_ADMIN.
KIND=${PHASE#*-}
AS=()
case "$PHASE" in
  user-* | cap-*)
    AS=(setpriv --reuid="$LANE_UID" --regid="$LANE_GID" --init-groups --
      env -u SUDO_USER -u SUDO_UID -u SUDO_GID -u SUDO_COMMAND -u SUDO_HOME USER="$LANE_USER" LOGNAME="$LANE_USER")
    LEG="$WORK/${PHASE%%-*}"
    ;;
  leftover-*) LEG="$WORK/leftover" ;;
  *) LEG="$WORK" ;;
esac
case "$PHASE" in
  cap-*)
    RECORD_BIN="$WORK/cap/keploy-record" REPLAY_BIN="$WORK/cap/keploy-replay"
    # Two of the variables glibc's loader removes from a program a file
    # capability starts in secure mode, given to keploy alone (not to this
    # root script's tools): the application must get them as keploy was
    # started with them.
    AS+=(TMPDIR="$LEG/tmp" LD_LIBRARY_PATH="$LEG/lib")
    ;;
esac
cd "$LEG" || exit 1
export HOME="$LEG/home" GODEBUG=netdns=cgo LOOKUP_NAME="$NAME" PORT APPENV_FILE="$WORK/$PHASE.appenv"
[ ${#AS[@]} -gt 0 ] || mkdir -p "$HOME"
PIDS=""
trap '[ -z "$PIDS" ] || kill $PIDS 2>/dev/null' EXIT
note() { echo "$*" >> "$WORK/$PHASE.notes"; }
# ready PID FILE: waits for PID to say "ready" in FILE.
ready() {
  until grep -qx ready "$2" 2>/dev/null; do
    kill -0 "$1" 2>/dev/null || { note "a server exited before it listened: $(cat "$2")"; exit 1; }
    sleep 0.1
  done
}
# ORIG: the file the phase's /etc/nsswitch.conf must stay (see phase).
same() { cmp -s /etc/nsswitch.conf "$WORK/$ORIG"; }

ip link set lo up || exit 1
case "$PHASE" in
  leftover-*)
    # An authselect host's /etc, in this namespace alone: a copy of the
    # machine's, on a tmpfs of the namespace's own (so that its secrets never
    # reach the disk, and it goes with the namespace), bound over /etc, whose
    # nsswitch.conf is authselect's link to /etc/authselect/nsswitch.conf and
    # whose authselect.conf selects a default profile authselect ships. The
    # lane's file is bound over the link's target below.
    ETC="$WORK/$PHASE.etc"
    { mkdir "$ETC" && mount -t tmpfs -o mode=0755 tmpfs "$ETC" && cp -a /etc/. "$ETC/"; } || { note "could not copy /etc"; exit 1; }
    { rm -f "${ETC:?}/nsswitch.conf" && mkdir -p "$ETC/authselect" && : > "$ETC/authselect/nsswitch.conf" &&
      ln -s /etc/authselect/nsswitch.conf "$ETC/nsswitch.conf" &&
      printf 'local\nwith-silent-lastlog\n' > "$ETC/authselect/authselect.conf"; } || { note "could not make an authselect /etc"; exit 1; }
    mount --bind "$ETC" /etc || { note "could not mount the authselect /etc"; exit 1; }
    ;;
esac
mount --bind "$WORK/nsswitch.conf" /etc/nsswitch.conf || exit 1
# resolved's own runtime directory, configuration and no D-Bus: the
# machine's are out of sight, and this resolved is the one glibc asks.
mkdir -p /run/systemd /etc/systemd || exit 1
mount -t tmpfs tmpfs /run/systemd || exit 1
mount -t tmpfs tmpfs /etc/systemd || exit 1
cp "$WORK/resolved.conf" /etc/systemd/resolved.conf || exit 1
if [ -d /run/dbus ]; then mount -t tmpfs tmpfs /run/dbus || exit 1; fi

if [ "$KIND" = record ]; then
  # A veth pair only to hold DEP_IP: the veth driver is there wherever
  # Docker is, which a dummy link's module is not on every runner kernel.
  ip link add kdep0 type veth peer name kdep1 || exit 1
  ip addr add "$DEP_IP/24" dev kdep0 || exit 1
  ip link set kdep0 up && ip link set kdep1 up || exit 1
  python3 "$WORK/dnsserver.py" "$DEP_IP" "$NAME" > "$WORK/$PHASE.dns.out" 2>&1 &
  PIDS="$!"
  ready "$!" "$WORK/$PHASE.dns.out"
fi
SYSTEMD_LOG_LEVEL=debug SYSTEMD_LOG_TARGET=console "$RESOLVED" > "$WORK/$PHASE.resolved.log" 2>&1 &
PIDS="$PIDS $!"
RESOLVED_PID=$!
up() { kill -0 "$RESOLVED_PID" 2>/dev/null || { note "systemd-resolved exited: $(tail -5 "$WORK/$PHASE.resolved.log")"; exit 1; }; }
until [ -S /run/systemd/resolve/io.systemd.Resolve ]; do up; sleep 0.1; done
# The lane's resolv.conf (see above), over wherever /etc/resolv.conf leads.
# A link into /run/systemd/resolve, the lane's tmpfs now, may lead to a file
# resolved has not written (yet), in the directory it made (its varlink
# socket is there): made there, where it goes with the namespace. Nothing is
# made anywhere else, which is the machine's.
RESOLV="$(readlink -f /etc/resolv.conf)"
case "$RESOLV" in
  /run/systemd/resolve/*) [ -e "$RESOLV" ] || touch "$RESOLV" || { note "could not make $RESOLV"; exit 1; } ;;
esac
mount --bind "$WORK/resolv.conf" "$RESOLV" ||
  { note "could not mount the lane's resolv.conf over $RESOLV (/etc/resolv.conf: $(ls -l /etc/resolv.conf))"; exit 1; }
up

# The lane's own proof that glibc asks resolved here: a lookup through the
# hosts line that resolved sees (record), and that cannot be answered
# without the recording (test).
getent ahostsv4 "$NAME" > "$WORK/$PHASE.getent" 2>&1
echo $? > "$WORK/$PHASE.getent.exit"
# resolved logs each lookup asked of it as it receives it, before it answers:
# a count of them, once the lookup has been answered, has them all.
asked() { grep -c "io.systemd.Resolve.ResolveHostname.*\"name\":\"$NAME\"" "$WORK/$PHASE.resolved.log"; }
asked > "$WORK/$PHASE.resolved.before"

# Watches the lane's file while keploy runs: any moment it differs is noted.
( while :; do same || { note "the lane's /etc/nsswitch.conf changed while keploy ran: $(grep -E '^[[:space:]]*hosts' /etc/nsswitch.conf)"; break; }; sleep 0.05; done ) &
WATCH=$!
PIDS="$PIDS $WATCH"

healthy() {
  local i
  for i in $(seq 1 600); do
    curl -sf "http://127.0.0.1:$PORT/health" > /dev/null && return 0
    kill -0 "$1" 2>/dev/null || return 1
    sleep 0.2
  done
  return 1
}
# descendants PID: every process under PID.
descendants() {
  local c
  for c in $(ps -o pid= --ppid "$1"); do
    echo "$c"
    descendants "$c"
  done
}

case "$KIND" in
  record)
    "${AS[@]}" "$RECORD_BIN" record -c "$WORK/nsapp" --generateGithubActions=false --disable-tele > "$WORK/$PHASE.log" 2>&1 &
    KP=$!
    if healthy "$KP"; then
      for i in 1 2; do curl -s "http://127.0.0.1:$PORT/resolve" >> "$WORK/$PHASE.responses"; done
    else
      note "the application never served"
    fi
    kill -INT "$KP"
    wait "$KP"
    echo $? > "$WORK/$PHASE.exit"
    ;;
  test)
    "${AS[@]}" "$REPLAY_BIN" test -c "$WORK/nsapp" --delay 5 --generateGithubActions=false --disable-tele > "$WORK/$PHASE.log" 2>&1
    echo $? > "$WORK/$PHASE.exit"
    ;;
  killed)
    "${AS[@]}" "$REPLAY_BIN" test -c "$WORK/nsapp" --delay 600 --generateGithubActions=false --disable-tele > "$WORK/$PHASE.log" 2>&1 &
    KP=$!
    healthy "$KP" || note "the application never served"
    # Stopped first, all of them, so that none sees another die.
    ALL="$KP $(descendants "$KP")"
    note "killing: $(ps -o pid=,args= -p "$(echo $ALL | tr ' ' ,)" | tr '\n' ';')"
    kill -STOP $ALL 2>/dev/null
    kill -KILL $ALL 2>/dev/null
    wait "$KP" 2>/dev/null
    for p in $ALL; do while kill -0 "$p" 2>/dev/null; do sleep 0.1; done; done
    ;;
esac
kill "$WATCH" 2>/dev/null
asked > "$WORK/$PHASE.resolved.after"
same || note "the lane's /etc/nsswitch.conf differs after keploy: $(grep -E '^[[:space:]]*hosts' /etc/nsswitch.conf)"
exit 0
SH
chmod +x "$WORK/inside.sh"

# phase NAME [ORIG]: ORIG is the lane's file for the phase, under $WORK
# (nsswitch.orig, Fedora's, unless named).
phase() {
  local name=$1 orig=${2:-nsswitch.orig}
  echo "::group::$name"
  # Each phase starts from the lane's file as it was, so that each is judged
  # by what it does itself (a keploy that left the file rewritten has failed
  # the phase it did it in).
  cp "$WORK/$orig" "$WORK/nsswitch.conf" || exit 1
  sudo env PATH="$PATH" WORK="$WORK" NAME="$NAME" PORT="$PORT" DEP_IP="$DEP_IP" \
    RESOLVED="$RESOLVED" RECORD_BIN="$RECORD_BIN" REPLAY_BIN="$REPLAY_BIN" \
    LANE_UID="$LANE_UID" LANE_GID="$LANE_GID" LANE_USER="$LANE_USER" ORIG="$orig" \
    unshare -n -m --propagation private "$WORK/inside.sh" "$name" || fail "$name: the namespace itself failed"
  cat "$WORK/$name.notes" 2>/dev/null
  grep -q "changed while keploy ran\|differs after keploy" "$WORK/$name.notes" 2>/dev/null &&
    fail "$name: keploy wrote the lane's /etc/nsswitch.conf"
  # A run that never served proves nothing of what it was to leave behind.
  grep -q "the application never served\|exited before it listened\|systemd-resolved exited" "$WORK/$name.notes" 2>/dev/null &&
    fail "$name: the phase did not get as far as the application serving"
  cmp -s "$WORK/nsswitch.conf" "$WORK/$orig" ||
    fail "$name: the lane's /etc/nsswitch.conf is not as it was: $(diff "$WORK/$orig" "$WORK/nsswitch.conf" | tr '\n' ' ')"
  echo "::endgroup::"
}

show_log() {
  echo "--- $1: the last 60 lines keploy logged ---"
  tail -n 60 "$WORK/$1.log" 2>/dev/null
}

# What keploy says, once, when it cannot make the application's view.
FALLBACK="without a view of /etc/nsswitch.conf of its own"
fallbacks() { grep -c -F "$FALLBACK" "$WORK/$1.log" 2>/dev/null || true; }
# dns_mocks DIR: how many DNS mocks keploy recorded in DIR's first test set.
dns_mocks() {
  local n
  n=$(grep -c '^kind: DNS' "$1/keploy/test-set-0/mocks.yaml" 2>/dev/null)
  echo "${n:-0}"
}

# lane_saw PHASE: the lane itself: glibc asked resolved, which answered from
# the DNS server.
lane_saw() {
  grep -q "$DEP_IP" "$WORK/$1.getent" 2>/dev/null ||
    fail "$1: getent did not resolve $NAME through resolved: $(cat "$WORK/$1.getent" 2>/dev/null)"
  [ "$(cat "$WORK/$1.resolved.before" 2>/dev/null)" -ge 1 ] 2>/dev/null ||
    fail "$1: resolved did not see the lane's own lookup, so the hosts line does not reach it here"
}

# recorded PHASE DIR: keploy record, run in DIR, recorded the application's
# lookup as a DNS mock, and resolved did not see it.
recorded() {
  local p=$1 mocks="$2/keploy/test-set-0/mocks.yaml"
  lane_saw "$p"
  [ "$(cat "$WORK/$p.exit" 2>/dev/null)" = 0 ] || fail "$p: keploy exited $(cat "$WORK/$p.exit" 2>/dev/null), want 0"
  grep -q "\"addrs\":\[\"$DEP_IP\"\]" "$WORK/$p.responses" 2>/dev/null ||
    fail "$p: the application did not resolve $NAME: $(cat "$WORK/$p.responses" 2>/dev/null)"
  grep -q '^kind: DNS' "$mocks" 2>/dev/null && grep -q "name: $NAME\.\?$" "$mocks" 2>/dev/null ||
    fail "$p: the application's lookup of $NAME was not recorded as a DNS mock"
  [ "$(cat "$WORK/$p.resolved.after")" = "$(cat "$WORK/$p.resolved.before")" ] ||
    fail "$p: the application's lookup went to systemd-resolved, out of keploy's sight"
  [ "$(fallbacks "$p")" = 0 ] || fail "$p: keploy started the application without its view: $(grep -F "$FALLBACK" "$WORK/$p.log")"
  echo "$p: $(dns_mocks "$2") DNS mock(s); the lane's getent: $(tr -s ' \n' ' ' < "$WORK/$p.getent" 2>/dev/null);" \
    "resolved was asked for $NAME $(cat "$WORK/$p.resolved.before") time(s) before keploy, $(cat "$WORK/$p.resolved.after") after;" \
    "the application got: $(cat "$WORK/$p.responses" 2>/dev/null)"
}

# replayed PHASE DIR: keploy test, run in DIR, with the name's server gone,
# passed: the application's lookups were answered from the recording.
replayed() {
  local p=$1 report
  grep -q "$DEP_IP" "$WORK/$p.getent" 2>/dev/null &&
    fail "$p: $NAME resolves without the recording, so the replay would prove nothing"
  report="$(ls -td "$2"/keploy/reports/test-run-* 2>/dev/null | head -n 1)/test-set-0-report.yaml"
  grep -q '^status: PASSED' "$report" 2>/dev/null ||
    fail "$p: the replay did not pass, so the application's lookups were not answered from the recording: $(grep -E '^(status|success|failure):' "$report" 2>/dev/null | tr '\n' ' ')"
  grep -q '^success: [1-9]' "$report" 2>/dev/null || fail "$p: no test passed"
  [ "$(fallbacks "$p")" = 0 ] || fail "$p: keploy started the application without its view: $(grep -F "$FALLBACK" "$WORK/$p.log")"
  echo "$p: the lane's getent, with the name's server gone: $(tr -s ' \n' ' ' < "$WORK/$p.getent" 2>/dev/null) (exit $(cat "$WORK/$p.getent.exit"));" \
    "the replay: $(grep -E '^(status|success|failure):' "$report" 2>/dev/null | tr '\n' ' ')"
}

phase record
recorded record "$WORK"
[ "$FAIL" -eq 0 ] || show_log record

phase test
replayed test "$WORK"
[ "$FAIL" -eq 0 ] || show_log test

phase killed
grep -q "^killing: .*nsapp" "$WORK/killed.notes" 2>/dev/null ||
  fail "killed: the application was not among the processes killed"
[ "$FAIL" -eq 0 ] || show_log killed

# A host that an earlier keploy left at "hosts: files dns", in the file
# authselect manages: no view is needed (dns is there), and keploy says once
# what the line likely is and how authselect puts its own back.
LEFTOVER="no default profile authselect ships writes it, and earlier keploy versions wrote it"
phase leftover-record nsswitch.leftover
[ "$(cat "$WORK/leftover-record.exit" 2>/dev/null)" = 0 ] || fail "leftover-record: keploy exited $(cat "$WORK/leftover-record.exit" 2>/dev/null), want 0"
grep -q "\"addrs\":\[\"$DEP_IP\"\]" "$WORK/leftover-record.responses" 2>/dev/null ||
  fail "leftover-record: the application did not resolve $NAME: $(cat "$WORK/leftover-record.responses" 2>/dev/null)"
n=$(grep -c -F "$LEFTOVER" "$WORK/leftover-record.log" 2>/dev/null)
[ "${n:-0}" = 1 ] || fail "leftover-record: keploy said ${n:-0} time(s) that the hosts line is likely an earlier keploy's, want once"
grep -F "$LEFTOVER" "$WORK/leftover-record.log" 2>/dev/null | grep -q -F "sudo authselect apply-changes -b" ||
  fail "leftover-record: keploy's leftover line does not name sudo authselect apply-changes -b"
[ "$(fallbacks leftover-record)" = 0 ] || fail "leftover-record: keploy said it starts the application without a view, where none is needed"
echo "leftover-record: keploy said: $(grep -F "$LEFTOVER" "$WORK/leftover-record.log" 2>/dev/null | head -n 1 | cut -c1-400)"
[ "$FAIL" -eq 0 ] || show_log leftover-record

# keploy run as a user, as keploy's docs run it, has no CAP_SYS_ADMIN: the
# application starts as it always did, with the host's file, and keploy says
# so once, naming the remedy. Its lookups then go to resolved, live, as they
# did before keploy had views (recorded as no mock; the line says so).
phase user-record
lane_saw user-record
[ "$(cat "$WORK/user-record.exit" 2>/dev/null)" = 0 ] || fail "user-record: keploy exited $(cat "$WORK/user-record.exit" 2>/dev/null), want 0"
grep -q "\"addrs\":\[\"$DEP_IP\"\]" "$WORK/user-record.responses" 2>/dev/null ||
  fail "user-record: the application did not resolve $NAME: $(cat "$WORK/user-record.responses" 2>/dev/null)"
[ "$(fallbacks user-record)" = 1 ] ||
  fail "user-record: keploy said $(fallbacks user-record) time(s) that it starts the application without its view, want once"
grep -F "$FALLBACK" "$WORK/user-record.log" 2>/dev/null | grep -q -F "sudo setcap cap_sys_admin+p " ||
  fail "user-record: keploy's fallback line does not name the remedy (setcap cap_sys_admin+p on its binary)"
echo "user-record: keploy said: $(grep -F "$FALLBACK" "$WORK/user-record.log" 2>/dev/null | head -n 1 | cut -c1-600);" \
  "$(dns_mocks "$WORK/user") DNS mock(s);" \
  "resolved was asked for $NAME $(cat "$WORK/user-record.resolved.before") time(s) before keploy, $(cat "$WORK/user-record.resolved.after") after"
[ "$FAIL" -eq 0 ] || show_log user-record

# The remedy: keploy run as the same user, its binary given CAP_SYS_ADMIN,
# records and replays the lookup as root does.
# cap_env PHASE: the application of a cap phase got TMPDIR and
# LD_LIBRARY_PATH as keploy was started with them.
cap_env() {
  local want
  want="$(printf 'TMPDIR=%s\nLD_LIBRARY_PATH=%s' "$WORK/cap/tmp" "$WORK/cap/lib")"
  [ "$(cat "$WORK/$1.appenv" 2>/dev/null)" = "$want" ] ||
    fail "$1: the application's environment has $(tr '\n' ' ' < "$WORK/$1.appenv" 2>/dev/null), want the one keploy was started with: $(echo "$want" | tr '\n' ' ')"
  echo "$1: the application's environment: $(tr '\n' ' ' < "$WORK/$1.appenv" 2>/dev/null)"
}

phase cap-record
recorded cap-record "$WORK/cap"
cap_env cap-record
[ "$FAIL" -eq 0 ] || show_log cap-record

phase cap-test
replayed cap-test "$WORK/cap"
cap_env cap-test
[ "$FAIL" -eq 0 ] || show_log cap-test

if [ "$FAIL" -ne 0 ]; then
  echo "nsswitch e2e FAILED; logs, mocks and reports are kept in $WORK"
  exit 1
fi
sudo rm -rf "$WORK"
echo "nsswitch e2e passed"
