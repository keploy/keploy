package proxy

import (
	"context"
	"net"
	"os"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
)

// replayOwnListener reports whether a keploy test connection the app made
// reaches a listener of the app itself, and which process holds it: a call
// the app makes to its own port (parse-server checking its own health, an
// app calling its own API), which reached the app at record and is passed
// through to it at replay (serveReplayOwnListener), never matched against
// mocks. keploy mock decides its own loopback calls (serveTreeListener).
//
// The listener is the app's when the destination is an address of the
// agent's network namespace, something listens there that is not keploy's
// own (the proxy, DNS and ingress ports, or a socket of the agent's
// process), and either
//
//   - the process that made the call holds that listening socket itself: a
//     process calling its own listener, never a dependency; or
//   - a process of the app holds it, and the recording did not reach its
//     port as a dependency (RecordedPort, as keploy mock's rule; not under
//     the proven switch, where a listener of the app's is always live). A
//     native app's processes are the keploy CLI's descendants other than
//     the CLI and the agent. In Docker the app shares the agent's network
//     and PID namespaces (network_mode and pid: service:keploy-agent), and
//     so may other containers (a Kubernetes pod's sidecars, with
//     shareProcessNamespace): there the app's processes are those of the
//     caller's own container (its mount namespace), never the agent's.
//
// Whatever keploy cannot tell (no destination address, no listener, no
// holder or caller it can name, no CLI pid in this agent's PID namespace, a
// mount namespace it cannot read) is not the app's: the call takes the mock
// path, as it did before.
func (p *Proxy) replayOwnListener(mode models.Mode, dest *agent.NetworkAddress, dstAddr string) (owner int, own bool) {
	if mode != models.MODE_TEST || p.mockMode {
		return 0, false
	}
	// An address that does not parse names no IP, which ownIP refuses.
	host, _, _ := net.SplitHostPort(dstAddr)
	ip := net.ParseIP(host)
	if !ownIP(ip) {
		return 0, false
	}
	if ip.IsUnspecified() {
		// A connect to the unspecified address reaches this host's loopback;
		// listenInodes takes a loopback listener of either family for it.
		ip = net.IPv4(127, 0, 0, 1)
	}
	if p.keployListens(dest.Port) {
		return 0, false
	}
	self := agentPID()
	// A caller keploy cannot name (0) holds nothing, and is in no container:
	// /proc/0 is no process.
	caller, _ := callerOf(dest)
	if caller != self && listenerHeldBy(ip, dest.Port, caller) {
		return caller, true
	}
	// 0 when nothing listens there, or keploy cannot read which process
	// holds the socket: no process, so in no container and the descendant
	// of no CLI (descends(0, root) only for a root of 0, which is refused).
	owner, _ = ownerOfListener(ip, dest.Port)
	if descends(owner, self) {
		return 0, false
	}
	if p.IsDocker {
		if !sameContainer(owner, caller) {
			return 0, false
		}
	} else {
		// No process descends from a CLI pid this agent cannot name (0).
		root := int(utils.ClientPIDHere(p.appPID, p.appPIDNS).PID)
		if owner == root || !descends(owner, root) {
			return 0, false
		}
	}
	if !provenswitch.On() && RecordedPort(dest.Port) {
		return 0, false
	}
	return owner, true
}

// agentPID is the agent's own process; a test swaps it to stand apart from
// the test process, which plays the app.
var agentPID = os.Getpid

// ownerOfListener and dialOwnListener are listenerOwner and
// util.DialDestination; a test swaps them for a holder keploy cannot read
// and a dial that fails.
var (
	ownerOfListener = listenerOwner
	dialOwnListener = util.DialDestination
)

// keployListens reports whether port is one of the agent's own listeners'
// ports: its proxy, DNS server or ingress proxy.
func (p *Proxy) keployListens(port uint32) bool {
	return port == p.Port || port == p.DNSPort || port == uint32(p.IncomingProxyPort)
}

// serveReplayOwnListener passes a keploy test (mode) connection to a listener of the
// app's own (replayOwnListener) through to it, as keploy record did: the
// app's own handler answers, and no mock is matched or missed. It returns
// false, having read nothing of the connection, for any other connection.
func (p *Proxy) serveReplayOwnListener(ctx context.Context, mode models.Mode, srcConn net.Conn, dest *agent.NetworkAddress, dstAddr string, predialed *util.Predialed) (served bool, err error) {
	owner, own := p.replayOwnListener(mode, dest, dstAddr)
	if !own {
		return false, nil
	}
	caller, _ := callerOf(dest)
	fields := []zap.Field{zap.String("destination", dstAddr), zap.Int("owner", owner), zap.Int("caller", caller),
		zap.Uint32("caller_kernel_pid", dest.KernelPid), zap.String("reason", models.ReasonLoopbackOutsideRun)}
	p.selfCallOwners.LoadOrStore(owner, struct{}{})
	if p.selfCalls.Add(1) == 1 {
		p.logger.Info("the app called a port it listens on; passed through to the app, as at record, not matched against mocks",
			append(fields, zap.String("note", "further such calls are logged at debug, and counted at the end of a run that finishes"))...)
	} else {
		p.logger.Debug("the app called a port it listens on; passed through to the app, as at record, not matched against mocks", fields...)
	}
	dstConn, err := dialOwnListener(ctx, p.logger, "tcp", util.DialTarget{Addr: dstAddr, Predialed: predialed})
	if err != nil {
		utils.LogError(p.logger, err, "failed to dial the app's own listener", zap.String("destination", dstAddr), zap.String("next_step", util.NextStepDialDestination))
		return true, err
	}
	defer func() { _ = dstConn.Close() }()
	util.RelayRawPassthrough(srcConn, dstConn)
	return true, nil
}
