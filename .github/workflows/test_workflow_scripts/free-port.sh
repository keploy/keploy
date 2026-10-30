# shellcheck shell=bash
# free_port: a TCP port free on this host, for a lane to publish an app on,
# outside every range a network namespace hands out ports from by itself.
#
# Sourced via:
#
#   source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/free-port.sh"
#   PORT=$(free_port)
#
# WHY THIS EXISTS
#
# A port the kernel picks (a bind to port 0, as the compose lanes took theirs)
# comes from the ephemeral range, 32768-60999, and, as bind(0) prefers odd
# ports there, it is an odd one. Docker's embedded DNS server listens in every
# container on a user-defined network (every compose stack's) on 127.0.0.11,
# TCP and UDP, on ports the kernel picks the same way, in that container's own
# network namespace: odd ports of the same range. An app in that namespace
# that listens on 0.0.0.0 at the port of that TCP listener cannot bind it:
# "OSError: [Errno 98] Address already in use" (job 124142018005, the app of
# a `keploy test` on a compose stack, which then ended the run). The lane had
# found the port free, but on the host: the namespace the app collides in is
# the container's, new with each container, its DNS port picked as the
# container starts, so no check made beforehand can see it. A lane that
# publishes the app on the port it listens on (PORT:PORT) meets this about
# once in 14,000 container starts.
#
# So free_port picks a port below the ephemeral range: the range of a new
# network namespace is the kernel's default, 32768-60999, whatever the
# host's, and the port is below the host's own range too, so that no outgoing
# connection on the host takes it before the app's container publishes it. It
# picks none of keploy's own ports either: under docker compose the app runs
# in the network namespace of keploy's agent, which listens on 16789 (its
# proxy), 26789 (its DNS server), 36789 (its incoming proxy) and its API's
# port, drawn above the ephemeral range (61000-65535; utils.GetAvailablePort
# says why). The window, 28000-32767, is clear of all of them, and of 27017
# (MongoDB's).
#
# It tries the window's ports in a random order, so that lanes running side by
# side on one host rarely pick the same one, and returns the first the host
# lets it bind on both 0.0.0.0 and [::], as Docker publishes a port: a search
# through the window, not a wait. When no port of it is free it says so on
# stderr and returns 1.
#
# Ports given as arguments are skipped: a lane that publishes two ports passes
# the first when it picks the second (free_port "$PORT"), as neither is bound
# until its container starts.
#
# FREE_PORT_LO and FREE_PORT_HI set another window (free-port.tests.sh uses
# them); its top is still cut to below the ephemeral ranges.
free_port() {
  python3 - "${FREE_PORT_LO:-28000}" "${FREE_PORT_HI:-32767}" "$@" <<'PY'
import random, socket, sys

lo, hi = int(sys.argv[1]), int(sys.argv[2])
taken = {int(p) for p in sys.argv[3:]}
with open("/proc/sys/net/ipv4/ip_local_port_range") as f:
    host_lo = int(f.read().split()[0])
# 32768: the start of the ephemeral range of every new network namespace.
top = min(hi, host_lo - 1, 32767)
if lo > top:
    print("free_port: the window %d-%d has no port below the ephemeral ranges "
          "(a new network namespace's starts at 32768, this host's at %d)" % (lo, hi, host_lo),
          file=sys.stderr)
    sys.exit(1)


def bindable(family, addr, port):
    try:
        s = socket.socket(family, socket.SOCK_STREAM)
    except OSError:
        # No IPv6 on this host: Docker publishes on IPv4 alone.
        return family == socket.AF_INET6
    try:
        if family == socket.AF_INET6:
            s.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 1)
        s.bind((addr, port))
        return True
    except OSError:
        return False
    finally:
        s.close()


ports = [p for p in range(lo, top + 1) if p not in taken]
random.shuffle(ports)
for port in ports:
    if bindable(socket.AF_INET, "0.0.0.0", port) and bindable(socket.AF_INET6, "::", port):
        print(port)
        sys.exit(0)
print("free_port: every port of %d-%d is in use on this host, or was given to skip" % (lo, top), file=sys.stderr)
sys.exit(1)
PY
}
