package http

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/docker/go-connections/nat"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"
)

// A replay whose ports were given the way a CI lane gives each phase its own:
// app port P, proxy P+1, DNS P+2. The tests were recorded with the app on
// 6123, and this replay runs it on 6122, published as "6122:6122", with the
// tests sent there; keploy's proxy now listens on 6123 and its DNS server on
// 6124, in the namespace the app shares. The first test went out on a busy
// host before the app listened. The agent, asked where the app listens on
// 6123, answered with keploy's own proxy socket, so the publish of 6122 was
// taken for a wrong one ("where nothing listens, instead of the app's port
// 6123 ..., where the app listens"): the readiness wait and the re-send of a
// refused test were turned off and every test failed. Nothing seen there is
// the app's, so where the app is still starting it cannot be told unreachable.
func TestUnreachableAppPortDoesNotTakeKeploysOwnListenerForTheApp(t *testing.T) {
	published := nat.PortMap{"6122/tcp": {{HostIP: "0.0.0.0", HostPort: "6122"}, {HostIP: "::", HostPort: "6122"}}}
	laneConf := func(c *config.Config) {
		c.Agent.ProxyPort, c.ProxyPort = 6123, 6123
		c.Agent.DnsPort, c.DNSPort = 6124, 6124
	}
	for _, tc := range []struct {
		name    string
		appPort uint16
		set     func(*config.Config)
		addrs   map[string][]string // the agent's answer, by port, counting keploy's own sockets as an older agent does
		want    string
		asked   []string
	}{
		{
			name: "the recorded port is keploy's proxy now, the app still starting", appPort: 6123, set: laneConf,
			addrs: map[string][]string{"6122": {}, "6123": {"0.0.0.0", "::"}}, asked: []string{"6122"},
		},
		{
			name: "the recorded port is keploy's DNS server now, the app still starting", appPort: 6124, set: laneConf,
			addrs: map[string][]string{"6122": {}, "6124": {"0.0.0.0"}}, asked: []string{"6122"},
		},
		{
			name: "the recorded port is keploy's API now, the app still starting", appPort: 16789,
			set:   func(c *config.Config) {}, // dockerAgent's agent port
			addrs: map[string][]string{"6122": {}, "16789": {"::"}}, asked: []string{"6122"},
		},
		{
			name: "the recorded port is keploy's proxy now, the app listening", appPort: 6123, set: laneConf,
			addrs: map[string][]string{"6122": {"0.0.0.0"}, "6123": {"0.0.0.0", "::"}}, asked: []string{"6122"},
		},
		{
			// Not keploy's: the app's own socket on its recorded port still
			// says the publish of 6122 is the wrong one.
			name: "the recorded port is the app's, where it listens", appPort: 6123,
			set:   func(c *config.Config) { c.Agent.ProxyPort, c.ProxyPort = 16790, 16790 },
			addrs: map[string][]string{"6122": {}, "6123": {"0.0.0.0"}}, asked: []string{"6122", "6123"},
			want: `host port 6122 is published to the container's port 6122, where nothing listens, instead of the app's port 6123 these tests were recorded on, where the app listens, and keploy sends these tests from the host to port 6122, for the app's port 6123 they were recorded on. Change "6122:6122" to "6122:6123" in the app service's ports` +
				`, or, to keep "6122:6122", publish the app's port 6123 on another host port too and send these tests there with a port map in keploy.yml (test.replaceWith.global.port) in place of the setting that sends them to port 6122 now`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &countingInspect{resp: agentContainer(published, true, "bridge")}
			l := &listeningAgent{addrs: tc.addrs, status: http.StatusNotFound}
			a := withAgent(t, dockerAgent(utils.DockerCompose, fake), l)
			tc.set(a.conf)

			if got := a.UnreachableAppPort(context.Background(), "localhost", 6122, tc.appPort); got != tc.want {
				t.Errorf("reason:\n got %q\nwant %q", got, tc.want)
			}
			var want []string
			for _, port := range tc.asked {
				want = append(want, "/agent/app/listen-addrs?port="+port)
			}
			if !slices.Equal(l.asked, want) {
				t.Errorf("asked the agent %v, want %v", l.asked, want)
			}
		})
	}
}

// When the port the tests have to reach is one keploy listens on in this run,
// the app cannot listen there: publishing it, or listening on 0.0.0.0 there,
// is no fix. The reason says the port is keploy's and advises moving keploy's
// listener (naming its flag) or the app's port, never publishing to it.
func TestUnreachableAppPortSaysTheRecordedPortIsKeploys(t *testing.T) {
	laneConf := func(c *config.Config) {
		c.Agent.ProxyPort, c.ProxyPort = 6123, 6123
		c.Agent.DnsPort, c.DNSPort = 6124, 6124
	}
	const sendThere = `have the app listen on another port and send these tests to a host port published to it with a port map in keploy.yml (test.replaceWith.global.port: {%s: <host port>})`
	moveProxy := "Give keploy's proxy another port with --proxy-port (proxyPort in keploy.yml), or " + sendThere
	for _, tc := range []struct {
		name          string
		published     nat.PortMap
		port, appPort uint16
		set           func(*config.Config)
		addrs         map[string][]string
		want          string
	}{
		{
			// The replay's app moved to 6122, the tests still go to 6123.
			name:      "recorded on keploy's proxy port, sent there, the app on another port",
			published: nat.PortMap{"6122/tcp": {{HostIP: "0.0.0.0", HostPort: "6122"}}},
			port:      6123, appPort: 6123, set: laneConf,
			want: "the app's port 6123 these tests were recorded on is keploy's proxy port in this run, where the app cannot listen, and keploy sends each test from the host to port 6123, the port it was recorded on. " +
				strings.ReplaceAll(moveProxy, "%s", "6123"),
		},
		{
			// The compose file still publishes "6123:6123" as recorded: on
			// this run that publish reaches keploy's proxy, not the app.
			name:      "recorded on keploy's proxy port, published as itself",
			published: nat.PortMap{"6123/tcp": {{HostIP: "0.0.0.0", HostPort: "6123"}}},
			port:      6123, appPort: 6123, set: laneConf,
			want: "the app's port 6123 these tests were recorded on is keploy's proxy port in this run, where the app cannot listen, and keploy sends each test from the host to port 6123, the port it was recorded on. " +
				strings.ReplaceAll(moveProxy, "%s", "6123"),
		},
		{
			name:      "recorded on keploy's DNS port",
			published: nat.PortMap{"6122/tcp": {{HostIP: "0.0.0.0", HostPort: "6122"}}},
			port:      6124, appPort: 6124, set: laneConf,
			want: "the app's port 6124 these tests were recorded on is keploy's DNS server port in this run, where the app cannot listen, and keploy sends each test from the host to port 6124, the port it was recorded on. " +
				"Give keploy's DNS server another port with --dns-port (dnsPort in keploy.yml), or " + strings.ReplaceAll(sendThere, "%s", "6124"),
		},
		{
			// keploy picks its API's port itself: no flag moves it.
			name:      "recorded on keploy's API port",
			published: nat.PortMap{"6122/tcp": {{HostIP: "0.0.0.0", HostPort: "6122"}}},
			port:      16789, appPort: 16789, set: func(*config.Config) {},
			want: "the app's port 16789 these tests were recorded on is keploy's API port in this run, where the app cannot listen, and keploy sends each test from the host to port 16789, the port it was recorded on. " +
				"keploy picks that port itself: " + strings.ReplaceAll(sendThere, "%s", "16789"),
		},
		{
			// A setting sends the tests to host port 6125, published nowhere.
			name:      "recorded on keploy's proxy port, sent to an unpublished host port",
			published: nat.PortMap{"6122/tcp": {{HostIP: "0.0.0.0", HostPort: "6122"}}},
			port:      6125, appPort: 6123, set: laneConf,
			want: "the app's port 6123 these tests were recorded on is keploy's proxy port in this run, where the app cannot listen, and keploy sends these tests from the host to port 6125, for the app's port 6123 they were recorded on. " +
				strings.ReplaceAll(moveProxy, "%s", "6123") + " in place of the setting that sends them to port 6125 now",
		},
		{
			// Tests that do not say their port: the port they are sent to
			// stands in for it, and it is keploy's proxy's in the container.
			name:      "not recorded with a port, sent to keploy's proxy port",
			published: nat.PortMap{"6122/tcp": {{HostIP: "0.0.0.0", HostPort: "6122"}}},
			port:      6123, set: laneConf,
			want: "the container's port 6123 is keploy's proxy port in this run, where the app cannot listen, and keploy sends these tests from the host to port 6123. " +
				strings.ReplaceAll(moveProxy, "%s", "6123"),
		},
		{
			// Published only where keploy's connections do not reach.
			name:      "recorded on keploy's proxy port, sent to a host port published elsewhere",
			published: nat.PortMap{"6122/tcp": {{HostIP: "10.1.2.3", HostPort: "6123"}}},
			port:      6123, appPort: 6123, set: laneConf,
			want: "the app's port 6123 these tests were recorded on is keploy's proxy port in this run, where the app cannot listen, and keploy sends each test from the host to port 6123, the port it was recorded on. " +
				strings.ReplaceAll(moveProxy, "%s", "6123"),
		},
		{
			// The tests go to host port 6122, published to the app's 6122,
			// where it listens only on loopback: listening on 0.0.0.0 there
			// is the fix, and publishing 6122 to 6123 is none.
			name:      "recorded on keploy's proxy port, sent to a publish where the app listens only on loopback",
			published: nat.PortMap{"6122/tcp": {{HostIP: "0.0.0.0", HostPort: "6122"}}},
			port:      6122, appPort: 6123, set: laneConf,
			addrs: map[string][]string{"6122": {"127.0.0.1"}},
			want: "host port 6122 is published to the container's port 6122, where the app listens only on 127.0.0.1, which no published port reaches, and keploy sends these tests from the host to port 6122, for the app's port 6123 they were recorded on. keploy cannot replay these tests from the host through it: the app would have to listen on 0.0.0.0:6122" +
				" (the app's port 6123 these tests were recorded on is keploy's proxy port in this run, where the app cannot listen)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &countingInspect{resp: agentContainer(tc.published, true, "bridge")}
			l := &listeningAgent{addrs: tc.addrs, status: http.StatusNotFound}
			a := withAgent(t, dockerAgent(utils.DockerCompose, fake), l)
			tc.set(a.conf)

			got := a.UnreachableAppPort(context.Background(), "localhost", tc.port, tc.appPort)
			if got != tc.want {
				t.Errorf("reason:\n got %q\nwant %q", got, tc.want)
			}
			target := strconv.Itoa(int(tc.appPort))
			if tc.appPort == 0 {
				target = strconv.Itoa(int(tc.port))
			}
			for _, wrong := range []string{"Publish it", ":" + target + `"`, "-p " + target, "0.0.0.0:" + target} {
				if strings.Contains(got, wrong) {
					t.Errorf("reason advises publishing to, or listening on, keploy's own port %s (%q): %q", target, wrong, got)
				}
			}
		})
	}
}
