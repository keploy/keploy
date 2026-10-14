#!/usr/bin/env bash
# End-to-end runs of the MySQL recorder against a real server: starts a MySQL 8
# container, hands its ROUTABLE container IP to the tests KEPLOY_E2E_MYSQL_RUN
# names in pkg/agent/proxy/integrations/mysql/recorder/ and removes the
# container again:
#   - TestE2E_MySQLTLSGreeting_* (greeting_e2e_test.go, the default): the
#     post-TLS greeting guard;
#   - TestE2E_MySQLTLSFullAuth_* (auth_redact_test.go): a TLS full
#     authentication records no password.
#
# The greeting test reads the server's own Aborted_connects counter, so nothing else may
# connect to this server while it runs: the container is private to this script.
#
# Usage: mysql-tls-greeting-e2e.sh [extra go test flags...]
#   KEPLOY_E2E_MYSQL_IMAGE      image to test against (default mysql:8.0)
#   KEPLOY_E2E_MYSQL_CONTAINER  container name (default keploy-greeting-e2e-mysql)
#   KEPLOY_E2E_MYSQL_RUN        the tests to run (default TestE2E_MySQLTLSGreeting_;
#                               TestE2E_MySQLTLSFullAuth_ is the other one)
set -Eeuo pipefail

image="${KEPLOY_E2E_MYSQL_IMAGE:-mysql:8.0}"
name="${KEPLOY_E2E_MYSQL_CONTAINER:-keploy-greeting-e2e-mysql}"
password="keploy-e2e-$$"

cleanup() { docker rm -f "$name" >/dev/null 2>&1 || true; }
trap cleanup EXIT
cleanup

docker run -d --name "$name" -e MYSQL_ROOT_PASSWORD="$password" "$image" >/dev/null
ip="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$name")"
if [ -z "$ip" ]; then
  echo "::error::the MySQL container has no IP address on a docker network"
  exit 1
fi

# Ready means the FINAL server accepts an authenticated TCP login. The image's
# entrypoint first runs a temporary server with networking off, so a TCP probe
# before then is refused rather than aborted; an authenticated login is never
# counted in Aborted_connects. The probe runs inside the container.
#
# It waits for as long as the server is starting, not for a fixed time: on a
# loaded runner the image's first-time init alone took over two minutes, and a
# 120-probe budget failed the lane on a server about to come up. A server that
# will not come up exits, which ends the wait; the job's timeout bounds a hang.
ready=0
while [ "$(docker inspect -f '{{.State.Running}}' "$name" 2>/dev/null)" = true ]; do
  if docker exec "$name" mysql -h127.0.0.1 -uroot -p"$password" -N -e 'SELECT 1' >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "::error::the MySQL container stopped before it accepted a login"
  docker logs "$name" | tail -40
  exit 1
fi
echo "MySQL $(docker exec "$name" mysql -h127.0.0.1 -uroot -p"$password" -N -e 'SELECT VERSION()' 2>/dev/null) ready at $ip:3306"

export KEPLOY_E2E_MYSQL_ADDR="$ip:3306"
export KEPLOY_E2E_MYSQL_PASSWORD="$password"
go test ./pkg/agent/proxy/integrations/mysql/recorder/ \
  -run "${KEPLOY_E2E_MYSQL_RUN:-TestE2E_MySQLTLSGreeting_}" -count=1 -v -timeout 10m "$@"
