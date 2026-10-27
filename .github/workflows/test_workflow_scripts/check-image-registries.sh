#!/bin/bash
# check-image-registries.sh: no lane, Dockerfile or compose file in this repo
# names ECR Public (public.ecr.aws).
#
# ECR Public caps anonymous pulls at "up to 500GB of data per month", limited
# by source IP when no AWS account is used
# (https://docs.aws.amazon.com/AmazonECR/latest/public/public-service-quotas.html,
# https://aws.amazon.com/ecr/pricing/). GitHub's hosted runners share egress
# IPs, so a job that lands on an IP whose quota others have spent gets
#
#   429 Too Many Requests - Server message: toomanyrequests: Data limit exceeded
#
# for every pull, whatever this repo pulls, and a retry on that runner does
# not clear it. On 2026-10-16 e2e-ca-bundle-merge failed so on main at
# c0e96f011 (15:38Z, debian:trixie-slim's manifest) and on #4830 at 20:05Z and
# 20:21Z (golang:1.27-bookworm's), and passed on #4830 at 19:53Z: which runner
# a job got decided it.
#
# The lanes pull Docker Hub's official images instead, through
# docker-build-retry.sh's docker_pull_retry: "GitHub-hosted runners are not
# subject to [Docker Hub's] limits based on an agreement between GitHub and
# Docker" (https://docs.github.com/en/actions/how-tos/write-workflows/choose-where-workflows-run/run-jobs-in-a-container).
# Self-hosted runners and developers' machines still meet Docker Hub's
# anonymous limit, which the retry only softens.
#
# Authenticated ECR Public pulls are kept out too: a lane run from a fork has
# no credentials, so it would meet the anonymous quota anyway.
#
#   bash .github/workflows/test_workflow_scripts/check-image-registries.sh
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
self=.github/workflows/test_workflow_scripts/check-image-registries.sh
rc=0
hits=$(git grep -n -I -F 'public.ecr.aws' -- . ":(exclude)$self") || rc=$?
case $rc in
  0)
    echo "::error::these lines name ECR Public (public.ecr.aws), whose anonymous quota is per source IP and runs out on GitHub's shared runner IPs (see $self):"
    printf '%s\n' "$hits"
    echo "Use the image's Docker Hub official name, pulled with docker_pull_retry (docker-build-retry.sh)."
    exit 1
    ;;
  1) echo "No line names ECR Public." ;;
  *) echo "::error::git grep failed (exit $rc)"; exit "$rc" ;;
esac
