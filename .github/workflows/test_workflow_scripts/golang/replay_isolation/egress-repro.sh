#!/bin/bash
# egress-repro.sh [MODE...]   MODE: native docker internal (default: all three)
#
# Replay network isolation: maps every way an app's traffic can leave a
# keploy replay. Runs inside lane-container.sh (privileged, its own netns and
# dockerd) from a keploy checkout; CI's runner is the same shape:
#   lane-container.sh -n riso-egress <keploy checkout> -- \
#     bash .github/workflows/test_workflow_scripts/golang/replay_isolation/egress-repro.sh
# KEPLOY_SRC (default: this checkout) is the keploy source under test.
#
# The probe (probe/) serves GET /fire, which tries each path at a sink and
# answers a constant, so record and replay agree. The sink lives in its own
# netns (riso-sink) behind a veth, so it is "the internet" for native and
# docker apps alike: 198.18.0.10, 198.18.0.11 (stands in for IMDS; the real
# 169.254.169.254 is never routed) and fd00::10. The container's resolv.conf
# points at the sink once the images are built, so a DNS forward that leaves
# keploy is counted too (the sink answers NXDOMAIN and logs each name).
#
# Per mode: baseline (no keploy: each path reaches the sink), record, replay;
# "internal" runs the probe on an internal: true docker network. Counts land
# in /work/riso/out/<phase>-<mode>/<path>.count and print as a table. It
# reports; it asserts nothing yet (the isolation fix makes replay's row 0).
# No secret is involved anywhere.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
KEPLOY_SRC=${KEPLOY_SRC:-$(cd "$HERE/../../../../.." && pwd)}
W=/work/riso
rm -rf "$W" && mkdir -p "$W/out"
S=""; [ "$(id -u)" = 0 ] || S="sudo -E"
MODES=("$@"); [ ${#MODES[@]} -gt 0 ] || MODES=(native docker internal)
SINKPID=""
FAILED=0
PATHS="tcp bypass-tcp fork-tcp tfo imds-tcp udp-connected udp-sendto udp-quic icmp-raw icmp-dgram dns-udp dns-other dns-tcp tcp6 udp6"
APPARGS="app -sink 198.18.0.10 -sink6 fd00::10 -imds 198.18.0.11"

log() { echo "== $(date +%T) $*"; }
die() { echo "FATAL: $*"; exit 1; }

cleanup() {
	[ -n "$SINKPID" ] && kill "$SINKPID" 2>/dev/null
	docker rm -f riso-app riso-base riso-int-c >/dev/null 2>&1
	[ -f "$W/resolv.orig" ] && cat "$W/resolv.orig" > /etc/resolv.conf
	$S ip netns del riso-sink 2>/dev/null
	true
}
trap cleanup EXIT

# Waits for a condition, polling once a second, at most $1 times.
waitfor() {
	local n=$1; shift
	until "$@" >/dev/null 2>&1; do
		n=$((n - 1)); [ "$n" -gt 0 ] || return 1
		sleep 1
	done
}

# A port-only bypass rule for 7002 (the sink's bypass port). In keploy.yml:
# the --pass-through-ports flag fails record at 3edc3ea23 ("record.passThroughPorts[0]
# expected a map or struct, got uint": the flag lands on the telemetry rules key).
bypass_yml() { printf 'bypassRules:\n  - port: 7002\n' > keploy.yml; }

sink_start() {
	mkdir -p "$W/out/$1"
	$S ip netns exec riso-sink "$W/egressprobe" sink -dir "$W/out/$1" 2> "$W/out/$1/sink.log" &
	SINKPID=$!
	waitfor 30 grep -q ready "$W/out/$1/sink.log" || die "sink for $1 did not start"
}
# The sink runs under ip netns exec, which execs it: $SINKPID is the sink.
sink_stop() { [ -n "$SINKPID" ] && { $S kill "$SINKPID" 2>/dev/null; wait "$SINKPID" 2>/dev/null; SINKPID=""; }; true; }

# Fires the probe at URL and waits for the double-forked child's line in LOG.
fire() {
	curl -s --max-time 120 "$1/fire" > "$2.fire" 2>&1
	waitfor 15 grep -q 'probe fork-tcp' "$2" || echo "(no fork-tcp line in $2)"
}

log "builds"
(cd "$KEPLOY_SRC" && CGO_ENABLED=0 go build -tags=viper_bind_struct -o "$W/keploy" .) || die "keploy build"
(cd "$HERE/probe" && CGO_ENABLED=0 go build -o "$W/egressprobe" .) || die "probe build"
"$W/keploy" --version 2>&1 | grep -i version | head -1
uname -r; docker version -f 'docker server {{.Server.Version}}' 2>&1

need_docker=0
for m in "${MODES[@]}"; do [ "$m" = native ] || need_docker=1; done
if [ $need_docker = 1 ]; then
	log "images"
	mkdir -p "$W/img/dist/keploy-linux-amd64" "$W/pimg"
	cp "$W/keploy" "$W/img/dist/keploy-linux-amd64/keploy"
	cp "$KEPLOY_SRC/entrypoint.sh" "$KEPLOY_SRC/Dockerfile.runtime" "$W/img/"
	docker build -q --build-arg TARGETARCH=amd64 -f "$W/img/Dockerfile.runtime" -t ghcr.io/keploy/keploy:v3-dev "$W/img" || die "agent image"
	cp "$W/egressprobe" "$W/pimg/"
	mkdir -p "$W/pimg/tmp" && touch "$W/pimg/tmp/.keep" # the fork path's done-file goes to /tmp
	printf 'FROM scratch\nCOPY egressprobe /egressprobe\nCOPY tmp /tmp\nENTRYPOINT ["/egressprobe"]\n' > "$W/pimg/Dockerfile"
	docker build -q -t riso-probe:local "$W/pimg" || die "probe image"
	docker network create keploy-network >/dev/null 2>&1 || true
fi

log "sink netns"
$S ip netns add riso-sink || die "ip netns add"
$S ip link add riso-h type veth peer name riso-s || die "veth"
$S ip link set riso-s netns riso-sink
$S sysctl -qw net.ipv4.ip_forward=1 net.ipv6.conf.all.disable_ipv6=0 net.ipv6.conf.all.forwarding=1 2>&1
$S ip addr add 198.18.0.1/24 dev riso-h
$S ip -6 addr add fd00::1/64 dev riso-h nodad
$S ip link set riso-h up
$S ip netns exec riso-sink sh -c '
	sysctl -qw net.ipv6.conf.all.disable_ipv6=0 net.ipv4.ping_group_range="0 2147483647"
	ip addr add 198.18.0.10/24 dev riso-s && ip addr add 198.18.0.11/24 dev riso-s &&
	ip -6 addr add fd00::10/64 dev riso-s nodad && ip link set lo up && ip link set riso-s up &&
	ip route add default via 198.18.0.1 && ip -6 route add default via fd00::1' || die "sink netns addresses"
cp /etc/resolv.conf "$W/resolv.orig"
echo "nameserver 198.18.0.10" > /etc/resolv.conf || die "resolv.conf"

for mode in "${MODES[@]}"; do
	case $mode in
	native)
		log "baseline-native"
		sink_start base-native
		"$W/egressprobe" app -sink 198.18.0.10 -sink6 fd00::10 -imds 198.18.0.11 -listen :18081 2> "$W/out/base-native/app.log" &
		AP=$!
		waitfor 30 curl -sf localhost:18081/health || echo "baseline app not up"
		fire http://localhost:18081 "$W/out/base-native/app.log"
		kill $AP; wait $AP 2>/dev/null
		sink_stop

		mkdir -p "$W/native" && cd "$W/native" || die cd
		bypass_yml
		log "record-native"
		sink_start record-native
		$S "$W/keploy" record -c "$W/egressprobe $APPARGS" --disable-tele --disableANSI > "$W/out/record-native/keploy.log" 2>&1 &
		KP=$!
		waitfor 180 curl -sf localhost:8080/health || echo "record app not up"
		fire http://localhost:8080 "$W/out/record-native/keploy.log"
		waitfor 60 sh -c 'ls keploy/test-set-*/tests/*.yaml' || { echo "no test recorded"; FAILED=1; }
		$S kill -INT $KP 2>/dev/null
		waitfor 120 sh -c "! kill -0 $KP" || { echo "record did not stop"; $S kill -9 $KP 2>/dev/null; }
		sink_stop

		log "replay-native"
		sink_start replay-native
		$S "$W/keploy" test -c "$W/egressprobe $APPARGS" --delay 5 --apiTimeout 60 --disable-tele --disableANSI --generate-github-actions=false > "$W/out/replay-native/keploy.log" 2>&1
		echo "replay-native exit $?"
		waitfor 15 grep -q 'probe fork-tcp' "$W/out/replay-native/keploy.log"
		grep -h 'status:' keploy/reports/test-run-0/*.yaml 2>/dev/null | sort | uniq -c
		sink_stop
		cd "$W" || die cd
		;;
	docker)
		log "baseline-docker"
		sink_start base-docker
		docker run -d --name riso-base -p 18082:8080 --net keploy-network riso-probe:local $APPARGS >/dev/null || die "baseline container"
		waitfor 30 curl -sf localhost:18082/health || echo "baseline container not up"
		curl -s --max-time 120 localhost:18082/fire > /dev/null
		waitfor 15 sh -c 'docker logs riso-base 2>&1 | grep -q "probe fork-tcp"'
		docker logs riso-base > "$W/out/base-docker/app.log" 2>&1
		docker rm -f riso-base >/dev/null
		sink_stop

		RUN="docker run -p 8080:8080 --net keploy-network --rm --name riso-app riso-probe:local $APPARGS"
		mkdir -p "$W/docker" && cd "$W/docker" || die cd
		bypass_yml
		log "record-docker"
		sink_start record-docker
		$S "$W/keploy" record -c "$RUN" --container-name riso-app --disable-tele --disableANSI > "$W/out/record-docker/keploy.log" 2>&1 &
		KP=$!
		waitfor 300 curl -sf localhost:8080/health || { echo "record app not up"; docker ps -a --format '{{.Names}} {{.Image}} {{.Status}}'; docker logs keploy-v3 2>&1 | tail -20 | cut -c1-300; }
		fire http://localhost:8080 "$W/out/record-docker/keploy.log"
		waitfor 60 sh -c 'ls keploy/test-set-*/tests/*.yaml' || { echo "no test recorded"; FAILED=1; }
		$S kill -INT $KP 2>/dev/null
		waitfor 120 sh -c "! kill -0 $KP" || { echo "record did not stop"; $S kill -9 $KP 2>/dev/null; }
		docker rm -f riso-app >/dev/null 2>&1
		sink_stop

		log "replay-docker"
		sink_start replay-docker
		$S "$W/keploy" test -c "$RUN" --containerName riso-app --delay 10 --apiTimeout 60 --disable-tele --disableANSI --generate-github-actions=false > "$W/out/replay-docker/keploy.log" 2>&1
		echo "replay-docker exit $?"
		grep -h 'status:' keploy/reports/test-run-0/*.yaml 2>/dev/null | sort | uniq -c
		docker rm -f riso-app >/dev/null 2>&1
		sink_stop
		cd "$W" || die cd
		;;
	internal)
		# B2 viability: an internal network has no route out, but can the
		# host (keploy's side) still reach the container by its IP?
		log "internal-docker"
		sink_start internal-docker
		docker network create --internal riso-int >/dev/null || die "internal network"
		docker run -d --name riso-int-c --net riso-int riso-probe:local $APPARGS >/dev/null || die "internal container"
		ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' riso-int-c)
		if waitfor 30 curl -sf "http://$ip:8080/health"; then echo "internal: host reaches the container at $ip:8080"; else echo "internal: host CANNOT reach the container at $ip:8080"; fi
		curl -s --max-time 120 "http://$ip:8080/fire" > /dev/null
		waitfor 15 sh -c 'docker logs riso-int-c 2>&1 | grep -q "probe fork-tcp"'
		docker logs riso-int-c > "$W/out/internal-docker/app.log" 2>&1
		docker rm -f riso-int-c >/dev/null
		sink_stop
		;;
	esac
done

log "keploy logs (errors, warnings, last lines)"
for f in "$W"/out/*/keploy.log; do
	[ -f "$f" ] || continue
	echo "-- $f"
	grep -E 'ERROR|WARN|FATAL|panic|level=error' "$f" | cut -c1-300 | sort | uniq -c | sort -rn | head -25
	echo "   last lines:"; tail -15 "$f" | cut -c1-300
done

log "counts (arrivals at the sink per path)"
phases=$(cd "$W/out" && ls -d */ | tr -d / | sort)
printf '%-14s' path; for p in $phases; do printf ' %15s' "$p"; done; echo
for path in $PATHS; do
	printf '%-14s' "$path"
	for p in $phases; do c=$(cat "$W/out/$p/$path.count" 2>/dev/null || echo 0); printf ' %15s' "$c"; done
	echo
done
log "probe outcomes (app side)"
for p in $phases; do
	f="$W/out/$p/app.log"; [ -f "$f" ] || f="$W/out/$p/keploy.log"
	echo "-- $p"; grep -o 'probe [a-z0-9-]*: .*' "$f" 2>/dev/null | cut -c1-160 | sort | uniq
done
log "dns names seen"
for p in $phases; do echo "-- $p"; sort "$W/out/$p/dns-names.txt" 2>/dev/null | uniq -c | head -20; done
if [ -d /keep ]; then
	tar -C "$W" -czf /keep/riso-out.tgz out $(cd "$W" && ls -d native/keploy docker/keploy 2>/dev/null)
	echo "artifact: /keep/riso-out.tgz (the box's ~/lane-keep/<run>/riso-out.tgz)"
fi
exit $FAILED
