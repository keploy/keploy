package docker

import (
	"context"
	"reflect"
	"testing"

	"github.com/compose-spec/compose-go/v2/loader"
	composetypes "github.com/compose-spec/compose-go/v2/types"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"go.keploy.io/server/v3/config"
)

// resolvedPorts is service's ports in doc as docker compose resolves them
// (compose's own loader) against env.
func resolvedPorts(t *testing.T, doc []byte, service string, env map[string]string) []composetypes.ServicePortConfig {
	t.Helper()
	project, err := loader.LoadWithContext(context.Background(), composetypes.ConfigDetails{
		ConfigFiles: []composetypes.ConfigFile{{Filename: "compose.yml", Content: doc}},
		WorkingDir:  t.TempDir(),
		Environment: env,
	}, func(o *loader.Options) {
		o.SetProjectName("p", true)
		o.SkipConsistencyCheck = true
		o.SkipResolveEnvironment = true
		o.SkipValidation = true
	})
	if err != nil {
		t.Fatalf("compose does not load:\n%s\n%v", doc, err)
	}
	svc, ok := project.Services[service]
	if !ok {
		t.Fatalf("no service %q in:\n%s", service, doc)
	}
	return svc.Ports
}

// keploy-agent publishes the app's ports in its place (the app joins its
// network namespace), so compose must resolve each of them on the agent
// exactly as it resolved it on the app: the host address, the protocol and
// the mode too. A long-syntax entry rewritten as "published:target" lost its
// host_ip, and a port the user bound to 127.0.0.1 was published on every
// interface.
func TestModifyComposeForAgent_AppPortsKeepWhatComposeResolves(t *testing.T) {
	env := map[string]string{"BIND_IP": "127.0.0.1", "PUB": "6100"}
	for name, ports := range map[string]string{
		"long syntax, host_ip 127.0.0.1":     "      - target: 80\n        published: 8080\n        host_ip: 127.0.0.1\n",
		"long syntax, host_ip ::1":           "      - target: 80\n        published: \"8080\"\n        host_ip: \"::1\"\n",
		"long syntax, protocol and mode":     "      - target: 53\n        published: 5353\n        host_ip: 127.0.0.1\n        protocol: udp\n        mode: host\n",
		"long syntax, name and app_protocol": "      - name: web\n        target: 80\n        published: 8080\n        host_ip: 127.0.0.1\n        app_protocol: http\n",
		"long syntax, from variables":        "      - target: 80\n        published: ${PUB:-7000}\n        host_ip: ${BIND_IP}\n",
		"long syntax, a range":               "      - target: 80\n        published: 8080-8090\n        host_ip: 127.0.0.1\n",
		"long syntax, no host_ip":            "      - target: 80\n        published: 8080\n",
		"long syntax, target only":           "      - target: 80\n",
		"long syntax, an aliased value":      "      - target: 80\n        published: *pub\n        host_ip: 127.0.0.1\n",
		"short syntax, host_ip":              "      - \"127.0.0.1:8080:8080\"\n",
		"short syntax, IPv6 host_ip":         "      - \"[::1]:8080:80/udp\"\n",
		"short syntax, no host_ip":           "      - \"8080:80\"\n      - 9090\n",
		"both syntaxes":                      "      - \"127.0.0.1:9000:9000\"\n      - target: 80\n        published: 8080\n        host_ip: 127.0.0.1\n",
	} {
		t.Run(name, func(t *testing.T) {
			doc := "x-pub: &pub 8080\nservices:\n  app:\n    image: x\n    ports:\n" + ports
			want := resolvedPorts(t, []byte(doc), "app", env)
			if len(want) == 0 {
				t.Fatalf("the row resolves to no port:\n%s", doc)
			}
			idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
			var compose Compose
			if err := yaml.Unmarshal([]byte(doc), &compose); err != nil {
				t.Fatal(err)
			}
			_, appPorts, found := idc.serviceByKey(&compose, "app")
			if !found {
				t.Fatal("no app service")
			}
			opts := buildAgentOpts()
			opts.AppPorts = appPorts
			if err := idc.ModifyComposeForAgent(&compose, opts, "app"); err != nil {
				t.Fatalf("ModifyComposeForAgent: %v", err)
			}
			out, err := idc.MarshalCompose(&compose)
			if err != nil {
				t.Fatal(err)
			}
			var got []composetypes.ServicePortConfig
			for _, p := range resolvedPorts(t, out, "keploy-agent", env) {
				if p.Target != opts.AgentPort {
					got = append(got, p)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("keploy-agent publishes %+v, want the app's %+v, from AppPorts %q:\n%s", got, want, appPorts, out)
			}
			if app := resolvedPorts(t, out, "app", env); len(app) != 0 {
				t.Fatalf("the app still publishes %+v under keploy-agent's network", app)
			}
		})
	}
}

// A long-syntax entry is one AppPorts string that keploy-agent takes back as
// the mapping it was; a short-syntax one stays the string as written.
func TestAgentPortNode(t *testing.T) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("a: &a 127.0.0.1\np:\n  target: 80 # c\n  host_ip: *a\n  published: \"8080\"\n"), &doc); err != nil {
		t.Fatal(err)
	}
	entry := composePortEntry(doc.Content[0].Content[3])
	if entry != `{target: 80, host_ip: 127.0.0.1, published: "8080"}` {
		t.Fatalf("composePortEntry = %q", entry)
	}
	if n := agentPortNode(entry); n.Kind != yaml.MappingNode || len(n.Content) != 6 || n.Content[3].Value != "127.0.0.1" {
		t.Fatalf("agentPortNode(%q) = %+v, want the mapping back", entry, n)
	}
	for _, short := range []string{"127.0.0.1:8080:8080", "8080", "${PORT}:80"} {
		if n := agentPortNode(short); n.Kind != yaml.ScalarNode || n.Value != short || n.Style != yaml.DoubleQuotedStyle {
			t.Fatalf("agentPortNode(%q) = %+v, want the string as written", short, n)
		}
	}
}
