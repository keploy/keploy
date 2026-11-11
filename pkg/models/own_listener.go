package models

import (
	"net"
	"strconv"
	"strings"
)

// MarkOwnListenerCall marks m, on the CLI side only, as the recording of a
// call the app made to a port it listens on itself (its health check of its
// own port, a call to its own API). keploy passes such a call through to the
// app at record and at replay, so a recording a keploy before that made of
// one is not a dependency's: no test and no worker scope requires it
// (OutsideTestScopes), and its going unused fails nothing. The mark is in
// memory only, no encoder carries it: the agent, which serves the recording
// to a call it cannot tell is the app's own, never reads it.
func (m *Mock) MarkOwnListenerCall() {
	if m != nil {
		m.ownListener = true
	}
}

// OwnListenerCall reports whether m is marked as the recording of a call the
// app made to its own port (MarkOwnListenerCall).
func (m *Mock) OwnListenerCall() bool {
	return m != nil && m.ownListener
}

// OwnListenerCallsHeader is the /consumedmocks response header in which keploy
// test's agent names the destinations of the calls it passed through, since
// the set's mocks were loaded, as calls of the app to a listener of its own:
// each OwnListenerDestination's spelling, comma-separated. The replay marks a
// mock recorded at one of them as such a call (MarkOwnListenerCall), beside
// the loopback and test-port check it makes as it reads the set. An agent
// that does not send the header, every agent released before it among them,
// has reported none: each mock is then judged as before.
const OwnListenerCallsHeader = "X-Keploy-Own-Listener-Calls"

// MaxOwnListenerCalls is how many destinations the agent reports in
// OwnListenerCallsHeader for one set, and how many the replay reads from it.
// An app calls a handful of its own addresses; one past the bound is not
// reported, so its mocks are judged as before.
const MaxOwnListenerCalls = 64

// OwnListenerDestination is addr ("ip:port") as the agent reports a
// destination of a call it passed through to the app's own listener, and as
// the replay compares a mock's recorded destination with one: the IP in its
// shortest form (net.IP.String: an IPv4-mapped IPv6 address as the IPv4
// address it is), and the port in decimal. ok is false for an addr whose
// host is no IP (a name is not where a connection went), or whose port is
// not one.
func OwnListenerDestination(addr string) (string, bool) {
	// An address that does not split names no host, which ParseIP refuses.
	host, port, _ := net.SplitHostPort(strings.TrimSpace(addr))
	ip := net.ParseIP(host)
	n, err := strconv.ParseUint(port, 10, 16)
	if ip == nil || err != nil || n == 0 {
		return "", false
	}
	return net.JoinHostPort(ip.String(), strconv.FormatUint(n, 10)), true
}
