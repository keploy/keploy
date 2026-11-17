#!/usr/bin/env bash
# Tests for setup-java-maven's scripts: maven-cache-key.sh, the key it caches
# ~/.m2/repository under, and offline/mvn, the mvn it runs after an exact
# hit. Needs bash, perl, sha256sum and Maven on PATH (one case asks the real
# one). Run by the lint workflow, or locally (it leaves nothing behind):
#
#   bash .github/actions/setup-java-maven/setup-java-maven.tests.sh
#
# The key, against a stub mvn: it names the Maven version, so a 3.10.0 and a
# 3.9.15 build of the same pom get different keys. The same inputs give the
# same key. Another JDK, a changed or added pom, a mirror in the user's or
# Maven's own settings.xml, and a .mvn/maven.config, jvm.config or
# extensions.xml of the pom's project (beside it or above it) each give
# another key; a .mvn above the nearest one is not the project's and does
# not. Blank lines and spaces around the names do not. A settings.xml whose
# servers, passwords, passphrases, private keys, usernames or properties
# change gives the same key, and none of them is printed. The log names the
# files hashed. A pom that is not there, an empty list, a failing `mvn -v`,
# one with no version (or two), and one with no Maven home fail and say why.
# Against the real mvn: the key names the version it prints.
# offline/mvn, against a stub of the real mvn: it passes --offline and every
# argument as given, and exits with the real one's code; it says nothing when
# that passes, and when it fails it says so, naming the cache key and none of
# the arguments; with its variables unset it refuses to run.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$here/maven-cache-key.sh"
shim="$here/offline/mvn"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fails=0
n=0
ok() { n=$((n + 1)); echo "ok   - $1"; }
fail() { n=$((n + 1)); fails=$((fails + 1)); echo "FAIL - $1"; [ -z "${2:-}" ] || printf '%s\n' "$2" | sed 's/^/       /'; }
check() { # check NAME CONDITION...: ok when the condition (a command) passes
  local name=$1
  shift
  if "$@"; then ok "$name"; else fail "$name" "out=$out"$'\n'"rc=$rc"$'\n'"$err"; fi
}

# A stub mvn: prints STUB_MVN_OUT for -v and exits with STUB_MVN_RC.
mkdir -p "$tmp/bin" "$tmp/ws/p/q" "$tmp/home/.m2" "$tmp/mavenhome/conf"
cat >"$tmp/bin/mvn" <<'EOF'
#!/usr/bin/env bash
[ "$*" = "-B -v" ] || { echo "stub mvn: unexpected arguments: $*" >&2; exit 64; }
printf '%s\n' "$STUB_MVN_OUT"
exit "${STUB_MVN_RC:-0}"
EOF
chmod +x "$tmp/bin/mvn"
mvn_out() {
  printf 'Apache Maven %s (98b2cdbfdb5f1ac8781f537ea9acccaed7922349)\nMaven home: %s\nJava version: 17.0.21, vendor: Eclipse Adoptium\n' "$1" "$tmp/mavenhome"
}
printf '<project><artifactId>a</artifactId></project>\n' >"$tmp/ws/a.xml"
printf '<project><artifactId>b</artifactId></project>\n' >"$tmp/ws/b.xml"
printf '<project><artifactId>c</artifactId></project>\n' >"$tmp/ws/p/q/pom.xml"
user_settings='<settings><servers><server><id>github</id><username>${env.GITHUB_ACTOR}</username><password>${env.GITHUB_TOKEN}</password></server></servers></settings>'
global_settings='<settings><mirrors><mirror><id>maven-default-http-blocker</id><mirrorOf>external:http:*</mirrorOf><url>http://0.0.0.0/</url><blocked>true</blocked></mirror></mirrors></settings>'
reset_config() {
  printf '%s\n' "$user_settings" >"$tmp/home/.m2/settings.xml"
  printf '%s\n' "$global_settings" >"$tmp/mavenhome/conf/settings.xml"
  rm -rf "$tmp/ws/.mvn" "$tmp/ws/p/.mvn" "$tmp/ws/p/q/.mvn"
}

# key MAVEN_VERSION JAVA DEPS [STUB_OUTPUT [STUB_RC]]: a run with the stub
# printing that version (or STUB_OUTPUT); its stdout in $out, its stderr in
# $err, its exit in $rc.
key() {
  local stub_out=${4:-$(mvn_out "$1")}
  out=$(cd "$tmp/ws" && HOME="$tmp/home" PATH="$tmp/bin:$PATH" STUB_MVN_OUT="$stub_out" STUB_MVN_RC="${5:-0}" \
    RUNNER_OS=Linux RUNNER_ARCH=X64 MAVEN_CACHE_JAVA_VERSION="$2" MAVEN_CACHE_DEPENDENCY_PATH="$3" \
    bash "$script" 2>"$tmp/err")
  rc=$?
  err=$(cat "$tmp/err")
}
differs() { [ "$rc" -eq 0 ] && [ -n "$out" ] && [ "$out" != "$1" ]; }
same() { [ "$rc" -eq 0 ] && [ "$out" = "$1" ]; }
failed_with() { [ "$rc" -ne 0 ] && [ -z "$out" ] && grep -q -- "$1" <<<"$err"; }

reset_config
key 3.10.0 17 a.xml; k310=$out
key 3.9.15 17 a.xml; base=$out
case "$k310|$base" in
  maven-repository-Linux-X64-maven3.10.0-java17-*"|"maven-repository-Linux-X64-maven3.9.15-java17-*) ok "the key names the Maven version (3.10.0 and 3.9.15 differ)" ;;
  *) fail "the key names the Maven version (3.10.0 and 3.9.15 differ)" "3.10.0: $k310"$'\n'"3.9.15: $base"$'\n'"$err" ;;
esac
key 3.9.15 17 a.xml; check "the same Maven, JDK, poms and configuration give the same key" same "$base"
grep -q -F "$tmp/home/.m2/settings.xml (credentials and properties left out)" <<<"$err" &&
  grep -q -F "$tmp/mavenhome/conf/settings.xml" <<<"$err" && grep -q -F "  a.xml" <<<"$err" &&
  ok "the log names the files hashed" || fail "the log names the files hashed" "$err"
key 3.9.15 8 a.xml; check "another JDK gives another key" differs "$base"
key 3.9.15 17 $'a.xml\nb.xml'; k_ab=$out; check "an added pom gives another key" differs "$base"
key 3.9.15 17 $'\n  a.xml  \n\n b.xml\n'; check "blank lines and spaces around the names do not change the key" same "$k_ab"
cp "$tmp/ws/a.xml" "$tmp/a.orig"
printf '<project><artifactId>a</artifactId><version>2</version></project>\n' >"$tmp/ws/a.xml"
key 3.9.15 17 a.xml; check "a changed pom gives another key" differs "$base"
cp "$tmp/a.orig" "$tmp/ws/a.xml"

# Configuration that changes which repositories a build has, under which ids.
mirror='<mirrors><mirror><id>central-mirror</id><mirrorOf>central</mirrorOf><url>https://mirror.example/maven2</url></mirror></mirrors>'
printf '%s\n' "${user_settings%</settings>}$mirror</settings>" >"$tmp/home/.m2/settings.xml"
key 3.9.15 17 a.xml; check "a mirror in the user's settings.xml gives another key" differs "$base"
reset_config
printf '%s\n' "${global_settings/maven-default-http-blocker/blocker-2}" >"$tmp/mavenhome/conf/settings.xml"
key 3.9.15 17 a.xml; check "a changed mirror in Maven's own settings.xml gives another key" differs "$base"
reset_config
rm "$tmp/home/.m2/settings.xml"
key 3.9.15 17 a.xml; check "no user settings.xml gives another key" differs "$base"
reset_config
mkdir -p "$tmp/ws/.mvn"
printf -- '-Daether.system.trackingRepositoryKeyFunction=nid\n' >"$tmp/ws/.mvn/maven.config"
key 3.9.15 17 a.xml; check "a .mvn/maven.config beside the pom gives another key" differs "$base"
grep -q -F "$tmp/ws/.mvn/maven.config" <<<"$err" && ok "the log names the .mvn/maven.config hashed" ||
  fail "the log names the .mvn/maven.config hashed" "$err"
reset_config
key 3.9.15 17 p/q/pom.xml; base_q=$out
mkdir -p "$tmp/ws/.mvn"
printf -- '-Xmx1g\n' >"$tmp/ws/.mvn/jvm.config"
key 3.9.15 17 p/q/pom.xml; check "a .mvn/jvm.config of a project above the pom gives another key" differs "$base_q"
k_jvm=$out
mkdir -p "$tmp/ws/p/.mvn"
key 3.9.15 17 p/q/pom.xml; check "a .mvn above the nearest one is not the pom's project's, and does not count" same "$base_q"
printf '<extensions/>\n' >"$tmp/ws/p/.mvn/extensions.xml"
key 3.9.15 17 p/q/pom.xml; check "a .mvn/extensions.xml of the nearest project gives another key" differs "$base_q"
[ "$out" != "$k_jvm" ] && ok "only the nearest project's .mvn counts (its extensions.xml, not the farther jvm.config)" ||
  fail "only the nearest project's .mvn counts (its extensions.xml, not the farther jvm.config)" "$out"
reset_config

# Credentials: never in the key, never in the log, and they do not change it.
secret="s3cr3t-$$-$RANDOM"
cat >"$tmp/home/.m2/settings.xml" <<EOF
<settings>
  <servers>
    <server><id>github</id><username>u-$secret</username><password>p-$secret</password></server>
    <server><id>deploy</id><privateKey>/k-$secret</privateKey><passphrase>pp-$secret</passphrase></server>
  </servers>
  <proxies><proxy><id>p</id><username>pu-$secret</username><password>pp-$secret</password></proxy></proxies>
  <profiles><profile><id>x</id><properties><gpg.passphrase>g-$secret</gpg.passphrase></properties></profile></profiles>
</settings>
EOF
key 3.9.15 17 a.xml; k_secret=$out
# The same file with other credentials.
sed -i "s/$secret/other/g" "$tmp/home/.m2/settings.xml"
key 3.9.15 17 a.xml
[ "$rc" -eq 0 ] && [ -n "$k_secret" ] && [ "$out" = "$k_secret" ] &&
  ok "other servers, passwords, passphrases, private keys, usernames and properties give the same key" ||
  fail "other servers, passwords, passphrases, private keys, usernames and properties give the same key" "$k_secret"$'\n'"$out"$'\n'"$err"
sed -i "s/other/$secret/g" "$tmp/home/.m2/settings.xml"
key 3.9.15 17 a.xml
! grep -q -F "$secret" <<<"$out$err" && [ "$rc" -eq 0 ] &&
  ok "no credential is printed" || fail "no credential is printed" "$out"$'\n'"$err"
reset_config

key 3.9.15 17 $'a.xml\nmissing/pom.xml'; check "a pom that is not there fails, naming it" failed_with 'missing/pom.xml.*is not a file'
key 3.9.15 17 $'\n  \n'; check "an empty list fails" failed_with 'names no file'
key 3.9.15 17 a.xml "$(mvn_out 3.9.15)" 1; check "a failing mvn -v fails" failed_with 'mvn -B -v failed'
key 3.9.15 17 a.xml "The JAVA_HOME environment variable is not defined correctly"
check "an mvn -v with no version fails" failed_with 'no single Maven version'
key 3.9.15 17 a.xml "$(mvn_out 3.9.15)"$'\n'"$(mvn_out 3.10.0)"
check "an mvn -v with two versions fails" failed_with 'no single Maven version'
key 3.9.15 17 a.xml "Apache Maven 3.9.15 (98b2cdbfdb5f1ac8781f537ea9acccaed7922349)"
check "an mvn -v with no Maven home fails" failed_with 'no single "Maven home:"'

# The real Maven: the version in its key is the one it prints.
if real_v=$(mvn -B -v 2>&1); then
  real=$(printf '%s\n' "$real_v" | sed -n 's/^Apache Maven \([0-9][^ ]*\).*/\1/p')
  real_key=$(cd "$tmp/ws" && RUNNER_OS=Linux RUNNER_ARCH=X64 MAVEN_CACHE_JAVA_VERSION=17 \
    MAVEN_CACHE_DEPENDENCY_PATH=a.xml bash "$script" 2>&1 >"$tmp/real.out"; cat "$tmp/real.out")
  case "$real_key" in
    *"maven-repository-Linux-X64-maven$real-java17-"*) [ -n "$real" ] && ok "the real mvn's key names its version ($real)" ||
      fail "the real mvn's key names its version" "$real_v" ;;
    *) fail "the real mvn's key names its version ($real)" "$real_key" ;;
  esac
else
  fail "the real mvn's key names its version: mvn -B -v failed" "$real_v"
fi

# offline/mvn, in front of a stub that records its arguments and exits STUB_RC.
cat >"$tmp/bin/real-mvn" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@" >"$STUB_ARGS"
exit "${STUB_RC:-0}"
EOF
chmod +x "$tmp/bin/real-mvn"
shim_run() { # shim_run RC ARGS...
  local want_rc=$1
  shift
  out=$(STUB_ARGS="$tmp/args" STUB_RC="$want_rc" SETUP_JAVA_MAVEN_REAL_MVN="$tmp/bin/real-mvn" \
    SETUP_JAVA_MAVEN_CACHE_KEY=maven-repository-Linux-X64-maven3.9.15-java17-abc "$shim" "$@" 2>"$tmp/err")
  rc=$?
  err=$(cat "$tmp/err")
}
shim_run 0 -B -U clean package '-Dname=a b' -Dmaven.test.skip=true
[ "$rc" -eq 0 ] && [ "$(cat "$tmp/args")" = $'--offline\n-B\n-U\nclean\npackage\n-Dname=a b\n-Dmaven.test.skip=true' ] &&
  [ -z "$err$out" ] && ok "offline/mvn runs mvn --offline with every argument as given, and says nothing when it passes" ||
  fail "offline/mvn runs mvn --offline with every argument as given, and says nothing when it passes" "rc=$rc args=$(cat "$tmp/args")"$'\n'"$err"
shim_run 1 -q -B -Dtoken=hidden-arg package
[ "$rc" -eq 1 ] && grep -q '^::error title=Maven failed offline on its restored cache::mvn exited 1' <<<"$err" &&
  grep -q -F 'maven-repository-Linux-X64-maven3.9.15-java17-abc' <<<"$err" && ! grep -q -F 'hidden-arg' <<<"$err" &&
  ok "a failing offline mvn exits with its code and says so, naming the cache key and none of its arguments" ||
  fail "a failing offline mvn exits with its code and says so, naming the cache key and none of its arguments" "rc=$rc"$'\n'"$err"
shim_run 3 package
[ "$rc" -eq 3 ] && ok "offline/mvn keeps the real mvn's exit code" || fail "offline/mvn keeps the real mvn's exit code" "rc=$rc"
out=$(env -u SETUP_JAVA_MAVEN_REAL_MVN SETUP_JAVA_MAVEN_CACHE_KEY=k "$shim" package 2>&1); rc=$?
[ "$rc" -ne 0 ] && grep -q 'SETUP_JAVA_MAVEN_REAL_MVN is not set' <<<"$out" &&
  ok "offline/mvn refuses to run without the mvn it shadows" || fail "offline/mvn refuses to run without the mvn it shadows" "rc=$rc $out"

echo "$((n - fails))/$n passed"
[ "$fails" -eq 0 ]
