#!/usr/bin/env bash
# Tests for apt-install.sh. Needs Docker and access to the Ubuntu archive. Run
# by linux-ci-scripts.yml, or locally (it changes nothing on the host):
#
#   bash .github/workflows/test_workflow_scripts/apt-install.tests.sh
#
# failover (ubuntu:22.04, one-line sources.list; ubuntu:24.04, deb822)
#   The container resolves azure.archive.ubuntu.com to a stub. The stub
#   answers every .deb with `502 Proxy Error`, the outage that failed job
#   110208231113, and proxies every other path to the real mirror. Through
#   apt-install.sh the install has to succeed: the indexes still come from
#   azure, which is tried first, and the .deb that azure refuses comes from
#   archive.ubuntu.com.
# control (ubuntu:22.04)
#   The same stub with packages from azure alone, as on that job: apt has to
#   exit 100 with the 502. This proves the stub reproduces the outage, so the
#   failover cases cannot pass by never reaching it. Its package lists may
#   still fall over to archive.ubuntu.com (an apt mirror list of type:index):
#   the stub passes them through to the real azure mirror, which at times
#   serves a list that does not match its InRelease ("Mirror sync in
#   progress?"), and that must not stop the control before the .deb.
# third-party (ubuntu:24.04)
#   thirdparty.test also resolves to the stub. Under /corrupt/ it answers
#   InRelease with a page that is not clearsigned, as packages.microsoft.com
#   has; under /good/ it serves a flat repository with one package. With the
#   corrupt source configured, the default run has to install from Ubuntu
#   without contacting it and without deleting its old list, while
#   --all-sources has to fail on it with exit 100 (the control for that stub).
#   With the good source, --all-sources installs its package and the default
#   run cannot see it. apt skips its list cleanup after a failed fetch, so that
#   makes the kept-list check inconclusive, and a control update shows that
#   cleanup does delete such a list.
# slow mirror (ubuntu:22.04, the helper's own mirror list; ubuntu:24.04, a list
# the image already had, as GitHub's hosted images do)
#   The stub answers azure's .debs at 256 B/s: up, never silent for the 30 s
#   after which apt gives a mirror up, and far too slow, as azure was on job
#   113004700461. archive.ubuntu.com and security.ubuntu.com resolve to the
#   stub as well, which passes them through at full speed, so the case does
#   not hang on how fast those two are on the day. Through apt-install.sh,
#   with 20 s slices in an 80 s budget, the install has to succeed: azure is
#   asked first, the helper puts archive.ubuntu.com in front, that mirror is
#   asked for the rest of the .deb azure had begun (a Range request, so what
#   was fetched was kept), the install itself fetches nothing, and the mirror
#   list ends as it began.
#   Its control (ubuntu:22.04) is the same stub and mirror list under apt
#   alone: `timeout 45 apt-get install`, the helper's old command with a
#   shorter limit. It has to run into the limit (exit 124) without an Ign: or
#   Err: line and without asking another mirror. That is the failure, and it
#   proves the stub is slow in the one way apt does not act on.
# every mirror slow (ubuntu:22.04)
#   All three mirrors answer .debs at 256 B/s. The helper has to put each in
#   front in turn, each asked to go on where the one before it stopped, exit
#   124 when the budget is spent, say in an ::error:: line how much was
#   fetched, at what rate and from which mirrors, install nothing, and put
#   the mirror list back. The budget is three slices, so the third mirror's
#   slice is the last, and there are exactly three.
# rewrite (ubuntu:22.04 and ubuntu:24.04, no network, apt-get shimmed out)
#   Fixture sources: exactly the .../ubuntu URIs on archive.ubuntu.com,
#   security.ubuntu.com and their subdomains are rewritten, the mirror
#   list is exact, a second run changes no file, and a re-run puts back a
#   deleted or outdated list that the sources name. No file is changed at all
#   with RUNNER_ENVIRONMENT=self-hosted, or on sources shaped like GitHub's
#   hosted images, which already use a mirror list of their own. The entries
#   the update and the install are given are exactly the Ubuntu ones, on the
#   fixture and on the hosted shape, and the files with entries left out are
#   named; CRLF line ends and disabled stanzas are read as apt reads them.
#   --all-sources and a self-hosted runner give apt no source options; with no
#   Ubuntu entry at all the default run stops before apt.
#   Then with a shim that outlasts its slice, which needs no mirror to be
#   slow, and that is stopped in the middle of a line every time, as apt is
#   when it is stopped while it starts. Whatever the helper writes after that
#   has to start a line of its own. Every download slice that runs out puts
#   the next mirror in front, in the helper's list and in the image's, and the
#   lists are put back at the end; a mirror that is on course stays in front
#   and no file changes; a slice counts all it fetched, also the one after a
#   slice in which apt began its .deb anew; a spent budget ends in exit 124
#   and the ::error:: line, with nothing installed, also when it runs out
#   between two slices, and then no mirror is put in front any more; an update
#   or an install that outlasts the budget ends in exit 124 too, each with an
#   ::error:: line of its own, and the update in one piece, with no list
#   reordered; and with no list to reorder, or on a self-hosted runner, the
#   download gets its budget in one piece and no file changes.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# amd64 only: an arm64 ubuntu image fetches from ports.ubuntu.com, which the
# helper rightly leaves alone, so the failover cases would fail for no fault.
arch="$(docker info -f '{{.Architecture}}')"
if [ "$arch" != x86_64 ]; then
  echo "FAIL: these tests need an x86_64 Docker host, this one is $arch"
  exit 1
fi
helper="$here/apt-install.sh"
work="$(mktemp -d)"
net="apt-install-tests-$$"
stub="apt-install-stub-$$"
failures=0

cleanup() {
  docker rm -f "$stub" >/dev/null 2>&1 || true
  docker network rm "$net" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*"
  failures=$((failures + 1))
}

# expect <log> <description> <grep -E pattern>
expect() {
  if grep -Eq -- "$3" "$1"; then
    echo "ok: $2"
  else
    fail "$2 (no line matching: $3)"
  fi
}

# absent <log> <description> <grep -E pattern>
absent() {
  if grep -Eq -- "$3" "$1"; then
    fail "$2 (a line matches: $3)"
  else
    echo "ok: $2"
  fi
}

# show_log_if_failed <log> <failure count when the case started>
show_log_if_failed() {
  [ "$failures" = "$2" ] && return 0
  echo "::group::$1"
  cat "$1"
  echo "::endgroup::"
}

# A size as the helper prints it; with /s after it, a rate.
size='[0-9.]+ (B|kB|MB)'

cat >"$work/stub.py" <<'EOF'
import http.server, os, socketserver, sys, time, urllib.error, urllib.request

# Where what the stub passes through comes from: azure, and archive.ubuntu.com
# for a file azure fails. The real azure at times lacks a list its InRelease
# names, and in the slow-mirror cases apt's own fallbacks lead here as well.
UPSTREAMS = ("http://azure.archive.ubuntu.com", "http://archive.ubuntu.com")
AZURE = "azure.archive.ubuntu.com"
REPO = "/repo"
# A slow answer is 128 bytes every half second: 256 B/s, at which no .deb here
# is done within a test, and never 30 s of silence, after which apt would give
# the mirror up by itself.
CHUNK, PAUSE = 128, 0.5
# What the real mirror answered for everything but a .deb, by path. The package
# lists are fetched from it once and served from here after that, so that no
# later case depends on how fast the real mirror is at that moment.
LISTS = {}

class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        sys.stdout.write("stub: " + (fmt % args) + "\n")
        sys.stdout.flush()

    def reply(self, code, reason, body, headers=(), slow=False):
        self.send_response(code, reason)
        for name, value in headers:
            self.send_header(name, value)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        if not slow:
            self.wfile.write(body)
            return
        try:
            for i in range(0, len(body), CHUNK):
                self.wfile.write(body[i:i + CHUNK])
                time.sleep(PAUSE)
        except OSError:
            # apt was stopped in the middle of the file.
            self.close_connection = True

    # A third-party apt repository. /corrupt/ answers InRelease with a page
    # that is not clearsigned; /good/ is the flat repository built below.
    def third_party(self):
        if self.path.startswith("/corrupt/") and self.path.endswith("/InRelease"):
            return self.reply(200, "OK", b"<html>not a release file</html>\n")
        name = os.path.basename(self.path)
        if self.path.startswith("/good/") and os.path.isfile(os.path.join(REPO, name)):
            with open(os.path.join(REPO, name), "rb") as f:
                return self.reply(200, "OK", f.read())
        return self.reply(404, "Not Found", b"")

    def do_GET(self):
        host = self.headers.get("Host", "").split(":")[0]
        if host == "thirdparty.test":
            return self.third_party()
        # A case asks for its fault, and names itself, in apt's User-Agent:
        # "apt-install-tests <fault> <case>". Without one, a .deb is refused.
        agent = self.headers.get("User-Agent", "").split()
        fault, case = "refuse", "-"
        if len(agent) == 3 and agent[0] == "apt-install-tests":
            fault, case = agent[1], agent[2]
        deb = self.path.endswith(".deb")
        if deb:
            self.log_message("case=%s host=%s range=%s %s", case, host,
                             self.headers.get("Range", "-"), self.path)
        if deb and fault == "refuse":
            return self.reply(502, "Proxy Error", b"502 Proxy Error\n")
        slow = deb and ((fault == "slow-debs" and host == AZURE)
                        or fault == "slow-debs-everywhere")
        whole = "Range" not in self.headers
        if whole and self.path in LISTS:
            return self.reply(200, "OK", *LISTS[self.path], slow)
        failed = (502, "Bad Gateway", b"")
        for upstream in UPSTREAMS:
            # A file apt has part of is asked for from where that part ends.
            ask = urllib.request.Request(upstream + self.path)
            for name in ("Range", "If-Range"):
                if self.headers.get(name):
                    ask.add_header(name, self.headers[name])
            try:
                with urllib.request.urlopen(ask, timeout=60) as r:
                    keep = [(k, v) for k, v in r.getheaders()
                            if k.lower() in ("last-modified", "content-range")]
                    body = r.read()
                    if whole and not deb and r.status == 200:
                        LISTS[self.path] = (body, keep)
                    return self.reply(r.status, r.reason, body, keep, slow)
            except urllib.error.HTTPError as e:
                failed = (e.code, e.reason, e.read() or b"")
            except OSError:
                # Not reached, or no answer in time.
                pass
        return self.reply(*failed)

class Server(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True

Server(("0.0.0.0", 80), Handler).serve_forever()
EOF

# The good third-party repository: one package, an unsigned Release (the
# source is marked trusted=yes) and its Packages index.
mkdir "$work/repo"
docker run --rm --user "$(id -u):$(id -g)" -v "$work/repo:/repo" ubuntu:24.04 bash -c '
set -euo pipefail
pkg="$(mktemp -d)"
mkdir -p "$pkg/DEBIAN" "$pkg/usr/share/keploy-apt-test"
printf "%s\n" "Package: keploy-apt-test" "Version: 1.0" "Architecture: all" \
  "Maintainer: keploy CI <ci@keploy.io>" "Description: apt-install.sh test package" \
  >"$pkg/DEBIAN/control"
echo ok >"$pkg/usr/share/keploy-apt-test/ok"
cd /repo
deb=keploy-apt-test_1.0_all.deb
dpkg-deb --root-owner-group --build "$pkg" "$deb" >/dev/null
{ cat "$pkg/DEBIAN/control"; echo "Filename: ./$deb"; echo "Size: $(stat -c %s "$deb")"
  echo "SHA256: $(sha256sum "$deb" | cut -d " " -f 1)"; } >Packages
printf "Date: %s\nSHA256:\n %s %s Packages\n" "$(date -Ru)" \
  "$(sha256sum Packages | cut -d " " -f 1)" "$(stat -c %s Packages)" >Release
'

docker network create "$net" >/dev/null
docker run -d --name "$stub" --network "$net" -v "$work/stub.py:/stub.py:ro" \
  -v "$work/repo:/repo:ro" python:3.12-alpine python -u /stub.py >/dev/null
stub_ip="$(docker inspect -f "{{(index .NetworkSettings.Networks \"$net\").IPAddress}}" "$stub")"
# Listening is enough: a request through to the real mirror could hang on the
# upstream instead of telling whether the stub is up.
ready=no
for _ in $(seq 1 30); do
  if docker exec "$stub" python -c 'import socket; socket.create_connection(("127.0.0.1", 80), 1)' 2>/dev/null; then
    ready=yes
    break
  fi
  sleep 1
done
if [ "$ready" != yes ]; then
  docker logs "$stub" || true
  echo "FAIL: the stub mirror never started listening"
  exit 1
fi

# in_ubuntu <image> <log> <script> [docker run option ...]: the script runs as
# root, with the helper at /h/apt-install.sh and azure.archive.ubuntu.com and
# thirdparty.test resolving to the stub. The longest wait a case has by design
# is the 90 s budget of the every-mirror-slow one; the 300 s cap makes a hung
# case fail with its log (exit 124) instead of running into the job's timeout
# silently. stderr joins stdout inside the container: docker keeps the two as
# separate streams and does not keep their order, so an apt error could
# otherwise land after the next section's header.
in_ubuntu() {
  local rc=0 image="$1" log="$2" script="$3"
  shift 3
  docker run --rm --network "$net" --add-host "azure.archive.ubuntu.com:$stub_ip" \
    --add-host "thirdparty.test:$stub_ip" "$@" -v "$helper:/h/apt-install.sh:ro" "$image" \
    timeout 300 bash -c "exec 2>&1; $script" >"$log" || rc=$?
  echo "$rc"
}

# section <log> <title>: the lines after `--- <title>`, up to the next `--- `.
section() {
  awk -v title="--- $2" '$0 == title { on = 1; next } /^--- / { on = 0 } on' "$1"
}

# A non-comment line still naming the Ubuntu archive over plain http(s).
plain_uri='^[^#]*https?://([A-Za-z0-9-]+\.)*(archive|security)\.ubuntu\.com/ubuntu/?([[:space:]]|$)'

for spec in jammy:ubuntu:22.04 noble:ubuntu:24.04; do
  codename="${spec%%:*}"
  image="${spec#*:}"
  log="$work/failover-$codename.log"
  started=$failures
  echo "--- failover ($image)"
  rc="$(in_ubuntu "$image" "$log" '
    set -euo pipefail
    bash /h/apt-install.sh hello
    hello
    echo "--- sources"
    cat /etc/apt/sources.list /etc/apt/sources.list.d/*.sources 2>/dev/null || true')"
  [ "$rc" = 0 ] || fail "$image: apt-install.sh exited $rc"
  expect "$log" "$image: indexes come from azure first" \
    "^Get:[0-9]+ http://azure\.archive\.ubuntu\.com/ubuntu $codename InRelease"
  expect "$log" "$image: azure refused the .deb" \
    "^(Ign|Err):[0-9]+ http://azure\.archive\.ubuntu\.com/ubuntu $codename(-updates|-security)?/main amd64 hello "
  expect "$log" "$image: archive.ubuntu.com served it" \
    "^Get:[0-9]+ http://archive\.ubuntu\.com/ubuntu $codename(-updates|-security)?/main amd64 hello "
  expect "$log" "$image: hello runs" '^Hello, world!$'
  if grep -Eq "$plain_uri" <(sed -n '/^--- sources$/,$p' "$log"); then
    fail "$image: an Ubuntu source still bypasses the mirror list"
  fi
  show_log_if_failed "$log" "$started"
done

echo "--- control: packages from azure alone, as on job 110208231113 (ubuntu:22.04)"
log="$work/control.log"
started=$failures
# shellcheck disable=SC2016 # $opts expands inside the container
rc="$(in_ubuntu ubuntu:22.04 "$log" '
  printf "%s\n" "http://azure.archive.ubuntu.com/ubuntu/	priority:1" \
    "http://archive.ubuntu.com/ubuntu/	priority:2	type:index" >/etc/apt/control-mirrors.txt
  sed -i -E "s#http://(archive|security)\.ubuntu\.com/ubuntu/?#mirror+file:/etc/apt/control-mirrors.txt#" \
    /etc/apt/sources.list
  opts="-o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 -o DPkg::Lock::Timeout=120"
  timeout 600 apt-get $opts -y update && timeout 600 apt-get $opts -y install hello')"
if [ "$rc" = 100 ]; then echo "ok: control exits 100"; else fail "control exited $rc, want 100"; fi
expect "$log" "control fails on the stub's 502" \
  "^E: Failed to fetch http://azure\.archive\.ubuntu\.com/.*/hello_.*502  Proxy Error"
show_log_if_failed "$log" "$started"

echo "--- third-party sources (ubuntu:24.04)"
log="$work/third-party.log"
started=$failures
# shellcheck disable=SC2016 # $? expands inside the container
rc="$(in_ubuntu ubuntu:24.04 "$log" '
  run() { echo "--- $1"; shift; bash /h/apt-install.sh "$@"; echo "exit $?"; }
  printf "deb http://thirdparty.test/corrupt ./\n" >/etc/apt/sources.list.d/thirdparty.list
  # What an earlier update of every source left behind.
  touch /var/lib/apt/lists/thirdparty.test_corrupt_._Packages
  run "default, corrupt third-party index" hello
  if [ -f /var/lib/apt/lists/thirdparty.test_corrupt_._Packages ]; then echo "third-party list kept"; fi
  run "all sources, corrupt third-party index" --all-sources hello
  printf "deb [trusted=yes] http://thirdparty.test/good ./\n" >/etc/apt/sources.list.d/thirdparty.list
  run "all sources, third-party package" --all-sources keploy-apt-test
  dpkg-query -W keploy-apt-test
  apt-get -y remove keploy-apt-test >/dev/null
  run "default, third-party package" keploy-apt-test
  # The control: with cleanup on, an update does delete such a list.
  echo "--- control"
  touch /var/lib/apt/lists/thirdparty.test_corrupt_._Packages
  apt-get -o Dir::Etc::SourceList=/dev/null -o Dir::Etc::SourceParts=/dev/null update >/dev/null
  if [ ! -f /var/lib/apt/lists/thirdparty.test_corrupt_._Packages ]; then echo "cleanup deletes the list"; fi
  echo "--- end"')"
[ "$rc" = 0 ] || fail "third-party case exited $rc"
section "$log" "default, corrupt third-party index" >"$work/tp-default"
expect "$work/tp-default" "an Ubuntu-only install succeeds beside a corrupt third-party index" '^exit 0$'
expect "$work/tp-default" "it installs the package" '^Setting up hello '
if grep -q 'thirdparty\.test' "$work/tp-default"; then
  fail "an Ubuntu-only update contacted the third-party source"
fi
# apt skips its list cleanup after a failed fetch, and then the list would be
# kept whatever the options.
if grep -q 'Some index files failed to download' "$work/tp-default"; then
  grep -E '^(W|E): ' "$work/tp-default" || true
  fail "an index fetch failed on every mirror, so apt skipped its list cleanup and the kept-list check is inconclusive"
fi
expect "$log" "an Ubuntu-only update keeps the third-party lists" '^third-party list kept$'
expect "$log" "control: an update with cleanup on deletes such a list" '^cleanup deletes the list$'
section "$log" "all sources, corrupt third-party index" >"$work/tp-all"
expect "$work/tp-all" "--all-sources exits 100 on the corrupt index (control)" '^exit 100$'
expect "$work/tp-all" "--all-sources fails on the stub's corrupt InRelease" \
  "thirdparty\.test/corrupt.*InRelease.*NOSPLIT"
section "$log" "all sources, third-party package" >"$work/tp-good"
expect "$work/tp-good" "--all-sources installs a third-party package" '^exit 0$'
expect "$log" "the third-party package is installed" '^keploy-apt-test[[:space:]]+1\.0$'
section "$log" "default, third-party package" >"$work/tp-hidden"
expect "$work/tp-hidden" "an Ubuntu-only install cannot see third-party packages" '^exit 100$'
expect "$work/tp-hidden" "apt cannot locate the third-party package" \
  '^E: Unable to locate package keploy-apt-test$'
show_log_if_failed "$log" "$started"

# The slow-mirror cases. In them archive.ubuntu.com and security.ubuntu.com
# resolve to the stub as well, which passes them through unless the fault says
# otherwise.
fallbacks_to_stub=(--add-host "archive.ubuntu.com:$stub_ip" --add-host "security.ubuntu.com:$stub_ip")
# fault <fault> <case>, inside the container: from then on apt there asks the
# stub for that fault, and the stub's log names the case beside every .deb it
# is asked for. `none` is a healthy mirror.
# shellcheck disable=SC2016 # expands inside the container
fault_fn='fault() { echo "Acquire::http::User-Agent \"apt-install-tests $1 $2\";" >/etc/apt/apt.conf.d/99stub; }'
# stub_saw <case>: the requests for a .deb the stub got in that case.
stub_saw() { docker logs "$stub" 2>&1 | grep -F "case=$1 " || true; }
# after_download <log>: what follows apt's last word on a finished download.
after_download() { sed -n '/^Download complete and in download only mode$/,$p' "$1" | sed 1d; }
# A line that apt had begun when it was stopped, with something written after
# it. apt ends each of these three with the line end alone when its output is
# not a terminal, so anything more on the line was written by someone else.
glued='(Reading package lists|Building dependency tree|Reading state information)\.\.\..'
# The helper under a slow azure, after a run that left the package lists and
# the mirror list in place. $case and $list are set by the script it ends.
# shellcheck disable=SC2016 # expands inside the container
past_slow_azure='
  echo "--- helper"
  fault slow-debs "$case"
  APT_INSTALL_SLICE_SECONDS=20 APT_INSTALL_BUDGET_SECONDS=80 bash /h/apt-install.sh jq
  echo "exit $?"
  jq --version
  if cmp /tmp/list.before "$list"; then echo "mirror list: as it was"; fi
  echo "--- end"'

# slow_mirror_checks <image> <codename> <case> <log>
slow_mirror_checks() {
  local image="$1" codename="$2" case="$3" log="$4" deb
  deb="$codename(-updates|-security)?/main amd64 (jq|libjq1|libonig5) "
  # With healthy mirrors: the two apt runs that replaced the one install.
  section "$log" healthy >"$work/$case.healthy"
  expect "$work/$case.healthy" "$image: with healthy mirrors the helper installs" '^exit 0$'
  expect "$work/$case.healthy" "$image: it downloads first" '^Download complete and in download only mode$'
  after_download "$work/$case.healthy" >"$work/$case.healthy-install"
  expect "$work/$case.healthy-install" "$image: and then installs" '^Setting up hello '
  absent "$work/$case.healthy-install" "$image: without fetching again" '^Get:'

  section "$log" helper >"$work/$case.helper"
  expect "$work/$case.helper" "$image: the helper installs past a slow azure" '^exit 0$'
  expect "$work/$case.helper" "$image: azure is asked first" \
    "^Get:[0-9]+ http://azure\.archive\.ubuntu\.com/ubuntu $deb"
  expect "$work/$case.helper" "$image: the helper finds azure too slow and puts archive.ubuntu.com in front" \
    "^apt-install: [0-9]+ s with http://azure\.archive\.ubuntu\.com/ubuntu/ in front fetched $size \($size/s\), too slow for the $size still to fetch in the [0-9]+ s left\. Putting http://archive\.ubuntu\.com/ubuntu/ in front; apt keeps what it has fetched\.\$"
  expect "$work/$case.helper" "$image: archive.ubuntu.com serves the .debs" \
    "^Get:[0-9]+ http://archive\.ubuntu\.com/ubuntu $deb"
  absent "$work/$case.helper" "$image: azure failed no fetch, so it was not apt that left it" \
    "^(Ign|Err):[0-9]+ http://azure\.archive\.ubuntu\.com/ubuntu $deb"
  absent "$work/$case.helper" "$image: nothing is written on a line apt was stopped in" "$glued"
  after_download "$work/$case.helper" >"$work/$case.helper-install"
  expect "$work/$case.helper-install" "$image: the install follows the download" '^Setting up jq '
  absent "$work/$case.helper-install" "$image: and fetches nothing" '^Get:'
  expect "$work/$case.helper" "$image: jq runs" '^jq-[0-9]'
  expect "$work/$case.helper" "$image: the mirror list ends as it began" '^mirror list: as it was$'
  stub_saw "$case" >"$work/$case.stub"
  expect "$work/$case.stub" "$image: archive.ubuntu.com is asked for the rest of the .deb azure began" \
    'host=archive\.ubuntu\.com range=bytes=[1-9][0-9]*- '
}

echo "--- slow mirror: azure up but slow, the helper's own mirror list (ubuntu:22.04)"
log="$work/slow-jammy.log"
started=$failures
# shellcheck disable=SC2016 # expands inside the container
rc="$(in_ubuntu ubuntu:22.04 "$log" "$fault_fn"'
  case=slow-jammy list=/etc/apt/ubuntu-mirrors.txt
  echo "--- healthy"
  fault none "$case"
  bash /h/apt-install.sh hello
  echo "exit $?"
  cp "$list" /tmp/list.before
  echo "--- control"
  fault slow-debs "$case-control"
  opts="-o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 -o DPkg::Lock::Timeout=120"
  rc=0
  timeout 45 apt-get $opts -y install jq || rc=$?
  # apt alone leaves its last line as it was when it was stopped.
  echo
  echo "exit $rc"
  # What the control fetched must not be a head start for the helper.
  apt-get clean'"$past_slow_azure" "${fallbacks_to_stub[@]}")"
[ "$rc" = 0 ] || fail "ubuntu:22.04: the slow-mirror case exited $rc"
section "$log" control >"$work/slow-control"
deb='jammy(-updates|-security)?/main amd64 (jq|libjq1|libonig5) '
expect "$work/slow-control" "control: apt alone runs into the time limit" '^exit 124$'
expect "$work/slow-control" "control: having asked azure" "^Get:[0-9]+ http://azure\.archive\.ubuntu\.com/ubuntu $deb"
absent "$work/slow-control" "control: with no failed fetch to act on" '^(Ign|Err):'
absent "$work/slow-control" "control: and no other mirror in its output" '^Get:[0-9]+ http://(archive|security)\.ubuntu\.com/'
stub_saw slow-jammy-control >"$work/slow-control.stub"
expect "$work/slow-control.stub" "control: the stub was asked for a .deb as azure" 'host=azure\.archive\.ubuntu\.com '
absent "$work/slow-control.stub" "control: and under no other name" 'host=(archive|security)\.ubuntu\.com '
slow_mirror_checks ubuntu:22.04 jammy slow-jammy "$log"
show_log_if_failed "$log" "$started"

echo "--- slow mirror: azure up but slow, a mirror list the image already had (ubuntu:24.04)"
log="$work/slow-noble.log"
started=$failures
# The sources get the shape of GitHub's hosted images, whose list is not the
# helper's to keep. Plain http for all three mirrors: a stock image has no
# ca-certificates for the https that the hosted list gives the last two.
# shellcheck disable=SC2016 # expands inside the container
rc="$(in_ubuntu ubuntu:24.04 "$log" "$fault_fn"'
  case=slow-noble list=/etc/apt/apt-mirrors.txt
  printf "%s\tpriority:%s\n" http://azure.archive.ubuntu.com/ubuntu/ 1 \
    http://archive.ubuntu.com/ubuntu/ 2 http://security.ubuntu.com/ubuntu/ 3 >"$list"
  sed -i -E "s#http://(archive|security)\.ubuntu\.com/ubuntu/?#mirror+file:$list#" \
    /etc/apt/sources.list.d/ubuntu.sources
  echo "--- healthy"
  fault none "$case"
  bash /h/apt-install.sh hello
  echo "exit $?"
  if [ ! -e /etc/apt/ubuntu-mirrors.txt ]; then echo "the helper wrote no list of its own"; fi
  cp "$list" /tmp/list.before'"$past_slow_azure" "${fallbacks_to_stub[@]}")"
[ "$rc" = 0 ] || fail "ubuntu:24.04: the slow-mirror case exited $rc"
expect "$log" "ubuntu:24.04: the image's list is the only one" '^the helper wrote no list of its own$'
slow_mirror_checks ubuntu:24.04 noble slow-noble "$log"
show_log_if_failed "$log" "$started"

echo "--- every mirror slow (ubuntu:22.04)"
log="$work/slow-everywhere.log"
started=$failures
# shellcheck disable=SC2016 # expands inside the container
rc="$(in_ubuntu ubuntu:22.04 "$log" "$fault_fn"'
  case=slow-everywhere list=/etc/apt/ubuntu-mirrors.txt
  fault none "$case"
  bash /h/apt-install.sh hello
  cp "$list" /tmp/list.before
  echo "--- helper"
  fault slow-debs-everywhere "$case"
  # A budget of three slices, one for each mirror. What the helper spends
  # between two slices comes out of the third, which it cuts to the time
  # left: so the third is the last, whatever the machine is busy with, and
  # still has most of its 30 s, in which apt gets as far as its .deb. A
  # budget of four slices would end in a fourth of whatever is left over, a
  # few seconds in which apt is stopped while it starts, or none.
  APT_INSTALL_SLICE_SECONDS=30 APT_INSTALL_BUDGET_SECONDS=90 bash /h/apt-install.sh jq
  echo "exit $?"
  if ! command -v jq >/dev/null; then echo "jq: not installed"; fi
  if cmp /tmp/list.before "$list"; then echo "mirror list: as it was"; fi
  echo "--- end"' "${fallbacks_to_stub[@]}")"
[ "$rc" = 0 ] || fail "the every-mirror-slow case exited $rc"
section "$log" helper >"$work/slow-everywhere.helper"
expect "$work/slow-everywhere.helper" "with every mirror slow the helper exits 124" '^exit 124$'
slice_of() { printf '%s %s in [0-9]+ s \\(%s/s\\)' "$1" "$size" "$size"; }
expect "$work/slow-everywhere.helper" "and says how much it fetched, how fast, and from which mirrors: one slice each" \
  "^::error::apt-install\.sh: the packages were not downloaded within 90 s, so none was installed \(exit 124\)\. Fetched $size in that time \($size/s\)\. Mirror in front, slice by slice: $(slice_of 'http://azure\.archive\.ubuntu\.com/ubuntu/'); $(slice_of 'http://archive\.ubuntu\.com/ubuntu/'); $(slice_of 'http://security\.ubuntu\.com/ubuntu/')\.\$"
absent "$work/slow-everywhere.helper" "nothing is written on a line apt was stopped in" "$glued"
expect "$work/slow-everywhere.helper" "nothing is installed" '^jq: not installed$'
expect "$work/slow-everywhere.helper" "the mirror list ends as it began" '^mirror list: as it was$'
stub_saw slow-everywhere >"$work/slow-everywhere.stub"
for host in azure.archive.ubuntu.com archive.ubuntu.com security.ubuntu.com; do
  expect "$work/slow-everywhere.stub" "$host was asked for a .deb" "host=${host//./\\.} "
done
for host in archive.ubuntu.com security.ubuntu.com; do
  expect "$work/slow-everywhere.stub" "$host was asked to go on where the mirror before it stopped" \
    "host=${host//./\\.} range=bytes=[1-9][0-9]*- "
done
show_log_if_failed "$log" "$started"

# The offline case runs under both images: their awk (mawk) builds differ, and
# the helper's entry filter and its mirror list reordering are awk.
# shellcheck disable=SC2016 # expands inside the container
rewrite_script='
set -euo pipefail
# The shim prints its arguments and keeps a copy of the source parts each
# subcommand is given, which the helper deletes when it exits.
cat >/usr/local/sbin/apt-get <<"S"
#!/bin/sh
echo "apt-get $*"
parts= sub= kind=
for a; do
  case "$a" in
    Dir::Etc::SourceParts=*) parts="${a#*=}" ;;
    update|install) sub="$a" kind="$a" ;;
    --download-only|--no-download|--print-uris) kind="${a#--}" ;;
  esac
done
if [ -n "$parts" ]; then rm -rf "/tmp/parts-$sub"; cp -r "$parts" "/tmp/parts-$sub"; fi
# The rest is for the runs under a slow apt-get, which make /tmp/slow. Each
# call says in what order it found the two mirror lists, on stderr because the
# helper reads the stdout of --print-uris, and keeps a copy of them.
[ -d /tmp/slow ] || exit 0
order() { awk "!/^[ \t]*(#|\$)/ { sub(/^https?:\/\//, \"\", \$1); sub(/\/.*/, \"\", \$1); printf \" %s\", \$1 }" "$1" 2>/dev/null; }
echo "slow: $kind: own list:$(order /etc/apt/ubuntu-mirrors.txt), image list:$(order /etc/apt/apt-mirrors.txt)" >&2
cp /etc/apt/ubuntu-mirrors.txt "/tmp/slow/own.$kind" 2>/dev/null
cp /etc/apt/apt-mirrors.txt "/tmp/slow/image.$kind" 2>/dev/null
# A call that outlasts its limit is stopped the way apt is when it is stopped
# while it starts: in the middle of a line, which it has begun and not ended.
outlast() { printf "Building dependency tree..."; exec sleep 20; }
case "$kind" in
  # One .deb of 100000 bytes is still to fetch. Saying so takes as long as
  # /tmp/slow/print-uris says, if it is there.
  print-uris)
    if [ -f /tmp/slow/print-uris ]; then sleep "$(cat /tmp/slow/print-uris)"; fi
    printf "\047mirror+file:/x/pool/a.deb\047 a.deb 100000 MD5Sum:0\n"
    exit 0
    ;;
  # The install is done at once, unless /tmp/slow/no-download is there: then
  # it outlasts any limit used here.
  no-download)
    [ -f /tmp/slow/no-download ] || exit 0
    outlast
    ;;
esac
# An update or a download: as many of each as /tmp/slow says outlast any limit
# used here. A download first fetches the bytes it is told to. The one call
# that finds the count /tmp/slow/anew names has apt begin the .deb anew
# instead, and is stopped with nothing of it.
calls="$(cat "/tmp/slow/$kind")"
[ "$calls" -gt 0 ] || exit 0
echo "$((calls - 1))" >"/tmp/slow/$kind"
if [ "$kind" = download-only ]; then
  if [ "$calls" = "$(cat /tmp/slow/anew 2>/dev/null)" ]; then
    : >/var/cache/apt/archives/partial/a.deb
  else
    head -c "$(cat /tmp/slow/fetch)" /dev/zero >>/var/cache/apt/archives/partial/a.deb
  fi
fi
outlast
S
chmod +x /usr/local/sbin/apt-get
# The entries the update was given, and whether the install got the same.
show_parts() {
  echo "--- $1"
  for f in /tmp/parts-update/*; do echo "-- ${f##*/}"; cat "$f"; done
  echo "--- end"
  if diff -r /tmp/parts-update /tmp/parts-install; then echo "$1: the install reads the same"; fi
}

# The list on the hosted images, whose fallbacks are https.
hosted_list() {
  printf "%s\tpriority:%s\n" http://azure.archive.ubuntu.com/ubuntu/ 1 \
    https://archive.ubuntu.com/ubuntu/ 2 https://security.ubuntu.com/ubuntu/ 3 \
    >/etc/apt/apt-mirrors.txt
}

fixture() {
  rm -rf /tmp/parts-* /etc/apt/*-mirrors.txt /etc/apt/sources.list.d/*
  hosted_list
  # One Ubuntu mirror and one other host: not an Ubuntu list.
  printf "%s\n" http://archive.ubuntu.com/ubuntu/ https://mirror.example.com/ubuntu/ \
    >/etc/apt/vendor-mirrors.txt
  cat >/etc/apt/sources.list <<"S"
# deb http://archive.ubuntu.com/ubuntu jammy main
deb http://archive.ubuntu.com/ubuntu/ jammy main restricted
deb-src http://archive.ubuntu.com/ubuntu jammy main
deb [arch=amd64 signed-by=/usr/share/keyrings/ubuntu-archive-keyring.gpg] http://us.archive.ubuntu.com/ubuntu jammy-updates main
deb [arch=amd64 ]http://archive.ubuntu.com/ubuntu jammy-proposed main
deb http://us-east-1.ec2.archive.ubuntu.com/ubuntu/ jammy-backports main
deb https://security.ubuntu.com/ubuntu jammy-security main
	deb	http://azure.archive.ubuntu.com/ubuntu	jammy	universe
deb mirror+file:/etc/apt/apt-mirrors.txt jammy main
deb mirror+file:/etc/apt/vendor-mirrors.txt jammy main
deb http://ports.ubuntu.com/ubuntu-ports jammy main
deb http://us.ports.ubuntu.com/ubuntu-ports jammy main
deb http://archive.ubuntu.com/ubuntu-ports jammy main
deb http://old-releases.ubuntu.com/ubuntu hirsute main
deb http://ppa.launchpadcontent.net/deadsnakes/ppa/ubuntu jammy main
deb [arch=amd64] https://download.docker.com/linux/ubuntu jammy stable
S
  cat >/etc/apt/sources.list.d/ubuntu.sources <<"S"
## URIs: http://archive.ubuntu.com/ubuntu/
Types: deb
URIs: http://archive.ubuntu.com/ubuntu/
Suites: noble noble-updates
Components: main

Types: deb
uris:http://security.ubuntu.com/ubuntu/ http://azure.archive.ubuntu.com/ubuntu/
 http://archive.ubuntu.com/ubuntu
Suites: noble-security
Components: main

Types: deb
URIs: http://archive.ubuntu.com/ubuntu/ https://download.docker.com/linux/ubuntu
Suites: noble
Components: stable

Types: deb
URIs: http://archive.ubuntu.com/ubuntu/
 https://download.docker.com/linux/ubuntu
Suites: noble
Components: stable

Types: deb
URIs: http://archive.ubuntu.com/ubuntu/
# apt skips a comment line and the field goes on
 https://download.docker.com/linux/ubuntu
Suites: noble
Components: stable

Types: deb
URIs: mirror+file:/etc/apt/vendor-mirrors.txt
Suites: noble
Components: main

Types: deb
URIs : http://archive.ubuntu.com/ubuntu/
Suites: noble-backports
Components: main
S
  # Disabled stanzas are no entries: not read, and not named as left out.
  cat >/etc/apt/sources.list.d/disabled.sources <<"S"
Types: deb
URIs: https://download.docker.com/linux/ubuntu
Suites: noble
Components: stable
Enabled: no

Types: deb
URIs: http://archive.ubuntu.com/ubuntu/
Suites: noble-proposed
Components: main
enabled: False
S
  # Fields but no URIs: left out, and named.
  printf "%s\n" "Types: deb" "Suites: noble" "Components: main" >/etc/apt/sources.list.d/nouris.sources
  cat >/etc/apt/sources.list.d/docker.sources <<"S"
Types: deb
URIs: https://download.docker.com/linux/ubuntu
Suites: noble
Components: stable
S
  printf "deb https://packages.microsoft.com/ubuntu/24.04/prod noble main\n" \
    >/etc/apt/sources.list.d/microsoft-prod.list
  printf "deb http://archive.ubuntu.com/ubuntu noble universe\n" \
    >/etc/apt/sources.list.d/extra.list
}

# The shape of the GitHub-hosted images: the sources already go through their
# own mirror list.
hosted_fixture() {
  rm -rf /tmp/parts-* /etc/apt/*-mirrors.txt /etc/apt/sources.list.d/*
  hosted_list
  printf "# Ubuntu sources have moved to ubuntu.sources\n" >/etc/apt/sources.list
  cat >/etc/apt/sources.list.d/ubuntu.sources <<"S"
Types: deb
URIs: mirror+file:/etc/apt/apt-mirrors.txt
Suites: noble noble-updates noble-backports noble-security
Components: main universe restricted multiverse
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
S
  printf "deb https://packages.microsoft.com/ubuntu/24.04/prod noble main\n" \
    >/etc/apt/sources.list.d/microsoft-prod.list
}

# Change time in ns: any write moves it, even one that keeps the inode, the
# size and the content, as a cp onto the same file does.
snap() { find /etc/apt -type f -exec stat -c "%n %i %.9Z %s" {} + | sort; find /etc/apt -type f -exec sha256sum {} + | sort; }

fixture
bash /h/apt-install.sh hello
echo "--- sources.list"; cat /etc/apt/sources.list
echo "--- ubuntu.sources"; cat /etc/apt/sources.list.d/ubuntu.sources
echo "--- docker.sources"; cat /etc/apt/sources.list.d/docker.sources
echo "--- microsoft-prod.list"; cat /etc/apt/sources.list.d/microsoft-prod.list
echo "--- extra.list"; cat /etc/apt/sources.list.d/extra.list
echo "--- ubuntu-mirrors.txt"; cat /etc/apt/ubuntu-mirrors.txt
show_parts "update reads"
snap >/tmp/run1
bash /h/apt-install.sh hello
snap >/tmp/run2
if diff /tmp/run1 /tmp/run2; then echo "second run: no change"; fi

# The sources already name the list, so a re-run has to put back a list that
# went missing or out of date.
cp /etc/apt/ubuntu-mirrors.txt /tmp/list.want
rm /etc/apt/ubuntu-mirrors.txt
bash /h/apt-install.sh hello
if cmp /tmp/list.want /etc/apt/ubuntu-mirrors.txt; then echo "deleted list: restored"; fi
echo stale >/etc/apt/ubuntu-mirrors.txt
bash /h/apt-install.sh hello
if cmp /tmp/list.want /etc/apt/ubuntu-mirrors.txt; then echo "stale list: restored"; fi

fixture
snap >/tmp/before
RUNNER_ENVIRONMENT=self-hosted bash /h/apt-install.sh hello | sed "s/^/self-hosted: /"
snap >/tmp/after
if diff /tmp/before /tmp/after; then echo "self-hosted: no change"; fi

hosted_fixture
snap >/tmp/before
bash /h/apt-install.sh hello | sed "s/^/hosted: /"
snap >/tmp/after
if diff /tmp/before /tmp/after; then echo "hosted image: no change"; fi
show_parts "hosted: update reads"

# CRLF line ends, which apt reads: in a deb822 file of two stanzas, split by a
# line of two CRs (empty to apt), and in a mirror list.
rm -rf /tmp/parts-* /etc/apt/*-mirrors.txt /etc/apt/sources.list.d/*
: >/etc/apt/sources.list
printf "%s\r\n" http://archive.ubuntu.com/ubuntu/ >/etc/apt/crlf-mirrors.txt
ubuntu_stanza=("Types: deb" "URIs: mirror+file:/etc/apt/crlf-mirrors.txt" "Suites: noble" "Components: main")
{ printf "%s\r\n" "${ubuntu_stanza[@]}"; printf "\r\r\n"
  printf "%s\r\n" "Types: deb" "URIs: https://download.docker.com/linux/ubuntu" "Suites: noble" "Components: stable"
} >/etc/apt/sources.list.d/crlf.sources
bash /h/apt-install.sh hello | sed "s/^/crlf: /"
printf "%s\n" "${ubuntu_stanza[@]}" >/tmp/crlf.want
if cmp /tmp/crlf.want /tmp/parts-update/000-crlf.sources; then echo "crlf: kept the Ubuntu stanza only"; fi

rm -rf /tmp/parts-* /etc/apt/*-mirrors.txt /etc/apt/sources.list.d/*
printf "deb https://download.docker.com/linux/ubuntu noble stable\n" >/etc/apt/sources.list
rc=0
bash /h/apt-install.sh hello >/tmp/out 2>&1 || rc=$?
sed "s/^/no Ubuntu source: /" /tmp/out
echo "no Ubuntu source: exit $rc"
bash /h/apt-install.sh --all-sources hello | sed "s/^/all sources: /"

# From here on apt-get is slow.
# slow <update calls> <download calls> [bytes each slow download call fetches]
slow() {
  rm -rf /tmp/slow /var/cache/apt/archives/partial/a.deb
  mkdir -p /tmp/slow /var/cache/apt/archives/partial
  echo "$1" >/tmp/slow/update
  echo "$2" >/tmp/slow/download-only
  echo "${3:-0}" >/tmp/slow/fetch
}
# slow_run <label> <slice> <budget> <helper argument ...>: the output of the
# helper under that label, then its exit code.
slow_run() {
  local label="$1" rc=0
  APT_INSTALL_SLICE_SECONDS="$2" APT_INSTALL_BUDGET_SECONDS="$3" bash /h/apt-install.sh "${@:4}" >/tmp/out 2>&1 || rc=$?
  sed "s/^/$label: /" /tmp/out
  echo "$label: exit $rc"
}
# What is in the files, whenever they were written.
sums() { find /etc/apt -type f -exec sha256sum {} + | sort; }

# Four downloads run out of their slice. Each fetched 3000 of 100000 bytes,
# which the 30 s budget cannot make enough, so every one of the four moves on
# to the next mirror: past the last of the three, and round to the first.
fixture
bash /h/apt-install.sh hello >/dev/null
sums >/tmp/before
slow 0 4 3000
slow_run "moves on" 1 30 hello
# `|| true`: a helper that never got as far as the install leaves no such
# copy, and the cases below have to run all the same.
echo "--- moved on: own list"; cat /tmp/slow/own.no-download || true
echo "--- moved on: image list"; cat /tmp/slow/image.no-download || true
echo "--- end"
sums >/tmp/after
if diff /tmp/before /tmp/after; then echo "moves on: lists put back"; fi

# One download runs out of its slice having fetched 60000 of 100000 bytes: at
# that rate the rest arrives in time, so the mirror stays where it is.
snap >/tmp/before
slow 0 1 60000
slow_run "on course" 1 30 hello
snap >/tmp/after
if diff /tmp/before /tmp/after; then echo "on course: no change"; fi

# Three downloads run out of their slice. The first fetches 40000 bytes. In
# the second apt begins the .deb anew and has nothing of it when it is
# stopped: the cache is smaller than it was, and that mirror is left. The
# third fetches 40000 bytes again, and all of them have to count for the
# mirror that served them: it is on course, and stays.
slow 0 3 40000
echo 2 >/tmp/slow/anew
slow_run "anew" 1 30 hello

# No download ever finishes. The 8 s budget is several slices more than the
# three that put each mirror in front once. With --all-sources the lists are
# looked for in the source files themselves, where one is not all Ubuntu.
sums >/tmp/before
vendor="$(stat -c %.9Z /etc/apt/vendor-mirrors.txt)"
slow 0 99 2000
slow_run "all slow" 1 8 --all-sources hello
sums >/tmp/after
if diff /tmp/before /tmp/after; then echo "all slow: lists put back"; fi
if [ "$vendor" = "$(stat -c %.9Z /etc/apt/vendor-mirrors.txt)" ]; then echo "all slow: the list that is not all Ubuntu was not written"; fi

# The budget runs out between two slices: after the one download that was
# stopped, apt takes 2 of the 3 s to say what is still to fetch. No further
# slice may start, least of all one of 0 s, which to timeout(1) is no limit,
# and no mirror is put in front for a slice that cannot start.
slow 0 99 2000
echo 2 >/tmp/slow/print-uris
slow_run "out of time" 1 3 hello

# The update and the download are done at once, and the install outlasts the
# budget, which it gets in one piece.
slow 0 0
touch /tmp/slow/no-download
slow_run "slow install" 1 3 hello

# The update never ends. It has the budget in one piece although there are
# lists to reorder, and they are left as they are: only a download leaves a
# slow mirror.
snap >/tmp/before
slow 99 0
slow_run "slow update" 1 3 hello
snap >/tmp/after
if diff /tmp/before /tmp/after; then echo "slow update: no change"; fi

# A self-hosted runner has the same lists, and the helper must leave them be.
fixture
snap >/tmp/before
slow 0 99 2000
RUNNER_ENVIRONMENT=self-hosted slow_run "self-hosted, slow" 1 3 hello
snap >/tmp/after
if diff /tmp/before /tmp/after; then echo "self-hosted, slow: no change"; fi

# Sources without a mirror list, as on arm64: nothing to put in front.
rm -rf /etc/apt/*-mirrors.txt /etc/apt/sources.list.d/*
printf "deb http://ports.ubuntu.com/ubuntu-ports jammy main\n" >/etc/apt/sources.list
slow 0 99 2000
slow_run "no list" 1 3 hello
'

L='mirror+file:/etc/apt/ubuntu-mirrors.txt'
cat >"$work/rewrite.want" <<EOF
--- sources.list
# deb http://archive.ubuntu.com/ubuntu jammy main
deb $L jammy main restricted
deb-src $L jammy main
deb [arch=amd64 signed-by=/usr/share/keyrings/ubuntu-archive-keyring.gpg] $L jammy-updates main
deb [arch=amd64 ]$L jammy-proposed main
deb $L jammy-backports main
deb $L jammy-security main
	deb	$L	jammy	universe
deb mirror+file:/etc/apt/apt-mirrors.txt jammy main
deb mirror+file:/etc/apt/vendor-mirrors.txt jammy main
deb http://ports.ubuntu.com/ubuntu-ports jammy main
deb http://us.ports.ubuntu.com/ubuntu-ports jammy main
deb http://archive.ubuntu.com/ubuntu-ports jammy main
deb http://old-releases.ubuntu.com/ubuntu hirsute main
deb http://ppa.launchpadcontent.net/deadsnakes/ppa/ubuntu jammy main
deb [arch=amd64] https://download.docker.com/linux/ubuntu jammy stable
--- ubuntu.sources
## URIs: http://archive.ubuntu.com/ubuntu/
Types: deb
URIs: $L
Suites: noble noble-updates
Components: main

Types: deb
uris:$L $L
 $L
Suites: noble-security
Components: main

Types: deb
URIs: $L https://download.docker.com/linux/ubuntu
Suites: noble
Components: stable

Types: deb
URIs: $L
 https://download.docker.com/linux/ubuntu
Suites: noble
Components: stable

Types: deb
URIs: $L
# apt skips a comment line and the field goes on
 https://download.docker.com/linux/ubuntu
Suites: noble
Components: stable

Types: deb
URIs: mirror+file:/etc/apt/vendor-mirrors.txt
Suites: noble
Components: main

Types: deb
URIs : $L
Suites: noble-backports
Components: main
--- docker.sources
Types: deb
URIs: https://download.docker.com/linux/ubuntu
Suites: noble
Components: stable
--- microsoft-prod.list
deb https://packages.microsoft.com/ubuntu/24.04/prod noble main
--- extra.list
deb $L noble universe
--- ubuntu-mirrors.txt
# Written by keploy's .github/workflows/test_workflow_scripts/apt-install.sh.
# apt tries the mirror with the lowest priority: number first and moves to the
# next when a fetch fails. See that script for why azure is first.
http://azure.archive.ubuntu.com/ubuntu/	priority:1
http://archive.ubuntu.com/ubuntu/	priority:2
http://security.ubuntu.com/ubuntu/	priority:3
--- update reads
-- 000-sources.list
deb $L jammy main restricted
deb-src $L jammy main
deb [arch=amd64 signed-by=/usr/share/keyrings/ubuntu-archive-keyring.gpg] $L jammy-updates main
deb [arch=amd64 ]$L jammy-proposed main
deb $L jammy-backports main
deb $L jammy-security main
	deb	$L	jammy	universe
deb mirror+file:/etc/apt/apt-mirrors.txt jammy main
deb http://ports.ubuntu.com/ubuntu-ports jammy main
deb http://us.ports.ubuntu.com/ubuntu-ports jammy main
deb http://old-releases.ubuntu.com/ubuntu hirsute main
-- 001-extra.list
deb $L noble universe
-- 002-ubuntu.sources
## URIs: http://archive.ubuntu.com/ubuntu/
Types: deb
URIs: $L
Suites: noble noble-updates
Components: main

Types: deb
uris:$L $L
 $L
Suites: noble-security
Components: main

Types: deb
URIs : $L
Suites: noble-backports
Components: main
--- end
EOF
cat >"$work/hosted.want" <<EOF
--- hosted: update reads
-- 000-ubuntu.sources
Types: deb
URIs: mirror+file:/etc/apt/apt-mirrors.txt
Suites: noble noble-updates noble-backports noble-security
Components: main universe restricted multiverse
Signed-By: /usr/share/keyrings/ubuntu-archive-keyring.gpg
--- end
EOF
bounded='apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 -o DPkg::Lock::Timeout=120'
ubuntu_only='-o Dir::Etc::SourceList=/dev/null -o Dir::Etc::SourceParts=/tmp/[^ ]+ -o APT::Get::List-Cleanup=0'
left_out='apt-install: apt reads only the Ubuntu archive entries; pass --all-sources to also read the rest of:'

# The order the slow shim has to find the mirror lists in, call by call. Both
# lists hold the same three mirrors, so both go through the same orders.
azure_first='azure.archive.ubuntu.com archive.ubuntu.com security.ubuntu.com'
archive_first='archive.ubuntu.com security.ubuntu.com azure.archive.ubuntu.com'
security_first='security.ubuntu.com azure.archive.ubuntu.com archive.ubuntu.com'
# found <label> <call> <order>
found() { echo "$1: slow: $2: own list: $3, image list: $3"; }
{
  found "moves on" update "$azure_first"
  found "moves on" download-only "$azure_first"
  found "moves on" print-uris "$azure_first"
  found "moves on" download-only "$archive_first"
  found "moves on" print-uris "$archive_first"
  found "moves on" download-only "$security_first"
  found "moves on" print-uris "$security_first"
  found "moves on" download-only "$azure_first"
  found "moves on" print-uris "$azure_first"
  found "moves on" download-only "$archive_first"
  found "moves on" no-download "$archive_first"
} >"$work/moves-on.want"
for call in update download-only print-uris download-only no-download; do
  found "on course" "$call" "$azure_first"
done >"$work/on-course.want"
# The two lists as the install of that first run found them: archive.ubuntu.com
# in front, the comment lines on top, the priorities counted anew, and the
# image's mirrors with the scheme each had.
cat >"$work/moved-on.want" <<EOF
--- moved on: own list
# Written by keploy's .github/workflows/test_workflow_scripts/apt-install.sh.
# apt tries the mirror with the lowest priority: number first and moves to the
# next when a fetch fails. See that script for why azure is first.
http://archive.ubuntu.com/ubuntu/	priority:1
http://security.ubuntu.com/ubuntu/	priority:2
http://azure.archive.ubuntu.com/ubuntu/	priority:3
--- moved on: image list
https://archive.ubuntu.com/ubuntu/	priority:1
https://security.ubuntu.com/ubuntu/	priority:2
http://azure.archive.ubuntu.com/ubuntu/	priority:3
--- end
EOF
# The mirrors as the helper names them: by the first list it found, which is
# the image's, with https for the last two.
azure='http://azure\.archive\.ubuntu\.com/ubuntu/'
archive='https://archive\.ubuntu\.com/ubuntu/'
security='https://security\.ubuntu\.com/ubuntu/'

for image in ubuntu:22.04 ubuntu:24.04; do
  log="$work/rewrite-${image#ubuntu:}.log"
  started=$failures
  echo "--- rewrite ($image, no network, apt-get shimmed)"
  docker run --rm --network none -v "$helper:/h/apt-install.sh:ro" "$image" \
    bash -c "exec 2>&1; $rewrite_script" >"$log" || fail "$image: rewrite case exited $?"
  if diff -u "$work/rewrite.want" <(sed -n '/^--- sources.list$/,/^--- end$/p' "$log"); then
    echo "ok: $image: exactly the archive and security URIs are rewritten, and apt reads exactly the Ubuntu entries"
  else
    fail "$image: rewritten sources or the entries apt reads differ from the expected (diff above: - want, + got)"
  fi
  if diff -u "$work/hosted.want" <(sed -n '/^--- hosted: update reads$/,/^--- end$/p' "$log"); then
    echo "ok: $image: on a hosted image apt reads its Ubuntu entries and not packages.microsoft.com"
  else
    fail "$image: the entries apt reads on a hosted image differ from the expected (diff above: - want, + got)"
  fi
  expect "$log" "$image: the update gets the bounded options and only the Ubuntu entries" \
    "^$bounded $ubuntu_only -y update\$"
  expect "$log" "$image: the download gets the bounded options and only the Ubuntu entries" \
    "^$bounded $ubuntu_only -y install --download-only hello\$"
  expect "$log" "$image: and so does the install, which may not download" \
    "^$bounded $ubuntu_only -y install --no-download hello\$"
  expect "$log" "$image: the install reads the entries the update read" '^update reads: the install reads the same$'
  expect "$log" "$image: on a hosted image too, the install reads the entries the update read" \
    '^hosted: update reads: the install reads the same$'
  expect "$log" "$image: it names every source file it left entries out of" \
    "^$left_out /etc/apt/sources.list /etc/apt/sources.list.d/microsoft-prod.list /etc/apt/sources.list.d/docker.sources /etc/apt/sources.list.d/nouris.sources /etc/apt/sources.list.d/ubuntu.sources\$"
  expect "$log" "$image: on a hosted image, packages.microsoft.com's file" \
    "^hosted: $left_out /etc/apt/sources.list.d/microsoft-prod.list\$"
  expect "$log" "$image: --all-sources: the update reads every source" "^all sources: $bounded -y update\$"
  expect "$log" "$image: --all-sources: the download reads every source" \
    "^all sources: $bounded -y install --download-only hello\$"
  expect "$log" "$image: --all-sources: the install reads every source" \
    "^all sources: $bounded -y install --no-download hello\$"
  expect "$log" "$image: a self-hosted runner's update reads every source" "^self-hosted: $bounded -y update\$"
  expect "$log" "$image: a self-hosted runner's download reads every source" \
    "^self-hosted: $bounded -y install --download-only hello\$"
  expect "$log" "$image: a self-hosted runner's install reads every source" \
    "^self-hosted: $bounded -y install --no-download hello\$"
  expect "$log" "$image: with no Ubuntu entry, the default run exits 1" '^no Ubuntu source: exit 1$'
  expect "$log" "$image: and says to pass --all-sources" '^no Ubuntu source: apt-install: .*pass --all-sources'
  if grep -q '^no Ubuntu source: apt-get ' "$log"; then
    fail "$image: with no Ubuntu entry, the default run still ran apt-get"
  fi
  expect "$log" "$image: CRLF: the Ubuntu stanza is kept, the other left out" '^crlf: kept the Ubuntu stanza only$'
  expect "$log" "$image: CRLF: the file is named as left out" "^crlf: $left_out /etc/apt/sources.list.d/crlf.sources\$"
  expect "$log" "$image: a second run changes no file" '^second run: no change$'
  expect "$log" "$image: a re-run puts back a deleted mirror list" '^deleted list: restored$'
  expect "$log" "$image: a re-run puts back an outdated mirror list" '^stale list: restored$'
  expect "$log" "$image: a self-hosted runner's sources are left alone" '^self-hosted: no change$'
  expect "$log" "$image: a hosted image's own mirror list is left alone, and no list is added" '^hosted image: no change$'

  # The slow shim is stopped in the middle of a line every time: it has
  # written "Building dependency tree..." and no line end. The first check is
  # the control, that it was; the second is the rule, for every message of
  # the helper and every later apt in every run below. The messages are also
  # looked for one by one further down, each from the start of its line.
  expect "$log" "$image: control: the slow shim is stopped in the middle of a line" \
    '^moves on: Building dependency tree\.\.\.$'
  absent "$log" "$image: nothing is written on a line apt was stopped in: what comes next starts its own" "$glued"

  if diff -u "$work/moves-on.want" <(grep '^moves on: slow: ' "$log"); then
    echo "ok: $image: every download slice that runs out puts the next mirror in front, in both lists, and the update none"
  else
    fail "$image: the order of the mirror lists, call by call, differs from the expected (diff above: - want, + got)"
  fi
  if diff -u "$work/moved-on.want" <(sed -n '/^--- moved on: own list$/,/^--- end$/p' "$log"); then
    echo "ok: $image: a reordered list keeps its comments and schemes, with the priorities counted anew"
  else
    fail "$image: the reordered mirror lists differ from the expected (diff above: - want, + got)"
  fi
  expect "$log" "$image: the helper says why the download moved on" \
    "^moves on: apt-install: [0-9]+ s with $azure in front fetched 3 kB \($size/s\), too slow for the 97 kB still to fetch in the [0-9]+ s left\. Putting $archive in front; apt keeps what it has fetched\.\$"
  expect "$log" "$image: and after the last mirror the first is in front again" \
    "^moves on: apt-install: [0-9]+ s with $security in front fetched 3 kB \($size/s\), too slow for the 91 kB still to fetch in the [0-9]+ s left\. Putting $azure in front; apt keeps what it has fetched\.\$"
  expect "$log" "$image: the run still installs" '^moves on: exit 0$'
  expect "$log" "$image: and puts both lists back" '^moves on: lists put back$'

  if diff -u "$work/on-course.want" <(grep '^on course: slow: ' "$log"); then
    echo "ok: $image: a mirror that is on course stays in front"
  else
    fail "$image: the order of the mirror lists under a mirror that is on course differs from the expected (diff above: - want, + got)"
  fi
  expect "$log" "$image: the helper says why it stays" \
    "^on course: apt-install: [0-9]+ s with $azure in front fetched 60 kB \($size/s\)\. At that rate the 40 kB still to fetch arrive within the [0-9]+ s left, so it stays in front\.\$"
  expect "$log" "$image: that run installs too" '^on course: exit 0$'
  expect "$log" "$image: and writes no file under /etc/apt" '^on course: no change$'

  expect "$log" "$image: control: apt began its .deb anew, and that slice fetched nothing" \
    "^anew: apt-install: [0-9]+ s with $azure in front fetched 0 B \(0 B/s\), too slow for the 100 kB still to fetch in the [0-9]+ s left\. Putting $archive in front; apt keeps what it has fetched\.\$"
  expect "$log" "$image: the slice after that counts all it fetched, and its mirror stays in front" \
    "^anew: apt-install: [0-9]+ s with $archive in front fetched 40 kB \($size/s\)\. At that rate the 60 kB still to fetch arrive within the [0-9]+ s left, so it stays in front\.\$"
  expect "$log" "$image: that run installs as well" '^anew: exit 0$'

  expect "$log" "$image: with every mirror slow the helper exits 124" '^all slow: exit 124$'
  for order in "$azure_first" "$archive_first" "$security_first"; do
    expect "$log" "$image: after a download with ${order%% *} in front" \
      "^all slow: slow: download-only: own list: $order, image list: $order\$"
  done
  expect "$log" "$image: it says how much was fetched, how fast, and from which mirrors" \
    "^all slow: ::error::apt-install\.sh: the packages were not downloaded within 8 s, so none was installed \(exit 124\)\. Fetched $size in that time \($size/s\)\. Mirror in front, slice by slice: $azure 2 kB in [0-9]+ s \($size/s\); $archive 2 kB in [0-9]+ s \($size/s\); $security 2 kB in [0-9]+ s \($size/s\)[;.]"
  absent "$log" "$image: it does not go on to install" '^all slow: slow: no-download:'
  expect "$log" "$image: it puts both lists back" '^all slow: lists put back$'
  expect "$log" "$image: a mirror list that is not all Ubuntu is never written" \
    '^all slow: the list that is not all Ubuntu was not written$'

  if [ "$(grep -c '^out of time: slow: download-only:' "$log")" = 1 ]; then
    echo "ok: $image: no slice starts once the budget is spent between two of them"
  else
    fail "$image: the download did not run exactly once in a budget that ran out right after it"
  fi
  expect "$log" "$image: that run exits 124" '^out of time: exit 124$'
  expect "$log" "$image: with the one slice it had in its ::error:: line" \
    "^out of time: ::error::apt-install\.sh: the packages were not downloaded within 3 s, so none was installed \(exit 124\)\. Fetched 2 kB in that time \($size/s\)\. Mirror in front, slice by slice: $azure 2 kB in [0-9]+ s \($size/s\)\.\$"
  absent "$log" "$image: and no install" '^out of time: slow: no-download:'
  absent "$log" "$image: and no mirror put in front for a slice that could not start" '^out of time: apt-install: .* Putting '

  expect "$log" "$image: an install that outlasts the budget exits 124" '^slow install: exit 124$'
  expect "$log" "$image: and the helper says the packages were there" \
    '^slow install: ::error::apt-install\.sh: the packages were downloaded, but installing them was not done within 3 s \(exit 124\)\.$'

  if [ "$(grep -c '^slow update: slow: update:' "$log")" = 1 ]; then
    echo "ok: $image: the update gets its budget in one piece, with lists it could reorder"
  else
    fail "$image: the update did not run exactly once"
  fi
  expect "$log" "$image: an update that outlasts the budget exits 124" '^slow update: exit 124$'
  expect "$log" "$image: and the helper says so, and that it put no other mirror in front" \
    '^slow update: ::error::apt-install\.sh: apt-get update was not done within 3 s \(exit 124\)\. No other mirror was put in front for it: the script does that while it downloads packages, not for the package lists\.$'
  absent "$log" "$image: no download follows" '^slow update: slow: download-only:'
  expect "$log" "$image: and no file under /etc/apt was written" '^slow update: no change$'

  for label in 'self-hosted, slow' 'no list'; do
    if [ "$(grep -c "^$label: slow: download-only:" "$log")" = 1 ]; then
      echo "ok: $image: $label: the download gets its budget in one piece"
    else
      fail "$image: $label: the download did not run exactly once"
    fi
    expect "$log" "$image: $label: exit 124 when the budget is spent" "^$label: exit 124\$"
    absent "$log" "$image: $label: and no install" "^$label: slow: no-download:"
  done
  expect "$log" "$image: a self-hosted runner's mirror lists are not reordered" '^self-hosted, slow: no change$'
  expect "$log" "$image: and the helper says why it tried no other mirror" \
    "^self-hosted, slow: ::error::apt-install\.sh: the packages were not downloaded within 3 s, so none was installed \(exit 124\)\. Fetched 2 kB in that time \($size/s\)\. This is a self-hosted runner, whose mirror lists the script does not reorder, so no other mirror was tried\.\$"
  expect "$log" "$image: without a list the helper says there was no other mirror" \
    "^no list: ::error::apt-install\.sh: the packages were not downloaded within 3 s, so none was installed \(exit 124\)\. Fetched 2 kB in that time \($size/s\)\. apt reads no mirror list with a second Ubuntu mirror here, so there was no other mirror to try\.\$"
  show_log_if_failed "$log" "$started"
done

if [ "$failures" != 0 ]; then
  echo "$failures check(s) failed"
  exit 1
fi
echo "all apt-install.sh checks passed"
