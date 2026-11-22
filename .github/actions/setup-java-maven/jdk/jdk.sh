#!/bin/bash
# jdk.sh: the steps of setup-java-maven/jdk (action.yml) that pick the pinned
# JDK's source and check its archive.
#
#   JDK_JAVA_VERSION=17 bash jdk.sh pin
#       Looks the major version up in jdks.txt and writes to $GITHUB_OUTPUT
#       the build (version) and where it comes from (source):
#       - image: the runner image's tool cache holds exactly that build
#         ($RUNNER_TOOL_CACHE/Java_Temurin-Hotspot_jdk/<build>/x64, complete,
#         and its release file has one IMPLEMENTOR line, "Eclipse Adoptium",
#         and one SEMANTIC_VERSION line, "<build>", as every Temurin build's).
#         actions/setup-java, asked for that exact build, takes it from there
#         and downloads nothing. The image ships no archive and no digest to
#         check, so the build string is what is pinned here.
#       - archive: it does not (ubuntu-26.04 has no JDK 8; an image update
#         moves its 17). Then the build's release archive is used, from the
#         Actions cache or downloaded, and checked (fetch). When the image
#         holds another build of the same major version, a ::warning says so
#         and which jdks.txt line to move, before the archive costs every job.
#       It also writes the directory the archive is kept in (dir: the cache's
#       path, one per digest), the archive in it (file) and the Actions cache
#       key (key), which names the build and its sha256, so that a moved pin
#       is a new entry. It fails, saying why, on a java-version that is not a
#       major version, on one jdks.txt has no line for (or two), and on a
#       runner that is not Linux X64 (jdks.txt pins Linux x64 builds).
#   JDK_JAVA_VERSION=17 JDK_CACHE_HIT=true|... bash jdk.sh fetch
#       The archive source. JDK_CACHE_HIT is the cache restore's cache-hit.
#       On a hit it checks the archive the cache restored against the pinned
#       sha256. On a miss it keeps an archive already here that has the pinned
#       digest (an earlier use in the job); else it removes whatever is here
#       (what a restore that failed part way left), downloads the archive from
#       the release once and checks that: the digest is checked on every run,
#       on a cached copy as on a downloaded one. An archive that fails it is removed, so that it is
#       neither installed nor saved, and the step fails, naming the archive,
#       where it came from, both digests, its size and what it starts with
#       (an HTML page from a release server is not a gzip stream). A download
#       that fails names the URL (and where it was redirected), curl's reason
#       and the last HTTP status. No download is tried twice: one bad answer
#       fails the step, and says what it was.
#
# Why: for java-version 8, actions/setup-java asked Adoptium's API for the
# latest build on every run and downloaded it from GitHub's release servers
# (the ubuntu-26.04 runner image has no JDK 8), then extracted whatever came
# back, unchecked. On 2026-11-20 three async-config-poll jobs failed so: the
# 103 MB archive of Temurin 8.0.514+1 "downloaded" in 25 to 60 ms, and tar
# said "gzip: stdin: not in gzip format": a body that was not the archive.
#
# HOME, RUNNER_OS, RUNNER_ARCH, RUNNER_TOOL_CACHE and GITHUB_OUTPUT come from
# the runner; JDK_PINS (default: jdks.txt beside this script) is for the tests.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
pins=${JDK_PINS:-$here/jdks.txt}

# fail TITLE MESSAGE: an error annotation, and the step fails. A % in the
# message is escaped, as workflow commands read %XX as an escape.
fail() {
  local m=$2
  echo "::error title=$1::${m//\%/%25}"
  exit 1
}

# lookup: build, sha, url, dir, file and key of JDK_JAVA_VERSION's pin.
lookup() {
  local want=${JDK_JAVA_VERSION:-} major b s u rest pinned=''
  want=${want#"${want%%[![:space:]]*}"}
  want=${want%"${want##*[![:space:]]}"}
  [ -f "$pins" ] || fail "JDK pin" "$pins is not a file"
  build=''
  while read -r major b s u rest; do
    case $major in '' | '#'*) continue ;; esac
    pinned="$pinned${pinned:+, }$major"
    [ "$major" = "$want" ] || continue
    [ -z "$build" ] || fail "JDK pin" "$pins has two lines for java-version $major"
    if ! { [ -z "$rest" ] && [ -n "$b" ] && [[ $s =~ ^[0-9a-f]{64}$ ]] && [[ $u == http*://*.tar.gz ]]; }; then
      fail "JDK pin" "the line of java-version $major in $pins is not 'MAJOR BUILD SHA256 URL.tar.gz'"
    fi
    build=$b sha=$s url=$u
  done <"$pins"
  case $want in
    '' | *[!0-9]*) fail "JDK pin" "java-version '$want' is not a major version: $pins pins each major version ($pinned) to one Temurin build" ;;
  esac
  [ -n "$build" ] || fail "JDK pin" "no JDK is pinned for java-version $want in $pins (it pins $pinned); add its line there"
  if [ "${RUNNER_OS:-}" != Linux ] || [ "${RUNNER_ARCH:-}" != X64 ]; then
    fail "JDK pin" "$pins pins Linux X64 builds only, and this runner is ${RUNNER_OS:-?} ${RUNNER_ARCH:-?}"
  fi
  dir="${HOME:?HOME is not set}/.cache/setup-java-maven-jdk/$sha"
  file="$dir/${url##*/}"
  key="setup-java-maven-jdk-Linux-X64-temurin-$build-$sha"
}

# image: sets image (the tool cache's copy of the pinned build) and returns 0
# when the runner image holds exactly that build: complete, and its release
# file has one IMPLEMENTOR line, "Eclipse Adoptium", and one SEMANTIC_VERSION
# line, the build. Else it says what the image holds, and when the image holds
# another build of the same major version (the image moved, so jobs take the
# archive until jdks.txt follows), it warns in the job's summary.
image() {
  local tc=${RUNNER_TOOL_CACHE:-} rel have='' same='' d v r=''
  image="$tc/Java_Temurin-Hotspot_jdk/${build//+/-}/x64"
  [ ! -f "$image/release" ] || r=$image/release
  if [ -n "$tc" ] && [ -f "$image.complete" ] && [ -n "$r" ] &&
    [ "$(grep -c '^IMPLEMENTOR=' "$r")" = 1 ] && grep -qx 'IMPLEMENTOR="Eclipse Adoptium"' "$r" &&
    [ "$(grep -c '^SEMANTIC_VERSION=' "$r")" = 1 ] && grep -qxF "SEMANTIC_VERSION=\"$build\"" "$r"; then
    return 0
  fi
  for d in "$tc"/Java_Temurin-Hotspot_jdk/*/x64; do
    [ -d "$d" ] || continue
    d=${d%/x64}
    d=${d##*/}
    have="$have${have:+ }$d"
    v=${d/-/+}
    [ "${v%%.*}" != "${build%%.*}" ] || same="$same${same:+, }$v"
  done
  rel=''
  if [ -n "$r" ]; then
    rel=$(grep -E '^(IMPLEMENTOR|SEMANTIC_VERSION)=' "$r" | tr '\n' ' ')
    rel=" (its release file: ${rel% })"
  fi
  [ ! -d "$image" ] || [ -f "$image.complete" ] || rel="$rel, not marked complete"
  echo "The runner image has no Temurin $build in ${tc:-its tool cache}; it has: ${have:-none}$rel"
  if [ -n "$same" ]; then
    local m="the runner image has Temurin $same for java-version ${build%%.*}, not the pinned $build$rel, so this job takes the pinned build's archive (from the Actions cache, else downloaded). To use the image's copy again, move the line of ${build%%.*} in $pins to the image's build, with its release's sha256 and URL."
    echo "::warning title=JDK pin::${m//\%/%25}"
  fi
  return 1
}

pin() {
  lookup
  local source=archive
  if image; then
    source=image
    echo "java-version ${JDK_JAVA_VERSION} is Temurin $build: the runner image has exactly that build ($image), so setup-java takes it from there and nothing is downloaded"
  else
    echo "java-version ${JDK_JAVA_VERSION} is Temurin $build, sha256 $sha ($url): from its archive, cache key $key"
  fi
  {
    echo "version=$build"
    echo "source=$source"
    echo "dir=$dir"
    echo "file=$file"
    echo "key=$key"
  } >>"${GITHUB_OUTPUT:?GITHUB_OUTPUT is not set}"
}

# check FILE WHERE WHAT-NOW: FILE has the pinned sha256, or it is removed and
# the step fails, saying what it holds. WHERE says where it came from.
check() {
  local got size what magic
  got=$(sha256sum <"$1" | cut -c1-64)
  [ "$got" != "$sha" ] || return 0
  size=$(wc -c <"$1" | tr -d '[:space:]')
  magic=$(od -An -tx1 -N2 "$1" | tr -d '[:space:]')
  if [ "$magic" = 1f8b ]; then
    what="a gzip stream"
  else
    what="not a gzip stream; it starts '$(head -c 64 "$1" | LC_ALL=C tr -c '[:print:]' '.')'"
  fi
  rm -f "$1"
  fail "JDK archive digest" "the JDK $build archive $2 has sha256 $got, not the pinned $sha ($pins): $size bytes, $what. $3"
}

fetch() {
  lookup
  if [ "${JDK_CACHE_HIT:-}" = true ]; then
    [ -f "$file" ] || fail "JDK archive" "the Actions cache entry $key was restored without the archive $file. An entry is saved only with an archive that passed its check: delete it (gh cache delete '$key') and the next run downloads the archive again."
    echo "JDK $build: checking the archive the Actions cache restored ($file)"
    check "$file" "restored from the Actions cache ($file, key $key)" \
      "It was removed, and no JDK was installed. An entry is saved only after this check passes, and never replaced: this one changed after it was saved, so delete it (gh cache delete '$key') and the next run downloads the archive again."
  elif [ -f "$file" ] && [ "$(sha256sum <"$file" | cut -c1-64)" = "$sha" ]; then
    # A miss with the archive already here (an earlier use of this action in
    # the job): it has the pinned digest, so it is kept.
    echo "JDK $build: $file is already here (not restored as a hit), and has the pinned sha256: kept"
  else
    # A miss. Whatever is here, such as what a restore that failed part way
    # left, is removed, and the archive downloaded.
    if [ -n "$(ls -A "$dir" 2>/dev/null)" ]; then
      echo "JDK $build: removing what is in $dir, which the Actions cache did not restore as a hit"
      rm -rf "$dir"
    fi
    download
  fi
  echo "JDK $build: $file has the pinned sha256 $sha"
}

# download: the archive from its release, once, into $file, checked.
download() {
  echo "JDK $build: the Actions cache has no copy of its archive (key $key); downloading $url"
  mkdir -p "$dir"
  local part="$file.part" errf meta rc=0 err code bytes secs served ctype where=''
  errf=$(mktemp)
  # A stalled transfer ends (under 10 KB/s for 60 s) rather than hanging the job.
  meta=$(curl --silent --show-error --location --fail --connect-timeout 30 \
    --speed-limit 10240 --speed-time 60 --output "$part" \
    --write-out '%{http_code} %{size_download} %{time_total} %{url_effective} %{content_type}' \
    "$url" 2>"$errf") || rc=$?
  err=$(tr -s '\r\n' '  ' <"$errf")
  rm -f "$errf"
  read -r code bytes secs served ctype <<<"$meta" || true
  [ "${code:-000}" != 000 ] || code="none (no HTTP answer)"
  # The server that answered, without the signed query of a release asset's URL.
  served=${served%%\?*}
  if [ "$rc" -ne 0 ]; then
    rm -f "$part"
    # After a redirect, curl's failure is the redirect target's, and the
    # status it got last is the redirect's own (302).
    [ -z "$served" ] || [ "$served" = "$url" ] || where=" while fetching $served, where it was redirected"
    fail "JDK download" "downloading the JDK $build archive from $url failed$where: curl exit $rc (${err% }); the last HTTP status it got: $code, ${bytes:-0} bytes received. No JDK was installed or cached."
  fi
  check "$part" "downloaded from $url (HTTP $code, ${ctype:-no content type}, ${bytes} bytes in ${secs} s from $served)" \
    "It was not installed or cached."
  mv "$part" "$file"
  echo "JDK $build: downloaded $bytes bytes in $secs s from $served"
}

case ${1:-} in
  pin) pin ;;
  fetch) fetch ;;
  *)
    echo "usage: JDK_JAVA_VERSION=MAJOR bash jdk.sh pin|fetch" >&2
    exit 2
    ;;
esac
