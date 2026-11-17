#!/usr/bin/env bash
# save-plan.sh: setup-java-maven/save's first step. It decides whether to
# save ~/.m2/repository, and under which entry of the key of the job's last
# setup-java-maven use, and writes save=true|false and key= to GITHUB_OUTPUT.
#
# It saves when the use restored nothing and a command line it declared ran
# and passed (its first fill, even when another use's repository already held
# all it needed); when the repository holds files it did not hold once
# restored (restored.sh's list); or when a command line
# its cache could not serve offline was resolved online (bin/mvn's
# "unresolved" list). The second can add no file at all: a file that was
# present but tracked in a form this Maven does not read is fetched again
# to the same path, and only its _remote.repositories changes. The entry is
# KEY-<digest of the new files' names and of what was unresolved>. The
# restore step takes the newest entry of KEY, so the next run restores this
# one, a superset of the one it replaces. Two cells that fetched the same
# files name the same entry, and the second save is refused as already made.
# Bookkeeping files (*.lastUpdated, *.part, *.lock,
# resolver-status.properties, _remote.repositories) do not count as new:
# they come with the files that do, or with downloads that failed.
#
# It does not save when the last run of a command line through bin/mvn
# failed: the repository may lack what that run needed. A command line that
# failed and then passed (the lanes try a build up to three times) does not
# stop it. It saves each use once: run again, it does nothing.
set -euo pipefail
: "${GITHUB_OUTPUT:?}"
out() { printf '%s\n' "$@" >>"$GITHUB_OUTPUT"; }
states=${SETUP_JAVA_MAVEN_STATES:-}
use=''
if [ -n "$states" ] && [ -d "$states" ]; then
  use=$(find "$states" -mindepth 1 -maxdepth 1 -type d -name '[0-9]*' | LC_ALL=C sort | tail -n 1)
fi
if [ -z "$use" ]; then
  echo "No setup-java-maven step ran in this job: nothing to save."
  out save=false
  exit 0
fi
if [ -e "$use/planned" ]; then
  echo "This job's Maven repository was already saved for $(cat "$use/key"), or found not to need it."
  out save=false
  exit 0
fi
touch "$use/planned"
key=$(cat "$use/key")

# The last run of each command line (runs: "rc mode id use" per run, in order).
if [ -f "$states/runs" ]; then
  failed=$(awk '{ last[$3] = $1 } END { for (i in last) if (last[i] != 0) n++; print n + 0 }' "$states/runs")
  if [ "$failed" -ne 0 ]; then
    echo "::warning title=Maven repository not saved::the last run of $failed mvn command line(s) in this job failed, so ~/.m2/repository may lack what it needed; not saving it for key $key."
    out save=false
    exit 0
  fi
fi

repo="$HOME/.m2/repository"
if [ ! -d "$repo" ]; then
  echo "No ~/.m2/repository: no Maven build ran, nothing to save."
  out save=false
  exit 0
fi
gained=$( (cd "$repo" && find . -type f | LC_ALL=C sort) | LC_ALL=C comm -13 "$use/manifest" - |
  grep -vE '(\.lastUpdated|\.part|\.lock|/resolver-status\.properties|/_remote\.repositories)$' || true)
unresolved=''
[ -s "$use/unresolved" ] && unresolved=$(LC_ALL=C sort -u "$use/unresolved")
matched=$(cat "$use/matched")
ran=0
[ -f "$states/runs" ] && ran=$(awk -v u="$(basename "$use")" '$4 == u && $1 == 0' "$states/runs" | wc -l)
if [ -z "$matched" ] && [ "$ran" -gt 0 ] && [ -z "$gained" ] && [ -z "$unresolved" ]; then
  entry="$key-$(printf 'fill\n' | sha256sum | cut -c1-16)"
  echo "Saving ~/.m2/repository as $entry: the first fill of key $key, whose build found all it needed already in ~/.m2/repository."
  out save=true "key=$entry"
  exit 0
fi
if [ -z "$gained" ] && [ -z "$unresolved" ]; then
  if [ -n "$matched" ]; then
    echo "~/.m2/repository holds nothing it did not hold once restored ($matched): nothing to save."
  else
    echo "No Maven cache was restored for key $key and no build it declared ran: nothing to save."
  fi
  out save=false
  exit 0
fi
n=0
[ -z "$gained" ] || n=$(printf '%s\n' "$gained" | wc -l)
entry="$key-$({ printf '%s\n' "$gained" -- "$unresolved"; } | sha256sum | cut -c1-16)"
if [ -n "$unresolved" ]; then
  echo "Saving ~/.m2/repository as $entry: the Maven cache $(cat "$use/matched") could not serve the build ($(printf '%s\n' "$unresolved" | head -n 3 | paste -sd';' | sed 's/;/; /g')), so it was resolved online ($n new files)."
elif [ -n "$matched" ]; then
  echo "Saving ~/.m2/repository as $entry: $n files more than the Maven cache $(cat "$use/matched") held."
else
  echo "Saving ~/.m2/repository as $entry: the first fill of key $key ($n files)."
fi
out save=true "key=$entry"
