#!/usr/bin/env bash
# Tests for setup-java-maven's scripts: maven-cache-key.sh (the key),
# restored.sh (the step after the restore), bin/mvn (the mvn the build runs)
# and save/save-plan.sh (whether and under which entry to save). Needs bash,
# perl, sha256sum and Maven on PATH (one case asks the real one). Run by the
# lint workflow, or locally (it leaves nothing behind):
#
#   bash .github/actions/setup-java-maven/setup-java-maven.tests.sh
#
# The key, against a stub mvn:
#   - it names the Maven version, so a 3.10.0 and a 3.9.15 build of the same
#     pom get different keys;
#   - another JDK, a changed or added pom, a mirror in the user's or Maven's
#     own settings.xml (also after a self-closing <properties/>), and a
#     .mvn/maven.config, jvm.config or extensions.xml of the pom's project
#     (beside it or above it) each give another key;
#   - the same inputs, blank lines and spaces around names, a .mvn above the
#     nearest one, other credentials, properties or URL user:password@ in
#     settings.xml, and MAVEN_OPTS or MAVEN_ARGS with a -D secret in them give
#     the same key, and no credential is printed;
#   - the log names the files hashed;
#   - a pom that is not there, a failing `mvn -v`, one with no version (or
#     two) or no Maven home fail and say why;
#   - with SETUP_JAVA_MAVEN_REAL_MVN set, it asks that mvn, not the one on
#     PATH.
# restored.sh: each use gets its own record (key, entry, hit, the restored
# files); the first puts bin/ on PATH and names the real mvn, a second keeps
# both; with bin/mvn on PATH and no real mvn named it refuses; an entry of
# another key is an error.
# bin/mvn, in front of a fake mvn:
#   - after a use that restored an entry, mvn runs once, with --offline and
#     every argument as given, and says one line; after one that missed, it
#     runs online and says nothing;
#   - the latest use decides: a hit then a miss runs online, a miss then a
#     hit offline;
#   - an offline run that cannot resolve an artifact (an added goal's plugin)
#     says so, naming it, the key and the entry, runs once more online, and
#     keeps the online run's code, then checks offline that the heal took
#     (if not, it says so, naming what is still missing); a missing plugin
#     prefix is named first, and
#     a plugin Maven names twice (g:a:v, g:a:jar:v) once; another failure is
#     not run again;
#   - a -D secret on the command line appears in none of its output, its
#     annotations or its state files, on any of those paths;
#   - it cannot loop: a real mvn that is bin/mvn is refused, and an mvn run
#     from inside it runs the real one as it is; -v runs as it is.
# save-plan.sh: nothing ran, nothing to save; a fill is saved as KEY-<digest
# of the new files>, the same files giving the same entry; a hit that added
# nothing is not saved; one that fetched files after its cache could not
# serve it is, even if it added no file, and under an entry named from the
# entry it restored too, so that two builds sharing a key that heal apart
# end at the union, not at a refused save; a miss whose build passed is, even
# if another use's repository held all it needed, and one whose lane ran no
# build is not; bookkeeping files alone are not; a last run that failed stops
# the save, one that failed and then passed does not; a heal that did not
# take fails the save step, naming what, and saves nothing; run again, it plans the
# next use, oldest first, then nothing.
# The real mvn: the key names the version it prints.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$here/maven-cache-key.sh"
restored="$here/restored.sh"
shim="$here/bin/mvn"
plan="$here/save/save-plan.sh"
# bounded: the repo's hang guard, so that a looping bin/mvn ends the case
# instead of the job (an exit code is never taken from timeout(1)).
# shellcheck source=../../workflows/test_workflow_scripts/hang_guard.sh
. "$here/../../workflows/test_workflow_scripts/hang_guard.sh" || exit 1
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fails=0
n=0
ok() { n=$((n + 1)); echo "ok   - $1"; }
fail() { n=$((n + 1)); fails=$((fails + 1)); echo "FAIL - $1"; [ -z "${2:-}" ] || printf '%s\n' "$2" | sed 's/^/       /'; }
check() { # check NAME COMMAND...: ok when the command passes
  local name=$1
  shift
  if "$@"; then ok "$name"; else fail "$name" "out=$out"$'\n'"rc=$rc"$'\n'"$err"; fi
}

# ---- the key ------------------------------------------------------------
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
# printing that version (or STUB_OUTPUT); stdout in $out, stderr in $err,
# exit in $rc. KEY_ENV: more VAR=value words for its environment.
KEY_ENV=()
key() {
  local stub_out=${4:-$(mvn_out "$1")}
  out=$(cd "$tmp/ws" && env HOME="$tmp/home" PATH="$tmp/bin:$PATH" STUB_MVN_OUT="$stub_out" STUB_MVN_RC="${5:-0}" \
    RUNNER_OS=Linux RUNNER_ARCH=X64 MAVEN_CACHE_JAVA_VERSION="$2" MAVEN_CACHE_DEPENDENCY_PATH="$3" \
    "${KEY_ENV[@]}" bash "$script" 2>"$tmp/err")
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
# A self-closing <properties/> must not swallow what follows it.
sc() { printf '<settings><profiles><profile><id>a</id><properties/></profile></profiles><mirrors><mirror><id>%s</id><mirrorOf>central</mirrorOf><url>https://m.example/</url></mirror></mirrors><profiles><profile><id>b</id><properties><x>1</x></properties></profile></profiles></settings>\n' "$1"; }
sc one >"$tmp/home/.m2/settings.xml"
key 3.9.15 17 a.xml; k_sc=$out
sc two >"$tmp/home/.m2/settings.xml"
key 3.9.15 17 a.xml; check "a mirror after a self-closing <properties/> still counts" differs "$k_sc"
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
  <mirrors><mirror><id>m</id><mirrorOf>central</mirrorOf><url>https://tok-$secret@mirror.example/maven2</url></mirror></mirrors>
</settings>
EOF
key 3.9.15 17 a.xml; k_secret=$out
sed -i "s/$secret/other/g" "$tmp/home/.m2/settings.xml"
key 3.9.15 17 a.xml
[ "$rc" -eq 0 ] && [ -n "$k_secret" ] && [ "$out" = "$k_secret" ] &&
  ok "other servers, passwords, passphrases, private keys, usernames, properties and URL user:password@ give the same key" ||
  fail "other servers, passwords, passphrases, private keys, usernames, properties and URL user:password@ give the same key" "$k_secret"$'\n'"$out"$'\n'"$err"
sed -i "s/other/$secret/g" "$tmp/home/.m2/settings.xml"
key 3.9.15 17 a.xml
! grep -q -F "$secret" <<<"$out$err" && [ "$rc" -eq 0 ] &&
  ok "no credential is printed" || fail "no credential is printed" "$out"$'\n'"$err"
reset_config
KEY_ENV=("MAVEN_OPTS=-Ddb.password=o-$secret" "MAVEN_ARGS=-Ddb.password=a-$secret clean package")
key 3.9.15 17 a.xml
KEY_ENV=()
[ "$rc" -eq 0 ] && [ "$out" = "$base" ] && ! grep -q -F "$secret" <<<"$out$err" &&
  ok "a -D secret in MAVEN_OPTS or MAVEN_ARGS leaves the key as it was, and is not printed" ||
  fail "a -D secret in MAVEN_OPTS or MAVEN_ARGS leaves the key as it was, and is not printed" "$base"$'\n'"$out"$'\n'"$err"

key 3.9.15 17 $'a.xml\nmissing/pom.xml'; check "a pom that is not there fails, naming it" failed_with 'missing/pom.xml.*is not a file'
key 3.9.15 17 $'\n  \n'; check "an empty list of poms fails" failed_with 'names no file'
key 3.9.15 17 a.xml "$(mvn_out 3.9.15)" 1; check "a failing mvn -v fails" failed_with 'mvn -B -v failed'
key 3.9.15 17 a.xml "The JAVA_HOME environment variable is not defined correctly"
check "an mvn -v with no version fails" failed_with 'no single Maven version'
key 3.9.15 17 a.xml "$(mvn_out 3.9.15)"$'\n'"$(mvn_out 3.10.0)"
check "an mvn -v with two versions fails" failed_with 'no single Maven version'
key 3.9.15 17 a.xml "Apache Maven 3.9.15 (98b2cdbfdb5f1ac8781f537ea9acccaed7922349)"
check "an mvn -v with no Maven home fails" failed_with 'no single "Maven home:"'
# SETUP_JAVA_MAVEN_REAL_MVN, as a second use of the action sees it: `mvn` on PATH is bin/mvn.
mkdir -p "$tmp/shadow"
printf '#!/usr/bin/env bash\necho "the mvn on PATH ran" >&2\nexit 9\n' >"$tmp/shadow/mvn"
chmod +x "$tmp/shadow/mvn"
out=$(cd "$tmp/ws" && HOME="$tmp/home" PATH="$tmp/shadow:$PATH" SETUP_JAVA_MAVEN_REAL_MVN="$tmp/bin/mvn" STUB_MVN_OUT="$(mvn_out 3.9.15)" \
  RUNNER_OS=Linux RUNNER_ARCH=X64 MAVEN_CACHE_JAVA_VERSION=17 MAVEN_CACHE_DEPENDENCY_PATH=a.xml \
  bash "$script" 2>"$tmp/err"); rc=$?; err=$(cat "$tmp/err")
check "with SETUP_JAVA_MAVEN_REAL_MVN set, the key asks that mvn, not the one on PATH" same "$base"

# ---- restored.sh ----------------------------------------------------------
# restore_run NAME KEY MATCHED [PATH_PREFIX]: restored.sh in job NAME
# (its own RUNNER_TEMP, GITHUB_ENV, GITHUB_PATH, HOME), with the env the
# job's earlier steps left (GITHUB_ENV applied).
job_env() { # job_env NAME: the job's GITHUB_ENV, as VAR=value words
  [ -f "$tmp/job-$1/env" ] && cat "$tmp/job-$1/env"
}
restore_run() {
  local j="$tmp/job-$1" envs=()
  mkdir -p "$j/temp" "$j/home/.m2"
  touch "$j/env" "$j/path"
  while IFS= read -r l; do [ -n "$l" ] && envs+=("$l"); done <"$j/env"
  out=$(env HOME="$j/home" RUNNER_TEMP="$j/temp" GITHUB_ENV="$j/env" GITHUB_PATH="$j/path" PATH="${4:-}$tmp/bin:$PATH" \
    KEY="$2" MATCHED="$3" "${envs[@]}" bash "$restored" 2>"$tmp/err")
  rc=$?
  err=$(cat "$tmp/err")
}
K=maven-repository-Linux-X64-maven3.9.15-java17-aaaa
restore_run miss "$K" ""
d="$tmp/job-miss/temp/setup-java-maven/001"
[ "$rc" -eq 0 ] && [ "$(cat "$d/hit")" = false ] && [ "$(cat "$d/key")" = "$K" ] && [ ! -s "$d/manifest" ] &&
  grep -qx "SETUP_JAVA_MAVEN_REAL_MVN=$tmp/bin/mvn" "$tmp/job-miss/env" &&
  grep -qx "SETUP_JAVA_MAVEN_STATES=$tmp/job-miss/temp/setup-java-maven" "$tmp/job-miss/env" &&
  [ "$(cat "$tmp/job-miss/path")" = "$here/bin" ] &&
  ok "a miss is recorded (no entry), and the first use names the real mvn and puts bin/ on PATH" ||
  fail "a miss is recorded (no entry), and the first use names the real mvn and puts bin/ on PATH" "rc=$rc $out"$'\n'"$err"
mkdir -p "$tmp/job-hit/home/.m2/repository/org/x/1"
touch "$tmp/job-hit/home/.m2/repository/org/x/1/x-1.jar"
restore_run hit "$K" "$K-0123456789abcdef"
d="$tmp/job-hit/temp/setup-java-maven/001"
[ "$rc" -eq 0 ] && [ "$(cat "$d/hit")" = true ] && [ "$(cat "$d/matched")" = "$K-0123456789abcdef" ] &&
  [ "$(cat "$d/manifest")" = "./org/x/1/x-1.jar" ] && grep -q '^::notice title=Maven cache restored::' <<<"$out" &&
  ok "a hit is recorded with the files restored, and says so" || fail "a hit is recorded with the files restored, and says so" "rc=$rc $out"$'\n'"$err"
# A second use in the same job: mvn on PATH is now bin/mvn.
restore_run hit "${K}2" "" "$here/bin:"
[ "$rc" -eq 0 ] && [ -d "$tmp/job-hit/temp/setup-java-maven/002" ] && [ "$(grep -c . "$tmp/job-hit/path")" = 1 ] &&
  [ "$(grep -c '^SETUP_JAVA_MAVEN_REAL_MVN=' "$tmp/job-hit/env")" = 1 ] &&
  grep -qx "SETUP_JAVA_MAVEN_REAL_MVN=$tmp/bin/mvn" "$tmp/job-hit/env" &&
  ok "a second use gets its own record, and keeps the real mvn and bin/ on PATH once" ||
  fail "a second use gets its own record, and keeps the real mvn and bin/ on PATH once" "rc=$rc"$'\n'"$(cat "$tmp/job-hit/env")"$'\n'"$err"
restore_run loop "$K" "" "$here/bin:"
[ "$rc" -ne 0 ] && grep -q "is setup-java-maven's own bin/mvn" <<<"$err" &&
  ok "with bin/mvn first on PATH and no real mvn named, it refuses" || fail "with bin/mvn first on PATH and no real mvn named, it refuses" "rc=$rc $err"
restore_run other "$K" "maven-repository-Linux-X64-maven3.9.15-java17-bbbb-1"
[ "$rc" -ne 0 ] && grep -q "is not one of key" <<<"$err" && ok "an entry of another key is an error" ||
  fail "an entry of another key is an error" "rc=$rc $err"

# ---- bin/mvn --------------------------------------------------------------
# A fake real mvn: records its arguments (one per line, a run per file) and
# acts on FAKE: ok; missing (offline: cannot resolve the dependency plugin,
# as after a lane added dependency:tree, until an online run fetched it);
# stubborn (offline: cannot resolve it, even after an online run); compile
# (fails, not for want of an artifact).
cat >"$tmp/bin/real-mvn" <<'EOF'
#!/usr/bin/env bash
n=$(ls "$CALLS" 2>/dev/null | wc -l)
printf '%s\n' "$@" >"$CALLS/$((n + 1))"
off=no
for a in "$@"; do [ "$a" = --offline ] && off=yes; done
case "$FAKE:$off" in
  ok:*) exit 0 ;;
  missing:yes | stubborn:yes)
    [ "$FAKE" = missing ] && [ -e "$FAKE_FETCHED" ] && exit 0
    echo "[ERROR] Plugin org.apache.maven.plugins:maven-dependency-plugin:3.6.1 or one of its dependencies could not be resolved: Cannot access central (https://repo.maven.apache.org/maven2) in offline mode and the artifact org.apache.maven.plugins:maven-dependency-plugin:jar:3.6.1 has not been downloaded from it before. -> [Help 1]"
    exit 1 ;;
  missing:no | stubborn:no | prefix:no) touch "$FAKE_FETCHED"; exit 0 ;;
  prefix:yes)
    [ -e "$FAKE_FETCHED" ] && exit 0
    echo "[ERROR] Plugin io.github.git-commit-id:git-commit-id-maven-plugin:6.0.0 or one of its dependencies could not be resolved: Cannot access central (https://repo.maven.apache.org/maven2) in offline mode and the artifact io.github.git-commit-id:git-commit-id-maven-plugin:jar:6.0.0 has not been downloaded from it before."
    echo "[ERROR] Plugin org.apache.maven.plugins:maven-antrun-plugin:3.1.0 or one of its dependencies could not be resolved: Cannot access central (https://repo.maven.apache.org/maven2) in offline mode and the artifact org.apache.maven.plugins:maven-antrun-plugin:jar:3.1.0 has not been downloaded from it before."
    echo "[ERROR] No plugin found for prefix 'dependency' in the current project and in the plugin groups [org.apache.maven.plugins, org.codehaus.mojo] available from the repositories [local (/home/runner/.m2/repository)] -> [Help 1]"
    exit 1 ;;
  compile:*) echo "[ERROR] COMPILATION ERROR : p/A.java:[3,1] class, interface, or enum expected"; exit 1 ;;
esac
EOF
chmod +x "$tmp/bin/real-mvn"
mkuse() { # mkuse STATES N KEY MATCHED HIT: a use's record, as restored.sh writes it
  local d
  d=$(printf '%s/%03d' "$1" "$2")
  mkdir -p "$d"
  printf '%s\n' "$3" >"$d/key"; printf '%s\n' "$4" >"$d/matched"; printf '%s\n' "$5" >"$d/hit"
  : >"$d/manifest"
}
shim_run() { # shim_run STATES FAKE ARGS...: bin/mvn; stdout+stderr in $out, exit in $rc, calls in $tmp/calls
  local st=$1 fake=$2
  shift 2
  rm -rf "$tmp/calls" "$tmp/fetched"; mkdir -p "$tmp/calls"
  out=$(CALLS="$tmp/calls" FAKE="$fake" FAKE_FETCHED="$tmp/fetched" SETUP_JAVA_MAVEN_REAL_MVN="$tmp/bin/real-mvn" SETUP_JAVA_MAVEN_STATES="$st" "$shim" "$@" 2>&1)
  rc=$?
  err=$out
}
calls() { ls "$tmp/calls" | wc -l | tr -d ' '; }
call() { cat "$tmp/calls/$1"; }
SECRET="hunter2-$$-$RANDOM"
LANE=(-B -U clean package -Dmaven.test.skip=true "-Ddb.password=$SECRET" -q)
no_secret() { # no_secret STATES: the secret is in no output and no state file
  ! grep -q -F "$SECRET" <<<"$out" && ! grep -r -q -F "$SECRET" "$1"
}
S="$tmp/st1"
mkuse "$S" 1 "$K" "$K-01" true
shim_run "$S" ok "${LANE[@]}"
[ "$rc" -eq 0 ] && [ "$(calls)" = 1 ] && [ "$(call 1)" = "$(printf '%s\n' --offline "${LANE[@]}")" ] &&
  [ "$(grep -c . <<<"$out")" = 1 ] && grep -qx "setup-java-maven: mvn runs offline on the Maven cache $K-01" <<<"$out" &&
  grep -qx "0 offline 001" "$S/runs" &&
  ok "after a use that restored an entry, mvn runs once, with --offline and every argument as given" ||
  fail "after a use that restored an entry, mvn runs once, with --offline and every argument as given" "rc=$rc calls=$(calls)"$'\n'"$out"
no_secret "$S" && ok "a -D secret is printed and kept nowhere on an offline run" || fail "a -D secret is printed and kept nowhere on an offline run" "$out"
S="$tmp/st2"
mkuse "$S" 1 "$K" "" false
shim_run "$S" ok "${LANE[@]}"
[ "$rc" -eq 0 ] && [ "$(call 1)" = "$(printf '%s\n' "${LANE[@]}")" ] && [ -z "$out" ] && no_secret "$S" &&
  ok "after a use that missed, mvn runs online, says nothing, and keeps no secret" ||
  fail "after a use that missed, mvn runs online, says nothing, and keeps no secret" "rc=$rc $(call 1 | paste -sd' ')"$'\n'"$out"
S="$tmp/st3"
mkuse "$S" 1 "$K" "$K-01" true
mkuse "$S" 2 "${K}2" "" false
shim_run "$S" ok clean package
a=$(head -1 "$tmp/calls/1")
S="$tmp/st3b"
mkuse "$S" 1 "$K" "" false
mkuse "$S" 2 "${K}2" "${K}2-01" true
shim_run "$S" ok clean package
b=$(head -1 "$tmp/calls/1")
[ "$a" = clean ] && [ "$b" = --offline ] &&
  ok "the latest use decides: a hit then a miss runs online, a miss then a hit offline" ||
  fail "the latest use decides: a hit then a miss runs online, a miss then a hit offline" "a=$a b=$b"
# An offline run that cannot resolve an artifact: a lane that added a goal.
S="$tmp/st4"
mkuse "$S" 1 "$K" "$K-01" true
shim_run "$S" missing "${LANE[@]}"
[ "$rc" -eq 0 ] && [ "$(calls)" = 3 ] && [ "$(head -1 "$tmp/calls/1")" = --offline ] && [ "$(call 2)" = "$(printf '%s\n' "${LANE[@]}")" ] &&
  [ "$(call 3)" = "$(printf '%s\n' --offline "${LANE[@]}")" ] && [ ! -e "$S/001/heal-failed" ] && ! grep -q 'heal did not take' <<<"$out" &&
  grep -q "^::error title=Maven cache cannot serve this build::mvn could not resolve, offline, org.apache.maven.plugins:maven-dependency-plugin:3.6.1 from the Maven cache $K-01 (key $K)" <<<"$out" &&
  [ "$(cat "$S/001/unresolved")" = 'org.apache.maven.plugins:maven-dependency-plugin:3.6.1' ] &&
  grep -qx "0 fallback 001" "$S/runs" &&
  ok "an offline run that cannot resolve an artifact names it, the key and the entry, runs once more online, and checks offline that it took" ||
  fail "an offline run that cannot resolve an artifact names it, the key and the entry, runs once more online, and checks offline that it took" "rc=$rc calls=$(calls)"$'\n'"$out"
no_secret "$S" && ok "a -D secret is printed and kept nowhere on a fallback, its annotation included" ||
  fail "a -D secret is printed and kept nowhere on a fallback, its annotation included" "$out"
S="$tmp/st4s"
mkuse "$S" 1 "$K" "$K-01" true
shim_run "$S" stubborn "${LANE[@]}"
[ "$rc" -eq 0 ] && [ "$(calls)" = 3 ] && [ "$(head -1 "$tmp/calls/3")" = --offline ] &&
  grep -q "^::error title=Maven cache heal did not take::after the online run, mvn still could not resolve, offline, org.apache.maven.plugins:maven-dependency-plugin:3.6.1 (key $K)" <<<"$out" &&
  [ "$(cat "$S/001/heal-failed")" = org.apache.maven.plugins:maven-dependency-plugin:3.6.1 ] && no_secret "$S" &&
  ok "a heal that did not take (still missing offline after the online run) says so, naming what" ||
  fail "a heal that did not take (still missing offline after the online run) says so, naming what" "rc=$rc calls=$(calls)"$'\n'"$out"
S="$tmp/st4b"
mkuse "$S" 1 "$K" "$K-01" true
shim_run "$S" prefix clean package dependency:tree
[ "$rc" -eq 0 ] && grep -q "^::error title=Maven cache cannot serve this build::mvn could not resolve, offline, plugin prefix dependency; io.github.git-commit-id:git-commit-id-maven-plugin:6.0.0; org.apache.maven.plugins:maven-antrun-plugin:3.1.0 from" <<<"$out" &&
  ok "a missing plugin prefix is named first, and each plugin once" ||
  fail "a missing plugin prefix is named first, and each plugin once" "rc=$rc"$'\n'"$out"
S="$tmp/st5"
mkuse "$S" 1 "$K" "$K-01" true
shim_run "$S" compile "${LANE[@]}"
[ "$rc" -eq 1 ] && [ "$(calls)" = 1 ] && ! grep -q '^::' <<<"$out" && [ ! -e "$S/001/unresolved" ] && grep -qx "1 offline 001" "$S/runs" &&
  no_secret "$S" &&
  ok "an offline run that fails for another reason fails, once, with its code, and keeps no secret" ||
  fail "an offline run that fails for another reason fails, once, with its code, and keeps no secret" "rc=$rc calls=$(calls)"$'\n'"$out"
# It cannot loop.
out=$(bounded -q 10 env SETUP_JAVA_MAVEN_REAL_MVN="$shim" SETUP_JAVA_MAVEN_STATES="$S" "$shim" clean package 2>&1); rc=$?
[ "$rc" -eq 2 ] && grep -q 'is this script; refusing' <<<"$out" && ok "a real mvn that is bin/mvn itself is refused" ||
  fail "a real mvn that is bin/mvn itself is refused" "rc=$rc $out"
rm -rf "$tmp/calls"; mkdir -p "$tmp/calls"
out=$(CALLS="$tmp/calls" FAKE=ok SETUP_JAVA_MAVEN_IN_MVN=1 SETUP_JAVA_MAVEN_REAL_MVN="$tmp/bin/real-mvn" SETUP_JAVA_MAVEN_STATES="$S" "$shim" clean package 2>&1); rc=$?
[ "$rc" -eq 0 ] && [ "$(call 1)" = $'clean\npackage' ] && ok "an mvn run from inside bin/mvn runs the real one as it is" ||
  fail "an mvn run from inside bin/mvn runs the real one as it is" "rc=$rc $(call 1 | paste -sd' ') $out"
shim_run "$S" ok -B -v
[ "$rc" -eq 0 ] && [ "$(call 1)" = $'-B\n-v' ] && [ -z "$out" ] && ok "mvn -v runs as it is, with nothing printed" ||
  fail "mvn -v runs as it is, with nothing printed" "rc=$rc $out"

# ---- save-plan.sh -----------------------------------------------------------
# plan_run HOME STATES: save-plan.sh; its GITHUB_OUTPUT in $po, stdout+stderr in $out.
plan_run() {
  : >"$tmp/po"
  out=$(HOME="$1" SETUP_JAVA_MAVEN_STATES="$2" GITHUB_OUTPUT="$tmp/po" bash "$plan" 2>&1)
  rc=$?
  po=$(cat "$tmp/po")
}
fill() { # fill HOME FILE...: files in HOME's ~/.m2/repository
  local h=$1 f
  shift
  for f in "$@"; do mkdir -p "$h/.m2/repository/$(dirname "$f")"; echo x >"$h/.m2/repository/$f"; done
}
manifest() { (cd "$1/.m2/repository" && find . -type f | LC_ALL=C sort) >"$2/manifest"; }
plan_run "$tmp/nohome" "$tmp/nostates"
[ "$rc" -eq 0 ] && [ "$po" = save=false ] && ok "no setup-java-maven step: nothing to save" || fail "no setup-java-maven step: nothing to save" "$po $out"
H="$tmp/h1"; S="$tmp/sp1"
mkuse "$S" 1 "$K" "" false
fill "$H" org/x/1/x-1.jar org/x/1/x-1.pom org/x/1/_remote.repositories
plan_run "$H" "$S"
e1=$(sed -n 's/^key=//p' <<<"$po")
[ "$rc" -eq 0 ] && grep -qx save=true <<<"$po" && [[ "$e1" =~ ^$K-[0-9a-f]{16}$ ]] && grep -q "first fill" <<<"$out" &&
  ok "a miss's fill is saved as KEY-<digest of its files>" || fail "a miss's fill is saved as KEY-<digest of its files>" "$po"$'\n'"$out"
plan_run "$H" "$S"
[ "$po" = save=false ] && grep -q "is left to save" <<<"$out" && ok "run again, with no use left, it saves nothing" ||
  fail "run again, with no use left, it saves nothing" "$po $out"
H2="$tmp/h2"; S2="$tmp/sp2"
mkuse "$S2" 1 "$K" "" false
fill "$H2" org/x/1/x-1.jar org/x/1/x-1.pom org/x/1/_remote.repositories
plan_run "$H2" "$S2"
[ "$(sed -n 's/^key=//p' <<<"$po")" = "$e1" ] && ok "two cells that fetched the same files name the same entry" ||
  fail "two cells that fetched the same files name the same entry" "$e1 vs $po"
H="$tmp/h3"; S="$tmp/sp3"
fill "$H" org/x/1/x-1.jar
mkuse "$S" 1 "$K" "$e1" true; manifest "$H" "$S/001"
plan_run "$H" "$S"
[ "$po" = save=false ] && grep -q "nothing to save" <<<"$out" && ok "a hit that added nothing is not saved" || fail "a hit that added nothing is not saved" "$po $out"
H="$tmp/h4"; S="$tmp/sp4"
fill "$H" org/x/1/x-1.jar
mkuse "$S" 1 "$K" "$e1" true; manifest "$H" "$S/001"
fill "$H" org/x/1/x-1.jar.lastUpdated org/y/2/resolver-status.properties org/y/2/_remote.repositories org/y/2/y-2.jar.part
plan_run "$H" "$S"
[ "$po" = save=false ] && ok "bookkeeping files alone are not saved" || fail "bookkeeping files alone are not saved" "$po $out"
H="$tmp/h5"; S="$tmp/sp5"
fill "$H" org/x/1/x-1.jar
mkuse "$S" 1 "$K" "$e1" true; manifest "$H" "$S/001"
echo org.y:y:jar:2 >"$S/001/unresolved"
fill "$H" org/y/2/y-2.jar
printf '0 fallback 001\n' >"$S/runs"
plan_run "$H" "$S"
e5=$(sed -n 's/^key=//p' <<<"$po")
grep -qx save=true <<<"$po" && [[ "$e5" =~ ^$K-[0-9a-f]{16}$ ]] && [ "$e5" != "$e1" ] && grep -q "could not serve the build" <<<"$out" &&
  ok "a hit that fetched files its cache could not serve is saved as a new entry of the key" ||
  fail "a hit that fetched files its cache could not serve is saved as a new entry of the key" "$po"$'\n'"$out"
H="$tmp/h5b"; S="$tmp/sp5b"
fill "$H" org/x/1/x-1.jar org/x/1/_remote.repositories
mkuse "$S" 1 "$K" "$e1" true; manifest "$H" "$S/001"
echo org.x:x:jar:1 >"$S/001/unresolved"
echo 'x-1.jar>central=' >>"$H/.m2/repository/org/x/1/_remote.repositories"
printf '0 fallback 001\n' >"$S/runs"
plan_run "$H" "$S"
e5b=$(sed -n 's/^key=//p' <<<"$po")
grep -qx save=true <<<"$po" && [[ "$e5b" =~ ^$K-[0-9a-f]{16}$ ]] && [ "$e5b" != "$e1" ] &&
  ok "a fallback that added no file (its files fetched again in place) is saved too" ||
  fail "a fallback that added no file (its files fetched again in place) is saved too" "$po"$'\n'"$out"
# Two builds share the key: X heals joda into the base entry E0, Y heals gson
# into E0 too and saves after X. X's next run restores Y's entry, lacks joda,
# and heals it again: that save must not be refused as X's first one.
heal_plan() { # heal_plan HOME STATES RESTORED UNRESOLVED NEWFILE...: a heal's plan; its entry in $entry
  local h=$1 st=$2 from=$3 unres=$4
  shift 4
  rm -rf "$h" "$st"
  fill "$h" org/base/1/base-1.jar
  mkuse "$st" 1 "$K" "$from" true; manifest "$h" "$st/001"
  fill "$h" "$@"
  echo "$unres" >"$st/001/unresolved"
  printf '0 fallback 001\n' >"$st/runs"
  plan_run "$h" "$st"
  entry=$(sed -n 's/^key=//p' <<<"$po")
}
heal_plan "$tmp/hx1" "$tmp/sx1" "$K-e0" joda-time:joda-time:jar:2.12.7 joda-time/joda-time/2.12.7/joda-time-2.12.7.jar; x1=$entry
heal_plan "$tmp/hx1b" "$tmp/sx1b" "$K-e0" joda-time:joda-time:jar:2.12.7 joda-time/joda-time/2.12.7/joda-time-2.12.7.jar; x1b=$entry
heal_plan "$tmp/hx2" "$tmp/sx2" "$K-ey" joda-time:joda-time:jar:2.12.7 joda-time/joda-time/2.12.7/joda-time-2.12.7.jar; x2=$entry
[ -n "$x1" ] && [ -n "$x2" ] && [ "$x1" != "$x2" ] &&
  ok "a heal names its entry from the entry it restored too: X healing joda into Y's entry is a new entry, not X's first one" ||
  fail "a heal names its entry from the entry it restored too: X healing joda into Y's entry is a new entry, not X's first one" "x1=$x1 x2=$x2"
[ -n "$x1" ] && [ "$x1" = "$x1b" ] && ok "cells that restored the same entry and fetched the same files name the same entry" ||
  fail "cells that restored the same entry and fetched the same files name the same entry" "$x1 $x1b"
H="$tmp/h5c"; S="$tmp/sp5c"
fill "$H" org/x/1/x-1.jar
mkuse "$S" 1 "$K" "$e1" true; manifest "$H" "$S/001"
mkuse "$S" 2 "${K}2" "" false; manifest "$H" "$S/002"
printf '0 offline 001\n0 online 002\n' >"$S/runs"
plan_run "$H" "$S"
first=$po
plan_run "$H" "$S"
e5c=$(sed -n 's/^key=//p' <<<"$po")
[ "$first" = save=false ] && grep -qx save=true <<<"$po" && [[ "$e5c" =~ ^${K}2-[0-9a-f]{16}$ ]] &&
  ok "a later use's miss whose build passed is saved under its key, though another use's repository held all it needed" ||
  fail "a later use's miss whose build passed is saved under its key, though another use's repository held all it needed" "$po"$'\n'"$out"
H="$tmp/h5d"; S="$tmp/sp5d"
mkuse "$S" 1 "$K" "" false
fill "$H" org/x/1/x-1.jar; manifest "$H" "$S/001"
plan_run "$H" "$S"
[ "$po" = save=false ] && grep -q "no mvn run passed after it" <<<"$out" &&
  ok "a miss whose lane ran no build (a skipped cell) saves nothing" ||
  fail "a miss whose lane ran no build (a skipped cell) saves nothing" "$po $out"
H="$tmp/h5e"; S="$tmp/sp5e"
fill "$H" org/x/1/x-1.jar
mkuse "$S" 1 "$K" "$e1" true; manifest "$H" "$S/001"
echo org.y:y:jar:2 >"$S/001/unresolved"; echo org.y:y:jar:2 >"$S/001/heal-failed"
fill "$H" org/y/2/y-2.jar
printf '0 fallback 001\n' >"$S/runs"
plan_run "$H" "$S"
[ "$rc" -ne 0 ] && [ "$po" = save=false ] &&
  grep -q "^::error title=Maven repository not saved::a heal in this job did not take: offline, mvn still could not resolve org.y:y:jar:2" <<<"$out" &&
  ok "a heal that did not take fails the save step, naming what, and saves nothing" ||
  fail "a heal that did not take fails the save step, naming what, and saves nothing" "rc=$rc $po"$'\n'"$out"
H="$tmp/h6"; S="$tmp/sp6"
mkuse "$S" 1 "$K" "" false
fill "$H" org/x/1/x-1.jar
printf '0 online 001\n1 online 001\n' >"$S/runs"
plan_run "$H" "$S"
[ "$po" = save=false ] && grep -q '^::warning title=Maven repository not saved::the last mvn run in this job after the Maven cache step' <<<"$out" &&
  ok "a last run that failed stops the save" || fail "a last run that failed stops the save" "$po $out"
H="$tmp/h7"; S="$tmp/sp7"
mkuse "$S" 1 "$K" "" false
fill "$H" org/x/1/x-1.jar
printf '1 online 001\n0 online 001\n' >"$S/runs"
plan_run "$H" "$S"
grep -qx save=true <<<"$po" && ok "one that failed and then passed does not" || fail "one that failed and then passed does not" "$po $out"

# ---- the real Maven -----------------------------------------------------
if real_v=$(mvn -B -v 2>&1); then
  real=$(printf '%s\n' "$real_v" | sed -n 's/^Apache Maven \([0-9][^ ]*\).*/\1/p')
  real_key=$(cd "$tmp/ws" && RUNNER_OS=Linux RUNNER_ARCH=X64 MAVEN_CACHE_JAVA_VERSION=17 \
    MAVEN_CACHE_DEPENDENCY_PATH=a.xml bash "$script" 2>/dev/null)
  case "$real_key" in
    "maven-repository-Linux-X64-maven$real-java17-"*) [ -n "$real" ] && ok "the real mvn's key names its version ($real)" ||
      fail "the real mvn's key names its version" "$real_v" ;;
    *) fail "the real mvn's key names its version ($real)" "$real_key" ;;
  esac
else
  fail "the real mvn's key names its version: mvn -B -v failed" "$real_v"
fi

echo "$((n - fails))/$n passed"
[ "$fails" -eq 0 ]
