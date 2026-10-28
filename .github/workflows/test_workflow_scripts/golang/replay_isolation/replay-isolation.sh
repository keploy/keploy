#!/bin/bash
# replay-isolation.sh [MODE...]   MODE: native docker internal (default: native docker)
#
# Replay network isolation lane. An app (probe/) tries every way out of a
# keploy replay against a sink that counts what arrives, and the lane asserts
# what keploy keeps inside today. Runs on a CI runner (sudo, docker) or inside
# lane-container.sh (privileged, its own netns and dockerd):
#   lane-container.sh -n riso <keploy checkout> -- \
#     bash .github/workflows/test_workflow_scripts/golang/replay_isolation/replay-isolation.sh
#
# Inputs:
#   KEPLOY_BIN   a built keploy; with it the agent image must already be loaded
#                as KEPLOY_IMAGE (CI: download-binary + download-image). Without
#                it, keploy and the image are built from KEPLOY_SRC (default:
#                this checkout).
#   KEPLOY_IMAGE the agent image (default ghcr.io/keploy/keploy:v3-dev).
#   RISO_WORK    the work dir (default /work/riso; CI: $RUNNER_TEMP/riso).
#
# The probe serves GET /fire, which tries each path at the sink and answers a
# constant, so record and replay agree. The sink lives in its own netns
# (riso-sink) behind a veth, so it is "the internet" for native and docker apps
# alike: 198.18.0.10, 198.18.0.11 (stands in for IMDS; the real
# 169.254.169.254 is never routed) and fd00::10. The sink is also the
# resolver the app is given, so a DNS lookup that leaves keploy is counted too
# (the sink answers NXDOMAIN and logs each name). That holds natively, where
# keploy and the app run in a mount namespace whose resolv.conf names the sink,
# so the runner's own resolver is left alone. Under docker the app shares the
# agent container's netns (keploy refuses an app --dns there: "conflicting
# options"), whose only resolver is docker's 127.0.0.11 forwarding to the
# runner's: docker DNS is asserted by its opt-in rows here and by
# pkg/agent/proxy's tests, not by sink counts.
#
# Per mode: baseline (no keploy: each path must reach the sink, so a zero in
# replay means keploy kept it in), record, replay, and the opt-in rows.
# "internal" (report only) runs the probe on an internal: true docker network.
# Counts land in $W/out/<phase>/<path>.count. Each assertion prints one
# "ROW <name>: PASS|FAIL" line and any FAIL fails the lane.
#
# Not asserted, KNOWN GAP lines instead: until a kernel-level deny lands,
# non-DNS UDP, ICMP and raw sockets, and a native app's double-forked child,
# still reach the real network in replay. No secret is involved anywhere.
set -uo pipefail
HERE=$(cd "$(dirname "$0")" && pwd)
KEPLOY_SRC=${KEPLOY_SRC:-$(cd "$HERE/../../../../.." && pwd)}
IMAGE=${KEPLOY_IMAGE:-ghcr.io/keploy/keploy:v3-dev}
W=${RISO_WORK:-/work/riso}
rm -rf "$W" && mkdir -p "$W/out"
S=""; [ "$(id -u)" = 0 ] || S="sudo -E"
MODES=("$@"); [ ${#MODES[@]} -gt 0 ] || MODES=(native docker)
SINKPID=""
FAILED=0
PATHS="tcp bypass-tcp fork-tcp tfo imds-tcp udp-connected udp-sendto udp-quic icmp-raw icmp-dgram dns-udp dns-other dns-tcp tcp6 udp6"
APPARGS="app -sink 198.18.0.10 -sink6 fd00::10 -imds 198.18.0.11"
# Native runs: as root, in a mount namespace of their own whose resolv.conf
# names the sink and whose nsswitch.conf asks files then DNS (a runner's image
# may list systemd-resolved first, which answers from outside the namespace).
SINKDNS=($S unshare -m sh -c 'mount --bind "$0/resolv.conf" /etc/resolv.conf && { [ ! -e /etc/nsswitch.conf ] || mount --bind "$0/nsswitch.conf" /etc/nsswitch.conf; } && exec "$@"' "$W")
# How keploy test is started: SINKDNS natively, plain (sudo) under docker.
RUNNER=()

log() { echo "== $(date +%T) $*"; }
die() { echo "FATAL: $*"; FAILED=1; exit 1; }

cleanup() {
	[ -n "$SINKPID" ] && $S kill "$SINKPID" 2>/dev/null
	docker rm -f riso-app riso-base riso-int-c >/dev/null 2>&1
	docker network rm riso-int >/dev/null 2>&1
	$S ip netns del riso-sink 2>/dev/null
	$S ip link del riso-h 2>/dev/null
	$S iptables -D FORWARD -d 198.18.0.0/24 -j ACCEPT 2>/dev/null
	$S iptables -D FORWARD -s 198.18.0.0/24 -j ACCEPT 2>/dev/null
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

# A port-only bypass rule for 7002 (the sink's bypass port), in keploy.yml.
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

count() { local c; c=$(cat "$W/out/$1/$2.count" 2>/dev/null) || c=0; echo "${c:-0}"; }

# row NAME OK DETAIL: one assertion's verdict.
row() {
	if [ "$2" = 1 ]; then echo "ROW $1: PASS ($3)"; else echo "ROW $1: FAIL ($3)"; FAILED=1; fi
}
# rows_zero PHASE PATH...: none of PATHs reached the sink in PHASE.
rows_zero() {
	local phase=$1 p c; shift
	for p in "$@"; do c=$(count "$phase" "$p"); row "$phase $p kept inside" "$([ "$c" = 0 ] && echo 1)" "$c at the sink"; done
}
# rows_reach PHASE PATH...: each PATH reached the sink in PHASE (the probe works).
rows_reach() {
	local phase=$1 p c; shift
	for p in "$@"; do c=$(count "$phase" "$p"); row "$phase $p reaches the sink" "$([ "$c" -gt 0 ] && echo 1)" "$c at the sink"; done
}
# row_log NAME FILE REGEX: FILE has a line matching REGEX.
row_log() { row "$1" "$(grep -qE -- "$3" "$2" 2>/dev/null && echo 1)" "/$3/ in $(basename "$(dirname "$2")")"; }
# row_nolog NAME FILE REGEX: FILE has no line matching REGEX.
row_nolog() { row "$1" "$(grep -qE -- "$3" "$2" 2>/dev/null || echo 1)" "no /$3/ in $(basename "$(dirname "$2")")"; }

# replay PHASE CMD...: one keploy test run in the current dir, its log and exit.
replay() {
	local phase=$1; shift
	log "$phase"
	sink_start "$phase"
	"${RUNNER[@]}" "$KEPLOY" test "$@" --apiTimeout 60 --disable-tele --disableANSI --generate-github-actions=false > "$W/out/$phase/keploy.log" 2>&1
	echo $? > "$W/out/$phase/exit"
	echo "$phase exit $(cat "$W/out/$phase/exit")"
	waitfor 15 grep -q 'probe fork-tcp' "$W/out/$phase/keploy.log"
	grep -h 'status:' keploy/reports/test-run-*/*.yaml 2>/dev/null | sort | uniq -c
	docker rm -f riso-app >/dev/null 2>&1
	sink_stop
}

log "builds"
if [ -n "${KEPLOY_BIN:-}" ]; then
	KEPLOY=$KEPLOY_BIN
	docker image inspect "$IMAGE" >/dev/null 2>&1 || [[ " ${MODES[*]} " != *" docker "* ]] || die "KEPLOY_BIN is set but the agent image $IMAGE is not loaded"
else
	KEPLOY=$W/keploy
	(cd "$KEPLOY_SRC" && CGO_ENABLED=0 go build -tags=viper_bind_struct -o "$KEPLOY" .) || die "keploy build"
fi
(cd "$HERE/probe" && CGO_ENABLED=0 go build -o "$W/egressprobe" .) || die "probe build"
"$KEPLOY" --version 2>&1 | grep -i version | head -1
uname -r; docker version -f 'docker server {{.Server.Version}}' 2>&1

need_docker=0
for m in "${MODES[@]}"; do [ "$m" = native ] || need_docker=1; done
if [ $need_docker = 1 ]; then
	log "images"
	if [ -z "${KEPLOY_BIN:-}" ]; then
		mkdir -p "$W/img/dist/keploy-linux-amd64"
		cp "$KEPLOY" "$W/img/dist/keploy-linux-amd64/keploy"
		cp "$KEPLOY_SRC/entrypoint.sh" "$KEPLOY_SRC/Dockerfile.runtime" "$W/img/"
		docker build -q --build-arg TARGETARCH=amd64 -f "$W/img/Dockerfile.runtime" -t "$IMAGE" "$W/img" || die "agent image"
	fi
	mkdir -p "$W/pimg/tmp" && touch "$W/pimg/tmp/.keep" # the fork path's done-file goes to /tmp
	cp "$W/egressprobe" "$W/pimg/"
	printf 'FROM scratch\nCOPY egressprobe /egressprobe\nCOPY tmp /tmp\nENTRYPOINT ["/egressprobe"]\n' > "$W/pimg/Dockerfile"
	docker build -q -t riso-probe:local "$W/pimg" || die "probe image"
	docker network create keploy-network >/dev/null 2>&1 || true
fi

log "sink netns"
$S ip netns add riso-sink || die "ip netns add"
$S ip link add riso-h type veth peer name riso-s || die "veth"
$S ip link set riso-s netns riso-sink
$S sysctl -qw net.ipv4.ip_forward=1 net.ipv6.conf.all.disable_ipv6=0 2>&1
$S ip addr add 198.18.0.1/24 dev riso-h
$S ip -6 addr add fd00::1/64 dev riso-h nodad
$S ip link set riso-h up
$S ip netns exec riso-sink sh -c '
	sysctl -qw net.ipv6.conf.all.disable_ipv6=0 net.ipv4.ping_group_range="0 2147483647"
	ip addr add 198.18.0.10/24 dev riso-s && ip addr add 198.18.0.11/24 dev riso-s &&
	ip -6 addr add fd00::10/64 dev riso-s nodad && ip link set lo up && ip link set riso-s up &&
	ip route add default via 198.18.0.1 && ip -6 route add default via fd00::1' || die "sink netns addresses"
# A runner's firewall may drop forwarded traffic (docker sets FORWARD to DROP):
# the docker app reaches the sink through the host's forwarding.
$S iptables -I FORWARD -d 198.18.0.0/24 -j ACCEPT 2>/dev/null
$S iptables -I FORWARD -s 198.18.0.0/24 -j ACCEPT 2>/dev/null
echo "nameserver 198.18.0.10" > "$W/resolv.conf"
{ grep -v '^hosts:' /etc/nsswitch.conf 2>/dev/null; echo "hosts: files dns"; } > "$W/nsswitch.conf"

for mode in "${MODES[@]}"; do
	case $mode in
	native)
		log "baseline-native"
		sink_start base-native
		"${SINKDNS[@]}" "$W/egressprobe" app -sink 198.18.0.10 -sink6 fd00::10 -imds 198.18.0.11 -listen :18081 2> "$W/out/base-native/app.log" &
		AP=$!
		waitfor 30 curl -sf localhost:18081/health || echo "baseline app not up"
		fire http://localhost:18081 "$W/out/base-native/app.log"
		$S kill $AP; wait $AP 2>/dev/null
		sink_stop

		mkdir -p "$W/native" && cd "$W/native" || die cd
		bypass_yml
		log "record-native"
		sink_start record-native
		"${SINKDNS[@]}" "$KEPLOY" record -c "$W/egressprobe $APPARGS" --disable-tele --disableANSI > "$W/out/record-native/keploy.log" 2>&1 &
		KP=$!
		waitfor 180 curl -sf localhost:8080/health || echo "record app not up"
		fire http://localhost:8080 "$W/out/record-native/keploy.log"
		waitfor 60 sh -c 'ls keploy/test-set-*/tests/*.yaml' || { echo "no test recorded"; FAILED=1; }
		$S kill -INT $KP 2>/dev/null
		waitfor 120 sh -c "! $S kill -0 $KP" || { echo "record did not stop"; $S kill -9 $KP 2>/dev/null; }
		sink_stop

		RUNNER=("${SINKDNS[@]}")
		replay replay-native -c "$W/egressprobe $APPARGS" --delay 5
		replay replay-native-dns -c "$W/egressprobe $APPARGS" --delay 5 --allow-egress dns

		# An entry keploy does not act on is refused before anything starts.
		log "refuse-native"
		mkdir -p "$W/out/refuse-native"
		"${SINKDNS[@]}" "$KEPLOY" test -c "$W/egressprobe $APPARGS" --allow-egress imds --disable-tele --disableANSI --generate-github-actions=false > "$W/out/refuse-native/keploy.log" 2>&1
		echo $? > "$W/out/refuse-native/exit"
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
		$S "$KEPLOY" record -c "$RUN" --container-name riso-app --disable-tele --disableANSI > "$W/out/record-docker/keploy.log" 2>&1 &
		KP=$!
		waitfor 300 curl -sf localhost:8080/health || { echo "record app not up"; docker ps -a --format '{{.Names}} {{.Image}} {{.Status}}'; }
		fire http://localhost:8080 "$W/out/record-docker/keploy.log"
		waitfor 60 sh -c 'ls keploy/test-set-*/tests/*.yaml' || { echo "no test recorded"; FAILED=1; }
		$S kill -INT $KP 2>/dev/null
		waitfor 120 sh -c "! $S kill -0 $KP" || { echo "record did not stop"; $S kill -9 $KP 2>/dev/null; }
		docker rm -f riso-app >/dev/null 2>&1
		sink_stop

		RUNNER=($S)
		replay replay-docker -c "$RUN" --containerName riso-app --delay 10
		replay replay-docker-dns -c "$RUN" --containerName riso-app --delay 10 --allow-egress dns
		cd "$W" || die cd
		;;
	internal)
		# Report only: an internal network has no route out, but can the host
		# (keploy's side) still reach the container by its IP?
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
phases=$(cd "$W/out" && ls -d */ | tr -d / | grep -v '^refuse' | sort)
printf '%-14s' path; for p in $phases; do printf ' %17s' "$p"; done; echo
for path in $PATHS; do
	printf '%-14s' "$path"
	for p in $phases; do printf ' %17s' "$(count "$p" "$path")"; done
	echo
done
log "probe outcomes (app side)"
for p in $phases; do
	f="$W/out/$p/app.log"; [ -f "$f" ] || f="$W/out/$p/keploy.log"
	echo "-- $p"; grep -o 'probe [a-z0-9-]*: .*' "$f" 2>/dev/null | cut -c1-160 | sort | uniq
done
log "dns names seen"
for p in $phases; do echo "-- $p"; sort "$W/out/$p/dns-names.txt" 2>/dev/null | uniq -c | head -20; done
log "kept inside (each replay's report)"
for p in $phases; do
	[ -f "$W/out/$p/keploy.log" ] || continue
	echo "-- $p"; grep -o 'kept inside the replay: .*' "$W/out/$p/keploy.log" | cut -c1-200
done

log "rows"
EGRESS_DNS='real egress opened by test.allowEgress \(dns\)'
KEPT='kept inside the replay: '
for mode in "${MODES[@]}"; do
	case $mode in
	native)
		rows_reach base-native tcp tfo imds-tcp dns-udp
		row "replay-native passes" "$([ "$(cat "$W/out/replay-native/exit" 2>/dev/null)" = 0 ] && echo 1)" "exit $(cat "$W/out/replay-native/exit" 2>/dev/null)"
		# A DNS miss is answered by keploy: native has no in-network resolver.
		rows_zero replay-native tcp tfo imds-tcp dns-udp dns-other dns-tcp
		row_log "replay-native names the resolvers it does not ask" "$W/out/replay-native/keploy.log" 'forwarded only to a resolver inside the replay.s own network.*not_asked.*198\.18\.0\.10'
		row_nolog "replay-native opens no DNS egress" "$W/out/replay-native/keploy.log" "$EGRESS_DNS"
		row_log "replay-native names the port-only bypass rule" "$W/out/replay-native/keploy.log" 'real egress opened by bypassRules.*7002'
		row_log "replay-native reports the IMDS call it kept inside" "$W/out/replay-native/keploy.log" "${KEPT}HTTP 198\.18\.0\.11"
		row_log "replay-native reports the DNS name it answered itself" "$W/out/replay-native/keploy.log" "${KEPT}DNS leak-probe\.invalid-replay-isolation\.test"
		# The opt-in: the same misses now reach the host's resolver.
		row "replay-native-dns passes" "$([ "$(cat "$W/out/replay-native-dns/exit" 2>/dev/null)" = 0 ] && echo 1)" "exit $(cat "$W/out/replay-native-dns/exit" 2>/dev/null)"
		c=$(( $(count replay-native-dns dns-udp) + $(count replay-native-dns dns-other) + $(count replay-native-dns dns-tcp) ))
		row "replay-native-dns forwards to the host's resolver" "$([ "$c" -gt 0 ] && echo 1)" "$c DNS at the sink"
		row_log "replay-native-dns names its opt-in" "$W/out/replay-native-dns/keploy.log" "$EGRESS_DNS"
		row_nolog "replay-native-dns does not report the forwarded name as kept inside" "$W/out/replay-native-dns/keploy.log" "${KEPT}DNS leak-probe"
		row "refuse-native fails" "$([ "$(cat "$W/out/refuse-native/exit" 2>/dev/null)" != 0 ] && echo 1)" "exit $(cat "$W/out/refuse-native/exit" 2>/dev/null)"
		row_log "refuse-native says why" "$W/out/refuse-native/keploy.log" 'entry \\?"imds\\?" is not one keploy can open'
		;;
	docker)
		rows_reach base-docker tcp fork-tcp tfo imds-tcp
		row "replay-docker passes" "$([ "$(cat "$W/out/replay-docker/exit" 2>/dev/null)" = 0 ] && echo 1)" "exit $(cat "$W/out/replay-docker/exit" 2>/dev/null)"
		rows_zero replay-docker tcp fork-tcp tfo imds-tcp
		row_nolog "replay-docker opens no DNS egress" "$W/out/replay-docker/keploy.log" "$EGRESS_DNS"
		row_log "replay-docker names the port-only bypass rule" "$W/out/replay-docker/keploy.log" 'real egress opened by bypassRules.*7002'
		row_log "replay-docker reports the IMDS call it kept inside" "$W/out/replay-docker/keploy.log" "${KEPT}HTTP 198\.18\.0\.11"
		row "replay-docker-dns passes" "$([ "$(cat "$W/out/replay-docker-dns/exit" 2>/dev/null)" = 0 ] && echo 1)" "exit $(cat "$W/out/replay-docker-dns/exit" 2>/dev/null)"
		row_log "replay-docker-dns: the agent gets the opt-in" "$W/out/replay-docker-dns/keploy.log" "$EGRESS_DNS"
		;;
	esac
done
# Reported, not asserted: what only a kernel-level deny (parked) can close.
for p in $phases; do
	case $p in replay-*) ;; *) continue ;; esac
	gaps=""
	for path in fork-tcp udp-connected udp-sendto udp-quic icmp-raw icmp-dgram tcp6 udp6; do
		c=$(count "$p" "$path"); [ "$c" = 0 ] || gaps="$gaps $path=$c"
	done
	[ -z "$gaps" ] || echo "KNOWN GAP $p (no kernel-level deny yet):$gaps"
	echo "OPTED IN $p: bypass-tcp=$(count "$p" bypass-tcp) (bypassRules port 7002)"
done

if [ -d /keep ]; then
	tar -C "$W" -czf /keep/riso-out.tgz out $(cd "$W" && ls -d native/keploy docker/keploy 2>/dev/null)
	echo "artifact: /keep/riso-out.tgz (the box's ~/lane-keep/<run>/riso-out.tgz)"
fi
[ "$FAILED" = 0 ] && echo "replay isolation: every row passed" || echo "replay isolation: a row FAILED"
exit $FAILED
