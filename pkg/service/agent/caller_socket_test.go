//go:build linux

// Linux only, as the hooks it builds on (runMemberHooks, claim_runmember_test.go).

package agent

import (
	"net/netip"
	"testing"
)

// callerSocketHooks are hooks that see sockets handed between processes
// (coreAgent.CallerSocketReporter), saying why for every call.
type callerSocketHooks struct {
	runMemberHooks
	why            string
	pid            int
	client, server netip.AddrPort
}

func (h *callerSocketHooks) CallerSocketWhy(pid int, client, server netip.AddrPort) string {
	h.pid, h.client, h.server = pid, client, server
	return h.why
}

// The agent asks hooks that see hand-offs, with the call's owner and ends,
// and says nothing with any other hooks.
func TestCallerSocketWhyIsTheHooksWord(t *testing.T) {
	client := netip.MustParseAddrPort("127.0.0.1:50001")
	server := netip.MustParseAddrPort("127.0.0.1:16789")
	h := &callerSocketHooks{why: "process 20 connected it"}
	a := &Agent{Hooks: h}
	if got := a.CallerSocketWhy(10, client, server); got != h.why || h.pid != 10 || h.client != client || h.server != server {
		t.Fatalf("CallerSocketWhy = %q, asked about %d over %v -> %v; want %q about 10 over %v -> %v",
			got, h.pid, h.client, h.server, h.why, client, server)
	}
	if got := (&Agent{Hooks: &runMemberHooks{}}).CallerSocketWhy(10, client, server); got != "" {
		t.Fatalf("CallerSocketWhy with hooks that do not see hand-offs = %q, want \"\"", got)
	}
}
