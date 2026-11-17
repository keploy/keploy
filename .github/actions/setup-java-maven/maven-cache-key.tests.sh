#!/usr/bin/env bash
# Tests for maven-cache-key.sh, the key setup-java-maven caches ~/.m2/repository
# under, and a check of this repository's workflows. Needs bash, sha256sum and
# Maven on PATH (the last case asks the real one). Run by the lint workflow, or
# locally (it leaves nothing behind):
#
#   bash .github/actions/setup-java-maven/maven-cache-key.tests.sh
#
# Against a stub mvn: the key names the Maven version, so a 3.10.0 and a
# 3.9.15 build of the same pom get different keys; the same inputs give the
# same key; a changed pom, an added pom or another JDK gives another key, and
# blank lines and spaces around the names do not; a pom that is not there, an
# empty list, a failing `mvn -v` and one with no version (or two) fail and say
# why. Against the real mvn: the key names the version it prints. And this
# repository: no workflow or action uses actions/setup-java's own Maven cache
# (`cache: maven`), whose key is the same for every Maven version, and the
# check finds one put in.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$here/maven-cache-key.sh"
repo="$(cd "$here/../../.." && pwd)"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fails=0
n=0
ok() { n=$((n + 1)); echo "ok   - $1"; }
fail() { n=$((n + 1)); fails=$((fails + 1)); echo "FAIL - $1"; [ -z "${2:-}" ] || printf '%s\n' "$2" | sed 's/^/       /'; }

# A stub mvn: prints STUB_MVN_OUT for -v and exits with STUB_MVN_RC.
mkdir -p "$tmp/bin" "$tmp/ws"
cat >"$tmp/bin/mvn" <<'EOF'
#!/usr/bin/env bash
[ "$*" = "-B -v" ] || { echo "stub mvn: unexpected arguments: $*" >&2; exit 64; }
printf '%s\n' "$STUB_MVN_OUT"
exit "${STUB_MVN_RC:-0}"
EOF
chmod +x "$tmp/bin/mvn"
mvn_out() {
  printf 'Apache Maven %s (98b2cdbfdb5f1ac8781f537ea9acccaed7922349)\nMaven home: /usr/share/apache-maven-%s\nJava version: 17.0.21, vendor: Eclipse Adoptium\n' "$1" "$1"
}
printf '<project><artifactId>a</artifactId></project>\n' >"$tmp/ws/a.xml"
printf '<project><artifactId>b</artifactId></project>\n' >"$tmp/ws/b.xml"

# key MAVEN_VERSION JAVA DEPS: a run with the stub printing that version; its
# stdout in $out, its stderr in $err, its exit in $rc.
key() {
  out=$(cd "$tmp/ws" && PATH="$tmp/bin:$PATH" STUB_MVN_OUT="$(mvn_out "$1")" STUB_MVN_RC=0 \
    RUNNER_OS=Linux RUNNER_ARCH=X64 MAVEN_CACHE_JAVA_VERSION="$2" MAVEN_CACHE_DEPENDENCY_PATH="$3" \
    bash "$script" 2>"$tmp/err")
  rc=$?
  err=$(cat "$tmp/err")
}

key 3.10.0 17 a.xml; k310=$out
key 3.9.15 17 a.xml; k3915=$out
case "$k310|$k3915" in
  maven-repository-Linux-X64-maven3.10.0-java17-*"|"maven-repository-Linux-X64-maven3.9.15-java17-*) ok "the key names the Maven version (3.10.0 and 3.9.15 differ)" ;;
  *) fail "the key names the Maven version (3.10.0 and 3.9.15 differ)" "3.10.0: $k310"$'\n'"3.9.15: $k3915"$'\n'"$err" ;;
esac
key 3.9.15 17 a.xml
[ "$out" = "$k3915" ] && [ -n "$k3915" ] &&
  ok "the same Maven, JDK and poms give the same key" ||
  fail "the same Maven, JDK and poms give the same key"
key 3.9.15 8 a.xml
[ -n "$out" ] && [ "$out" != "$k3915" ] && ok "another JDK gives another key" || fail "another JDK gives another key"
key 3.9.15 17 $'a.xml\nb.xml'; k_ab=$out
[ -n "$k_ab" ] && [ "$k_ab" != "$k3915" ] && ok "an added pom gives another key" || fail "an added pom gives another key" "$err"
key 3.9.15 17 $'\n  a.xml  \n\n b.xml\n'
[ "$out" = "$k_ab" ] &&
  ok "blank lines and spaces around the names do not change the key" ||
  fail "blank lines and spaces around the names do not change the key" "$err"
cp "$tmp/ws/a.xml" "$tmp/a.orig"
printf '<project><artifactId>a</artifactId><version>2</version></project>\n' >"$tmp/ws/a.xml"
key 3.9.15 17 a.xml; k_changed=$out
cp "$tmp/a.orig" "$tmp/ws/a.xml"
[ -n "$k_changed" ] && [ "$k_changed" != "$k3915" ] && ok "a changed pom gives another key" || fail "a changed pom gives another key"

key 3.9.15 17 $'a.xml\nmissing/pom.xml'
[ "$rc" -ne 0 ] && [ -z "$out" ] && grep -q 'missing/pom.xml.*is not a file' <<<"$err" &&
  ok "a pom that is not there fails, naming it" || fail "a pom that is not there fails, naming it" "rc=$rc out=$out"$'\n'"$err"
key 3.9.15 17 $'\n  \n'
[ "$rc" -ne 0 ] && [ -z "$out" ] && grep -q 'names no file' <<<"$err" &&
  ok "an empty list fails" || fail "an empty list fails" "rc=$rc out=$out"$'\n'"$err"

stub() { # stub OUT RC: a run with the stub printing OUT and exiting RC
  out=$(cd "$tmp/ws" && PATH="$tmp/bin:$PATH" STUB_MVN_OUT="$1" STUB_MVN_RC="$2" RUNNER_OS=Linux RUNNER_ARCH=X64 \
    MAVEN_CACHE_JAVA_VERSION=17 MAVEN_CACHE_DEPENDENCY_PATH=a.xml bash "$script" 2>"$tmp/err")
  rc=$?
  err=$(cat "$tmp/err")
}
stub "$(mvn_out 3.9.15)" 1
[ "$rc" -ne 0 ] && [ -z "$out" ] && grep -q 'mvn -B -v failed' <<<"$err" &&
  ok "a failing mvn -v fails" || fail "a failing mvn -v fails" "rc=$rc out=$out"$'\n'"$err"
stub "The JAVA_HOME environment variable is not defined correctly" 0
[ "$rc" -ne 0 ] && [ -z "$out" ] && grep -q 'no single Maven version' <<<"$err" &&
  ok "an mvn -v with no version fails" || fail "an mvn -v with no version fails" "rc=$rc out=$out"$'\n'"$err"
stub "$(mvn_out 3.9.15)"$'\n'"$(mvn_out 3.10.0)" 0
[ "$rc" -ne 0 ] && [ -z "$out" ] && grep -q 'no single Maven version' <<<"$err" &&
  ok "an mvn -v with two versions fails" || fail "an mvn -v with two versions fails" "rc=$rc out=$out"$'\n'"$err"

# The real Maven: the version in its key is the one it prints.
if real_v=$(mvn -B -v 2>&1); then
  real=$(printf '%s\n' "$real_v" | sed -n 's/^Apache Maven \([0-9][^ ]*\).*/\1/p')
  real_key=$(cd "$tmp/ws" && RUNNER_OS=Linux RUNNER_ARCH=X64 MAVEN_CACHE_JAVA_VERSION=17 \
    MAVEN_CACHE_DEPENDENCY_PATH=a.xml bash "$script" 2>&1)
  case "$real_key" in
    "maven-repository-Linux-X64-maven$real-java17-"*) [ -n "$real" ] && ok "the real mvn's key names its version ($real)" ||
      fail "the real mvn's key names its version" "$real_v" ;;
    *) fail "the real mvn's key names its version ($real)" "$real_key" ;;
  esac
else
  fail "the real mvn's key names its version: mvn -B -v failed" "$real_v"
fi

# maven_cache_uses DIR: every line under DIR's workflows and actions that turns on
# actions/setup-java's own Maven cache.
maven_cache_uses() {
  local d=$1
  (cd "$d" && grep -nE "^[[:space:]]*cache:[[:space:]]*['\"]?maven['\"]?[[:space:]]*(#.*)?$" \
    .github/workflows/*.yml .github/workflows/*.yaml .github/actions/*/action.yml .github/actions/*/action.yaml 2>/dev/null)
}
uses=$(maven_cache_uses "$repo")
[ -z "$uses" ] && ok "no workflow or action here uses setup-java's own Maven cache" ||
  fail "no workflow or action here uses setup-java's own Maven cache (use ./.github/actions/setup-java-maven)" "$uses"
mkdir -p "$tmp/r/.github/workflows" "$tmp/r/.github/actions/x"
printf 'jobs:\n  j:\n    steps:\n      - uses: actions/setup-java@v4\n        with:\n          java-version: "17"\n          cache: "maven"  # setup-java'"'"'s\n' >"$tmp/r/.github/workflows/w.yml"
printf 'runs:\n  using: composite\n  steps:\n    - uses: actions/setup-java@v4\n      with:\n        cache: maven\n' >"$tmp/r/.github/actions/x/action.yml"
uses=$(maven_cache_uses "$tmp/r")
[ "$(printf '%s\n' "$uses" | grep -c .)" = 2 ] && grep -q '^.github/workflows/w.yml:7:' <<<"$uses" &&
  grep -q '^.github/actions/x/action.yml:6:' <<<"$uses" &&
  ok "the check finds setup-java's Maven cache put in a workflow and an action" ||
  fail "the check finds setup-java's Maven cache put in a workflow and an action" "$uses"

echo "$((n - fails))/$n passed"
[ "$fails" -eq 0 ]
