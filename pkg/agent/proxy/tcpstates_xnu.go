package proxy

import "fmt"

// XNU's TCP states (bsd/netinet/tcp_fsm.h), as TCP_CONNECTION_INFO's
// tcpi_state gives them on macOS. They number differently from Linux's
// (include/net/tcp_states.h: ESTABLISHED is 1 there, 4 here). They and the
// reading of them below are in an untagged file so that every platform's
// tests check the mapping, not only a Mac's.
const (
	xnuTCPClosed      = 0
	xnuTCPListen      = 1
	xnuTCPSynSent     = 2
	xnuTCPSynReceived = 3
	xnuTCPEstablished = 4
	xnuTCPCloseWait   = 5
	xnuTCPFinWait1    = 6
	xnuTCPClosing     = 7
	xnuTCPLastAck     = 8
	xnuTCPFinWait2    = 9
	xnuTCPTimeWait    = 10
)

// xnuTCPStateNames are XNU's names for its TCP states, as tcp_fsm.h's
// tcpstates and macOS's netstat print them.
var xnuTCPStateNames = map[uint8]string{
	xnuTCPClosed:      "CLOSED",
	xnuTCPListen:      "LISTEN",
	xnuTCPSynSent:     "SYN_SENT",
	xnuTCPSynReceived: "SYN_RCVD",
	xnuTCPEstablished: "ESTABLISHED",
	xnuTCPCloseWait:   "CLOSE_WAIT",
	xnuTCPFinWait1:    "FIN_WAIT_1",
	xnuTCPClosing:     "CLOSING",
	xnuTCPLastAck:     "LAST_ACK",
	xnuTCPFinWait2:    "FIN_WAIT_2",
	xnuTCPTimeWait:    "TIME_WAIT",
}

// xnuTCPStateName names XNU TCP state s for a log line: XNU's name for it,
// or its number for a state XNU has no name for.
func xnuTCPStateName(s uint8) string {
	if name, ok := xnuTCPStateNames[s]; ok {
		return name
	}
	return fmt.Sprintf("TCP state %d", s)
}

// xnuClientGone reports whether a socket in XNU TCP state state, which its
// handler has not closed, has received its client's FIN or a reset:
// CLOSE_WAIT, LAST_ACK and CLOSING are reached only by a FIN; TIME_WAIT, for
// a socket still open, by the client's FIN after keploy's own half-close
// (FIN_WAIT_2 then the FIN: XNU leaves the held socket in TIME_WAIT, where
// Linux shows TCP_CLOSE); and CLOSED by a reset. Either way the client has
// gone. FIN_WAIT_1 and FIN_WAIT_2 are keploy's own half-close, its client
// not yet done.
func xnuClientGone(state uint8) bool {
	switch state {
	case xnuTCPCloseWait, xnuTCPLastAck, xnuTCPClosing, xnuTCPTimeWait, xnuTCPClosed:
		return true
	}
	return false
}
