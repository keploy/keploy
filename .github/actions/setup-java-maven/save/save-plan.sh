#!/usr/bin/env bash
# save-plan.sh: setup-java-maven/save's first step. It decides whether to
# save ~/.m2/repository, and under which entry of the key of the job's
# oldest setup-java-maven use not yet planned, and writes save=true|false
# and key= to GITHUB_OUTPUT. So a job with two uses saves each once, with a
# save step after each build or both at the end.
#
# It saves when:
#   - the use restored nothing and an mvn run after it passed: its first
#     fill, even when the repository already held all it needed (another
#     use's);
#   - the repository holds files it did not hold once the use restored its
#     entry (restored.sh's list), as a lane that added a goal leaves it;
#   - or mvn could not run offline on the entry and resolved online
#     (bin/mvn's "unresolved" list). That can add no file at all: a file
#     tracked in a form this Maven does not read is fetched again to the
#     same path, and only its _remote.repositories changes.
# The entry is KEY-<digest of the entry restored, the new files' names and
# what was unresolved>: a fingerprint of what healed it, from what. The
# restore step takes the newest entry of KEY, so the next run restores this
# one, a superset of the one it restored, and builds offline. The entry
# restored is in the digest because two builds can share a key and heal it
# apart: X heals joda-time into the base entry, Y heals gson into the same
# base, and Y's entry, the newer, lacks joda-time. X's next run restores it
# and heals joda-time again. Named from the base alone, that heal would be
# X's first entry again, refused as made, and X would heal on every run;
# named from Y's entry, it is a new one, the union of both, and from then on
# both build offline. Two cells that restored the same entry and fetched the
# same files name the same entry, and the second save is refused as already
# made.
# Bookkeeping files (*.lastUpdated, *.part, *.lock,
# resolver-status.properties, _remote.repositories) do not count as new:
# they come with the files that do, or with downloads that failed.
#
# It does not save when the last mvn run after the use failed: the
# repository may lack what that run needed. Nor when a heal did not take
# (bin/mvn's "heal-failed" list: offline, the healed repository still could
# not resolve something): it fails instead, naming what, since that entry
# would heal no better and get a new name, and a full save, every run. A build that failed and then
# passed (the lanes try up to three times) does not stop it.
set -euo pipefail
: "${GITHUB_OUTPUT:?}"
out() { printf '%s\n' "$@" >>"$GITHUB_OUTPUT"; }
states=${SETUP_JAVA_MAVEN_STATES:-}
use=''
if [ -n "$states" ] && [ -d "$states" ]; then
  while IFS= read -r d; do
    [ -e "$d/planned" ] || { use=$d; break; }
  done < <(find "$states" -mindepth 1 -maxdepth 1 -type d -name '[0-9]*' | LC_ALL=C sort)
fi
if [ -z "$use" ]; then
  echo "No setup-java-maven step in this job is left to save."
  out save=false
  exit 0
fi
touch "$use/planned"
key=$(cat "$use/key")
matched=$(cat "$use/matched")
u=$(basename "$use")

# The runs after this use (runs: "rc mode use" per run, in order).
last='' ran=0
if [ -f "$states/runs" ]; then
  last=$(awk -v u="$u" '$3 == u { rc = $1 } END { print rc }' "$states/runs")
  ran=$(awk -v u="$u" '$3 == u && $1 == 0' "$states/runs" | wc -l)
fi
if [ -n "$last" ] && [ "$last" != 0 ]; then
  echo "::warning title=Maven repository not saved::the last mvn run in this job after the Maven cache step for key $key failed, so ~/.m2/repository may lack what it needed; not saving it."
  out save=false
  exit 0
fi

if [ -s "$use/heal-failed" ]; then
  echo "::error title=Maven repository not saved::a heal in this job did not take: offline, mvn still could not resolve $(LC_ALL=C sort -u "$use/heal-failed" | head -n 5 | paste -sd';' | sed 's/;/; /g') after resolving it online (key $key)."
  out save=false
  exit 1
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
    echo "No Maven cache was restored for key $key and no mvn run passed after it: nothing to save."
  fi
  out save=false
  exit 0
fi
n=0
[ -z "$gained" ] || n=$(printf '%s\n' "$gained" | wc -l)
entry="$key-$({ printf '%s\n' "$matched" -- "$gained" -- "$unresolved"; } | sha256sum | cut -c1-16)"
if [ -n "$unresolved" ]; then
  echo "Saving ~/.m2/repository as $entry: the Maven cache $matched could not serve the build ($(printf '%s\n' "$unresolved" | head -n 3 | paste -sd';' | sed 's/;/; /g')), so it was resolved online ($n new files)."
elif [ -n "$matched" ]; then
  echo "Saving ~/.m2/repository as $entry: $n files more than the Maven cache $matched held."
else
  echo "Saving ~/.m2/repository as $entry: the first fill of key $key ($n files)."
fi
out save=true "key=$entry"
