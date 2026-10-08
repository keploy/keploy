#!/bin/bash
# DEPRECATED no-op. Kept so old Dockerfiles that do
#   ADD .../pkg/core/proxy/tls/asset/setup_ca.sh .
#   RUN/CMD source ./setup_ca.sh
# keep building and running instead of 404-ing.
#
# This script used to copy a committed static MITM CA into the image's trust
# store and export NODE_EXTRA_CA_CERTS / REQUESTS_CA_BUNDLE to point at it. That
# static CA has been retired (its private key was public), and crucially those
# two exports used to OVERRIDE the per-run trust that keploy now injects, which
# broke interception for Node and Python `requests` apps. So this script no
# longer copies anything, no longer imports into Java, and no longer exports
# those variables.
#
# You do not need this script any more. Since keploy v3.3.0 the tooling injects
# its per-run CA and the trust environment variables into your application
# container automatically (docker run, docker compose and --from-container).
#
# Removing the ADD/`source` lines from your Dockerfile is the right fix; this
# no-op only keeps un-migrated images building in the meantime.

echo "keploy: setup_ca.sh is deprecated and does nothing; keploy injects its per-run CA automatically. See https://keploy.io/docs/running-keploy/docker-tls/" >&2

# Exit 0 whether run directly or sourced, so an image build / entrypoint that
# calls it does not fail.
return 0 2>/dev/null || exit 0
