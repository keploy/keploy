#!/bin/bash
# check-image-builds.sh: every image build in this repo's lanes goes through
# docker_build_retry (docker-build-retry.sh), or says it has no base to pull.
#
# A build pulls its base image's manifest and layers, and often fetches
# packages or Go modules, before it compiles anything. Each of those can fail
# for a reason that has nothing to do with the change under test: a registry's
# rate limit, an egress IP's spent quota (ECR Public's, check-image-registries.sh),
# or a transport fault. docker_build_retry retries those, and only those, so a
# real build break still fails on its first attempt. A build of a static
# binary FROM scratch pulls nothing, and says so within eight lines above it with
# "no base image to pull".
#
# Not counted: comments, `docker buildx imagetools` (not a build), and a
# `docker build` inside a quoted -c "..." that a lane hands keploy as the
# command under test.
#
#   bash .github/workflows/test_workflow_scripts/check-image-builds.sh
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
self=.github/workflows/test_workflow_scripts/check-image-builds.sh
bad=0
while IFS= read -r file; do
  [ "$file" != "$self" ] || continue
  awk -v f="$file" '
    { line[NR] = $0 }
    /^[[:space:]]*#/ { next }
    /docker buildx imagetools/ { next }
    /-c "[^"]*docker (buildx )?build/ { next }
    /(^|[^-[:alnum:]_])docker (buildx )?build([^[:alnum:]_-]|$)|compose[^#]*[[:space:]]build([^[:alnum:]_-]|$)/ {
      l = $0
      sub(/[[:space:]]#.*$/, "", l) # a trailing comment does not count
      if (l ~ /(^|[[:space:]])docker_build_retry[[:space:]]/) next
      for (i = NR - 1; i >= NR - 8 && i > 0; i--) if (line[i] ~ /no base image to pull/) next
      printf "%s:%d: %s\n", f, NR, $0
      found = 1
    }
    END { exit found ? 1 : 0 }
  ' "$file" || bad=1
done < <(git ls-files -- '.github/workflows/*.yml' '.github/workflows/*.yaml' '.github/actions/*/action.y*ml' '*.sh')
if [ "$bad" = 1 ]; then
  echo "::error::the image builds above do not go through docker_build_retry (source .github/workflows/test_workflow_scripts/docker-build-retry.sh), and do not say they have no base image to pull (see $self)"
  exit 1
fi
echo "Every image build goes through docker_build_retry or pulls no base."
