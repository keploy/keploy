package proxy

import (
	"fmt"
	"testing"
)

// XNU's TCP states read as its client's hang-up exactly where only a FIN or a
// reset leads: CLOSE_WAIT, LAST_ACK and CLOSING (a FIN), TIME_WAIT (the
// client's FIN after keploy's own half-close: a macOS socket its handler
// still holds stays there, where Linux shows TCP_CLOSE) and CLOSED (a
// reset). keploy's own half-close (FIN_WAIT_1, FIN_WAIT_2) and every state
// before it are no hang-up. Checked on every platform: the table is not a
// Mac's alone.
func TestXNUTCPStatesReadAsAHangUpOnlyAfterTheClientsFINOrReset(t *testing.T) {
	for _, c := range []struct {
		name  string
		state uint8
		gone  bool
	}{
		{"CLOSED", xnuTCPClosed, true},
		{"LISTEN", xnuTCPListen, false},
		{"SYN_SENT", xnuTCPSynSent, false},
		{"SYN_RCVD", xnuTCPSynReceived, false},
		{"ESTABLISHED", xnuTCPEstablished, false},
		{"CLOSE_WAIT", xnuTCPCloseWait, true},
		{"FIN_WAIT_1", xnuTCPFinWait1, false},
		{"CLOSING", xnuTCPClosing, true},
		{"LAST_ACK", xnuTCPLastAck, true},
		{"FIN_WAIT_2", xnuTCPFinWait2, false},
		{"TIME_WAIT", xnuTCPTimeWait, true},
	} {
		if got := xnuClientGone(c.state); got != c.gone {
			t.Errorf("xnuClientGone(%s = %d) = %v, want %v", c.name, c.state, got, c.gone)
		}
	}
	// The numbering is XNU's (bsd/netinet/tcp_fsm.h), not Linux's.
	if xnuTCPEstablished != 4 || xnuTCPCloseWait != 5 || xnuTCPTimeWait != 10 {
		t.Fatalf("XNU's states are ESTABLISHED 4, CLOSE_WAIT 5, TIME_WAIT 10; the table says %d, %d, %d", xnuTCPEstablished, xnuTCPCloseWait, xnuTCPTimeWait)
	}
}

// A line that names an XNU TCP state names it as macOS's netstat does
// (tcp_fsm.h's tcpstates, by XNU's numbering), and a state XNU has no name
// for by its number. Checked on every platform.
func TestXNUTCPStatesAreNamedAsMacOSNamesThem(t *testing.T) {
	for state, want := range []string{"CLOSED", "LISTEN", "SYN_SENT", "SYN_RCVD", "ESTABLISHED", "CLOSE_WAIT", "FIN_WAIT_1", "CLOSING", "LAST_ACK", "FIN_WAIT_2", "TIME_WAIT"} {
		if got := xnuTCPStateName(uint8(state)); got != want {
			t.Errorf("xnuTCPStateName(%d) = %q, want %q", state, got, want)
		}
	}
	for _, state := range []uint8{11, 255} {
		if got, want := xnuTCPStateName(state), fmt.Sprintf("TCP state %d", state); got != want {
			t.Errorf("xnuTCPStateName(%d) = %q, want %q: XNU has no state %d", state, got, want, state)
		}
	}
}
