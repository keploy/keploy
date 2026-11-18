#!/bin/bash
# replay-isolation.sh [MODE...]   MODE: native docker compose internal (default: native docker)
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
# so the runner's own resolver is left alone. Under docker the app's resolver
# is docker's 127.0.0.11, which answers the network's own names and forwards
# every other one to its upstream. The app is run with --dns 198.18.0.10
# (a compose app with dns: [198.18.0.10]), so that upstream is the sink and
# only the sink: keploy moves an app's DNS servers onto its agent container,
# whose netns the app joins (docker refuses them on the app there), and docker
# gives a container's own DNS servers precedence over a DNS server set for
# the whole daemon (dockerd --dns, daemon.json "dns") and over the host's
# /etc/resolv.conf. The host's file is no way to set it: on a runner whose
# daemon has a DNS server of its own, 127.0.0.11 never asks the hosts the file
# names. GitHub's runners in its westus2 egress-proxy experiment are such
# runners (their resolv.conf is written by repoint-resolv-conf); a lane that
# listed the sink first in /etc/resolv.conf counted 0 for every docker DNS row
# there, the baseline's included. Each phase also asks the sink one question
# of its own as it starts (sink-check), and keeps the resolv.conf docker wrote
# for its app, whose ExtServers line names what 127.0.0.11 forwards to: a DNS
# row with nothing at the sink then says which side failed (dns_why).
#
# Per mode: baseline (no keploy: each path must reach the sink, so a zero in
# replay means keploy kept it in), record, replay, and the opt-in rows.
# The recording's probe runs without -unrecorded, so the names the isolation
# rows count (leak-probe*, the peer's other types) are not in the recording
# and stay misses at replay. It does look up recorded-nx.*, which does not
# exist: the recording holds its NXDOMAIN and the replay must serve it, with
# no resolver asked even under the DNS opt-in; and the peer's name, before
# the peer is up, so the recording holds that NXDOMAIN too and the replay must
# still ask docker's resolver, which now knows the peer.
# With -unrecorded the probe also looks up startup-probe.* as it starts,
# before it serves, so outside every test (the startup rows, rows_startup).
# It also looks up stop-probe.* as it stops, after its last test (the
# after-last rows, rows_after_last). Recording and replay both look up
# stop-recorded.* as the app stops (-stop-recorded): keploy record stops the
# app before it saves what the agent captured, in every mode, so the
# recording holds that NXDOMAIN and the replay serves it (rows_recorded_nx).
# "internal" (report only) runs the probe on an internal: true docker network.
# Counts land in $W/out/<phase>/<path>.count. Each assertion prints one
# "ROW <name>: PASS|FAIL" line and any FAIL fails the lane.
#
# Not asserted, KNOWN GAP lines instead: until a kernel-level deny lands,
# non-DNS UDP, ICMP and raw sockets, and a native app's double-forked child,
# still reach the real network in replay. No secret is involved anywhere.

# keploy runs under sudo, which hands it only the variables its command line
# names (sudo-rs, Ubuntu 26.04's sudo, ignores -E): SUDO_ENV names the
# switches a workflow sets for keploy, each only while it is set.
# shellcheck source=../../sudo_env.sh
source "$(dirname "${BASH_SOURCE[0]}")/../../sudo_env.sh" || exit 1

set -uo pipefail
# The proven switch is not among them (sudo_env.tests.sh): each keploy
# command here names KEPLOY_PROVEN itself, as this script has it (unset stays
# unset), so keploy runs in the mode the rows expect. Each replay's report
# then says that mode (the proven rows, rows_mode).
PROVEN_ENV=()
[ -z "${KEPLOY_PROVEN+x}" ] || PROVEN_ENV=("KEPLOY_PROVEN=$KEPLOY_PROVEN")
HERE=$(cd "$(dirname "$0")" && pwd)
KEPLOY_SRC=${KEPLOY_SRC:-$(cd "$HERE/../../../../.." && pwd)}
IMAGE=${KEPLOY_IMAGE:-ghcr.io/keploy/keploy:v3-dev}
W=${RISO_WORK:-/work/riso}
rm -rf "$W" && mkdir -p "$W/out"
S=""; [ "$(id -u)" = 0 ] || S="sudo"
MODES=("$@"); [ ${#MODES[@]} -gt 0 ] || MODES=(native docker)
SINKPID=""
FAILED=0
PATHS="tcp bypass-tcp fork-tcp tfo imds-tcp udp-connected udp-sendto udp-quic icmp-raw icmp-dgram dns-udp dns-bare dns-other dns-tcp tcp6 udp6 dns-owned dns-recorded"
# The recording makes no metadata call (RECARGS has no -imds), so the replay's
# is one no mock answers, as the IMDS rows need. keploy records a call that
# ended with no answer as its fault: the probe writes its metadata request and
# closes without reading, and the sink closes as it accepts, so a recorded one
# would be a fault mock (whichever close came first) that the replay serves,
# and the replay's report would not name the call.
RECARGS="app -sink 198.18.0.10 -sink6 fd00::10 -peer riso-peer -stop-recorded"
APPARGS="$RECARGS -imds 198.18.0.11 -unrecorded"
# keploy test stops each set's application before it reads the calls made
# after the set's last test (with the switch off after the set's
# post-script, before its report), so a call the application makes as it
# stops is read and named as made after its last test (rows_after_last).
# Under docker compose too: keploy stops the application's container itself,
# its stack, keploy's agent among it, running on.
APPARGS="$APPARGS -stop-lookup"
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
	[ ! -f "$W/compose/docker-compose.yml" ] || (cd "$W/compose" && docker compose down --remove-orphans >/dev/null 2>&1)
	[ ! -f "$W/compose-base/docker-compose.yml" ] || (cd "$W/compose-base" && docker compose down --remove-orphans >/dev/null 2>&1)
	[ ! -f "$W/compose-runner/docker-compose.yml" ] || (cd "$W/compose-runner" && docker compose down --remove-orphans >/dev/null 2>&1)
	[ ! -f "$W/compose-early/docker-compose.yml" ] || (cd "$W/compose-early" && docker compose down --remove-orphans >/dev/null 2>&1)
	docker rm -f riso-app riso-base riso-int-c riso-peer riso-oneshot riso-runner riso-early >/dev/null 2>&1
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

# The docker phases' app DNS server: the sink, as 127.0.0.11's only upstream
# (see the top).
APPDNS="--dns 198.18.0.10"

# docker_resolv PHASE CONTAINER: keeps the resolv.conf docker wrote for
# CONTAINER (an app in keploy's agent's netns reads the agent's), as
# $W/out/PHASE/resolv.conf. Fails until docker has written it.
docker_resolv() {
	local f
	f=$(docker inspect -f '{{.ResolvConfPath}}' "$2" 2>/dev/null) && [ -n "$f" ] &&
		$S cat "$f" > "$W/out/$1/resolv.conf.part" 2>/dev/null && grep -q '^nameserver' "$W/out/$1/resolv.conf.part" &&
		mv "$W/out/$1/resolv.conf.part" "$W/out/$1/resolv.conf"
}
# upstreams PHASE: what docker's 127.0.0.11 forwarded PHASE's names to, as
# docker wrote it (its ExtServers line: "host(ADDR)" is asked from the
# host's netns, a bare ADDR, set as a DNS server of the container's or the
# daemon's, from the container's).
upstreams() { sed -n 's/^# ExtServers: \[\(.*\)\]$/\1/p' "$W/out/$1/resolv.conf" 2>/dev/null; }
# dns_why PHASE: for a DNS row with nothing at the sink, which side failed:
# the sink, when it did not answer the lane's own question at the phase's
# start (sink_start); else the resolver the app asked, when what it forwards
# to does not start with the sink (another resolver answered the name); else
# the path between them.
dns_why() {
	local p=$1 chk ext
	chk=$(cat "$W/out/$p/sink-check" 2>/dev/null)
	case $chk in
	"answered rcode="*) ;;
	"") echo "the lane asked the sink no DNS question of its own in $p, so which side failed is not known"; return ;;
	*) echo "the sink did not answer the lane's own DNS question at the start of $p ($chk): the sink was not receiving"; return ;;
	esac
	case $p in
	*native*) echo "the sink answered the lane's own DNS question at the start of $p, and $p's resolv.conf names the sink alone: nothing in $p asked it"; return ;;
	esac
	ext=$(upstreams "$p")
	case "$ext" in
	"") echo "the sink answered the lane's own DNS question at the start of $p; docker's resolv.conf for $p was not read, so where 127.0.0.11 forwarded is not known" ;;
	198.18.0.10 | "198.18.0.10 "* | "host(198.18.0.10)"*) echo "the sink answered the lane's own DNS question at the start of $p, and docker's 127.0.0.11 there forwards to [$ext], the sink first: the queries were lost between them" ;;
	*) echo "the sink answered the lane's own DNS question at the start of $p, but docker's 127.0.0.11 there forwards to [$ext], not to the sink: another resolver answered the names" ;;
	esac
}

# compose_yml NAME HOSTPORT ARGS [peer] [oneshot] [runner] [early] [migrate]
# [flaky] [seed] [watcher]: a compose file whose service is the probe run with
# ARGS, container NAME, its 8080 published on HOSTPORT, the sink its DNS
# server; with "peer", also riso-peer, a container the probe resolves by name;
# with "oneshot", also riso-oneshot, a container that exits 0 as soon as it
# starts, as a stack's one-shot migration does; with "runner", also the
# service runner (container riso-runner), a test driver that calls the app
# through its published port, once its health check answers 5 more times, and
# exits 0; with "early", also the service early (container riso-early), which
# waits on nothing and exits 3 at once, before the app's container starts
# (that waits for keploy's agent's health); with "migrate", also the service
# migrate, which exits 0 15 s after it starts (after keploy's agent is
# healthy), and the app waits on it (depends_on,
# service_completed_successfully); with "flaky", also the service flaky, which
# exits 1 on its first start only (a mark on the stack's volume flakymark) and
# runs on at a later one, and the app depends on it in list form (`depends_on:
# [flaky]`: no wait on its exit); with "seed", also the service seed, which
# starts once the app has (`depends_on: [NAME]`) and exits 0 8 s later, and
# the runner waits on it (service_completed_successfully); with "watcher",
# also the service watcher, which exits 1 once the app, which it calls through
# its published port, stops answering.
compose_yml() {
	cat > docker-compose.yml <<YML
services:
  $1:
    image: riso-probe:local
    container_name: $1
    command: [$(printf '"%s", ' $3 | sed 's/, $//')]
    ports: ["$2:8080"]
    dns: ["198.18.0.10"]
YML
	case " ${*:4} " in *" migrate "*) cat >> docker-compose.yml <<YML
    depends_on:
      migrate:
        condition: service_completed_successfully
YML
	;; esac
	case " ${*:4} " in *" flaky "*) cat >> docker-compose.yml <<YML
    depends_on: [flaky]
YML
	;; esac
	case " ${*:4} " in *" peer "*) cat >> docker-compose.yml <<YML
  riso-peer:
    image: riso-probe:local
    container_name: riso-peer
    command: ["app"]
YML
	;; esac
	case " ${*:4} " in *" oneshot "*) cat >> docker-compose.yml <<YML
  riso-oneshot:
    image: riso-probe:local
    container_name: riso-oneshot
    command: ["oneshot"]
YML
	;; esac
	case " ${*:4} " in *" runner "*) cat >> docker-compose.yml <<YML
  runner:
    image: riso-probe:local
    container_name: riso-runner
    command: ["runner", "-url", "http://host.docker.internal:$2", "-n", "5"]
    extra_hosts: ["host.docker.internal:host-gateway"]
YML
		case " ${*:4} " in *" seed "*) cat >> docker-compose.yml <<YML
    depends_on:
      seed:
        condition: service_completed_successfully
YML
		;; esac
	;; esac
	case " ${*:4} " in *" early "*) cat >> docker-compose.yml <<YML
  early:
    image: riso-probe:local
    container_name: riso-early
    command: ["early"]
YML
	;; esac
	case " ${*:4} " in *" migrate "*) cat >> docker-compose.yml <<YML
  migrate:
    image: riso-probe:local
    container_name: riso-migrate
    command: ["after", "-wait", "15s", "-say", "MIGRATE"]
YML
	;; esac
	case " ${*:4} " in *" flaky "*) cat >> docker-compose.yml <<YML
  flaky:
    image: riso-probe:local
    container_name: riso-flaky
    command: ["flaky", "-mark", "/w/ran"]
    volumes: ["flakymark:/w"]
YML
	;; esac
	case " ${*:4} " in *" seed "*) cat >> docker-compose.yml <<YML
  seed:
    image: riso-probe:local
    container_name: riso-seed
    depends_on: [$1]
    command: ["after", "-wait", "8s", "-say", "SEED"]
YML
	;; esac
	case " ${*:4} " in *" watcher "*) cat >> docker-compose.yml <<YML
  watcher:
    image: riso-probe:local
    container_name: riso-watcher
    depends_on: [$1]
    command: ["watcher", "-url", "http://host.docker.internal:$2"]
    extra_hosts: ["host.docker.internal:host-gateway"]
YML
	;; esac
	case " ${*:4} " in *" flaky "*) printf 'volumes:\n  flakymark: {}\n' >> docker-compose.yml ;; esac
}

# record_runner PHASE [SERVICE...]: keploy record of `docker compose up
# --exit-code-from runner` in $W/PHASE, the app run with RECARGS beside the
# runner and SERVICEs (compose_yml); its log, its exit and when it ended go
# to $W/out/PHASE. timeout stops a record that would run on (its 124). The
# stack's volumes are removed before and after it, so a service's first
# start in it is its first.
record_runner() {
	local phase=$1; shift
	mkdir -p "$W/$phase" "$W/out/$phase" && cd "$W/$phase" || die cd
	compose_yml riso-app 8080 "$RECARGS" runner "$@"
	docker compose down -v --remove-orphans >/dev/null 2>&1
	log "$phase"
	$S env PATH="$PATH" "${SUDO_ENV[@]}" "${PROVEN_ENV[@]}" timeout -s INT -k 30 240 "$KEPLOY" record -c "docker compose up --exit-code-from runner" --container-name riso-app --disable-tele --disableANSI > "$W/out/$phase/keploy.log" 2>&1
	echo $? > "$W/out/$phase/exit"
	date +%s > "$W/out/$phase/ended"
	echo "$phase exit $(cat "$W/out/$phase/exit")"
	docker compose down -v --remove-orphans >/dev/null 2>&1
}

# A port-only bypass rule for 7002 (the sink's bypass port), in keploy.yml.
bypass_yml() { printf 'bypassRules:\n  - port: 7002\n' > keploy.yml; }

sink_start() {
	mkdir -p "$W/out/$1"
	$S ip netns exec riso-sink "$W/egressprobe" sink -dir "$W/out/$1" 2> "$W/out/$1/sink.log" &
	SINKPID=$!
	waitfor 30 grep -q ready "$W/out/$1/sink.log" || die "sink for $1 did not start"
	# One DNS question of the lane's own, from the host, for dns_why: an
	# answer says the sink was receiving DNS as the phase started.
	"$W/egressprobe" ask 198.18.0.10 "sink-check.$1.invalid" > "$W/out/$1/sink-check" 2>&1
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
# rows_reach PHASE PATH...: each PATH reached the sink in PHASE (the probe
# works). A DNS path with nothing there says which side failed (dns_why).
rows_reach() {
	local phase=$1 p c why; shift
	for p in "$@"; do
		c=$(count "$phase" "$p"); why=""
		[ "$c" -gt 0 ] || [ "${p#dns-}" = "$p" ] || why="; $(dns_why "$phase")"
		row "$phase $p reaches the sink" "$([ "$c" -gt 0 ] && echo 1)" "$c at the sink$why"
	done
}
# row_log NAME FILE REGEX: FILE has a line matching REGEX.
row_log() { row "$1" "$(grep -qE -- "$3" "$2" 2>/dev/null && echo 1)" "/$3/ in $(basename "$(dirname "$2")")"; }
# rows_recorded_nx MODE: the recording holds an NXDOMAIN for recorded-nx.*
# (and, under docker, for the peer, which was not up yet), and for
# stop-recorded.*, which the app looked up as keploy record stopped it, after
# its last test; the replay serves each, so the app's lookup fails as it did
# while recording, the name is not reported as one keploy answered itself nor
# as a miss after the last test, and no resolver is asked, with the DNS opt-in
# too.
STOPREC=stop-recorded.invalid-replay-isolation.test.
rows_recorded_nx() {
	local m=$1 names=(recorded-nx.invalid-replay-isolation.test. "$STOPREC") n p
	[ "$m" = native ] || names+=(riso-peer.)
	rows_reach "record-$m" dns-recorded
	for n in "${names[@]}"; do
		row "record-$m records the NXDOMAIN of $n" "$(cat "$W/$m"/keploy/test-set-*/mocks.yaml 2>/dev/null | grep -A5 -F -- "name: $n" | grep -q 'rcode: 3' && echo 1)" "$m/keploy mocks"
	done
	for p in "replay-$m" "replay-$m-dns"; do
		rows_zero "$p" dns-recorded
		row_log "$p serves the recorded NXDOMAIN" "$W/out/$p/keploy.log" 'probe dns-recorded-lookup: .*no such host'
		row_not_kept_dns "$p does not report the recorded name as a miss" "$p" recorded-nx.invalid-replay-isolation.test.
		row_log "$p serves the NXDOMAIN recorded as the app stopped" "$W/out/$p/keploy.log" 'probe dns-stop-recorded-lookup: .*no such host'
		row_not_kept_dns "$p does not report the name recorded as the app stopped as a miss" "$p" "$STOPREC"
		row "$p's misses file does not name the lookup recorded as the app stopped" "$(grep -qF "${STOPREC%.}" "$W/out/$p/last-test-misses.yaml" 2>/dev/null || echo 1)" "${STOPREC%.} in $p's last-test-misses.yaml"
	done
}
# row_nolog NAME FILE REGEX: FILE has no line matching REGEX.
row_nolog() { row "$1" "$(grep -qE -- "$3" "$2" 2>/dev/null || echo 1)" "no /$3/ in $(basename "$(dirname "$2")")"; }
# A kept-inside line names a DNS name as keploy's lines name a host: by its
# labels' shape beside its host tag ("DNS {}.{}.{}. #3fa2c1d0e9b8"), never
# whole. The run's report names each DNS question a test made that no mock
# answered whole, beside that tag; keploy test's misses file names each one
# made outside every test whole, beside the same tag (the report names those
# by their shape). The rows read which name a line names from the two.
# whole_dns PHASE: "TAG NAME" for each DNS question PHASE's report or misses
# file names whole.
whole_dns() {
	{
		cat "$W/out/$1"/report/*.yaml 2>/dev/null | awk '
			/^[[:space:]]*(- )?protocol:/ { dns = ($NF == "DNS"); name = ""; next }
			dns && /^[[:space:]]*actual_summary:/ { name = $NF; next }
			dns && name != "" && /^[[:space:]]*host_tag:/ { t = $NF; gsub(/[\047"]/, "", t); print t, name; name = "" }'
		yaml_list misses "$W/out/$1/last-test-misses.yaml" | awk -F'|' '{
			p = c = t = ""
			for (i = 2; i < NF; i++) {
				k = $i; sub(/=.*/, "", k); v = substr($i, length(k) + 2)
				if (k == "protocol") p = v; else if (k == "call") c = v; else if (k == "host_tag") t = v
			}
			if (p == "DNS" && t != "" && c != "") { n = split(c, w, " "); print t, w[n] }
		}'
	} | sort -u
}
# kept_dns_tags PHASE: the tag of each DNS name PHASE's kept-inside report
# names, and "untagged" for a line that names one by its shape alone (keploy
# writes that when no miss carried a tag): no report name is told from it, so
# row_not_kept_dns counts it as one it cannot clear.
kept_dns_tags() {
	grep -oE "${KEPT}DNS [^[:space:]]+( #[0-9a-f]+)?" "$W/out/$1/keploy.log" 2>/dev/null |
		awk '{ print ($NF ~ /^#[0-9a-f]+$/) ? $NF : "untagged" }' | sort -u
}
# row_kept_dns NAME PHASE QNAME: PHASE's kept-inside report names the DNS
# question QNAME (fully qualified), by the tag PHASE's report names it by.
row_kept_dns() {
	local tag
	tag=$(whole_dns "$2" | awk -v n="$3" '$2 == n { print $1; exit }')
	row "$1" "$([ -n "$tag" ] && kept_dns_tags "$2" | grep -qxF -- "$tag" && echo 1)" "$3 is ${tag:-not in the report} in $2"
}
# row_not_kept_dns NAME PHASE QNAME: no kept-inside DNS line of PHASE names
# QNAME: each names the tag of another name PHASE's report or misses file
# holds whole (a tag neither holds could be QNAME's, so it fails the row).
row_not_kept_dns() {
	local t n bad=""
	for t in $(kept_dns_tags "$2"); do
		n=$(whole_dns "$2" | awk -v t="$t" '$1 == t { print $2; exit }')
		[ -n "$n" ] && [ "$n" != "$3" ] || bad="$bad $t=${n:-unknown}"
	done
	row "$1" "$([ -z "$bad" ] && echo 1)" "kept DNS tags in $2:${bad:- none names $3}"
}

# The start-up lookup: the probe looks STARTUP up as it starts, only with
# -unrecorded (the replay's args, never the recording's), so outside every
# test and in no recording. keploy test answers it itself and fails nothing
# for it; the run's report names it among the set's outside-test misses by
# its shape beside its host tag, never whole (a report may be uploaded), and
# the log says its next step; keploy test's own misses file
# (keploy/<set>/last-test-misses.yaml, by: keploy test) names it whole beside
# the same tag, and git ignores that file. keploy mock replay's misses file
# (last-replay-misses.yaml) is left as it was.
STARTUP=startup-probe.invalid-replay-isolation.test.
# mock_misses SET: keploy mock replay's misses file of SET, as the lane plants
# it before each keploy test run.
mock_misses() { printf '# Written by `keploy mock replay` (planted by the lane)\nset: %s\nby: keploy mock replay\n' "$1"; }
# yaml_list KEY FILE...: each entry of the top-level list KEY in FILEs, one
# line each, as "|key=value|key=value|", each value unquoted.
yaml_list() {
	local key=$1; shift
	cat "$@" 2>/dev/null | awk -v key="$key" '
		function flush() { if (e != "") print e; e = "" }
		$0 ~ "^" key ":" { inl = 1; next }
		inl && /^[^[:space:]#-]/ { flush(); inl = 0 }
		!inl { next }
		/^[[:space:]]*- / { flush(); e = "|" }
		e != "" {
			l = $0; sub(/^[[:space:]]*(- )?/, "", l)
			k = l; sub(/:.*/, "", k)
			v = l; sub(/^[^:]*:[[:space:]]*/, "", v); gsub(/^[\047"]|[\047"]$/, "", v)
			e = e k "=" v "|"
		}
		END { flush() }'
}
# git_ignores GITIGNORE: git ignores a set's keploy test misses file in a
# repository whose keploy/.gitignore is GITIGNORE (the lane's work dirs are
# no repository, so it reads the file keploy wrote in one of its own).
git_ignores() {
	local g rc
	g=$(mktemp -d) || return 1
	mkdir -p "$g/keploy/test-set-0" && cp "$1" "$g/keploy/.gitignore" && : > "$g/keploy/test-set-0/last-test-misses.yaml" &&
		git -C "$g" init -q && git -C "$g" check-ignore -q keploy/test-set-0/last-test-misses.yaml
	rc=$?
	rm -rf "$g"
	return $rc
}
# rows_startup PHASE: PHASE's report, log and misses files name the start-up
# lookup as above.
rows_startup() {
	local p=$1 whole tag shaped
	whole=$(yaml_list misses "$W/out/$p/last-test-misses.yaml" | grep -E "[|]call=(A|AAAA) ${STARTUP//./[.]}[|]")
	tag=$(printf '%s\n' "$whole" | grep -oE '[|]host_tag=#[0-9a-f]+[|]' | head -1 | sed 's/^|host_tag=//; s/|$//')
	row "$p's misses file names the start-up lookup whole" "$([ -n "$tag" ] && grep -qx 'by: keploy test' "$W/out/$p/last-test-misses.yaml" && echo 1)" "${whole:-none in last-test-misses.yaml}"
	shaped=$(yaml_list outside_test_misses "$W/out/$p"/report/*.yaml | grep -F '|protocol=DNS|' | grep -F '|when=before_first_test|' | grep -F " $tag|")
	row "$p's report names the start-up lookup by its shape, before the first test" "$([ -n "$tag" ] && [ -n "$shaped" ] && ! printf '%s' "$shaped" | grep -qF startup-probe && echo 1)" "$(printf '%s' "${shaped:-no outside-test miss with tag ${tag:-unknown}}" | cut -c1-160)"
	row "$p's report names the start-up lookup nowhere whole" "$(cat "$W/out/$p"/report/*.yaml 2>/dev/null | grep -qF startup-probe || echo 1)" "startup-probe in $p's report"
	row_log "$p says the start-up lookup's next step" "$W/out/$p/keploy.log" 'made a call outside every test that no mock answered.*record the test set again'
	row_log "$p points to its misses file" "$W/out/$p/keploy.log" 'misses file names each call the application made outside every test whole.*last-test-misses[.]yaml'
	row "git ignores $p's misses file" "$(git_ignores "$W/out/$p/keploy.gitignore" && echo 1)" "keploy/.gitignore $p left"
	row "$p leaves mock replay's misses file as it was" "$([ -f "$W/out/$p/mock-misses-kept" ] && echo 1)" "last-replay-misses.yaml"
}

# The lookup after the last test (-stop-lookup):
# the probe looks STOPNAME up as it stops, in no recording. keploy test
# answers it itself and fails nothing for it; the run's report names it among
# the set's outside-test misses as made after the last test, by its shape
# beside its host tag, never whole, and keploy test's misses file names it
# whole beside the same tag.
STOPNAME=stop-probe.invalid-replay-isolation.test.
# rows_after_last PHASE: PHASE's report and misses file name the lookup made
# after the last test as above.
rows_after_last() {
	local p=$1 whole tag shaped
	whole=$(yaml_list misses "$W/out/$p/last-test-misses.yaml" | grep -E "[|]call=(A|AAAA) ${STOPNAME//./[.]}[|]")
	tag=$(printf '%s\n' "$whole" | grep -oE '[|]host_tag=#[0-9a-f]+[|]' | head -1 | sed 's/^|host_tag=//; s/|$//')
	row "$p's misses file names the lookup made after the last test whole" "$([ -n "$tag" ] && echo 1)" "${whole:-none in last-test-misses.yaml}"
	shaped=$(yaml_list outside_test_misses "$W/out/$p"/report/*.yaml | grep -F '|protocol=DNS|' | grep -F '|when=after_last_test|' | grep -F " $tag|")
	row "$p's report names the lookup made after the last test by its shape" "$([ -n "$tag" ] && [ -n "$shaped" ] && ! printf '%s' "$shaped" | grep -qF stop-probe && echo 1)" "$(printf '%s' "${shaped:-no outside-test miss with tag ${tag:-unknown}}" | cut -c1-160)"
	row "$p's report names the lookup made after the last test nowhere whole" "$(cat "$W/out/$p"/report/*.yaml 2>/dev/null | grep -qF stop-probe || echo 1)" "stop-probe in $p's report"
}
# rows_runner PHASE: PHASE's record (record_runner) ended on the runner's
# exit with its code, 0, within a minute of the runner's last line (a record
# that went on until timeout's stop is 124), and holds the runner's 6 calls:
# its health check and its 5 more.
rows_runner() {
	local p=$1 rexit rdone rend rtests
	rexit=$(cat "$W/out/$p/exit" 2>/dev/null)
	rdone=$(grep -aoE 'RUNNER DONE [0-9]+' "$W/out/$p/keploy.log" 2>/dev/null | head -1 | awk '{print $3}')
	rend=$(cat "$W/out/$p/ended" 2>/dev/null)
	row "$p ends with the runner's code, 0" "$([ "$rexit" = 0 ] && echo 1)" "exit ${rexit:-none}"
	row "$p ends within 60 s of the runner's last line" "$([ -n "$rdone" ] && [ -n "$rend" ] && [ $((rend - rdone)) -le 60 ] && echo 1)" "$([ -n "$rdone" ] && [ -n "$rend" ] && echo "$((rend - rdone)) s after RUNNER DONE" || echo 'no RUNNER DONE line in its log')"
	rtests=$(ls "$W/$p"/keploy/test-set-*/tests/*.yaml 2>/dev/null | wc -l)
	row "$p records the runner's 6 calls" "$([ "$rtests" = 6 ] && echo 1)" "$rtests tests"
}
# rows_mode PHASE: PHASE's keploy test ran in this script's mode: its report
# says it ran under the proven switch (proven: true) only when KEPLOY_PROVEN=1,
# so a sudo line that drops the switch fails a row instead of running the
# rows in the other mode.
rows_mode() {
	local p=$1 said want=0
	said=$(grep -hc '^proven: true$' "$W/out/$p"/report/*.yaml 2>/dev/null | head -1)
	[ "${KEPLOY_PROVEN:-}" != 1 ] || want=1
	row "$p ran in this lane's mode (KEPLOY_PROVEN=${KEPLOY_PROVEN:-unset})" "$([ "${said:-0}" = "$want" ] && echo 1)" "proven: true in $p's report ${said:-0} time(s), want $want"
}

# replay PHASE CMD...: one keploy test run in the current dir, its log and exit.
replay() {
	local phase=$1; shift
	log "$phase"
	sink_start "$phase"
	# The peer's address while the replay runs (compose starts it), for
	# peer_row: once it has one (a container created and not yet started
	# has none, which docker prints as "invalid IP").
	waitfor 300 sh -c "docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' riso-peer 2>/dev/null | grep -E '^[0-9]+([.][0-9]+){3}\$' > '$W/out/$phase/peer-ip'" &
	local peerwatch=$!
	# Under docker, the resolv.conf docker wrote for the app (for dns_why).
	waitfor 300 docker_resolv "$phase" riso-app &
	local resolvwatch=$!
	# keploy mock replay's misses file of the set, as a last mock replay
	# would leave it, for the startup rows: keploy test never writes or
	# removes it.
	local set
	set=$(ls -d keploy/test-set-* 2>/dev/null | head -1)
	[ -z "$set" ] || mock_misses "${set##*/}" | $S tee "$set/last-replay-misses.yaml" >/dev/null
	"${RUNNER[@]}" env PATH="$PATH" "${SUDO_ENV[@]}" "${PROVEN_ENV[@]}" "$KEPLOY" test "$@" --apiTimeout 60 --disable-tele --disableANSI --generate-github-actions=false > "$W/out/$phase/keploy.log" 2>&1
	echo $? > "$W/out/$phase/exit"
	echo "$phase exit $(cat "$W/out/$phase/exit")"
	waitfor 15 grep -q 'probe fork-tcp' "$W/out/$phase/keploy.log"
	grep -h 'status:' keploy/reports/test-run-*/*.yaml 2>/dev/null | sort | uniq -c
	# This run's report, for the DNS rows (whole_dns).
	local run
	run=$(ls -td keploy/reports/test-run-* 2>/dev/null | head -1)
	[ -z "$run" ] || cp -r "$run" "$W/out/$phase/report"
	# keploy test's own misses file and the keploy/.gitignore it was
	# written under, and whether mock replay's file is as it was.
	if [ -n "$set" ]; then
		cp "$set/last-test-misses.yaml" "$W/out/$phase/" 2>/dev/null
		if cmp -s "$set/last-replay-misses.yaml" <(mock_misses "${set##*/}"); then : > "$W/out/$phase/mock-misses-kept"; fi
	fi
	cp keploy/.gitignore "$W/out/$phase/keploy.gitignore" 2>/dev/null
	kill $peerwatch $resolvwatch 2>/dev/null; wait $peerwatch $resolvwatch 2>/dev/null
	docker rm -f riso-app >/dev/null 2>&1
	[ ! -f docker-compose.yml ] || docker compose down --remove-orphans >/dev/null 2>&1
	sink_stop
}

# peer_row PHASE: the probe resolved riso-peer, a container the replay's
# network holds, to that container's own address, which only docker's
# resolver knows: keploy still forwards the network's own names to it.
peer_row() {
	local got want
	got=$(grep -o 'probe dns-peer: [0-9.]*$' "$W/out/$1/keploy.log" | tail -1 | awk '{print $3}')
	want=$(cat "$W/out/$1/peer-ip" 2>/dev/null)
	row "$1 resolves a container on its network through docker's resolver" "$([ -n "$got" ] && [ "$got" = "$want" ] && echo 1)" "got ${got:-none}, riso-peer at ${want:-unknown}"
}

log "builds"
if [ -n "${KEPLOY_BIN:-}" ]; then
	KEPLOY=$KEPLOY_BIN
	docker image inspect "$IMAGE" >/dev/null 2>&1 || [[ " ${MODES[*]} " != *" docker "* && " ${MODES[*]} " != *" compose "* ]] || die "KEPLOY_BIN is set but the agent image $IMAGE is not loaded"
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
		"${SINKDNS[@]}" "$W/egressprobe" app -sink 198.18.0.10 -sink6 fd00::10 -imds 198.18.0.11 -unrecorded -listen :18081 2> "$W/out/base-native/app.log" &
		AP=$!
		waitfor 30 curl -sf localhost:18081/health || echo "baseline app not up"
		fire http://localhost:18081 "$W/out/base-native/app.log"
		$S kill $AP; wait $AP 2>/dev/null
		sink_stop

		mkdir -p "$W/native" && cd "$W/native" || die cd
		bypass_yml
		log "record-native"
		sink_start record-native
		"${SINKDNS[@]}" env PATH="$PATH" "${SUDO_ENV[@]}" "${PROVEN_ENV[@]}" "$KEPLOY" record -c "$W/egressprobe $RECARGS" --disable-tele --disableANSI > "$W/out/record-native/keploy.log" 2>&1 &
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
		"${SINKDNS[@]}" env PATH="$PATH" "${SUDO_ENV[@]}" "${PROVEN_ENV[@]}" "$KEPLOY" test -c "$W/egressprobe $APPARGS" --allow-egress imds --disable-tele --disableANSI --generate-github-actions=false > "$W/out/refuse-native/keploy.log" 2>&1
		echo $? > "$W/out/refuse-native/exit"
		cd "$W" || die cd
		;;
	docker)
		log "baseline-docker"
		sink_start base-docker
		docker run -d --name riso-base -p 18082:8080 $APPDNS --net keploy-network riso-probe:local $APPARGS >/dev/null || die "baseline container"
		waitfor 30 curl -sf localhost:18082/health || echo "baseline container not up"
		docker_resolv base-docker riso-base
		curl -s --max-time 120 localhost:18082/fire > /dev/null
		waitfor 15 sh -c 'docker logs riso-base 2>&1 | grep -q "probe fork-tcp"'
		docker logs riso-base > "$W/out/base-docker/app.log" 2>&1
		docker rm -f riso-base >/dev/null
		sink_stop

		RUN="docker run -p 8080:8080 $APPDNS --net keploy-network --rm --name riso-app riso-probe:local $APPARGS"
		RECRUN="docker run -p 8080:8080 $APPDNS --net keploy-network --rm --name riso-app riso-probe:local $RECARGS"
		mkdir -p "$W/docker" && cd "$W/docker" || die cd
		bypass_yml
		log "record-docker"
		sink_start record-docker
		$S env PATH="$PATH" "${SUDO_ENV[@]}" "${PROVEN_ENV[@]}" "$KEPLOY" record -c "$RECRUN" --container-name riso-app --disable-tele --disableANSI > "$W/out/record-docker/keploy.log" 2>&1 &
		KP=$!
		waitfor 300 curl -sf localhost:8080/health || { echo "record app not up"; docker ps -a --format '{{.Names}} {{.Image}} {{.Status}}'; }
		docker_resolv record-docker riso-app
		fire http://localhost:8080 "$W/out/record-docker/keploy.log"
		waitfor 60 sh -c 'ls keploy/test-set-*/tests/*.yaml' || { echo "no test recorded"; FAILED=1; }
		$S kill -INT $KP 2>/dev/null
		waitfor 120 sh -c "! $S kill -0 $KP" || { echo "record did not stop"; $S kill -9 $KP 2>/dev/null; }
		docker rm -f riso-app >/dev/null 2>&1
		sink_stop

		# A container keploy-network holds, which the probe resolves by name.
		# Started after the recording, which holds only its NXDOMAIN: the
		# replay must still ask docker's resolver, which alone knows its
		# address now.
		docker run -d --name riso-peer --net keploy-network riso-probe:local app >/dev/null || die "peer container"
		RUNNER=($S)
		replay replay-docker -c "$RUN" --containerName riso-app --delay 10
		replay replay-docker-dns -c "$RUN" --containerName riso-app --delay 10 --allow-egress dns
		docker rm -f riso-peer >/dev/null 2>&1
		cd "$W" || die cd
		;;
	compose)
		# Baseline: the same compose app with no keploy, on its project's own
		# network, so a zero in replay-compose means keploy kept the name in.
		log "baseline-compose"
		sink_start base-compose
		mkdir -p "$W/compose-base" && cd "$W/compose-base" || die cd
		compose_yml riso-cbase 18083 "$APPARGS" peer
		docker compose up -d >/dev/null 2>&1 || die "baseline compose"
		waitfor 30 curl -sf localhost:18083/health || echo "baseline compose app not up"
		docker_resolv base-compose riso-cbase
		curl -s --max-time 120 localhost:18083/fire > /dev/null
		waitfor 15 sh -c 'docker logs riso-cbase 2>&1 | grep -q "probe fork-tcp"'
		docker logs riso-cbase > "$W/out/base-compose/app.log" 2>&1
		docker compose down --remove-orphans >/dev/null 2>&1
		sink_stop

		mkdir -p "$W/compose" && cd "$W/compose" || die cd
		compose_yml riso-app 8080 "$RECARGS"
		bypass_yml
		log "record-compose"
		sink_start record-compose
		$S env PATH="$PATH" "${SUDO_ENV[@]}" "${PROVEN_ENV[@]}" "$KEPLOY" record -c "docker compose up" --container-name riso-app --disable-tele --disableANSI > "$W/out/record-compose/keploy.log" 2>&1 &
		KP=$!
		# A call before keploy's agent holds the published port is reset
		# (record-bind-race), so the probe is fired once it does.
		waitfor 300 sh -c "curl -sf localhost:8080/health && grep -q 'Started ingress forwarding' '$W/out/record-compose/keploy.log'" ||
			{ echo "record compose app not up"; docker ps -a --format '{{.Names}} {{.Image}} {{.Status}}'; }
		docker_resolv record-compose riso-app
		fire http://localhost:8080 "$W/out/record-compose/keploy.log"
		waitfor 60 sh -c 'ls keploy/test-set-*/tests/*.yaml' || { echo "no test recorded"; FAILED=1; }
		$S kill -INT $KP 2>/dev/null
		waitfor 120 sh -c "! $S kill -0 $KP" || { echo "record did not stop"; $S kill -9 $KP 2>/dev/null; }
		docker compose down --remove-orphans >/dev/null 2>&1
		sink_stop

		# The peer service joins only now, after the recording, which holds
		# only its NXDOMAIN (as in the docker phase).
		# A one-shot container beside it exits 0 as the stack starts, and
		# ends nothing.
		compose_yml riso-app 8080 "$APPARGS" peer oneshot
		RUNNER=($S)
		replay replay-compose -c "docker compose up" --containerName riso-app --delay 10
		replay replay-compose-dns -c "docker compose up" --containerName riso-app --delay 10 --allow-egress dns
		# The application crashes during its test (exit 3): the run ends on
		# it, the set APP_HALTED, naming the container's exit code.
		compose_yml riso-app 8080 "$APPARGS -crash-on-fire 3"
		replay replay-compose-crash -c "docker compose up" --containerName riso-app --delay 10
		# keploy's agent's container is killed once the application runs,
		# before the tests are sent: the run ends, naming the agent, its exit
		# code and the next step, not the application.
		compose_yml riso-app 8080 "$APPARGS"
		mkdir -p "$W/out/replay-compose-agent"
		(
			waitfor 300 sh -c "docker inspect -f '{{.State.Running}}' riso-app 2>/dev/null | grep -qx true" || exit 0
			agent=$(docker ps --filter name=^keploy-v3- --format '{{.Names}}' | head -1)
			[ -z "$agent" ] || docker kill "$agent" > "$W/out/replay-compose-agent/killed" 2>&1
		) &
		killer=$!
		replay replay-compose-agent -c "docker compose up" --containerName riso-app --delay 20
		kill $killer 2>/dev/null; wait $killer 2>/dev/null
		# `up riso-app` attaches compose to the application alone, so its up
		# returns as soon as keploy stops the application: keploy holds the
		# run until its own teardown, so the read after the last test is
		# made with its agent still up (rows_after_last).
		compose_yml riso-app 8080 "$APPARGS"
		replay replay-compose-app -c "docker compose up riso-app" --containerName riso-app --delay 10
		# The user's own `up --abort-on-container-failure`, a watcher beside
		# the app that exits 1 once the app stops answering, and two sets (the
		# recording's, twice): keploy's own stop of the app at the first
		# set's end makes the watcher exit 1. That exit is keploy's stop, not
		# a failure: it keeps no halt and ends nothing, and the second set
		# runs and passes.
		mkdir -p "$W/compose-watch/keploy" && cd "$W/compose-watch" || die cd
		cp "$W/compose/keploy.yml" . || die "keploy.yml"
		for s in 0 1; do
			mkdir -p "keploy/test-set-$s" && cp -r "$W/compose/keploy/test-set-0/tests" "$W/compose/keploy/test-set-0/mocks.yaml" "keploy/test-set-$s/" || die "the watcher's sets"
		done
		compose_yml riso-app 8080 "$APPARGS" watcher
		replay replay-compose-watch -c "docker compose up --abort-on-container-failure" --containerName riso-app --delay 10
		cd "$W/compose" || die cd

		# The user's own `--exit-code-from runner`: keploy record ends when
		# the test-driver service exits, with its code (0 here), at once, and
		# holds what the runner sent: its health check and its 5 calls.
		sink_start record-compose-runner
		record_runner record-compose-runner

		# The user's own `--exit-code-from early`, early exiting 3 before the
		# application's container has started: keploy record ends at once,
		# with early's 3, and names its exit. timeout stops a record that
		# would run on.
		mkdir -p "$W/compose-early" "$W/out/record-compose-early" && cd "$W/compose-early" || die cd
		compose_yml riso-app 8080 "$RECARGS" early
		log "record-compose-early"
		date +%s > "$W/out/record-compose-early/began"
		$S env PATH="$PATH" "${SUDO_ENV[@]}" "${PROVEN_ENV[@]}" timeout -s INT -k 30 240 "$KEPLOY" record -c "docker compose up --exit-code-from early" --container-name riso-app --disable-tele --disableANSI > "$W/out/record-compose-early/keploy.log" 2>&1
		echo $? > "$W/out/record-compose-early/exit"
		date +%s > "$W/out/record-compose-early/ended"
		echo "record-compose-early exit $(cat "$W/out/record-compose-early/exit")"
		docker compose down --remove-orphans >/dev/null 2>&1
		# A dependency of the app's (`depends_on: [flaky]`, no wait on its
		# exit) that crashes on its first start only, before the app starts:
		# keploy stops the stack, as compose's abort stopped it, and retries
		# the bring-up, and the record ends on the runner's exit, as above.
		record_runner record-compose-flaky flaky
		# A migration the app waits on (service_completed_successfully) that
		# exits 0 after keploy's agent is healthy, so compose starts the app
		# as soon as it has exited: its exit ends nothing, whichever of the two
		# keploy reads first (the daemon's instants decide, not the read).
		record_runner record-compose-migrate migrate
		# A seed that starts once the app has (`depends_on: [riso-app]`) and
		# exits 0 8 s later, and that the runner waits on: its exit ends
		# nothing, compose's up then starts the runner, and the record ends on
		# the runner's exit, as above (before, a stack stop that missed the
		# runner, which compose started after it, and a record that never
		# ended).
		record_runner record-compose-seed seed
		sink_stop
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
log "DNS: the sink's answer to the lane's own question, and what docker's 127.0.0.11 forwarded to"
for p in $phases; do
	printf -- '-- %s: sink %s' "$p" "$(cat "$W/out/$p/sink-check" 2>/dev/null || echo 'not asked')"
	[ ! -f "$W/out/$p/resolv.conf" ] || printf '; docker %s' "$(grep -E '^# (ExtServers|Overrides)' "$W/out/$p/resolv.conf" | tr '\n' ' ')"
	echo
done
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
		# The start-up lookup, outside every test (rows_startup), and the one
		# made after the last test, as the application stopped
		# (rows_after_last).
		rows_startup replay-native
		rows_mode replay-native
		rows_after_last replay-native
		# A DNS miss is answered by keploy: native has no in-network resolver.
		rows_zero replay-native tcp tfo imds-tcp dns-udp dns-bare dns-other dns-tcp
		row_log "replay-native names the resolvers it does not ask" "$W/out/replay-native/keploy.log" 'forwarded only to a resolver inside the replay.s own network.*not_asked.*198\.18\.0\.10'
		row_nolog "replay-native opens no DNS egress" "$W/out/replay-native/keploy.log" "$EGRESS_DNS"
		row_log "replay-native names the port-only bypass rule" "$W/out/replay-native/keploy.log" 'real egress opened by bypassRules.*7002'
		row_log "replay-native reports the IMDS call it kept inside" "$W/out/replay-native/keploy.log" "${KEPT}HTTP 198\.18\.0\.11"
		row_kept_dns "replay-native reports the DNS name it answered itself" replay-native leak-probe.invalid-replay-isolation.test.
		# The opt-in: the same misses now reach the host's resolver.
		row "replay-native-dns passes" "$([ "$(cat "$W/out/replay-native-dns/exit" 2>/dev/null)" = 0 ] && echo 1)" "exit $(cat "$W/out/replay-native-dns/exit" 2>/dev/null)"
		c=$(( $(count replay-native-dns dns-udp) + $(count replay-native-dns dns-other) + $(count replay-native-dns dns-tcp) ))
		row "replay-native-dns forwards to the host's resolver" "$([ "$c" -gt 0 ] && echo 1)" "$c DNS at the sink$([ "$c" -gt 0 ] || echo "; $(dns_why replay-native-dns)")"
		row_log "replay-native-dns names its opt-in" "$W/out/replay-native-dns/keploy.log" "$EGRESS_DNS"
		row_not_kept_dns "replay-native-dns does not report the forwarded name as kept inside" replay-native-dns leak-probe.invalid-replay-isolation.test.
		rows_recorded_nx native
		row "refuse-native fails" "$([ "$(cat "$W/out/refuse-native/exit" 2>/dev/null)" != 0 ] && echo 1)" "exit $(cat "$W/out/refuse-native/exit" 2>/dev/null)"
		row_log "refuse-native says why" "$W/out/refuse-native/keploy.log" 'entry \\?"imds\\?" is not one keploy can open'
		;;
	docker)
		# dns-udp: docker's 127.0.0.11 forwards the probe's unrecorded outside
		# name to its upstream, the sink, so the DNS rows below can count.
		# dns-bare: it recurses an unrecorded single-label name there too.
		rows_reach base-docker tcp fork-tcp tfo imds-tcp dns-udp dns-bare
		row "replay-docker passes" "$([ "$(cat "$W/out/replay-docker/exit" 2>/dev/null)" = 0 ] && echo 1)" "exit $(cat "$W/out/replay-docker/exit" 2>/dev/null)"
		# The start-up lookup, outside every test (rows_startup), and the one
		# made after the last test, as the application stopped
		# (rows_after_last).
		rows_startup replay-docker
		rows_mode replay-docker
		rows_after_last replay-docker
		# keploy answers that name itself: 127.0.0.11 gets only the network's
		# own names, so the name never reaches its upstream. Nor does any
		# other (dns-other): only 127.0.0.11 asks the sink here.
		rows_zero replay-docker tcp fork-tcp tfo imds-tcp dns-udp dns-bare dns-other dns-tcp
		# ... and every name it does hold still goes to it: the peer resolves
		# to its own address, which keploy could not have made up.
		peer_row replay-docker
		# The peer's TXT, SRV and CNAME lookups, which docker's resolver
		# recurses: keploy answers each itself, so none reaches its upstream.
		rows_zero replay-docker dns-owned
		row_kept_dns "replay-docker reports the DNS name it answered itself" replay-docker leak-probe.invalid-replay-isolation.test.
		row_nolog "replay-docker opens no DNS egress" "$W/out/replay-docker/keploy.log" "$EGRESS_DNS"
		row_log "replay-docker names the port-only bypass rule" "$W/out/replay-docker/keploy.log" 'real egress opened by bypassRules.*7002'
		row_log "replay-docker reports the IMDS call it kept inside" "$W/out/replay-docker/keploy.log" "${KEPT}HTTP 198\.18\.0\.11"
		row "replay-docker-dns passes" "$([ "$(cat "$W/out/replay-docker-dns/exit" 2>/dev/null)" = 0 ] && echo 1)" "exit $(cat "$W/out/replay-docker-dns/exit" 2>/dev/null)"
		row_log "replay-docker-dns: the agent gets the opt-in" "$W/out/replay-docker-dns/keploy.log" "$EGRESS_DNS"
		# The opt-in sends the miss on, through 127.0.0.11 to its upstream,
		# and the peer's other types with it (the probe's dns-owned works).
		rows_reach replay-docker-dns dns-udp dns-owned
		rows_recorded_nx docker
		;;
	compose)
		# dns-udp at the baseline: the project network's 127.0.0.11 forwards
		# the unrecorded outside name to its upstream, the sink.
		rows_reach base-compose tcp fork-tcp tfo imds-tcp dns-udp dns-bare
		# dns-owned: it recurses the peer service's TXT, SRV and CNAME lookups.
		rows_reach base-compose dns-owned
		row_log "base-compose resolves the peer service" "$W/out/base-compose/app.log" 'probe dns-peer: [0-9]'
		row "replay-compose passes" "$([ "$(cat "$W/out/replay-compose/exit" 2>/dev/null)" = 0 ] && echo 1)" "exit $(cat "$W/out/replay-compose/exit" 2>/dev/null)"
		# The start-up lookup, outside every test (rows_startup), and the one
		# made after the last test (rows_after_last), in either mode.
		rows_startup replay-compose
		rows_mode replay-compose
		rows_after_last replay-compose
		# keploy takes the stack down as the run ends, the one-shot container
		# with it, so its exit is read from compose's own line in the run's
		# log: a test was sent after it, and the run passed (the row above).
		row "replay-compose: the one-shot container exited 0 and ended nothing" "$(awk '/riso-oneshot exited with code 0/ { o = 1 } o && /starting test for/ { t = 1 } END { exit !t }' "$W/out/replay-compose/keploy.log" 2>/dev/null && echo 1)" "$(grep -aom1 'riso-oneshot exited with code [0-9]*' "$W/out/replay-compose/keploy.log" 2>/dev/null || echo 'no exit of riso-oneshot'), then a test sent after it"
		# The application's crash ends the run on it.
		row "replay-compose-crash fails" "$([ "$(cat "$W/out/replay-compose-crash/exit" 2>/dev/null)" != 0 ] && echo 1)" "exit $(cat "$W/out/replay-compose-crash/exit" 2>/dev/null)"
		row_log "replay-compose-crash: the application crashed" "$W/out/replay-compose-crash/keploy.log" 'probe crash: exit 3'
		row_log "replay-compose-crash names the application's exit code" "$W/out/replay-compose-crash/keploy.log" "the application's container exited with code 3, and keploy had not stopped it"
		row "replay-compose-crash: the set is APP_HALTED" "$(cat "$W/out/replay-compose-crash"/report/*.yaml 2>/dev/null | grep -q '^status: APP_HALTED$' && echo 1)" "$(cat "$W/out/replay-compose-crash"/report/*.yaml 2>/dev/null | grep -m1 '^status:' || echo 'no report')"
		rows_mode replay-compose-crash
		# keploy's agent's death ends the run on it, named.
		row "replay-compose-agent: keploy's agent was killed" "$(grep -q . "$W/out/replay-compose-agent/killed" 2>/dev/null && echo 1)" "$(cat "$W/out/replay-compose-agent/killed" 2>/dev/null || echo 'no agent container found')"
		row "replay-compose-agent fails" "$([ "$(cat "$W/out/replay-compose-agent/exit" 2>/dev/null)" != 0 ] && echo 1)" "exit $(cat "$W/out/replay-compose-agent/exit" 2>/dev/null)"
		row_log "replay-compose-agent names keploy's agent, its exit code and the next step" "$W/out/replay-compose-agent/keploy.log" "keploy's agent container [^ ]*keploy-v3-[^ ]* exited with code 137 after the application started.*Next step: find why the agent stopped"
		row_nolog "replay-compose-agent does not name the application's own exit" "$W/out/replay-compose-agent/keploy.log" "the application's container exited with code [0-9]+, and keploy had not stopped it"
		# keploy answers that name itself, so it never reaches 127.0.0.11's
		# upstream, nor does any other name (dns-other).
		rows_zero replay-compose tcp fork-tcp tfo imds-tcp dns-udp dns-bare dns-other dns-tcp
		# The peer service resolves to its address on the project network,
		# as docker's resolver answers it, not to keploy's own answer.
		peer_row replay-compose
		# keploy answers the peer's other types itself.
		rows_zero replay-compose dns-owned
		row_kept_dns "replay-compose reports the DNS name it answered itself" replay-compose leak-probe.invalid-replay-isolation.test.
		row_nolog "replay-compose opens no DNS egress" "$W/out/replay-compose/keploy.log" "$EGRESS_DNS"
		row_log "replay-compose names the port-only bypass rule" "$W/out/replay-compose/keploy.log" 'real egress opened by bypassRules.*7002'
		row_log "replay-compose reports the IMDS call it kept inside" "$W/out/replay-compose/keploy.log" "${KEPT}HTTP 198\.18\.0\.11"
		row "replay-compose-dns passes" "$([ "$(cat "$W/out/replay-compose-dns/exit" 2>/dev/null)" = 0 ] && echo 1)" "exit $(cat "$W/out/replay-compose-dns/exit" 2>/dev/null)"
		row_log "replay-compose-dns: the agent gets the opt-in" "$W/out/replay-compose-dns/keploy.log" "$EGRESS_DNS"
		rows_reach replay-compose-dns dns-udp
		rows_recorded_nx compose
		# `up riso-app`: its up returned at keploy's stop of the application,
		# and the run read the calls after the last test from its agent.
		row "replay-compose-app passes" "$([ "$(cat "$W/out/replay-compose-app/exit" 2>/dev/null)" = 0 ] && echo 1)" "exit $(cat "$W/out/replay-compose-app/exit" 2>/dev/null)"
		rows_mode replay-compose-app
		rows_startup replay-compose-app
		rows_after_last replay-compose-app
		row_nolog "replay-compose-app read every call made after the last test" "$W/out/replay-compose-app/keploy.log" 'could not read every call the application made after'
		row_nolog "replay-compose-app does not name keploy's agent as stopped" "$W/out/replay-compose-app/keploy.log" "keploy's agent container [^ ]* exited with code"
		# The watcher (`up --abort-on-container-failure`): keploy's stop of
		# the app at the first set's end made it exit 1, which ended nothing.
		row "replay-compose-watch passes" "$([ "$(cat "$W/out/replay-compose-watch/exit" 2>/dev/null)" = 0 ] && echo 1)" "exit $(cat "$W/out/replay-compose-watch/exit" 2>/dev/null)"
		rows_mode replay-compose-watch
		wpassed=$(cat "$W/out/replay-compose-watch"/report/*.yaml 2>/dev/null | grep -c '^status: PASSED$')
		row "replay-compose-watch passes both sets" "$([ "$wpassed" = 2 ] && echo 1)" "$wpassed sets PASSED"
		row_log "replay-compose-watch: the watcher exited 1 as keploy stopped the app" "$W/out/replay-compose-watch/keploy.log" 'WATCHER EXITS 1'
		row_log "replay-compose-watch takes the watcher's exit for keploy's own stop" "$W/out/replay-compose-watch/keploy.log" 'exited while keploy held the application stopped.*it ends nothing.*watcher'
		row_nolog "replay-compose-watch ends nothing on the watcher's exit" "$W/out/replay-compose-watch/keploy.log" "the command.s abort flags end the run on it"
		row_nolog "replay-compose-watch keeps no halt" "$W/out/replay-compose-watch/keploy.log" 'APP_HALTED|exited with code [0-9]+, and keploy had not stopped it'
		# `up --exit-code-from runner` at record: it ended on the runner's
		# exit, with its code, within a minute of the runner's last line
		# (before, compose's own abort), and holds the runner's 6 calls.
		rows_runner record-compose-runner
		# `up --exit-code-from early` at record, early exiting 3 before the
		# application started: it ended on early's exit, with its 3, within a
		# minute (before, a record that ran on until it was stopped; timeout's
		# stop is 124), and its log names early's exit.
		eexit=$(cat "$W/out/record-compose-early/exit" 2>/dev/null)
		ebegan=$(cat "$W/out/record-compose-early/began" 2>/dev/null)
		eend=$(cat "$W/out/record-compose-early/ended" 2>/dev/null)
		row "record-compose-early ends with early's code, 3" "$([ "$eexit" = 3 ] && echo 1)" "exit ${eexit:-none}"
		row "record-compose-early ends within 60 s" "$([ -n "$ebegan" ] && [ -n "$eend" ] && [ $((eend - ebegan)) -le 60 ] && echo 1)" "$([ -n "$ebegan" ] && [ -n "$eend" ] && echo "$((eend - ebegan)) s" || echo 'no times')"
		row "record-compose-early names early's exit before the application started" "$(grep -aqE 'service .{0,2}early.{0,2} exited with code 3 before the application started' "$W/out/record-compose-early/keploy.log" && echo 1)" "$(grep -acE 'exited with code 3 before the application started' "$W/out/record-compose-early/keploy.log") lines"
		# flaky's crash on its first start, before the app started: the
		# bring-up was retried, flaky came up, and the record is the runner's.
		rows_runner record-compose-flaky
		row_log "record-compose-flaky: flaky crashed on its first start" "$W/out/record-compose-flaky/keploy.log" 'FLAKY CRASHES 1'
		row_log "record-compose-flaky retried the bring-up on flaky's crash" "$W/out/record-compose-flaky/keploy.log" 'crashed before the application started: keploy stops the stack, as compose.s abort stopped it, and retries the bring-up'
		row_log "record-compose-flaky: flaky came up on the retry" "$W/out/record-compose-flaky/keploy.log" 'FLAKY UP'
		# migrate's 0 after keploy's agent was healthy: compose's up waits on
		# it, so it ended nothing.
		rows_runner record-compose-migrate
		row_log "record-compose-migrate: the migration exited 0" "$W/out/record-compose-migrate/keploy.log" 'MIGRATE DONE'
		row_log "record-compose-migrate: its exit ends nothing, as compose's up waits on it" "$W/out/record-compose-migrate/keploy.log" 'docker compose up waits on that exit before it starts another service.*migrate'
		row_nolog "record-compose-migrate: its exit does not end the run" "$W/out/record-compose-migrate/keploy.log" "the command.s abort flags end the run on it"
		# seed's 0 after the app started, which the runner waits on: it ended
		# nothing, and the runner compose started after it ended the record.
		rows_runner record-compose-seed
		row_log "record-compose-seed: the seed exited 0" "$W/out/record-compose-seed/keploy.log" 'SEED DONE'
		row_log "record-compose-seed: its exit ends nothing, as compose's up waits on it" "$W/out/record-compose-seed/keploy.log" 'docker compose up waits on that exit before it starts another service.*seed'
		row_nolog "record-compose-seed: its exit does not end the run" "$W/out/record-compose-seed/keploy.log" "the command.s abort flags end the run on it"
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
	tar -C "$W" -czf /keep/riso-out.tgz out $(cd "$W" && ls -d native/keploy docker/keploy compose/keploy 2>/dev/null)
	echo "artifact: /keep/riso-out.tgz (the box's ~/lane-keep/<run>/riso-out.tgz)"
fi
[ "$FAILED" = 0 ] && echo "replay isolation: every row passed" || echo "replay isolation: a row FAILED"
exit $FAILED
