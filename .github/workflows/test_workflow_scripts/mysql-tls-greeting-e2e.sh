#!/usr/bin/env bash
# End-to-end run of the MySQL post-TLS greeting guard against a real server:
# starts a MySQL 8 container, hands its ROUTABLE container IP to
# TestE2E_MySQLTLSGreeting_* (pkg/agent/proxy/integrations/mysql/recorder/
# greeting_e2e_test.go) and removes the container again.
#
# The test reads the server's own Aborted_connects counter, so nothing else may
# connect to this server while it runs: the container is private to this script.
#
# Usage: mysql-tls-greeting-e2e.sh [extra go test flags...]
#   KEPLOY_E2E_MYSQL_IMAGE      image to test against (default mysql:8.0)
#   KEPLOY_E2E_MYSQL_CONTAINER  container name (default keploy-greeting-e2e-mysql)
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
ready=0
for _ in $(seq 1 120); do
  if docker exec "$name" mysql -h127.0.0.1 -uroot -p"$password" -N -e 'SELECT 1' >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 1
done
if [ "$ready" != 1 ]; then
  echo "::error::MySQL did not become ready"
  docker logs "$name" | tail -40
  exit 1
fi
echo "MySQL $(docker exec "$name" mysql -h127.0.0.1 -uroot -p"$password" -N -e 'SELECT VERSION()' 2>/dev/null) ready at $ip:3306"

export KEPLOY_E2E_MYSQL_ADDR="$ip:3306"
export KEPLOY_E2E_MYSQL_PASSWORD="$password"
go test ./pkg/agent/proxy/integrations/mysql/recorder/ \
  -run 'TestE2E_MySQLTLSGreeting_' -count=1 -v -timeout 10m "$@"
