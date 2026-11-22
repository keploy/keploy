#!/bin/bash
# Tests for setup-java-maven/jdk's jdk.sh. Needs bash, curl, python3 (a local
# HTTP server stands in for the release servers), tar, gzip and sha256sum.
# Run by the lint workflow, or locally (it leaves nothing behind):
#
#   bash .github/actions/setup-java-maven/jdk/jdk.tests.sh
#
# Matches read the output in full ([[ ]]), never through a pipe into a reader
# that stops early (under pipefail its writer's SIGPIPE fails the row): a row
# checks that on a body larger than a pipe's buffer.
# action.yml: its steps in order, each under its if: (the image's setup-java
# for source image; restore, fetch with cache-hit, save on a miss on main only
# and the jdkfile setup-java for source archive).
# jdks.txt: every line pins a major version to a build, a sha256 and that
# build's Temurin x64 Linux archive on Adoptium's GitHub release (https), and
# no major version has two lines.
# pin: 8 and 17 (spaces around them too) give the build, source archive, the
# archive's directory (one per digest) and file, and a key that names the
# build and the digest; a version jdks.txt has no line for (or two), one that
# is not a major version or empty, a runner that is not Linux X64, a pins file
# that is not there and a malformed line each fail, saying so.
# pin's source, against a tool cache laid out as the runner image's: image
# when it holds exactly the pinned build (8 and 17), complete, and its release
# file says Eclipse Adoptium and that SEMANTIC_VERSION; archive, saying what
# it holds, when it holds another build only, the build's directory with
# another SEMANTIC_VERSION (17.0.21+90 too) or implementor, a second other
# IMPLEMENTOR or SEMANTIC_VERSION line, or one not marked complete. When the
# image holds another build of the same major version (8 as 17), a ::warning
# names it and the jdks.txt line to move; another major only (21) is not one.
# fetch, against the local server:
#   - with no cached copy it downloads the archive once, checks it and keeps
#     it where the cache saves it from, saying how many bytes came from where;
#     a redirect is followed, and the signed query of the URL it ends at is
#     not printed;
#   - with a cached copy the restore called a hit it checks that, and
#     downloads nothing; on a miss, an archive already here with the pinned
#     digest is kept, and any other file left there is removed and the
#     archive downloaded; a hit without the archive fails, naming the key to
#     delete;
#   - an HTML page (HTTP 200), a truncated archive, a 502, a refused
#     connection, a redirect to a refused connection and a body with a % in it
#     each fail the step with an error that names the URL (and the redirect's
#     target), the digests or curl's reason and the last HTTP status, the size
#     and what the body starts with (escaped for the annotation), and leave no
#     archive to install or save;
#   - a cached copy that fails the digest is removed, and the error says to
#     delete its cache entry, naming the key.
set -uo pipefail

dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$dir/jdk.sh"
tmp=$(mktemp -d)
server=''
cleanup() {
  [ -z "$server" ] || kill "$server" 2>/dev/null
  rm -rf "$tmp"
}
trap cleanup EXIT

fails=0
n=0
ok() { n=$((n + 1)); echo "ok   $1"; }
bad() { n=$((n + 1)); fails=$((fails + 1)); echo "FAIL $1"; [ -z "${2:-}" ] || printf '%s\n' "$2" | sed 's/^/    /'; }

# The real pins.
want_url() { # MAJOR BUILD: the Temurin x64 Linux archive of BUILD on Adoptium's release
  local v=${2%%+*} b=${2#*+}
  if [ "$1" = 8 ]; then
    local u=${v#8.0.}
    printf 'https://github.com/adoptium/temurin8-binaries/releases/download/jdk8u%s-b%02d/OpenJDK8U-jdk_x64_linux_hotspot_8u%sb%02d.tar.gz' "$u" "$b" "$u" "$b"
  else
    printf 'https://github.com/adoptium/temurin%s-binaries/releases/download/jdk-%s%%2B%s/OpenJDK%sU-jdk_x64_linux_hotspot_%s_%s.tar.gz' "$1" "$v" "$b" "$1" "$v" "$b"
  fi
}
lines=0 wrong='' seen=' '
while read -r major build sum url rest; do
  case $major in '' | '#'*) continue ;; esac
  lines=$((lines + 1))
  [[ $seen != *" $major "* ]] || wrong="$wrong$major: a second line"$'\n'
  seen="$seen$major "
  [[ $major =~ ^[0-9]+$ && $build == "$major".* && $sum =~ ^[0-9a-f]{64}$ && -z $rest &&
    $url == "$(want_url "$major" "$build")" ]] || wrong="$wrong$major $build $sum $url $rest"$'\n'
done <"$dir/jdks.txt"
if [ "$lines" -ge 2 ] && [ -z "$wrong" ]; then ok "jdks.txt: $lines pins, one per major version, each its build's Temurin archive (https) and a sha256"; else
  bad "jdks.txt: $lines pins; these are not MAJOR BUILD SHA256 and that build's Temurin archive:" "$wrong"
fi

# run NAME ENV... -- ARGS: jdk.sh with a fresh GITHUB_OUTPUT and the runner's
# variables (overridable in ENV; the tool cache is an empty one, never the
# machine's own); sets out (stdout and stderr), rc, outputs.
run() {
  local name=$1 envs=()
  shift
  while [ "$1" != -- ]; do envs+=("$1"); shift; done
  shift
  : >"$tmp/out.$name"
  rc=0
  out=$(env HOME="${HOME_T:-$tmp/home}" RUNNER_OS=Linux RUNNER_ARCH=X64 RUNNER_TOOL_CACHE="$tmp/no-toolcache" GITHUB_OUTPUT="$tmp/out.$name" \
    "${envs[@]}" bash "$script" "$@" 2>&1) || rc=$?
  outputs=$(cat "$tmp/out.$name")
}
# has NAME TEXT...: out holds every TEXT, else a failure for NAME. A match on
# the variable itself: piped into a reader that stops at its first match
# (grep -q), the writer could die of SIGPIPE, which pipefail reports as a
# failure although the text is there.
has() {
  local name=$1 t
  shift
  for t in "$@"; do
    [[ $out == *"$t"* ]] || { bad "$name: lacks '$t'; it said:" "$out"; return 1; }
  done
}
# has() reads all of a body larger than a pipe's buffer, the text on its first line.
out="needle on the first line"$'\n'$(head -c 300000 /dev/zero | tr '\0' x | fold -w 100)
if has big-body "needle on the first line"; then ok "has() finds the text on the first line of a ${#out}-byte body"; fi

# action.yml: its steps, in order, and the if: each runs under. The image's
# setup-java only for source image; restore, fetch, the jdkfile setup-java
# only for source archive; the save also only on a miss, and only on main.
steps=$(awk '
  function sig() { return cond " | " what dist }
  /^    - / { if (n) print sig(); n++; cond = "-"; what = ""; dist = "" }
  { line = $0; sub(/^    - /, "      ", line) }
  n && line ~ /^      if: / { cond = line; sub(/^      if: /, "", cond) }
  n && line ~ /^      (uses|run): / { what = line; sub(/^      (uses|run): /, "", what) }
  n && line ~ /^        distribution: / { dist = line; sub(/^        distribution:/, "", dist) }
  END { if (n) print sig() }' "$dir/action.yml")
want_steps=$(cat <<'STEPS'
- | bash "$GITHUB_ACTION_PATH/jdk.sh" pin
steps.pin.outputs.source == 'image' | actions/setup-java@v4 temurin
steps.pin.outputs.source == 'archive' | actions/cache/restore@v4
steps.pin.outputs.source == 'archive' | bash "$GITHUB_ACTION_PATH/jdk.sh" fetch
steps.pin.outputs.source == 'archive' && steps.cache.outputs.cache-hit != 'true' && github.ref == 'refs/heads/main' | actions/cache/save@v4
steps.pin.outputs.source == 'archive' | actions/setup-java@v4 jdkfile
STEPS
)
# shellcheck disable=SC2016 # ${{ }} is action.yml's expression, not the shell's
hitenv=$(grep -c '^        JDK_CACHE_HIT: ${{ steps.cache.outputs.cache-hit }}$' "$dir/action.yml")
if [ "$steps" = "$want_steps" ] && [ "$hitenv" = 1 ]; then ok "action.yml: pin; image: setup-java temurin; archive: restore, fetch (with cache-hit), save (miss, main only), setup-java jdkfile"; else
  bad "action.yml's steps are not these (JDK_CACHE_HIT lines: $hitenv):" "$want_steps"$'\n'"they are:"$'\n'"$steps"
fi

# pin, on the real jdks.txt.
for v in 8 ' 17 '; do
  read -r major build sum url _ < <(grep -E "^${v// /} " "$dir/jdks.txt")
  run "pin$major" JDK_JAVA_VERSION="$v" -- pin
  d="$tmp/home/.cache/setup-java-maven-jdk/$sum"
  want="version=$build
source=archive
dir=$d
file=$d/${url##*/}
key=setup-java-maven-jdk-Linux-X64-temurin-$build-$sum"
  if [ "$rc" -eq 0 ] && [ "$outputs" = "$want" ]; then ok "pin '$v': $build from its archive (no image copy), its dir and file, a key naming build and digest"; else
    bad "pin '$v': rc $rc, outputs:" "$outputs"$'\n'"want:"$'\n'"$want"$'\n'"$out"
  fi
done
pin_fails() { # NAME TEXT ENV...: pin fails, saying TEXT, and writes no output
  local name=$1 text=$2
  shift 2
  run "$name" "$@" -- pin
  if [ "$rc" -ne 0 ] && [ -z "$outputs" ]; then has "$name" "::error title=JDK pin::" "$text" && ok "pin fails: $name"; else
    bad "pin $name: rc $rc, outputs '$outputs'" "$out"
  fi
}
pin_fails unpinned "no JDK is pinned for java-version 11 in $dir/jdks.txt (it pins 8, 17)" JDK_JAVA_VERSION=11
pin_fails not-major "java-version '17.0.21' is not a major version" JDK_JAVA_VERSION=17.0.21
pin_fails empty "java-version '' is not a major version" JDK_JAVA_VERSION=
pin_fails arm "pins Linux X64 builds only, and this runner is Linux ARM64" JDK_JAVA_VERSION=17 RUNNER_ARCH=ARM64
pin_fails macos "pins Linux X64 builds only, and this runner is macOS X64" JDK_JAVA_VERSION=17 RUNNER_OS=macOS
pin_fails no-pins "$tmp/none.txt is not a file" JDK_JAVA_VERSION=17 JDK_PINS="$tmp/none.txt"
printf '17 17.0.1+1 abc https://x/y.tar.gz\n' >"$tmp/short.txt"
pin_fails short-sum "the line of java-version 17 in $tmp/short.txt is not 'MAJOR BUILD SHA256 URL.tar.gz'" JDK_JAVA_VERSION=17 JDK_PINS="$tmp/short.txt"
grep -E '^17 ' "$dir/jdks.txt" >"$tmp/twice.txt"
grep -E '^17 ' "$dir/jdks.txt" >>"$tmp/twice.txt"
pin_fails twice "$tmp/twice.txt has two lines for java-version 17" JDK_JAVA_VERSION=17 JDK_PINS="$tmp/twice.txt"

# pin's source, against a tool cache laid out as the runner image's.
# toolcache NAME DIR IMPLEMENTOR SEMVER [complete]: $tmp/tc-NAME holding
# Java_Temurin-Hotspot_jdk/DIR/x64 with that release file (as Temurin's).
toolcache() {
  local d="$tmp/tc-$1/Java_Temurin-Hotspot_jdk/$2/x64"
  mkdir -p "$d/bin"
  printf 'IMPLEMENTOR="%s"\nJAVA_VERSION="x"\nSEMANTIC_VERSION="%s"\n' "$3" "$4" >"$d/release"
  [ -z "${5:-}" ] || : >"$d.complete"
}
# source_is NAME MAJOR SOURCE WARN TEXT...: pin with tool cache tc-NAME gives
# SOURCE, warns (WARN: warn) or not (-), and says each TEXT.
source_is() {
  local name=$1 major=$2 want=$3 warn=$4
  shift 4
  run "src-$name" JDK_JAVA_VERSION="$major" RUNNER_TOOL_CACHE="$tmp/tc-$name" -- pin
  local w=-
  [[ $out != *"::warning title=JDK pin::"* ]] || w=warn
  if [ "$rc" -eq 0 ] && [[ $'\n'$outputs$'\n' == *$'\n'"source=$want"$'\n'* ]] && [ "$w" = "$warn" ]; then
    has "src-$name" "$@" && ok "pin's source: $name -> $want$([ "$warn" = - ] || echo ', with a warning')"
  else
    bad "pin's source $name: rc $rc, warning $w (want $warn), outputs:" "$outputs"$'\n'"$out"
  fi
}
toolcache image17 17.0.21-9 "Eclipse Adoptium" 17.0.21+9 complete
source_is image17 17 image - "the runner image has exactly that build ($tmp/tc-image17/Java_Temurin-Hotspot_jdk/17.0.21-9/x64), so setup-java takes it from there and nothing is downloaded"
toolcache image8 8.0.514-1 "Eclipse Adoptium" 8.0.514+1 complete
source_is image8 8 image - "($tmp/tc-image8/Java_Temurin-Hotspot_jdk/8.0.514-1/x64)"
toolcache other 17.0.20-8 "Eclipse Adoptium" 17.0.20+8 complete
toolcache other 21.0.9-10 "Eclipse Adoptium" 21.0.9+10 complete
source_is other 17 archive warn "The runner image has no Temurin 17.0.21+9 in $tmp/tc-other; it has: 17.0.20-8 21.0.9-10" \
  "::warning title=JDK pin::the runner image has Temurin 17.0.20+8 for java-version 17, not the pinned 17.0.21+9, so this job takes the pinned build's archive" \
  "move the line of 17 in $dir/jdks.txt to the image's build"
toolcache only21 21.0.9-10 "Eclipse Adoptium" 21.0.9+10 complete
source_is only21 17 archive - "it has: 21.0.9-10"
toolcache other8 8.0.522-3 "Eclipse Adoptium" 8.0.522+3 complete
source_is other8 8 archive warn "::warning title=JDK pin::the runner image has Temurin 8.0.522+3 for java-version 8, not the pinned 8.0.514+1"
toolcache semver 17.0.21-9 "Eclipse Adoptium" 17.0.21+10 complete
source_is semver 17 archive warn "it has: 17.0.21-9 (its release file: IMPLEMENTOR=\"Eclipse Adoptium\" SEMANTIC_VERSION=\"17.0.21+10\")"
toolcache vendor 17.0.21-9 "Oracle Corporation" 17.0.21+9 complete
source_is vendor 17 archive warn "IMPLEMENTOR=\"Oracle Corporation\""
toolcache partial 17.0.21-9 "Eclipse Adoptium" 17.0.21+9
source_is partial 17 archive warn "SEMANTIC_VERSION=\"17.0.21+9\"), not marked complete"
toolcache prefix 17.0.21-9 "Eclipse Adoptium" 17.0.21+90 complete
source_is prefix 17 archive warn "SEMANTIC_VERSION=\"17.0.21+90\")"
# A release file with a second, other IMPLEMENTOR or SEMANTIC_VERSION line.
toolcache twovendors 17.0.21-9 "Eclipse Adoptium" 17.0.21+9 complete
printf 'IMPLEMENTOR="Oracle Corporation"\n' >>"$tmp/tc-twovendors/Java_Temurin-Hotspot_jdk/17.0.21-9/x64/release"
source_is twovendors 17 archive warn "IMPLEMENTOR=\"Oracle Corporation\""
toolcache twoversions 17.0.21-9 "Eclipse Adoptium" 17.0.21+9 complete
printf 'SEMANTIC_VERSION="17.0.22+7"\n' >>"$tmp/tc-twoversions/Java_Temurin-Hotspot_jdk/17.0.21-9/x64/release"
source_is twoversions 17 archive warn "SEMANTIC_VERSION=\"17.0.22+7\""

# fetch, against a local server.
mkdir -p "$tmp/jdk/jdk-test/bin"
printf '#!/bin/sh\necho test jdk\n' >"$tmp/jdk/jdk-test/bin/java"
head -c 200000 /dev/urandom >"$tmp/jdk/jdk-test/lib.bin"
tar -czf "$tmp/good.tar.gz" -C "$tmp/jdk" jdk-test
sum=$(sha256sum <"$tmp/good.tar.gz" | cut -c1-64)
size=$(wc -c <"$tmp/good.tar.gz" | tr -d '[:space:]')
head -c $((size / 2)) "$tmp/good.tar.gz" >"$tmp/trunc.tar.gz"
cat >"$tmp/server.py" <<'EOF'
import http.server, sys
port_file, log, good, trunc, dead_file = sys.argv[1:6]
pages = {
    "/html.tar.gz": (200, "text/html; charset=utf-8", b"<!DOCTYPE html>\n<html><body>Not the archive</body></html>\n"),
    "/pct.tar.gz": (200, "text/plain", b"100% of %0A nothing\n"),
    "/e502.tar.gz": (502, "text/html", b"<html>Bad Gateway</html>\n"),
}
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        with open(log, "a") as f:
            f.write(self.path + "\n")
        path = self.path.split("?")[0]
        if path in ("/redir.tar.gz", "/redir-dead.tar.gz"):
            self.send_response(302)
            self.send_header("Location", "/good.tar.gz?sig=SECRET-SIGNATURE" if path == "/redir.tar.gz"
                             else "http://127.0.0.1:%s/moved.tar.gz" % open(dead_file).read().strip())
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        if path in ("/good.tar.gz", "/trunc.tar.gz"):
            body = open(good if path == "/good.tar.gz" else trunc, "rb").read()
            code, ctype = 200, "application/octet-stream"
        elif path in pages:
            code, ctype, body = pages[path]
        else:
            code, ctype, body = 404, "text/plain", b"no\n"
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a):
        pass
s = http.server.HTTPServer(("127.0.0.1", 0), H)
with open(port_file + ".tmp", "w") as f:
    f.write(str(s.server_address[1]))
import os
os.rename(port_file + ".tmp", port_file)
s.serve_forever()
EOF
: >"$tmp/requests"
python3 -I "$tmp/server.py" "$tmp/port" "$tmp/requests" "$tmp/good.tar.gz" "$tmp/trunc.tar.gz" "$tmp/refused" &
server=$!
for _ in $(seq 1 100); do [ -s "$tmp/port" ] && break; sleep 0.1; done
[ -s "$tmp/port" ] || { echo "FAIL the local server did not start"; exit 1; }
base="http://127.0.0.1:$(cat "$tmp/port")"
# A port nothing listens on: one a socket bound here (while the server holds
# its own), then closed. /redir-dead.tar.gz redirects there.
refused=$(python3 -I -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')
echo "$refused" >"$tmp/refused"

# fetch_case NAME PATH [CACHED [HIT]]: fresh HOME and requests log; pins 8 to
# the test archive at PATH; with CACHED, the cache's dir already holds that
# file; HIT is the restore's cache-hit.
fetch_case() {
  local name=$1 path=$2
  HOME_T="$tmp/h-$name"
  mkdir -p "$HOME_T"
  printf '8 8.0.1+2 %s %s\n' "$sum" "$path" >"$tmp/pins-$name"
  cdir="$HOME_T/.cache/setup-java-maven-jdk/$sum"
  cfile="$cdir/${path##*/}"
  if [ -n "${3:-}" ]; then mkdir -p "$cdir" && cp "$3" "$cfile"; fi
  : >"$tmp/requests"
  run "$name" JDK_JAVA_VERSION=8 JDK_PINS="$tmp/pins-$name" JDK_CACHE_HIT="${4:-}" -- fetch
  reqs=$(cat "$tmp/requests")
  left=$(ls -A "$cdir" 2>/dev/null)
}
key="setup-java-maven-jdk-Linux-X64-temurin-8.0.1+2-$sum"

fetch_case miss "$base/good.tar.gz"
if [ "$rc" -eq 0 ] && [ "$reqs" = /good.tar.gz ] && [ "$left" = good.tar.gz ] &&
  [ "$(sha256sum <"$cfile" | cut -c1-64)" = "$sum" ]; then
  has miss "has no copy of its archive (key $key); downloading $base/good.tar.gz" "downloaded $size bytes in" \
    "has the pinned sha256 $sum" && ok "fetch, no cached copy: downloaded once, checked, kept for the cache"
else bad "fetch miss: rc $rc, requests '$reqs', left '$left'" "$out"; fi

fetch_case redirect "$base/redir.tar.gz"
if [ "$rc" -eq 0 ] && [ "$left" = redir.tar.gz ] && [[ $out != *SECRET* ]]; then
  has redirect "from $base/good.tar.gz" && ok "fetch follows a redirect, and prints none of its signed query"
else bad "fetch redirect: rc $rc, left '$left'" "$out"; fi

fetch_case hit "$base/good.tar.gz" "$tmp/good.tar.gz" true
if [ "$rc" -eq 0 ] && [ -z "$reqs" ] && [ "$left" = good.tar.gz ]; then
  has hit "checking the archive the Actions cache restored" "has the pinned sha256 $sum" &&
    ok "fetch, cached copy: checked, nothing downloaded"
else bad "fetch hit: rc $rc, requests '$reqs', left '$left'" "$out"; fi

fetch_case html "$base/html.tar.gz"
page='<!DOCTYPE html>\n<html><body>Not the archive</body></html>\n'
plen=$(printf %b "$page" | wc -c | tr -d '[:space:]')
if [ "$rc" -ne 0 ] && [ -z "$left" ]; then
  has html "::error title=JDK archive digest::the JDK 8.0.1+2 archive downloaded from $base/html.tar.gz (HTTP 200, text/html; charset=utf-8, $plen bytes in" \
    "has sha256 $(printf %b "$page" | sha256sum | cut -c1-64), not the pinned $sum" \
    "$plen bytes, not a gzip stream; it starts '<!DOCTYPE html>.<html><body>Not the archive</body></html>.'" \
    "It was not installed or cached." && ok "fetch: an HTML page fails the step, named and described; nothing kept"
else bad "fetch html: rc $rc, left '$left'" "$out"; fi

fetch_case trunc "$base/trunc.tar.gz"
if [ "$rc" -ne 0 ] && [ -z "$left" ]; then
  has trunc "::error title=JDK archive digest::the JDK 8.0.1+2 archive downloaded from $base/trunc.tar.gz (HTTP 200" \
    "$((size / 2)) bytes, a gzip stream. It was not installed or cached." && ok "fetch: a truncated archive fails the step; nothing kept"
else bad "fetch trunc: rc $rc, left '$left'" "$out"; fi

fetch_case e502 "$base/e502.tar.gz"
if [ "$rc" -ne 0 ] && [ -z "$left" ]; then
  has e502 "::error title=JDK download::downloading the JDK 8.0.1+2 archive from $base/e502.tar.gz failed: curl exit 22 (" \
    "returned error: 502); the last HTTP status it got: 502, 0 bytes received. No JDK was installed or cached." &&
    ok "fetch: an HTTP 502 fails the step, naming the URL and status"
else bad "fetch e502: rc $rc, left '$left'" "$out"; fi

fetch_case refused "http://127.0.0.1:$refused/good.tar.gz"
if [ "$rc" -ne 0 ] && [ -z "$left" ]; then
  has refused "::error title=JDK download::downloading the JDK 8.0.1+2 archive from http://127.0.0.1:$refused/good.tar.gz failed: curl exit 7 (" \
    "the last HTTP status it got: none (no HTTP answer), 0 bytes received" && ok "fetch: a refused connection fails the step, saying no HTTP answer came"
else bad "fetch refused: rc $rc, left '$left'" "$out"; fi

fetch_case redirect-dead "$base/redir-dead.tar.gz"
if [ "$rc" -ne 0 ] && [ -z "$left" ]; then
  has redirect-dead "archive from $base/redir-dead.tar.gz failed while fetching http://127.0.0.1:$refused/moved.tar.gz, where it was redirected: curl exit 7 (" \
    "the last HTTP status it got: 302, 0 bytes received" && ok "fetch: a redirect to a server that refuses names the redirect's target and its 302"
else bad "fetch redirect-dead: rc $rc, left '$left'" "$out"; fi

fetch_case pct "$base/pct.tar.gz"
if [ "$rc" -ne 0 ] && [ -z "$left" ]; then
  has pct "it starts '100%25 of %250A nothing.'" && ok "fetch: a % in the body is escaped in the annotation"
else bad "fetch pct: rc $rc, left '$left'" "$out"; fi

# The archive with its byte at offset 100 changed: to another value, always
# (writing a fixed byte leaves the file as it was when that byte is already
# there, 1 run in 256 of a random archive).
cp "$tmp/good.tar.gz" "$tmp/flipped.tar.gz"
b=$(od -An -tu1 -j 100 -N 1 "$tmp/flipped.tar.gz" | tr -d '[:space:]')
printf '%b' "\\0$(printf %03o $(((b + 1) % 256)))" | dd of="$tmp/flipped.tar.gz" bs=1 seek=100 conv=notrunc status=none
if cmp -s "$tmp/good.tar.gz" "$tmp/flipped.tar.gz"; then
  bad "the flipped archive is the archive (byte 100 was $b)"
else ok "the flipped archive differs from the archive (byte 100: $b -> $(((b + 1) % 256)))"; fi
fetch_case corrupt "$base/good.tar.gz" "$tmp/flipped.tar.gz" true
if [ "$rc" -ne 0 ] && [ -z "$reqs" ] && [ -z "$left" ]; then
  has corrupt "::error title=JDK archive digest::the JDK 8.0.1+2 archive restored from the Actions cache ($cfile, key $key) has sha256" \
    "$size bytes, a gzip stream. It was removed, and no JDK was installed." "delete it (gh cache delete '$key')" &&
    ok "fetch: a cached copy that fails the digest is removed, nothing downloaded, the error names its key"
else bad "fetch corrupt: rc $rc, requests '$reqs', left '$left'" "$out"; fi

# A miss with the archive already here (an earlier use in the job): checked and kept.
fetch_case reuse "$base/good.tar.gz" "$tmp/good.tar.gz" false
if [ "$rc" -eq 0 ] && [ -z "$reqs" ] && [ "$left" = good.tar.gz ]; then
  has reuse "$cfile is already here (not restored as a hit), and has the pinned sha256: kept" &&
    ok "fetch: a miss with the archive already here keeps it, nothing downloaded"
else bad "fetch reuse: rc $rc, requests '$reqs', left '$left'" "$out"; fi

# A restore that failed part way: a miss, with a partial file left behind.
fetch_case leftover "$base/good.tar.gz" "$tmp/flipped.tar.gz" false
if [ "$rc" -eq 0 ] && [ "$reqs" = /good.tar.gz ] && [ "$left" = good.tar.gz ] &&
  [ "$(sha256sum <"$cfile" | cut -c1-64)" = "$sum" ]; then
  has leftover "removing what is in $cdir, which the Actions cache did not restore as a hit" "downloaded $size bytes in" &&
    ok "fetch: what a restore left without a hit is removed, and the archive downloaded and checked"
else bad "fetch leftover: rc $rc, requests '$reqs', left '$left'" "$out"; fi

fetch_case hit-empty "$base/good.tar.gz" "" true
if [ "$rc" -ne 0 ] && [ -z "$reqs" ]; then
  has hit-empty "::error title=JDK archive::the Actions cache entry $key was restored without the archive $cfile" \
    "delete it (gh cache delete '$key')" && ok "fetch: a hit without the archive fails, naming the key to delete, and downloads nothing"
else bad "fetch hit-empty: rc $rc, requests '$reqs'" "$out"; fi

run usage -- other
if [ "$rc" -eq 2 ]; then ok "an unknown step is a usage error"; else bad "usage: rc $rc" "$out"; fi

echo "$((n - fails))/$n passed"
[ "$fails" -eq 0 ]
