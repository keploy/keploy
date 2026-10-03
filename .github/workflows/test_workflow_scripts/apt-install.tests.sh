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

# show_log_if_failed <log> <failure count when the case started>
show_log_if_failed() {
  [ "$failures" = "$2" ] && return 0
  echo "::group::$1"
  cat "$1"
  echo "::endgroup::"
}

cat >"$work/stub.py" <<'EOF'
import http.server, os, socketserver, sys, urllib.error, urllib.request

UPSTREAM = "http://azure.archive.ubuntu.com"
REPO = "/repo"

class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt, *args):
        sys.stdout.write("stub: " + (fmt % args) + "\n")
        sys.stdout.flush()

    def reply(self, code, reason, body, headers=()):
        self.send_response(code, reason)
        for name, value in headers:
            self.send_header(name, value)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

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
        if self.headers.get("Host", "").split(":")[0] == "thirdparty.test":
            return self.third_party()
        if self.path.endswith(".deb"):
            return self.reply(502, "Proxy Error", b"502 Proxy Error\n")
        try:
            with urllib.request.urlopen(UPSTREAM + self.path, timeout=60) as r:
                keep = [(k, v) for k, v in r.getheaders() if k.lower() == "last-modified"]
                return self.reply(r.status, r.reason, r.read(), keep)
        except urllib.error.HTTPError as e:
            return self.reply(e.code, e.reason, e.read() or b"")

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

# in_ubuntu <image> <log> <script>: the script runs as root, with the helper at
# /h/apt-install.sh and azure.archive.ubuntu.com and thirdparty.test resolving
# to the stub. A healthy case takes well under a minute; the 300 s cap makes a
# hung one fail with its log (exit 124) instead of running into the job's
# timeout silently. stderr joins stdout inside the container: docker keeps the
# two as separate streams and does not keep their order, so an apt error could
# otherwise land after the next section's header.
in_ubuntu() {
  local rc=0
  docker run --rm --network "$net" --add-host "azure.archive.ubuntu.com:$stub_ip" \
    --add-host "thirdparty.test:$stub_ip" -v "$helper:/h/apt-install.sh:ro" "$1" \
    timeout 300 bash -c "exec 2>&1; $3" >"$2" || rc=$?
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

# The offline case runs under both images: their awk (mawk) builds differ, and
# the helper's entry filter is awk.
# shellcheck disable=SC2016 # expands inside the container
rewrite_script='
set -euo pipefail
# The shim prints its arguments and keeps a copy of the source parts each
# subcommand is given, which the helper deletes when it exits.
cat >/usr/local/sbin/apt-get <<"S"
#!/bin/sh
echo "apt-get $*"
parts= sub=
for a; do
  case "$a" in
    Dir::Etc::SourceParts=*) parts="${a#*=}" ;;
    update|install) sub="$a" ;;
  esac
done
if [ -n "$parts" ]; then rm -rf "/tmp/parts-$sub"; cp -r "$parts" "/tmp/parts-$sub"; fi
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
  expect "$log" "$image: the install gets the bounded options and only the Ubuntu entries" \
    "^$bounded $ubuntu_only -y install hello\$"
  expect "$log" "$image: the install reads the entries the update read" '^update reads: the install reads the same$'
  expect "$log" "$image: on a hosted image too, the install reads the entries the update read" \
    '^hosted: update reads: the install reads the same$'
  expect "$log" "$image: it names every source file it left entries out of" \
    "^$left_out /etc/apt/sources.list /etc/apt/sources.list.d/microsoft-prod.list /etc/apt/sources.list.d/docker.sources /etc/apt/sources.list.d/nouris.sources /etc/apt/sources.list.d/ubuntu.sources\$"
  expect "$log" "$image: on a hosted image, packages.microsoft.com's file" \
    "^hosted: $left_out /etc/apt/sources.list.d/microsoft-prod.list\$"
  expect "$log" "$image: --all-sources: the update reads every source" "^all sources: $bounded -y update\$"
  expect "$log" "$image: --all-sources: the install reads every source" "^all sources: $bounded -y install hello\$"
  expect "$log" "$image: a self-hosted runner's update reads every source" "^self-hosted: $bounded -y update\$"
  expect "$log" "$image: a self-hosted runner's install reads every source" "^self-hosted: $bounded -y install hello\$"
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
  show_log_if_failed "$log" "$started"
done

if [ "$failures" != 0 ]; then
  echo "$failures check(s) failed"
  exit 1
fi
echo "all apt-install.sh checks passed"
