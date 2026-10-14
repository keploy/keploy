package linux

import (
	"net/netip"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
)

// The targets are the addresses the kernel was told to redirect to, read the
// way connect4/connect6 read proxy_info: IP4 one host-order word, IP6 four.
func TestProxyConnTargetsAreWhereTheKernelRedirects(t *testing.T) {
	for _, tc := range []struct {
		name string
		info structs.ProxyInfo
		want []netip.AddrPort
	}{
		{"native: loopback, both families", structs.ProxyInfo{IP4: 0x7f000001, IP6: [4]uint32{0, 0, 0, 1}, Port: 16789},
			[]netip.AddrPort{netip.MustParseAddrPort("127.0.0.1:16789"), netip.MustParseAddrPort("[::1]:16789")}},
		{"docker: the container's address, no IPv6", structs.ProxyInfo{IP4: 0xac110002, Port: 36789},
			[]netip.AddrPort{netip.MustParseAddrPort("172.17.0.2:36789")}},
		{"an IPv6 address word by word", structs.ProxyInfo{IP6: [4]uint32{0xfd000000, 0, 0, 0x00000002}, Port: 1},
			[]netip.AddrPort{netip.MustParseAddrPort("[fd00::2]:1")}},
		{"no port: nothing is counted", structs.ProxyInfo{IP4: 0x7f000001}, nil},
	} {
		got := proxyConnTargets(tc.info)
		if len(got) != len(tc.want) {
			t.Fatalf("%s: %v, want %v", tc.name, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: %v, want %v", tc.name, got, tc.want)
			}
		}
	}
	if got := (&Hooks{}).ProxyConnTargets(); got != nil {
		t.Fatalf("hooks that never resolved the proxy: %v, want none", got)
	}
}
