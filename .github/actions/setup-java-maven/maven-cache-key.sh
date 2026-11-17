#!/usr/bin/env bash
# maven-cache-key.sh: prints the actions/cache key that setup-java-maven
# saves and restores ~/.m2/repository under. Run by that action, or by hand:
#
#   RUNNER_OS=Linux RUNNER_ARCH=X64 MAVEN_CACHE_JAVA_VERSION=17 \
#     MAVEN_CACHE_DEPENDENCY_PATH=samples-java/mysql-dual-conn/pom.xml \
#     bash .github/actions/setup-java-maven/maven-cache-key.sh
#
# MAVEN_CACHE_DEPENDENCY_PATH: the pom.xml files the build reads its
# dependencies from, one per line (blank lines and surrounding spaces are
# ignored). Every one must exist: a key that silently left one out would not
# change when its dependencies do.
# MAVEN_CACHE_JAVA_VERSION: the JDK the build runs on, as setup-java takes it.
#
# The key is maven-repository-<os>-<arch>-maven<version>-java<version>-<hash of
# the files>, where the Maven version is the one `mvn` on PATH reports: the
# one the build runs. It is in the key because a local repository is only of
# use to the Maven that filled it, or one that tracks files the same way.
# Maven records, in each directory's _remote.repositories, the repository a
# file came from, and uses a cached file only when that record names a
# repository of the current build. Maven Resolver 2.0.23 and later (Maven
# 3.10.0 ships 2.0.24) write that record URL-qualified
# (aether.system.trackingRepositoryKeyFunction, default nid_hurl:
# `x.pom>central-<hash of the URL>=`); Resolver 1.9 (every Maven 3.9) looks
# for the bare id (`x.pom>central=`). So a 3.9 build finds every file a 3.10
# build cached tracked but "present, but unavailable", and fetches it from
# Maven Central again. The Maven version also picks the default versions of
# the build's own plugins, so it is part of what the build downloads.
# actions/setup-java's own `cache: maven` key is the OS, the arch and the
# pom's hash only, and a cache entry is never replaced: when
# GitHub's runner image moved from Maven 3.10.0 (ubuntu-24.04) to Maven
# 3.9.15 (ubuntu-26.04), every Java job kept restoring a 3.10.0 cache it
# could not use and fetched its whole repository from Central on every run,
# until Central answered 429 Too Many Requests.
set -euo pipefail

: "${RUNNER_OS:?RUNNER_OS is not set}" "${RUNNER_ARCH:?RUNNER_ARCH is not set}"
java_version=${MAVEN_CACHE_JAVA_VERSION:?MAVEN_CACHE_JAVA_VERSION is not set}

files=()
while IFS= read -r line; do
  line="${line#"${line%%[![:space:]]*}"}"
  line="${line%"${line##*[![:space:]]}"}"
  [ -n "$line" ] && files+=("$line")
done <<<"${MAVEN_CACHE_DEPENDENCY_PATH:-}"
if [ "${#files[@]}" -eq 0 ]; then
  echo "::error::maven-cache-key.sh: MAVEN_CACHE_DEPENDENCY_PATH names no file" >&2
  exit 1
fi
for f in "${files[@]}"; do
  if [ ! -f "$f" ]; then
    echo "::error::maven-cache-key.sh: $f (in cache-dependency-path) is not a file" >&2
    exit 1
  fi
done

# Captured whole before it is read: under pipefail, a reader that stops
# early could fail mvn with SIGPIPE. -B: no color codes in the output.
if ! mvn_v=$(mvn -B -v 2>&1); then
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

deps=$(sha256sum -- "${files[@]}" | sha256sum | cut -c1-64)
echo "maven-repository-${RUNNER_OS}-${RUNNER_ARCH}-maven${maven}-java${java_version}-${deps}"
