package http

import (
	"context"
	"net/http"
	"slices"
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
