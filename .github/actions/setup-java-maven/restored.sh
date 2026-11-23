#!/usr/bin/env bash
# restored.sh: setup-java-maven's step after actions/cache/restore. It
# records this use of the action, and puts bin/mvn first on PATH once per
# job.
#
# KEY: the key (maven-cache-key.sh). MATCHED: the key of the entry restored,
# empty on a miss. Entries are saved as KEY-<suffix> (save/save-plan.sh) and
# restored by the prefix KEY-, newest first, so any match is one saved for
# this Maven, JDK, configuration and poms: a hit.
#
# Each use gets a directory of its own under SETUP_JAVA_MAVEN_STATES
# ($RUNNER_TEMP/setup-java-maven), numbered in order: its key, the entry it
# restored (if any), and the list of the files ~/.m2/repository held once
# restored, which the save step compares against. bin/mvn follows the latest
# use: mvn runs offline after a use that restored an entry, online after one
# that missed. So a second use of the action in a job, which missed, does
# not run offline.
#
# Run twice in a job, it keeps the mvn the first use found
# (SETUP_JAVA_MAVEN_REAL_MVN) and adds bin/ to PATH once: by then `mvn` on
# PATH is bin/mvn itself.
set -euo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
key=${KEY:?KEY is not set}
matched=${MATCHED:-}
: "${GITHUB_ENV:?}" "${GITHUB_PATH:?}"

states=${SETUP_JAVA_MAVEN_STATES:-${RUNNER_TEMP:?RUNNER_TEMP is not set}/setup-java-maven}
mkdir -p "$states"
n=$(find "$states" -mindepth 1 -maxdepth 1 -type d -name '[0-9]*' | wc -l)
dir=$(printf '%s/%03d' "$states" $((n + 1)))
mkdir "$dir"

hit=false
case "$matched" in
  "$key" | "$key"-*) hit=true ;;
  '') ;;
  *)
    echo "::error::restored.sh: the entry restored ($matched) is not one of key $key" >&2
    exit 1
    ;;
esac
printf '%s\n' "$key" >"$dir/key"
printf '%s\n' "$matched" >"$dir/matched"
printf '%s\n' "$hit" >"$dir/hit"
repo="$HOME/.m2/repository"
if [ -d "$repo" ]; then
  (cd "$repo" && find . -type f | LC_ALL=C sort) >"$dir/manifest"
else
  : >"$dir/manifest"
fi

if [ -z "${SETUP_JAVA_MAVEN_REAL_MVN:-}" ]; then
  real=$(command -v mvn) || { echo "::error::restored.sh: no mvn on PATH" >&2; exit 1; }
  if [ "$real" -ef "$here/bin/mvn" ]; then
    echo "::error::restored.sh: mvn on PATH is setup-java-maven's own bin/mvn, and SETUP_JAVA_MAVEN_REAL_MVN is not set" >&2
    exit 1
  fi
  {
    echo "SETUP_JAVA_MAVEN_REAL_MVN=$real"
    echo "SETUP_JAVA_MAVEN_STATES=$states"
  } >>"$GITHUB_ENV"
  echo "$here/bin" >>"$GITHUB_PATH"
fi

if [ "$hit" = true ]; then
  echo "::notice title=Maven cache restored::$matched (key $key). mvn runs offline on it in the steps that follow."
else
  echo "No Maven cache for key $key yet: mvn resolves online, and the save step keeps what it fetches."
fi
