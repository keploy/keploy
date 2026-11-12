package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.keploy.io/server/v3/config"
)

// The agent container's sockets during a replay whose ports were given the
// way a CI lane gives each phase its own: app port P, proxy P+1, DNS P+2. The
// tests were recorded on 6123, and this replay's app port is 6122, so keploy's
// proxy now listens on 6123 (0.0.0.0 and ::) and its DNS server on 6124. The
// agent's API is on :::16789. The app is still starting: nothing listens on
// 6122 yet.
const ownPortsTCP = `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:17EB 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 1 1 0000000000000000 100 0 0 10 0
   1: 00000000:17EC 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 2 1 0000000000000000 100 0 0 10 0
`

const ownPortsTCP6 = `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:17EB 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 3 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000000000000:4195 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 4 1 0000000000000000 100 0 0 10 0
`

func useProcNetTables(t *testing.T, tcp, tcp6 string) {
	t.Helper()
	dir := t.TempDir()
	tcpPath, tcp6Path := filepath.Join(dir, "tcp"), filepath.Join(dir, "tcp6")
	if err := os.WriteFile(tcpPath, []byte(tcp), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tcp6Path, []byte(tcp6), 0o600); err != nil {
		t.Fatal(err)
	}
	saved := procNetTables
	t.Cleanup(func() { procNetTables = saved })
	procNetTables = []string{tcpPath, tcp6Path}
}

// In Docker mode the app shares the agent's network namespace, so keploy's
// own listeners are in the tables the agent reads. A socket on one of keploy's
// ports is keploy's, never the app's: answering from it said the app listened
// on its recorded port 6123 while it was still starting, and the replay took
// the publish of 6122 for a wrong one, failing every test at once instead of
// waiting for the app.
func TestAppListenAddrsDoesNotAnswerFromKeploysOwnListeners(t *testing.T) {
	useProcNetTables(t, ownPortsTCP, ownPortsTCP6)
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		port uint16
		set  func(*config.Config)
	}{
		{"the proxy, as the agent's flags set it", 6123, func(c *config.Config) { c.Agent.ProxyPort = 6123 }},
		{"the proxy, as keploy.yml sets it", 6123, func(c *config.Config) { c.ProxyPort = 6123 }},
		{"the DNS server, as the agent's flags set it", 6124, func(c *config.Config) { c.Agent.DnsPort = 6124 }},
		{"the DNS server, as keploy.yml sets it", 6124, func(c *config.Config) { c.DNSPort = 6124 }},
		{"the agent's API", 16789, func(c *config.Config) { c.Agent.AgentPort = 16789 }},
	} {
		conf := &config.Config{}
		tc.set(conf)
		replaying := &Agent{config: conf}
		if got, err := replaying.AppListenAddrs(ctx, tc.port); err == nil {
			t.Errorf("%s: port %d is keploy's own, and the agent answered %v for the app there; it cannot tell where the app listens", tc.name, tc.port, got)
		}
		// Asked through the ingress's answer too: the port the app's bind
		// is on, not the one asked about, is the one read.
		forwarded := &Agent{config: conf, IncomingProxy: movedIngress{moved: map[uint16]uint16{8080: tc.port}}}
		if got, err := forwarded.AppListenAddrs(ctx, 8080); err == nil {
			t.Errorf("%s: the app's port 8080 reads port %d, keploy's own, and the agent answered %v for the app there", tc.name, tc.port, got)
		}
	}

	// The same sockets on ports that are not keploy's are the app's.
	conf := &config.Config{}
	conf.Agent.ProxyPort, conf.Agent.DnsPort, conf.Agent.AgentPort = 16790, 16791, 16792
	conf.ProxyPort, conf.DNSPort = 16790, 16791
	a := &Agent{config: conf}
	for port, want := range map[uint16][]string{6123: {"0.0.0.0", "::"}, 6124: {"0.0.0.0"}, 16789: {"::"}, 6122: nil} {
		got, err := a.AppListenAddrs(ctx, port)
		if err != nil || !slices.Equal(got, addrs(t, want...)) {
			t.Errorf("port %d, not keploy's: got %v, %v; want %v", port, got, err, want)
		}
	}
	// An agent without a configuration (a test's, or one not set up yet)
	// knows no ports of its own.
	if got, err := (&Agent{}).AppListenAddrs(ctx, 6123); err != nil || len(got) != 2 {
		t.Errorf("an agent with no configuration: got %v, %v", got, err)
	}
}
