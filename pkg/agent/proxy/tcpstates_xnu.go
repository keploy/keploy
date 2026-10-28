package proxy

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
