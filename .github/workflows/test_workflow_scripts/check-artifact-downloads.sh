#!/bin/bash
# check-artifact-downloads.sh: every artifact download in this repo's
# workflows and actions goes through .github/actions/download-artifact-retry,
# which tries actions/download-artifact up to three times, and every
# `uses: actions/download-artifact@<ref>` is v8 or later.
#
# Why v8. Before v6, a download whose stream stops part-way, whether it goes
# silent or its connection drops, ends the action with exit 0 and a
# truncated file on disk. The action's 30 s chunk timer destroys the
# response but never rejects the download's promise, so nothing retries it:
# node finds nothing left to wait for and exits 0, and the step is green.
# Its only trace is the lines a complete download ends with -- "SHA256
# digest of downloaded artifact is ...", "Artifact download completed
# successfully." -- which are missing. v6 and v7 reject on that timer and
# retry, but a digest that does not match the one recorded at upload is only
# a warning there. v8 retries the blob download (five requests), then fails
# the step, and fails it on a digest mismatch by default. So a download step
# that passes on v8 has the bytes that were uploaded.
#
# The self-hosted macOS runners' downloads from GitHub's artifact store take
# minutes, and the machine's network drops now and then while they run. Run
# 39760264652 (PR #4880): on one machine, two jobs' downloads stopped in the
# same second, about 10 s into gin-mongo's replay download of build-darwin.
# That download ended "successfully" 30 s later, leaving part of keploy where
# the record download, complete seconds before it began, had put the whole
# binary at the same path. The first `keploy record` then died at exec, before
# it printed a byte:
#
#   golang-docker-macos.sh: line 251: 68532 Killed: 9  $RECORD_BIN record ...
#
# The other was echo-sql's record download, and that job passed only because
# its replay download, of the same artifact to the same path, came second and
# rewrote the file whole. Runs 38529644196 and 39569418600 had the image
# download end so on another machine; download-image's tarball check caught
# those, and the second download it then made failed at once ("Failed to
# ListArtifacts: ... ENOTFOUND"), the network still down.
#
# Against a stand-in for the artifact service whose blob stream stops after
# 4 MiB of a 24 MiB artifact, silent or cut: v4.3.0, the version that run
# used, exits 0 after 30 s with 4192988 of keploy's 25165824 bytes on disk, no
# digest line and no second request. v8.0.2 exits 1 after five requests
# ("Artifact download failed after 5 retries"), and when only the first
# request stops, it writes the whole file.
#
# Why the retry. Before the blob, v8 asks the artifact service where the
# artifact is (ListArtifacts, then GetSignedArtifactURL). Those calls are
# retried on HTTP 500, 502, 503, 504 and 429 only: a connection reset or
# refused, or a name that does not resolve, fails the step at once: the
# second downloads of those two runs, and run 42655632271's, on a
# GitHub-hosted runner, six seconds into its job ("Failed to
# GetSignedArtifactURL: Unable to make request: ECONNRESET"). Against the
# stand-in, with the first GetSignedArtifactURL reset: v8.0.2 fails at
# once, after one request; download-artifact-retry fails its first try and
# passes its second, 15 s later, with the whole file. A github-token is no
# cure: the REST client it brings retries the listing, but not the request
# for the artifact's zip that follows it, and with that one reset v8.0.2
# fails at once too.
# download-artifact-retry's header says more.
#
# Only `uses:` lines are judged; a comment or a line of prose that names the
# action is not a use. A ref that is not a version tag (a branch, a SHA) says
# nothing this check can judge, so it is reported too: pin a version tag.
#
#   bash .github/workflows/test_workflow_scripts/check-artifact-downloads.sh
#
# check-artifact-downloads.tests.sh runs it on repositories made for each
# case.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
wrapper=.github/actions/download-artifact-retry/action.yml
use_re="^[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]*['\"]?actions/download-artifact@"
rc=0
uses=$(git grep -n -I -E "$use_re" -- .github) || rc=$?
case $rc in
  0) ;;
  1) echo "No workflow or action uses actions/download-artifact."; exit 0 ;;
  *) echo "::error::git grep failed (exit $rc)"; exit "$rc" ;;
esac
bad=0

# git grep prefixes each line with path:line:, so a line is the wrapper's
# when it starts with the wrapper's path and a colon.
direct=$(printf '%s\n' "$uses" | awk -v w="$wrapper:" 'index($0, w) != 1')
if [ -n "$direct" ]; then
  echo "::error::these lines use actions/download-artifact directly, which fails the job on one reset connection to the artifact service (see .github/workflows/test_workflow_scripts/check-artifact-downloads.sh):"
  printf '%s\n' "$direct"
  echo "Use ./.github/actions/download-artifact-retry, which tries again when a download fails (the job must check out this repository first)."
  bad=1
fi

tries=$(printf '%s\n' "$uses" | awk -v w="$wrapper:" 'index($0, w) == 1' | wc -l | tr -d ' ')
if [ "$tries" -lt 2 ]; then
  echo "::error::$wrapper uses actions/download-artifact $tries time(s): it must try at least twice, or one reset connection fails the job"
  bad=1
fi

# The ref is what follows the @, up to a quote, a space or the end.
ok_re="^[^:]*:[0-9]+:${use_re#^}v([89]|[1-9][0-9]+)([.][0-9]+)*(['\"]|[[:space:]]|$)"
rc=0
old=$(printf '%s\n' "$uses" | grep -v -E "$ok_re") || rc=$?
case $rc in
  0)
    echo "::error::these lines use actions/download-artifact older than v8, or at a ref that is not a version tag: before v6 a download that stops part-way ends green with a truncated file, and before v8 a digest mismatch is only a warning (see .github/workflows/test_workflow_scripts/check-artifact-downloads.sh):"
    printf '%s\n' "$old"
    echo "Use actions/download-artifact@v8."
    bad=1
    ;;
  1) ;;
  *) echo "::error::grep failed (exit $rc)"; exit "$rc" ;;
esac

[ "$bad" -eq 0 ] || exit 1
echo "Every artifact download goes through $wrapper, which tries actions/download-artifact v8 or later up to $tries times."
