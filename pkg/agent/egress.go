package agent

import "go.uber.org/zap"

// A replay reaches the real network only through a knob the user turned on,
// and each knob says so when it takes effect, in one form: RealEgressPrefix,
// the knob, and what it opens. One search for the prefix then finds every way
// a replay was let out.
//
// What logs it, and when:
//   - --global-passthrough: the proxy at its start (and the DNS forward's own
//     line, logReplayDNSForwarding);
//   - test.allowEgress: the DNS forward, at the proxy's start;
//   - mocking off, onMiss passthrough or record, opportunistic TLS intercept:
//     the proxy, when a replay session starts with one of them on;
//   - mock.real: the proxy, when the list is installed;
//   - the ports the kernel hooks leave alone: the hooks, when they register
//     the client.

// RealEgressPrefix starts every line that names a knob opening real egress in
// a replay.
const RealEgressPrefix = "real egress opened by "

// LogRealEgress logs that knob lets the replay reach dest for real.
func LogRealEgress(logger *zap.Logger, knob, dest string, fields ...zap.Field) {
	logger.Info(RealEgressPrefix+knob+": "+dest, fields...)
}

// PassThroughSource is one knob that puts ports in the list the kernel hooks
// leave alone, and those ports.
type PassThroughSource struct {
	Knob  string
	Ports []uint
}

// KernelPassThroughDest is what a port the kernel hooks leave alone opens in a
// replay.
const KernelPassThroughDest = "any host on these ports; the kernel hands their connections to the real destination, so keploy neither serves nor reports them"
