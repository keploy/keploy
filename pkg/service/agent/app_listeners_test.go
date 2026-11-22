package agent

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	coreAgent "go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// Rows as the kernel prints them, taken from a keploy agent container while it
// recorded an app that listens on 0.0.0.0:8095 and 127.0.0.1:8097: the record
// hooks moved the app's binds to 35281 and 41541, and keploy's ingress
// forwarder holds 8095 and 8097 on 0.0.0.0. Plus an established connection on
// 8097, which is not a listener.
const procNetTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:A245 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 174588491 1 0000000000000000 100 0 0 10 0
   1: 00000000:1F9F 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 174584613 1 0000000000000000 100 0 0 10 0
   2: 00000000:1FA1 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 174584614 1 0000000000000000 100 0 0 10 0
   3: 00000000:89D1 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 174584615 1 0000000000000000 100 0 0 10 0
   4: 0100007F:1FA1 0100007F:C350 01 00000000:00000000 00:00000000 00000000     0        0 174584616 1 0000000000000000 20 4 30 10 -1
`

// An IPv6 table: :: on 16789 (keploy's agent), ::1 and ::ffff:127.0.0.1 on
// 41541.
const procNetTCP6 = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:4195 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000001000000:A245 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2 1 0000000000000000 100 0 0 10 0
   2: 0000000000000000FFFF00000100007F:A245 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 3 1 0000000000000000 100 0 0 10 0
`

func addrs(t *testing.T, ss ...string) []netip.Addr {
	t.Helper()
	var out []netip.Addr
	for _, s := range ss {
		out = append(out, netip.MustParseAddr(s))
	}
	return out
}

func TestListenAddrsFromProcNet(t *testing.T) {
	for _, tc := range []struct {
		table string
		port  uint16
		want  []netip.Addr
	}{
		{procNetTCP, 41541, addrs(t, "127.0.0.1")},
		{procNetTCP, 8097, addrs(t, "0.0.0.0")}, // the listener, not the connection
		{procNetTCP, 35281, addrs(t, "0.0.0.0")},
		{procNetTCP, 9999, nil},
		{procNetTCP6, 16789, addrs(t, "::")},
		{procNetTCP6, 41541, addrs(t, "::1", "127.0.0.1")},
	} {
		got, err := listenAddrsFromProcNet(strings.NewReader(tc.table), tc.port)
		if err != nil {
			t.Fatalf("port %d: %v", tc.port, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("port %d: got %v, want %v", tc.port, got, tc.want)
		}
	}
	if _, err := listenAddrsFromProcNet(strings.NewReader("header\n 0: XYZ:1FA1 0 0A\n"), 8097); err == nil {
		t.Error("a malformed row must be an error, not an empty answer")
	}
}

// movedIngress is the ingress manager while recording: it holds the app's
// ports for keploy's forwarder and knows where each app bind was moved to.
type movedIngress struct {
	coreAgent.IncomingProxy
	moved map[uint16]uint16
}

func (m movedIngress) AppListenPort(orig uint16) (uint16, bool) {
	if p, ok := m.moved[orig]; ok {
		return p, p != 0
	}
	return orig, true
}

// The agent is asked about the app's port, and while recording that port is
// keploy's own forwarder on 0.0.0.0: answering from it would call every app
// reachable. It answers from the socket the app itself listens on.
func TestAppListenAddrsAnswersForTheAppsOwnSocket(t *testing.T) {
	dir := t.TempDir()
	tcp, tcp6 := filepath.Join(dir, "tcp"), filepath.Join(dir, "tcp6")
	if err := os.WriteFile(tcp, []byte(procNetTCP), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tcp6, []byte(procNetTCP6), 0o600); err != nil {
		t.Fatal(err)
	}
	defer func(t []string) { procNetTables = t }(procNetTables)
	procNetTables = []string{tcp, tcp6}

	recording := &Agent{IncomingProxy: movedIngress{moved: map[uint16]uint16{8097: 41541, 8095: 35281, 8098: 0}}}
	for port, want := range map[uint16][]netip.Addr{
		8097: addrs(t, "127.0.0.1", "::1", "127.0.0.1"),
		8095: addrs(t, "0.0.0.0"),
	} {
		got, err := recording.AppListenAddrs(context.Background(), port)
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("port %d: got %v, %v; want %v", port, got, err, want)
		}
	}
	if got, err := recording.AppListenAddrs(context.Background(), 8098); err == nil {
		t.Errorf("a forwarded port whose app port is unknown answered %v; it cannot tell", got)
	}

	// Replaying: nothing moved, the app's port is its own.
	replaying := &Agent{IncomingProxy: movedIngress{}}
	if got, err := replaying.AppListenAddrs(context.Background(), 8097); err != nil || !slices.Equal(got, addrs(t, "0.0.0.0")) {
		t.Errorf("replay: got %v, %v", got, err)
	}

	// An ingress that cannot say where the app listens may be holding the
	// port itself.
	other := &Agent{IncomingProxy: plainIngress{}}
	if got, err := other.AppListenAddrs(context.Background(), 8097); err == nil {
		t.Errorf("an ingress without AppListenPort answered %v; it cannot tell", got)
	}
}

type plainIngress struct{}

func (plainIngress) Start(context.Context, models.IncomingOptions) chan *models.TestCase { return nil }
