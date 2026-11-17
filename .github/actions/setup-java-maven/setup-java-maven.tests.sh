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
#   - another JDK, a changed or added pom, another mvn command line (an added
#     goal), a mirror in the user's or Maven's own settings.xml (also after a
#     self-closing <properties/>), and a .mvn/maven.config, jvm.config or
#     extensions.xml of the pom's project (beside it or above it) each give
#     another key;
#   - the same inputs, blank lines and spaces around names, flags that change
#     only output or update checks (-B -U -q -e), the order of the command
#     lines, a .mvn above the nearest one, and other credentials, properties
#     or URL user:password@ give the same key, and no credential is printed;
#   - the log names the files hashed and the command lines;
#   - a pom that is not there, no command line, a failing `mvn -v`, one with
#     no version (or two) or no Maven home fail and say why;
#   - with SETUP_JAVA_MAVEN_REAL_MVN set, it asks that mvn, not the one on
#     PATH.
# restored.sh: each use gets its own record (key, entry, hit, command lines,
# the restored files); the first puts bin/ on PATH and names the real mvn,
# a second keeps both; with bin/mvn on PATH and no real mvn named it refuses;
# an entry of another key is an error.
# bin/mvn, in front of a fake mvn:
#   - a declared command line on a restored cache runs with --offline and
#     every argument as given, and one on a miss runs online;
#   - one nobody declared runs online, with a warning that leaves -D values
#     out;
#   - of two uses, the later one that declared a command line decides, so a
#     hit's offline run does not reach a later use's miss;
#   - an offline run that cannot resolve an artifact says so (naming it, the
#     key and the entry), runs once more online, and keeps its code; another
#     failure is not run again;
#   - it cannot loop: a real mvn that is bin/mvn is refused, and an mvn run
#     from inside it runs the real one as it is; -v runs as it is.
# save-plan.sh: nothing ran, nothing to save; a fill is saved as KEY-<digest
# of the new files>, the same files giving the same entry; a hit that added
# nothing is not saved; one that fetched files after its cache could not
# serve it is, even if it added no file; a miss whose build passed is, even
# if another use's repository held all it needed, and one whose lane ran no
# build is not; bookkeeping files alone are not; a command line whose last run
# failed stops the save, one that failed and then passed does not; run twice,
# it saves once.
# The real mvn: the key names the version it prints.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$here/maven-cache-key.sh"
restored="$here/restored.sh"
shim="$here/bin/mvn"
plan="$here/save/save-plan.sh"
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
ARGS='-B -U clean package -Dmaven.test.skip=true -q'

# key MAVEN_VERSION JAVA DEPS [MAVEN_ARGS [STUB_OUTPUT [STUB_RC]]]: a run with
# the stub printing that version (or STUB_OUTPUT); stdout in $out, stderr in
# $err, exit in $rc.
key() {
  local stub_out=${5:-$(mvn_out "$1")}
  out=$(cd "$tmp/ws" && HOME="$tmp/home" PATH="$tmp/bin:$PATH" STUB_MVN_OUT="$stub_out" STUB_MVN_RC="${6:-0}" \
    RUNNER_OS=Linux RUNNER_ARCH=X64 MAVEN_CACHE_JAVA_VERSION="$2" MAVEN_CACHE_DEPENDENCY_PATH="$3" \
    MAVEN_CACHE_MAVEN_ARGS="${4-$ARGS}" bash "$script" 2>"$tmp/err")
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
key 3.9.15 17 a.xml; check "the same Maven, JDK, poms, command lines and configuration give the same key" same "$base"
grep -q -F "$tmp/home/.m2/settings.xml (credentials and properties left out)" <<<"$err" &&
  grep -q -F "$tmp/mavenhome/conf/settings.xml" <<<"$err" && grep -q -F "  a.xml" <<<"$err" &&
  grep -q -F "  mvn clean package -Dmaven.test.skip=true" <<<"$err" &&
  ok "the log names the files hashed and the command lines" || fail "the log names the files hashed and the command lines" "$err"
key 3.9.15 8 a.xml; check "another JDK gives another key" differs "$base"
key 3.9.15 17 $'a.xml\nb.xml'; k_ab=$out; check "an added pom gives another key" differs "$base"
key 3.9.15 17 $'\n  a.xml  \n\n b.xml\n'; check "blank lines and spaces around the names do not change the key" same "$k_ab"
cp "$tmp/ws/a.xml" "$tmp/a.orig"
printf '<project><artifactId>a</artifactId><version>2</version></project>\n' >"$tmp/ws/a.xml"
key 3.9.15 17 a.xml; check "a changed pom gives another key" differs "$base"
cp "$tmp/a.orig" "$tmp/ws/a.xml"

# The mvn command lines.
key 3.9.15 17 a.xml '-B -U clean package dependency:tree -Dmaven.test.skip=true -q'
check "an added goal gives another key" differs "$base"
key 3.9.15 17 a.xml '-B -U clean package -Dmaven.test.skip=false -q'
check "another -D property gives another key" differs "$base"
key 3.9.15 17 a.xml '-P it -B -U clean package -Dmaven.test.skip=true -q'
check "a profile gives another key" differs "$base"
key 3.9.15 17 a.xml 'clean package -Dmaven.test.skip=true'
check "-B, -U and -q do not change the key" same "$base"
key 3.9.15 17 a.xml '--batch-mode -e --update-snapshots clean   package -Dmaven.test.skip=true --quiet'
check "their long forms, -e and spacing do not either" same "$base"
key 3.9.15 17 a.xml $'clean package -Dmaven.test.skip=true\n-f b.xml package'; k_two=$out
key 3.9.15 17 a.xml $'-f b.xml package\n\n-q clean package -Dmaven.test.skip=true\n-f b.xml package'
check "the order and repeats of the command lines do not change the key" same "$k_two"
key 3.9.15 17 a.xml ''; check "no command line fails" failed_with 'names no mvn command line'
key 3.9.15 17 a.xml '-B -q'; check "a command line of flags alone is none, and fails" failed_with 'names no mvn command line'

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

key 3.9.15 17 $'a.xml\nmissing/pom.xml'; check "a pom that is not there fails, naming it" failed_with 'missing/pom.xml.*is not a file'
key 3.9.15 17 $'\n  \n'; check "an empty list of poms fails" failed_with 'names no file'
key 3.9.15 17 a.xml "$ARGS" "$(mvn_out 3.9.15)" 1; check "a failing mvn -v fails" failed_with 'mvn -B -v failed'
key 3.9.15 17 a.xml "$ARGS" "The JAVA_HOME environment variable is not defined correctly"
check "an mvn -v with no version fails" failed_with 'no single Maven version'
key 3.9.15 17 a.xml "$ARGS" "$(mvn_out 3.9.15)"$'\n'"$(mvn_out 3.10.0)"
check "an mvn -v with two versions fails" failed_with 'no single Maven version'
key 3.9.15 17 a.xml "$ARGS" "Apache Maven 3.9.15 (98b2cdbfdb5f1ac8781f537ea9acccaed7922349)"
check "an mvn -v with no Maven home fails" failed_with 'no single "Maven home:"'
# SETUP_JAVA_MAVEN_REAL_MVN, as a second use of the action sees it: `mvn` on PATH is bin/mvn.
mkdir -p "$tmp/shadow"
printf '#!/usr/bin/env bash\necho "the mvn on PATH ran" >&2\nexit 9\n' >"$tmp/shadow/mvn"
chmod +x "$tmp/shadow/mvn"
out=$(cd "$tmp/ws" && HOME="$tmp/home" PATH="$tmp/shadow:$PATH" SETUP_JAVA_MAVEN_REAL_MVN="$tmp/bin/mvn" STUB_MVN_OUT="$(mvn_out 3.9.15)" \
  RUNNER_OS=Linux RUNNER_ARCH=X64 MAVEN_CACHE_JAVA_VERSION=17 MAVEN_CACHE_DEPENDENCY_PATH=a.xml MAVEN_CACHE_MAVEN_ARGS="$ARGS" \
  bash "$script" 2>"$tmp/err"); rc=$?; err=$(cat "$tmp/err")
check "with SETUP_JAVA_MAVEN_REAL_MVN set, the key asks that mvn, not the one on PATH" same "$base"

# ---- restored.sh ----------------------------------------------------------
# restore_run NAME KEY MATCHED ARGS [PATH_PREFIX] [REAL]: restored.sh in job NAME
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
  out=$(env HOME="$j/home" RUNNER_TEMP="$j/temp" GITHUB_ENV="$j/env" GITHUB_PATH="$j/path" PATH="${5:-}$tmp/bin:$PATH" \
    KEY="$2" MATCHED="$3" MAVEN_CACHE_MAVEN_ARGS="$4" "${envs[@]}" bash "$restored" 2>"$tmp/err")
  rc=$?
  err=$(cat "$tmp/err")
}
K=maven-repository-Linux-X64-maven3.9.15-java17-aaaa
restore_run miss "$K" "" "$ARGS"
d="$tmp/job-miss/temp/setup-java-maven/001"
[ "$rc" -eq 0 ] && [ "$(cat "$d/hit")" = false ] && [ "$(cat "$d/key")" = "$K" ] && [ ! -s "$d/manifest" ] &&
  [ "$(cat "$d/declared")" = "clean package -Dmaven.test.skip=true" ] &&
  grep -qx "SETUP_JAVA_MAVEN_REAL_MVN=$tmp/bin/mvn" "$tmp/job-miss/env" &&
  grep -qx "SETUP_JAVA_MAVEN_STATES=$tmp/job-miss/temp/setup-java-maven" "$tmp/job-miss/env" &&
  [ "$(cat "$tmp/job-miss/path")" = "$here/bin" ] &&
  ok "a miss is recorded (no entry, its command lines), and the first use names the real mvn and puts bin/ on PATH" ||
  fail "a miss is recorded (no entry, its command lines), and the first use names the real mvn and puts bin/ on PATH" "rc=$rc $out"$'\n'"$err"
mkdir -p "$tmp/job-hit/home/.m2/repository/org/x/1"
touch "$tmp/job-hit/home/.m2/repository/org/x/1/x-1.jar"
restore_run hit "$K" "$K-0123456789abcdef" "$ARGS"
d="$tmp/job-hit/temp/setup-java-maven/001"
[ "$rc" -eq 0 ] && [ "$(cat "$d/hit")" = true ] && [ "$(cat "$d/matched")" = "$K-0123456789abcdef" ] &&
  [ "$(cat "$d/manifest")" = "./org/x/1/x-1.jar" ] && grep -q '^::notice title=Maven cache restored::' <<<"$out" &&
  ok "a hit is recorded with the files restored, and says so" || fail "a hit is recorded with the files restored, and says so" "rc=$rc $out"$'\n'"$err"
# A second use in the same job: mvn on PATH is now bin/mvn.
restore_run hit "${K}2" "" "-f b.xml package" "$here/bin:"
[ "$rc" -eq 0 ] && [ -d "$tmp/job-hit/temp/setup-java-maven/002" ] && [ "$(grep -c . "$tmp/job-hit/path")" = 1 ] &&
  [ "$(grep -c '^SETUP_JAVA_MAVEN_REAL_MVN=' "$tmp/job-hit/env")" = 1 ] &&
  grep -qx "SETUP_JAVA_MAVEN_REAL_MVN=$tmp/bin/mvn" "$tmp/job-hit/env" &&
  ok "a second use gets its own record, and keeps the real mvn and bin/ on PATH once" ||
  fail "a second use gets its own record, and keeps the real mvn and bin/ on PATH once" "rc=$rc"$'\n'"$(cat "$tmp/job-hit/env")"$'\n'"$err"
restore_run loop "$K" "" "$ARGS" "$here/bin:"
[ "$rc" -ne 0 ] && grep -q "is setup-java-maven's own bin/mvn" <<<"$err" &&
  ok "with bin/mvn first on PATH and no real mvn named, it refuses" || fail "with bin/mvn first on PATH and no real mvn named, it refuses" "rc=$rc $err"
restore_run other "$K" "maven-repository-Linux-X64-maven3.9.15-java17-bbbb-1" "$ARGS"
[ "$rc" -ne 0 ] && grep -q "is not one of key" <<<"$err" && ok "an entry of another key is an error" ||
  fail "an entry of another key is an error" "rc=$rc $err"

# ---- bin/mvn --------------------------------------------------------------
# A fake real mvn: records its arguments (one per line, a run per file) and
# acts on FAKE: ok; missing (offline: cannot resolve org.x:y:jar:1.0; online:
# fetches it); compile (fails, not for want of an artifact).
cat >"$tmp/bin/real-mvn" <<'EOF'
#!/usr/bin/env bash
n=$(ls "$CALLS" 2>/dev/null | wc -l)
printf '%s\n' "$@" >"$CALLS/$((n + 1))"
off=no
for a in "$@"; do [ "$a" = --offline ] && off=yes; done
case "$FAKE:$off" in
  ok:*) exit 0 ;;
  missing:yes)
    echo "[ERROR] Failed to execute goal on project p: Could not resolve dependencies for project p: The following artifacts could not be resolved: org.x:y:jar:1.0 (present, but unavailable): Cannot access central (https://repo.maven.apache.org/maven2) in offline mode and the artifact org.x:y:jar:1.0 has not been downloaded from it before. -> [Help 1]"
    exit 1 ;;
  missing:no) exit 0 ;;
  compile:*) echo "[ERROR] COMPILATION ERROR : p/A.java:[3,1] class, interface, or enum expected"; exit 1 ;;
esac
EOF
chmod +x "$tmp/bin/real-mvn"
# states DIR: USE=KEY|MATCHED|HIT|LINES...: records as restored.sh writes them.
mkuse() { # mkuse STATES N KEY MATCHED HIT DECLARED
  local d
  d=$(printf '%s/%03d' "$1" "$2")
  mkdir -p "$d"
  printf '%s\n' "$3" >"$d/key"; printf '%s\n' "$4" >"$d/matched"; printf '%s\n' "$5" >"$d/hit"
  printf '%s\n' "$6" >"$d/declared"; : >"$d/manifest"
}
shim_run() { # shim_run STATES FAKE ARGS...: bin/mvn; stdout+stderr in $out, exit in $rc, calls in $tmp/calls
  local st=$1 fake=$2
  shift 2
  rm -rf "$tmp/calls"; mkdir -p "$tmp/calls"
  out=$(CALLS="$tmp/calls" FAKE="$fake" SETUP_JAVA_MAVEN_REAL_MVN="$tmp/bin/real-mvn" SETUP_JAVA_MAVEN_STATES="$st" "$shim" "$@" 2>&1)
  rc=$?
  err=$out
}
calls() { ls "$tmp/calls" | wc -l | tr -d ' '; }
call() { cat "$tmp/calls/$1"; }
S="$tmp/st1"
mkuse "$S" 1 "$K" "$K-01" true "clean package -Dmaven.test.skip=true"
shim_run "$S" ok -B -U clean package '-Dname=a b' -Dmaven.test.skip=true -q
# '-Dname=a b' is not declared: its words are "-Dname=a" "b".
[ "$rc" -eq 0 ] && [ "$(calls)" = 1 ] && [ "$(head -1 "$tmp/calls/1")" != --offline ] && grep -q '^::warning title=mvn not covered' <<<"$out" &&
  ok "a command line nobody declared runs online, with a warning" || fail "a command line nobody declared runs online, with a warning" "rc=$rc calls=$(calls)"$'\n'"$out"
grep -q -F -- '-Dname=...' <<<"$out" && ! grep -q -F 'a b' <<<"$out" && grep -q -F -- '-Dmaven.test.skip=...' <<<"$out" &&
  ok "that warning leaves the -D values out" || fail "that warning leaves the -D values out" "$out"
shim_run "$S" ok -B -U clean package -Dmaven.test.skip=true -q
[ "$rc" -eq 0 ] && [ "$(calls)" = 1 ] && [ "$(call 1)" = $'--offline\n-B\n-U\nclean\npackage\n-Dmaven.test.skip=true\n-q' ] &&
  ! grep -q '^::' <<<"$out" && grep -qE "^0 offline [0-9a-f]{16} 001$" "$S/runs" &&
  ok "a declared command line on a restored cache runs once, with --offline and every argument as given" ||
  fail "a declared command line on a restored cache runs once, with --offline and every argument as given" "rc=$rc calls=$(calls) $(call 1 2>/dev/null | paste -sd' ')"$'\n'"$out"
S="$tmp/st2"
mkuse "$S" 1 "$K" "" false "clean package -Dmaven.test.skip=true"
shim_run "$S" ok clean package -Dmaven.test.skip=true
[ "$rc" -eq 0 ] && [ "$(call 1)" = $'clean\npackage\n-Dmaven.test.skip=true' ] && ! grep -q '^::' <<<"$out" &&
  ok "a declared command line on a miss runs online" || fail "a declared command line on a miss runs online" "rc=$rc $(call 1 | paste -sd' ')"$'\n'"$out"
# Two uses: the first hit and declared X and Y; the second missed and declared Y.
S="$tmp/st3"
mkuse "$S" 1 "$K" "$K-01" true $'clean package\n-f b.xml package'
mkuse "$S" 2 "${K}2" "" false "-f b.xml package"
shim_run "$S" ok clean package
x_off=$(head -1 "$tmp/calls/1")
shim_run "$S" ok -f b.xml package
y_first=$(head -1 "$tmp/calls/1")
[ "$x_off" = --offline ] && [ "$y_first" = -f ] &&
  ok "of two uses, the later one that declared a command line decides: the first's hit runs offline, the second's miss online" ||
  fail "of two uses, the later one that declared a command line decides: the first's hit runs offline, the second's miss online" "x=$x_off y=$y_first"
# An offline run that cannot resolve an artifact.
S="$tmp/st4"
mkuse "$S" 1 "$K" "$K-01" true "clean package"
shim_run "$S" missing -q clean package
[ "$rc" -eq 0 ] && [ "$(calls)" = 2 ] && [ "$(head -1 "$tmp/calls/1")" = --offline ] && [ "$(call 2)" = $'-q\nclean\npackage' ] &&
  grep -q "^::error title=Maven cache cannot serve this build::mvn clean package could not resolve, offline, org.x:y:jar:1.0 from the Maven cache $K-01 (key $K)" <<<"$out" &&
  [ "$(cat "$S/001/unresolved")" = org.x:y:jar:1.0 ] && grep -q "0 fallback" "$S/runs" &&
  ok "an offline run that cannot resolve an artifact names it, the key and the entry, and runs once more online" ||
  fail "an offline run that cannot resolve an artifact names it, the key and the entry, and runs once more online" "rc=$rc calls=$(calls)"$'\n'"$out"
S="$tmp/st5"
mkuse "$S" 1 "$K" "$K-01" true "clean package"
shim_run "$S" compile clean package
[ "$rc" -eq 1 ] && [ "$(calls)" = 1 ] && ! grep -q '^::error' <<<"$out" && [ ! -e "$S/001/unresolved" ] && grep -q "^1 offline" "$S/runs" &&
  ok "an offline run that fails for another reason fails, once, with its code" ||
  fail "an offline run that fails for another reason fails, once, with its code" "rc=$rc calls=$(calls)"$'\n'"$out"
# It cannot loop.
out=$(SETUP_JAVA_MAVEN_REAL_MVN="$shim" SETUP_JAVA_MAVEN_STATES="$S" timeout 10 "$shim" clean package 2>&1); rc=$?
[ "$rc" -eq 2 ] && grep -q 'is this script; refusing' <<<"$out" && ok "a real mvn that is bin/mvn itself is refused" ||
  fail "a real mvn that is bin/mvn itself is refused" "rc=$rc $out"
rm -rf "$tmp/calls"; mkdir -p "$tmp/calls"
out=$(CALLS="$tmp/calls" FAKE=ok SETUP_JAVA_MAVEN_IN_MVN=1 SETUP_JAVA_MAVEN_REAL_MVN="$tmp/bin/real-mvn" SETUP_JAVA_MAVEN_STATES="$S" "$shim" clean package 2>&1); rc=$?
[ "$rc" -eq 0 ] && [ "$(call 1)" = $'clean\npackage' ] && ok "an mvn run from inside bin/mvn runs the real one as it is" ||
  fail "an mvn run from inside bin/mvn runs the real one as it is" "rc=$rc $(call 1 | paste -sd' ') $out"
shim_run "$S" ok -B -v
[ "$rc" -eq 0 ] && [ "$(call 1)" = $'-B\n-v' ] && [ -z "$out" ] && ok "mvn -v runs as it is, with no warning" ||
  fail "mvn -v runs as it is, with no warning" "rc=$rc $out"

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
mkuse "$S" 1 "$K" "" false "clean package"
fill "$H" org/x/1/x-1.jar org/x/1/x-1.pom org/x/1/_remote.repositories
plan_run "$H" "$S"
e1=$(sed -n 's/^key=//p' <<<"$po")
[ "$rc" -eq 0 ] && grep -qx save=true <<<"$po" && [[ "$e1" =~ ^$K-[0-9a-f]{16}$ ]] && grep -q "first fill" <<<"$out" &&
  ok "a miss's fill is saved as KEY-<digest of its files>" || fail "a miss's fill is saved as KEY-<digest of its files>" "$po"$'\n'"$out"
plan_run "$H" "$S"
[ "$po" = save=false ] && grep -q "already saved" <<<"$out" && ok "run twice, it saves once" || fail "run twice, it saves once" "$po $out"
H2="$tmp/h2"; S2="$tmp/sp2"
mkuse "$S2" 1 "$K" "" false "clean package"
fill "$H2" org/x/1/x-1.jar org/x/1/x-1.pom org/x/1/_remote.repositories
plan_run "$H2" "$S2"
[ "$(sed -n 's/^key=//p' <<<"$po")" = "$e1" ] && ok "two cells that fetched the same files name the same entry" ||
  fail "two cells that fetched the same files name the same entry" "$e1 vs $po"
H="$tmp/h3"; S="$tmp/sp3"
fill "$H" org/x/1/x-1.jar
mkuse "$S" 1 "$K" "$e1" true "clean package"; manifest "$H" "$S/001"
plan_run "$H" "$S"
[ "$po" = save=false ] && grep -q "nothing to save" <<<"$out" && ok "a hit that added nothing is not saved" || fail "a hit that added nothing is not saved" "$po $out"
H="$tmp/h4"; S="$tmp/sp4"
fill "$H" org/x/1/x-1.jar
mkuse "$S" 1 "$K" "$e1" true "clean package"; manifest "$H" "$S/001"
fill "$H" org/x/1/x-1.jar.lastUpdated org/y/2/resolver-status.properties org/y/2/_remote.repositories org/y/2/y-2.jar.part
plan_run "$H" "$S"
[ "$po" = save=false ] && ok "bookkeeping files alone are not saved" || fail "bookkeeping files alone are not saved" "$po $out"
H="$tmp/h5"; S="$tmp/sp5"
fill "$H" org/x/1/x-1.jar
mkuse "$S" 1 "$K" "$e1" true "clean package"; manifest "$H" "$S/001"
echo org.y:y:jar:2 >"$S/001/unresolved"
fill "$H" org/y/2/y-2.jar
printf '0 fallback abc\n' >"$S/runs"
plan_run "$H" "$S"
e5=$(sed -n 's/^key=//p' <<<"$po")
grep -qx save=true <<<"$po" && [[ "$e5" =~ ^$K-[0-9a-f]{16}$ ]] && [ "$e5" != "$e1" ] && grep -q "could not serve the build" <<<"$out" &&
  ok "a hit that fetched files its cache could not serve is saved as a new entry of the key" ||
  fail "a hit that fetched files its cache could not serve is saved as a new entry of the key" "$po"$'\n'"$out"
H="$tmp/h5b"; S="$tmp/sp5b"
fill "$H" org/x/1/x-1.jar org/x/1/_remote.repositories
mkuse "$S" 1 "$K" "$e1" true "clean package"; manifest "$H" "$S/001"
echo org.x:x:jar:1 >"$S/001/unresolved"
echo 'x-1.jar>central=' >>"$H/.m2/repository/org/x/1/_remote.repositories"
printf '0 fallback abc\n' >"$S/runs"
plan_run "$H" "$S"
e5b=$(sed -n 's/^key=//p' <<<"$po")
grep -qx save=true <<<"$po" && [[ "$e5b" =~ ^$K-[0-9a-f]{16}$ ]] && [ "$e5b" != "$e1" ] &&
  ok "a fallback that added no file (its files fetched again in place) is saved too" ||
  fail "a fallback that added no file (its files fetched again in place) is saved too" "$po"$'\n'"$out"
H="$tmp/h5c"; S="$tmp/sp5c"
fill "$H" org/x/1/x-1.jar
mkuse "$S" 1 "$K" "$e1" true "clean package"; manifest "$H" "$S/001"
mkuse "$S" 2 "${K}2" "" false "-f b.xml package"; manifest "$H" "$S/002"
printf '0 offline aaa 001\n0 online bbb 002\n' >"$S/runs"
plan_run "$H" "$S"
e5c=$(sed -n 's/^key=//p' <<<"$po")
grep -qx save=true <<<"$po" && [[ "$e5c" =~ ^${K}2-[0-9a-f]{16}$ ]] &&
  ok "a later use's miss whose build passed is saved under its key, though another use's repository held all it needed" ||
  fail "a later use's miss whose build passed is saved under its key, though another use's repository held all it needed" "$po"$'\n'"$out"
H="$tmp/h5d"; S="$tmp/sp5d"
mkuse "$S" 1 "$K" "" false "clean package"
fill "$H" org/x/1/x-1.jar; manifest "$H" "$S/001"
plan_run "$H" "$S"
[ "$po" = save=false ] && grep -q "no build it declared ran" <<<"$out" &&
  ok "a miss whose lane ran no build (a skipped cell) saves nothing" ||
  fail "a miss whose lane ran no build (a skipped cell) saves nothing" "$po $out"
H="$tmp/h6"; S="$tmp/sp6"
mkuse "$S" 1 "$K" "" false "clean package"
fill "$H" org/x/1/x-1.jar
printf '0 online aaa\n1 online bbb\n' >"$S/runs"
plan_run "$H" "$S"
[ "$po" = save=false ] && grep -q '^::warning title=Maven repository not saved::the last run of 1 mvn command line' <<<"$out" &&
  ok "a command line whose last run failed stops the save" || fail "a command line whose last run failed stops the save" "$po $out"
H="$tmp/h7"; S="$tmp/sp7"
mkuse "$S" 1 "$K" "" false "clean package"
fill "$H" org/x/1/x-1.jar
printf '1 online aaa\n0 online aaa\n' >"$S/runs"
plan_run "$H" "$S"
grep -qx save=true <<<"$po" && ok "one that failed and then passed does not" || fail "one that failed and then passed does not" "$po $out"

# ---- the real Maven -----------------------------------------------------
if real_v=$(mvn -B -v 2>&1); then
  real=$(printf '%s\n' "$real_v" | sed -n 's/^Apache Maven \([0-9][^ ]*\).*/\1/p')
  real_key=$(cd "$tmp/ws" && RUNNER_OS=Linux RUNNER_ARCH=X64 MAVEN_CACHE_JAVA_VERSION=17 \
    MAVEN_CACHE_DEPENDENCY_PATH=a.xml MAVEN_CACHE_MAVEN_ARGS="$ARGS" bash "$script" 2>/dev/null)
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
