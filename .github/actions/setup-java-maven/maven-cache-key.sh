#!/usr/bin/env bash
# maven-cache-key.sh: prints the actions/cache key that setup-java-maven
# saves and restores ~/.m2/repository under. Run by that action, or by hand
# from the workspace root:
#
#   RUNNER_OS=Linux RUNNER_ARCH=X64 MAVEN_CACHE_JAVA_VERSION=17 \
#     MAVEN_CACHE_DEPENDENCY_PATH=samples-java/mysql-dual-conn/pom.xml \
#     MAVEN_CACHE_MAVEN_ARGS='clean package -Dmaven.test.skip=true' \
#     bash .github/actions/setup-java-maven/maven-cache-key.sh
#
# MAVEN_CACHE_DEPENDENCY_PATH: the pom.xml files the build reads its
# dependencies from, one per line (blank lines and surrounding spaces are
# ignored). Every one must exist: a key that silently left one out would not
# change when its dependencies do.
# MAVEN_CACHE_JAVA_VERSION: the JDK the build runs on, as setup-java takes it.
# MAVEN_CACHE_MAVEN_ARGS: the mvn command lines the build runs, one per line
# (maven-args.sh says which flags do not count). The goals, profiles and -D
# properties decide which plugins and artifacts the build resolves, as much
# as the poms do: a lane that adds a goal needs a cache of its own.
#
# The key is maven-repository-<os>-<arch>-maven<version>-java<version>-<hash>.
# The Maven version is the one `mvn` on PATH reports: the one the build runs.
# The hash is of the mvn command lines, the poms, and the Maven configuration
# the build reads: the user and global settings.xml, and the
# .mvn/maven.config, jvm.config and extensions.xml of each pom's project,
# found as Maven finds them (the nearest directory at or above the pom's that
# has a .mvn). The command lines and poms decide what the build resolves. The
# Maven and its configuration decide whether a local repository filled by
# one build is of use to another, which needs it to track files the same way:
#
# Maven records, in each directory's _remote.repositories, the repository a
# file came from, and uses a cached file only when that record names a
# repository of the current build. Maven Resolver 2.0.23 and later (Maven
# 3.10.0 ships 2.0.24) write that record URL-qualified
# (aether.system.trackingRepositoryKeyFunction, default nid_hurl:
# `x.pom>central-<hash of the URL>=`); Resolver 1.9 (every Maven 3.9) looks
# for the bare id (`x.pom>central=`). So a 3.9 build finds every file a 3.10
# build cached tracked but "present, but unavailable", and fetches it from
# Maven Central again. A mirror in settings.xml renames the repository a
# file comes from (to the mirror's id), with the same effect; a settings
# profile can add repositories, and .mvn/maven.config can set the resolver's
# own properties. The Maven version also picks the default versions of the
# build's own plugins, so it is part of what the build downloads.
#
# actions/setup-java's own `cache: maven` key is the OS, the arch and the
# pom's hash only, and a cache entry is never replaced: when GitHub's runner
# image moved from Maven 3.10.0 (ubuntu-24.04) to Maven 3.9.15
# (ubuntu-26.04), every Java job kept restoring a 3.10.0 cache it could not
# use and fetched its whole repository from Central on every run, until
# Central answered 429 Too Many Requests.
#
# No credential reaches the key or the log. A settings.xml is hashed with
# its <servers> and <properties> elements, every <username>, <password>,
# <passphrase> and <privateKey>, and the user:password@ of every <url> left
# out, wherever they are: they hold credentials, and only the properties
# could change which repositories a build has. Environment variables
# (MAVEN_OPTS, MAVEN_ARGS) are not read for the same reason. What the key
# leaves out is caught where it matters: a declared command line runs
# offline on a restored cache (bin/mvn), so a cache that cannot serve it is
# named, re-resolved online once, and saved anew (save/save-plan.sh). The
# log names the files hashed and the command lines, never what the files
# hold.
set -euo pipefail

: "${RUNNER_OS:?RUNNER_OS is not set}" "${RUNNER_ARCH:?RUNNER_ARCH is not set}"
java_version=${MAVEN_CACHE_JAVA_VERSION:?MAVEN_CACHE_JAVA_VERSION is not set}
die() { printf '::error::maven-cache-key.sh: %s\n' "$1" >&2; exit 1; }
# shellcheck source=maven-args.sh
. "$(dirname "${BASH_SOURCE[0]}")/maven-args.sh"
invocations=$(maven_args_lines "${MAVEN_CACHE_MAVEN_ARGS:-}")
[ -n "$invocations" ] || die "MAVEN_CACHE_MAVEN_ARGS names no mvn command line (maven-args)"

files=()
while IFS= read -r line; do
  line="${line#"${line%%[![:space:]]*}"}"
  line="${line%"${line##*[![:space:]]}"}"
  [ -n "$line" ] && files+=("$line")
done <<<"${MAVEN_CACHE_DEPENDENCY_PATH:-}"
[ "${#files[@]}" -gt 0 ] || die "MAVEN_CACHE_DEPENDENCY_PATH names no file"
for f in "${files[@]}"; do
  [ -f "$f" ] || die "$f (in cache-dependency-path) is not a file"
done
command -v perl >/dev/null || die "perl is needed to leave the credentials out of settings.xml"

# Captured whole before it is read: under pipefail, a reader that stops
# early could fail mvn with SIGPIPE. -B: no color codes in the output.
# The job's own mvn: after a first use of setup-java-maven, `mvn` on PATH is
# its bin/mvn, which runs this one.
mvn=${SETUP_JAVA_MAVEN_REAL_MVN:-mvn}
if ! mvn_v=$("$mvn" -B -v 2>&1); then
  printf '::error::maven-cache-key.sh: mvn -B -v failed:\n%s\n' "$mvn_v" >&2
  exit 1
fi
maven=$(printf '%s\n' "$mvn_v" | sed -n 's/^Apache Maven \([0-9][^ ]*\).*/\1/p')
case "$maven" in
  '' | *[!0-9A-Za-z.-]*)
    printf '::error::maven-cache-key.sh: no single Maven version in `mvn -B -v`:\n%s\n' "$mvn_v" >&2
    exit 1
    ;;
esac
maven_home=$(printf '%s\n' "$mvn_v" | sed -n 's/^Maven home: //p')
case "$maven_home" in
  '' | *$'\n'*)
    printf '::error::maven-cache-key.sh: no single "Maven home:" in `mvn -B -v`:\n%s\n' "$mvn_v" >&2
    exit 1
    ;;
esac
[ -d "$maven_home" ] || die "Maven home $maven_home (from mvn -B -v) is not a directory"

# settings_digest FILE: the sha256 of FILE without the elements that hold
# credentials (above).
# A self-closing element (<properties/>) is cut on its own first, so that the
# match of an element with content never starts there and runs on to the
# next one's end tag.
settings_digest() {
  perl -0777 -pe '
    s{<(servers|properties|username|password|passphrase|privateKey)\b[^>]*/>}{}gs;
    s{<(servers|properties)\b[^>]*>.*?</\1\s*>}{}gs;
    s{<(username|password|passphrase|privateKey)\b[^>]*>.*?</\1\s*>}{}gs;
    s{(<url>\s*[A-Za-z][A-Za-z0-9+.-]*://)[^/@<\s]*@}{$1}g;
  ' "$1" | sha256sum | cut -c1-64
}

parts=()
hashed=()
while IFS= read -r inv; do parts+=("mvn $inv"); done <<<"$invocations"
for f in "${files[@]}"; do
  parts+=("pom $f $(sha256sum <"$f" | cut -c1-64)")
  hashed+=("$f")
done
for s in "user-settings:$HOME/.m2/settings.xml" "global-settings:$maven_home/conf/settings.xml"; do
  label=${s%%:*} path=${s#*:}
  if [ -f "$path" ]; then
    parts+=("$label $(settings_digest "$path")")
    hashed+=("$path (credentials and properties left out)")
  else
    parts+=("$label none")
  fi
done
for f in "${files[@]}"; do
  d=$(cd "$(dirname "$f")" && pwd)
  while [ ! -d "$d/.mvn" ] && [ "$d" != / ]; do d=$(dirname "$d"); done
  for c in maven.config jvm.config extensions.xml; do
    if [ -d "$d/.mvn" ] && [ -f "$d/.mvn/$c" ]; then
      parts+=("project-of $f .mvn/$c $(sha256sum <"$d/.mvn/$c" | cut -c1-64)")
      hashed+=("$d/.mvn/$c")
    else
      parts+=("project-of $f .mvn/$c none")
    fi
  done
done

digest=$(printf '%s\n' "${parts[@]}" | sha256sum | cut -c1-64)
{
  echo "maven-cache-key.sh: Maven $maven ($maven_home), JDK $java_version; mvn command lines:"
  sed 's/^/  mvn /' <<<"$invocations"
  echo "hashed:"
  printf '  %s\n' "${hashed[@]}"
} >&2
echo "maven-repository-${RUNNER_OS}-${RUNNER_ARCH}-maven${maven}-java${java_version}-${digest}"
