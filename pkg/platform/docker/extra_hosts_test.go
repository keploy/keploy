package docker

import (
	"context"
	"errors"
	"go/ast"
	"go/token"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// versionOnly answers ServerVersion and nothing else; podman, when set, is
// the version of the Podman Engine component Podman's API names.
type versionOnly struct {
	client.APIClient
	api    string
	podman string
	err    error
}

func (v versionOnly) ServerVersion(context.Context) (types.Version, error) {
	out := types.Version{APIVersion: v.api}
	if v.podman != "" {
		out.Components = []types.ComponentVersion{{Name: "Podman Engine", Version: v.podman}}
	}
	return out, v.err
}

// host-gateway is resolved by Engine 20.10 (API 1.41) and later; a daemon that
// cannot be asked is taken for one that does not resolve it, as an entry it
// cannot resolve would stop the agent container from starting.
func TestHostGatewayIsMappedOnlyWhereTheDaemonResolvesIt(t *testing.T) {
	for _, tc := range []struct {
		api, podman string
		err         error
		want        bool
	}{
		{"1.41", "", nil, true},
		{"1.47", "", nil, true},
		{"1.40", "", nil, false},
		{"", "", errors.New("cannot connect"), false},
		// Podman's compatible API says 1.41 or later from 4.2 on; its
		// --add-host takes host-gateway from 4.7.0 on.
		{"1.41", "4.3.1", nil, false},
		{"1.41", "4.6.2", nil, false},
		{"1.41", "4.7.0", nil, true},
		{"1.41", "5.8.7", nil, true},
	} {
		got, err := HostGatewaySupported(context.Background(), versionOnly{api: tc.api, podman: tc.podman, err: tc.err})
		if err != nil || got != tc.want {
			t.Errorf("API %q, Podman %q (err %v): host-gateway supported %v (%v), want %v", tc.api, tc.podman, tc.err, got, err, tc.want)
		}
	}
	if got, err := HostGatewaySupported(context.Background(), nil); got || err != nil {
		t.Errorf("no daemon resolves host-gateway: %v, %v", got, err)
	}
	// A daemon that has not answered when ctx ends has not said no: the
	// caller is told, rather than the app's /etc/hosts losing the entry.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := HostGatewaySupported(ctx, versionOnly{err: context.Canceled}); got || !errors.Is(err, context.Canceled) {
		t.Errorf("an unanswered question was taken for an answer: %v, %v", got, err)
	}
}

// The agent container carries the app's host entries, then
// host.docker.internal at host-gateway, unless the app maps that name itself.
// An entry for the agent's `docker run` that is not NAME:ADDRESS is refused,
// naming it.
func TestTheAgentCarriesTheAppsHostEntries(t *testing.T) {
	for _, tc := range []struct {
		name        string
		hosts       []string
		hostGateway bool
		want        []string
	}{
		{"the app's, then the Docker host", []string{"db.test:10.1.2.3", "api.test=host-gateway"}, true,
			[]string{"db.test:10.1.2.3", "api.test=host-gateway", "host.docker.internal:host-gateway"}},
		{"the app's own Docker host wins", []string{"Host.Docker.Internal:10.9.9.9"}, true,
			[]string{"Host.Docker.Internal:10.9.9.9"}},
		{"no host-gateway where the daemon cannot resolve it", []string{"db.test:10.1.2.3"}, false,
			[]string{"db.test:10.1.2.3"}},
		{"an IPv6 address, bracketed or bare", []string{`"v6.test:[::1]"`, "v6b.test:::1", "v6c.test=[fd00::1]"}, false,
			[]string{"v6.test:::1", "v6b.test:::1", "v6c.test=fd00::1"}},
		{"none", nil, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := agentHostEntries(tc.hosts, tc.hostGateway, true)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("entries %q, %v; want %q", got, err, tc.want)
			}
			flags, _ := agentHostFlags(tc.hosts, tc.hostGateway)
			var want strings.Builder
			for _, e := range tc.want {
				want.WriteString(" --add-host " + e)
			}
			if flags != want.String() {
				t.Fatalf("flags %q, want %q", flags, want.String())
			}
		})
	}
	for _, bad := range []string{"db.test", "db.test:10.1.2.3;rm -rf /", "db test:10.1.2.3", "db.test:fe80::1%PATH%", "db.test:[::1]*"} {
		if _, err := agentHostFlags([]string{bad}, true); err == nil || !strings.Contains(err.Error(), bad) {
			t.Errorf("entry %q was taken for the agent's docker run: %v", bad, err)
		}
	}
}

// The linux arm's agent `docker run` carries the entries.
func TestGetAlias_LinuxCarriesTheAppsHostEntries(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("getAlias switches on runtime.GOOS; the linux arm only executes here")
	}
	alias, err := getAlias(context.Background(), zap.NewNop(), models.SetupOptions{
		KeployContainer: "keploy-test", AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791, ClientNSPID: 4242,
		Mode: models.MODE_RECORD, AppExtraHosts: []string{"db.test:10.1.2.3"}, HostGateway: true,
	}, &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(alias, " --add-host db.test:10.1.2.3 --add-host host.docker.internal:host-gateway ") {
		t.Fatalf("the agent's docker run carries no host entries:\n%s", alias)
	}
	if _, err := getAlias(context.Background(), zap.NewNop(), models.SetupOptions{AppExtraHosts: []string{"db.test"}}, &config.Config{}); err == nil {
		t.Fatal("an entry that is not NAME:ADDRESS went into the agent's docker run")
	}
}

// Every platform arm builds its alias with the host entries: the app joins
// the agent container's network on each.
func TestGetAlias_EveryPlatformBranchCarriesTheAppsHostEntries(t *testing.T) {
	forEachAliasReturningBranch(t, func(t *testing.T, pos token.Position, list []ast.Stmt) {
		t.Helper()
		for _, stmt := range list {
			if stmtAssignsAliasUsing(stmt, "appHostsStr") {
				return
			}
		}
		t.Errorf("%s: this getAlias platform branch builds its alias without appHostsStr: the app's host entries, which Docker refuses on the app under the agent's network mode, would be lost", pos)
	})
}

// composeOut is the services of a generated compose file.
func composeOut(t *testing.T, idc *Impl, compose *Compose) map[string]map[string]interface{} {
	t.Helper()
	data, err := idc.MarshalCompose(compose)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Services map[string]map[string]interface{} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatalf("the generated compose does not load: %v\n%s", err, data)
	}
	return out.Services
}

// A compose app service's extra_hosts move onto keploy-agent, in either of the
// spec's forms, after any keploy-agent has, and keploy-agent maps
// host.docker.internal to host-gateway unless the app or keploy-agent maps it:
// compose refuses extra_hosts with the app's network_mode
// ("conflicting options: custom host-to-IP mapping and the network mode").
func TestComposeExtraHostsMoveToKeployAgent(t *testing.T) {
	for _, tc := range []struct {
		name        string
		app, agent  string
		hostGateway bool
		want        []interface{}
	}{
		{"a list", `extra_hosts: ["db.test:10.1.2.3", "api.test=host-gateway"]`, "", true,
			[]interface{}{"db.test:10.1.2.3", "api.test=host-gateway", "host.docker.internal:host-gateway"}},
		{"a map", "extra_hosts: {db.test: 10.1.2.3, multi.test: [10.0.0.1, 10.0.0.2]}", "", false,
			[]interface{}{"db.test:10.1.2.3", "multi.test:10.0.0.1", "multi.test:10.0.0.2"}},
		{"interpolated, as compose will read it", `extra_hosts: ["${DB_HOST}:${DB_IP}"]`, "", false,
			[]interface{}{"${DB_HOST}:${DB_IP}"}},
		{"the app's own Docker host wins", `extra_hosts: ["host.docker.internal:10.9.9.9"]`, "", true,
			[]interface{}{"host.docker.internal:10.9.9.9"}},
		{"after keploy-agent's own", `extra_hosts: ["db.test:10.1.2.3"]`, `extra_hosts: ["host.docker.internal:10.8.8.8"]`, true,
			[]interface{}{"host.docker.internal:10.8.8.8", "db.test:10.1.2.3"}},
		{"none of the app's", "", "", true, []interface{}{"host.docker.internal:host-gateway"}},
		{"none at all", "", "", false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
			in := "services:\n  app:\n    image: alpine:3.21\n"
			if tc.app != "" {
				in += "    " + tc.app + "\n"
			}
			in += "  keploy-agent:\n    image: keploy/agent\n"
			if tc.agent != "" {
				in += "    " + tc.agent + "\n"
			}
			var compose Compose
			if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
				t.Fatal(err)
			}
			if err := idc.modifyAppServiceForKeploy(&compose, "app", tc.hostGateway); err != nil {
				t.Fatal(err)
			}
			svcs := composeOut(t, idc, &compose)
			if hosts, ok := svcs["app"]["extra_hosts"]; ok {
				t.Fatalf("the app keeps extra_hosts %v beside network_mode %v", hosts, svcs["app"]["network_mode"])
			}
			got, _ := svcs["keploy-agent"]["extra_hosts"].([]interface{})
			if !slices.Equal(got, tc.want) {
				t.Fatalf("keploy-agent's extra_hosts %q, want %q", got, tc.want)
			}
		})
	}
}

// An extra_hosts that is neither a list nor a map is refused, naming it.
func TestComposeExtraHostsOfAnotherShapeAreRefused(t *testing.T) {
	idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
	var compose Compose
	if err := yaml.Unmarshal([]byte("services:\n  app:\n    image: alpine:3.21\n    extra_hosts: db.test\n  keploy-agent:\n    image: keploy/agent\n"), &compose); err != nil {
		t.Fatal(err)
	}
	if err := idc.modifyAppServiceForKeploy(&compose, "app", true); err == nil || !strings.Contains(err.Error(), "extra_hosts") {
		t.Fatalf("an extra_hosts keploy cannot read was taken: %v", err)
	}
}

// An app service's extra_hosts are read through YAML aliases and a map's `<<`
// merge keys, as compose reads them, through the real rewrite
// (ModifyComposeForAgent), and another service reading the same anchor keeps
// its entries.
func TestComposeExtraHostsReadThroughAliasesAndMergeKeys(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     []interface{}
	}{
		{"an aliased list", "x-hosts: &hosts\n  - \"db.test:10.0.0.1\"\nservices:\n  app:\n    image: alpine:3.21\n    extra_hosts: *hosts\n  other:\n    image: alpine:3.21\n    extra_hosts: *hosts\n",
			[]interface{}{"db.test:10.0.0.1"}},
		{"an aliased entry", "x-h: &h \"db.test:10.0.0.1\"\nservices:\n  app:\n    image: alpine:3.21\n    extra_hosts:\n      - *h\n",
			[]interface{}{"db.test:10.0.0.1"}},
		{"aliased addresses in a map", "x-ip: &ip 10.0.0.1\nservices:\n  app:\n    image: alpine:3.21\n    extra_hosts:\n      db.test: *ip\n      multi.test: [*ip, 10.0.0.2]\n",
			[]interface{}{"db.test:10.0.0.1", "multi.test:10.0.0.1", "multi.test:10.0.0.2"}},
		{"a map's merge key, its own names winning", "x-hosts: &hosts\n  db.test: 10.0.0.1\n  api.test: 10.9.9.9\nservices:\n  app:\n    image: alpine:3.21\n    extra_hosts:\n      <<: *hosts\n      api.test: 10.0.0.2\n",
			[]interface{}{"api.test:10.0.0.2", "db.test:10.0.0.1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
			var compose Compose
			if err := yaml.Unmarshal([]byte(tc.in), &compose); err != nil {
				t.Fatal(err)
			}
			if err := idc.ModifyComposeForAgent(&compose, buildAgentOpts(), "app"); err != nil {
				t.Fatal(err)
			}
			svcs := composeOut(t, idc, &compose)
			if hosts, ok := svcs["app"]["extra_hosts"]; ok {
				t.Fatalf("the app keeps extra_hosts %v", hosts)
			}
			if got, _ := svcs["keploy-agent"]["extra_hosts"].([]interface{}); !slices.Equal(got, tc.want) {
				t.Fatalf("keploy-agent's extra_hosts %q, want %q", got, tc.want)
			}
			if other, ok := svcs["other"]; ok {
				if got, _ := other["extra_hosts"].([]interface{}); !slices.Equal(got, []interface{}{"db.test:10.0.0.1"}) {
					t.Fatalf("the other service's extra_hosts became %q", got)
				}
			}
		})
	}
}
