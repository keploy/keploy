package docker

import (
	"strings"
	"testing"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
)

// buildAgentOpts is the minimum GenerateKeployAgentService needs to succeed.
func buildAgentOpts() models.SetupOptions {
	return models.SetupOptions{
		KeployContainer: "keploy-agent",
		AgentPort:       16789,
		ProxyPort:       16790,
		DnsPort:         16791,
		Mode:            models.MODE_TEST,
	}
}

// baseFragment carries the two keys any real base fragment carries — and the two
// keploy actually writes into. A fixture without them cannot detect a shallow
// copy, which is how an earlier version of this test passed while siblings were
// being corrupted.
const baseFragment = `x-base: &base
  image: alpine:3.21
  environment:
    SSL_CERT_FILE: /etc/mine/ca.pem
  volumes:
    - ./data:/data
services:
  app: *base
  sidecar: *base
`

// TestServiceAlias_EditsLandAndSiblingsAreUntouched is the regression test for
// the silent half of the services-alias bug.
//
// `services: {app: *appdef}` is legal compose. The node findServiceNodeAndName
// returns is exactly what modifyAppServiceForKeploy writes into, and an alias
// holds no entries — so the agent wiring, TLS mounts and entrypoint rewrite all
// went into a node the encoder never emits. No error, no warning: the app ran
// uninstrumented and nothing was recorded.
//
// Resolving is only half of it. The copy must be DEEP, because keploy does not
// merely append to a service — it deletes keys and overwrites values in place.
// With children shared, `sidecar` inherits keploy's TLS environment while living
// outside the agent's network namespace, so its outbound TLS fails verification
// silently; and the user's own SSL_CERT_FILE is overwritten inside their
// fragment.
func TestServiceAlias_EditsLandAndSiblingsAreUntouched(t *testing.T) {
	idc := &Impl{logger: zap.NewNop()}
	var compose Compose
	if err := yaml.Unmarshal([]byte(baseFragment), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	node, name, err := idc.findServiceNodeAndName(&compose, "app")
	if err != nil {
		t.Fatalf("findServiceNodeAndName: %v", err)
	}
	if name != "app" || node.Kind != yaml.MappingNode {
		t.Fatalf("resolved to name=%q kind=%d, want app as a mapping", name, node.Kind)
	}

	// Exactly what keploy does to the app service: append, and overwrite an
	// existing value in place.
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "container_name"},
		&yaml.Node{Kind: yaml.ScalarNode, Value: "patched-by-keploy"})
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == "environment" {
			env := node.Content[i+1]
			for j := 0; j+1 < len(env.Content); j += 2 {
				if env.Content[j].Value == "SSL_CERT_FILE" {
					env.Content[j+1].Value = "/tmp/keploy-tls/ca.crt"
				}
			}
		}
	}

	data, err := idc.MarshalCompose(&compose)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]interface{}
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatalf("generated compose does not load: %v\n%s", err, data)
	}
	svcs, _ := out["services"].(map[string]interface{})

	app, _ := svcs["app"].(map[string]interface{})
	if app == nil || app["container_name"] != "patched-by-keploy" {
		t.Errorf("keploy's edit was silently lost; the app would run uninstrumented "+
			"with nothing recorded:\n%s", data)
	}
	if env, _ := app["environment"].(map[string]interface{}); env == nil ||
		env["SSL_CERT_FILE"] != "/tmp/keploy-tls/ca.crt" {
		t.Errorf("keploy's in-place env rewrite did not land on app: %v", app["environment"])
	}

	// The sibling must be exactly as the user wrote it.
	sidecar, _ := svcs["sidecar"].(map[string]interface{})
	if sidecar == nil {
		t.Fatalf("sidecar disappeared:\n%s", data)
	}
	if _, leaked := sidecar["container_name"]; leaked {
		t.Errorf("sidecar inherited keploy's container_name — the copy was shallow:\n%s", data)
	}
	if env, _ := sidecar["environment"].(map[string]interface{}); env == nil ||
		env["SSL_CERT_FILE"] != "/etc/mine/ca.pem" {
		t.Errorf("sidecar's own SSL_CERT_FILE was overwritten with keploy's CA (%v); "+
			"it is not in the agent's netns, so its outbound TLS would fail "+
			"verification silently:\n%s", env, data)
	}
	// And the user's fragment itself.
	frag, _ := out["x-base"].(map[string]interface{})
	if env, _ := frag["environment"].(map[string]interface{}); env == nil ||
		env["SSL_CERT_FILE"] != "/etc/mine/ca.pem" {
		t.Errorf("the user's own fragment was rewritten: %v", env)
	}
}

// TestServiceAlias_ContainerNameMatchAlsoResolves pins the lookup path that
// matches on container_name rather than on the service key. It scans the service
// node's contents, so against an alias it scanned nil and could never match —
// leaving the shape reported as "service not found" on a file that has it.
func TestServiceAlias_ContainerNameMatchAlsoResolves(t *testing.T) {
	idc := &Impl{logger: zap.NewNop()}
	const in = `x-app: &app
  image: alpine:3.21
  container_name: myapp
services:
  web: *app
`
	var compose Compose
	if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	node, name, err := idc.findServiceNodeAndName(&compose, "myapp")
	if err != nil {
		t.Fatalf("lookup by container_name failed: %v — the service is there, "+
			"behind an alias", err)
	}
	if name != "web" || node.Kind != yaml.MappingNode {
		t.Errorf("resolved to name=%q kind=%d, want web as a mapping", name, node.Kind)
	}
}

// TestServiceAlias_PortsAndNetworksAreRead pins the gate that feeds
// opts.AppPorts / opts.AppNetworks.
//
// It runs BEFORE ModifyComposeForAgent. Read off an unresolved alias they come
// back empty, so the app's published port never reaches keploy-agent — which
// publishes on the app's behalf under network_mode: service:keploy-agent — and
// the app is unreachable from the host even when everything else works.
func TestServiceAlias_PortsAndNetworksAreRead(t *testing.T) {
	idc := &Impl{logger: zap.NewNop()}
	const in = `x-app: &app
  image: alpine:3.21
  ports:
    - "8080:8080"
  networks:
    - appnet
services:
  app: *app
networks:
  appnet: {}
`
	var compose Compose
	if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	nets, ports, name, found := idc.findContainerInServices(&compose, "app")
	if !found {
		t.Fatalf("the app service was not found behind its alias")
	}
	if name != "app" {
		t.Errorf("name = %q, want app", name)
	}
	if len(ports) == 0 {
		t.Errorf("no ports read from the aliased service; keploy-agent would publish "+
			"none on its behalf and the app would be unreachable from the host (got %v)", ports)
	}
	if len(nets) == 0 {
		t.Errorf("no networks read from the aliased service (got %v)", nets)
	}
}

// TestServiceAlias_WholeSectionAliased covers `services: *svcs`, and pins that
// resolving it does not drag unrelated services into keploy's changes.
func TestServiceAlias_WholeSectionAliased(t *testing.T) {
	idc := &Impl{logger: zap.NewNop()}
	const in = `x-svcs: &svcs
  app:
    image: alpine:3.21
  db:
    image: postgres:16
services: *svcs
`
	var compose Compose
	if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	node, name, err := idc.findServiceNodeAndName(&compose, "app")
	if err != nil {
		t.Fatalf("findServiceNodeAndName: %v — the section is aliased, not absent", err)
	}
	if name != "app" {
		t.Fatalf("name = %q, want app", name)
	}
	node.Content = append(node.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "container_name"},
		&yaml.Node{Kind: yaml.ScalarNode, Value: "patched-by-keploy"})

	data, err := idc.MarshalCompose(&compose)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]interface{}
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatalf("generated compose does not load: %v\n%s", err, data)
	}
	svcs, _ := out["services"].(map[string]interface{})
	db, _ := svcs["db"].(map[string]interface{})
	if _, leaked := db["container_name"]; leaked {
		t.Errorf("the unrelated db service inherited keploy's edit:\n%s", data)
	}
	if db["image"] != "postgres:16" {
		t.Errorf("db was altered: %v", db)
	}
}

// TestAddKeployAgent_RefusesADuplicate pins the guard for a user compose that
// already defines a service named keploy-agent. Appending beside it produced a
// generated file that does not parse at all — `mapping key "keploy-agent"
// already defined` — which reads as a keploy bug rather than a name collision.
func TestAddKeployAgent_RefusesADuplicate(t *testing.T) {
	idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
	const in = `services:
  keploy-agent:
    image: mine/agent
  app:
    image: alpine:3.21
`
	var compose Compose
	if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	err := idc.AddKeployAgentToCompose(&compose, buildAgentOpts())
	if err == nil {
		data, _ := idc.MarshalCompose(&compose)
		var probe map[string]interface{}
		if e := yaml.Unmarshal(data, &probe); e != nil {
			t.Fatalf("a second keploy-agent was appended and the generated file no "+
				"longer parses (%v):\n%s", e, data)
		}
		t.Fatalf("expected a clear error for the name collision, got none")
	}
	if !strings.Contains(err.Error(), "keploy-agent") {
		t.Errorf("error does not name the colliding service: %v", err)
	}
}

// TestServiceAlias_PortsReadThroughAnAliasedSection pins the section-level
// resolution in findContainerInServices specifically.
//
// The per-service resolution inside the loop covers `services: {app: *app}`,
// where the section itself is a real mapping. It cannot help when the whole
// section is an alias: the loop never runs, the gate reports not-found, and the
// caller fails with "container 'X' not found in any of the compose files" on a
// file that plainly declares it.
func TestServiceAlias_PortsReadThroughAnAliasedSection(t *testing.T) {
	idc := &Impl{logger: zap.NewNop()}
	const in = `x-svcs: &svcs
  app:
    image: alpine:3.21
    ports:
      - "8080:8080"
services: *svcs
`
	var compose Compose
	if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	_, ports, name, found := idc.findContainerInServices(&compose, "app")
	if !found {
		t.Fatalf("the app was not found behind a wholly-aliased `services:` section; " +
			"the caller reports \"container not found\" on a file that declares it")
	}
	if name != "app" {
		t.Errorf("name = %q, want app", name)
	}
	if len(ports) == 0 {
		t.Errorf("no ports read, so keploy-agent would publish none on the app's "+
			"behalf and the app would be unreachable from the host (got %v)", ports)
	}
}

// TestServiceAlias_RealModifyComposeForAgent drives the ACTUAL pipeline rather
// than hand-simulating keploy's edits.
//
// The other tests append to the service node themselves, which cannot notice the
// simulation drifting from what the code does — and a mutation removing the
// loop-top resolve in modifyAppServiceForKeploy survived precisely because
// nothing here exercised that function. This runs ModifyComposeForAgent end to
// end on the shape that matters: two services aliasing one fragment.
func TestServiceAlias_RealModifyComposeForAgent(t *testing.T) {
	idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
	const in = `x-base: &base
  image: alpine:3.21
  environment:
    SSL_CERT_FILE: /etc/mine/ca.pem
  ports:
    - "8080:8080"
services:
  app: *base
  sidecar: *base
`
	var compose Compose
	if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// The gate the caller runs first; its ports feed opts.AppPorts.
	_, ports, _, found := idc.findContainerInServices(&compose, "app")
	if !found || len(ports) == 0 {
		t.Fatalf("gate did not resolve the aliased service: found=%v ports=%v", found, ports)
	}

	opts := buildAgentOpts()
	opts.AppPorts = ports
	if err := idc.ModifyComposeForAgent(&compose, opts, "app"); err != nil {
		t.Fatalf("ModifyComposeForAgent: %v", err)
	}

	data, err := idc.MarshalCompose(&compose)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]interface{}
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatalf("generated compose does not load: %v\n%s", err, data)
	}
	svcs, _ := out["services"].(map[string]interface{})

	// The app must actually be wired to the agent.
	app, _ := svcs["app"].(map[string]interface{})
	if app == nil {
		t.Fatalf("app service missing:\n%s", data)
	}
	if app["network_mode"] == nil && app["depends_on"] == nil {
		t.Errorf("the app was not wired to keploy-agent — every edit went into a "+
			"node the encoder never emits, so it would run uninstrumented:\n%s", data)
	}
	if _, ok := svcs["keploy-agent"]; !ok {
		t.Errorf("keploy-agent was not added:\n%s", data)
	}

	// The sibling must be exactly as the user wrote it.
	sidecar, _ := svcs["sidecar"].(map[string]interface{})
	if sidecar == nil {
		t.Fatalf("sidecar disappeared:\n%s", data)
	}
	if sidecar["network_mode"] != nil || sidecar["depends_on"] != nil {
		t.Errorf("the sibling was dragged into keploy's wiring:\n%s", data)
	}
	if env, _ := sidecar["environment"].(map[string]interface{}); env != nil {
		if env["SSL_CERT_FILE"] != "/etc/mine/ca.pem" {
			t.Errorf("the sibling's own SSL_CERT_FILE was replaced with keploy's CA "+
				"(%v); it is not in the agent's netns, so its outbound TLS would fail "+
				"verification silently", env["SSL_CERT_FILE"])
		}
	}
}

// TestServiceAlias_ModifyResolvesWhenAgentComesFirst pins the loop-top resolve
// inside modifyAppServiceForKeploy specifically.
//
// Through ModifyComposeForAgent that line looks redundant: the keploy-agent
// lookup runs first and, because the agent is appended LAST, walks the whole
// list and resolves every alias on the way. Delete the line and the output is
// byte-identical — which is exactly why a mutation removing it survived.
//
// It stops being redundant the moment the agent is NOT last: the lookup returns
// early, leaving later services unresolved, and the app's wiring goes into an
// alias node that is never emitted.
func TestServiceAlias_ModifyResolvesWhenAgentComesFirst(t *testing.T) {
	idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
	const in = `x-base: &base
  image: alpine:3.21
services:
  keploy-agent:
    image: keploy/agent
  app: *base
`
	var compose Compose
	if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := idc.modifyAppServiceForKeploy(&compose, "app"); err != nil {
		t.Fatalf("modifyAppServiceForKeploy: %v", err)
	}
	data, err := idc.MarshalCompose(&compose)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]interface{}
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatalf("generated compose does not load: %v\n%s", err, data)
	}
	svcs, _ := out["services"].(map[string]interface{})
	app, _ := svcs["app"].(map[string]interface{})
	if app == nil {
		t.Fatalf("app missing:\n%s", data)
	}
	if app["network_mode"] == nil && app["depends_on"] == nil && app["pid"] == nil {
		t.Errorf("the app got no keploy wiring — the service was still an alias when "+
			"it was modified, so every edit went into a node that is never "+
			"emitted:\n%s", data)
	}
}

// TestAddKeployAgent_RefusesAContainerNameCollision covers the collision the key
// check cannot see.
//
// findServiceNodeAndName matches on container_name as well as on the service
// key, so a user service carrying `container_name: keploy-agent` is handed back
// as keploy's OWN agent — keploy then moves the app's dns onto the user's
// service, and compose rejects the file with `container name "keploy-agent" is
// already in use`. Worse than a duplicate key, and invisible to a key-only guard.
func TestAddKeployAgent_RefusesAContainerNameCollision(t *testing.T) {
	idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
	const in = `services:
  myagent:
    image: mine/agent
    container_name: keploy-agent
  app:
    image: alpine:3.21
`
	var compose Compose
	if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	err := idc.AddKeployAgentToCompose(&compose, buildAgentOpts())
	if err == nil {
		t.Fatalf("expected a refusal: the user's service already uses container_name " +
			"keploy-agent, so keploy would adopt it as its own agent")
	}
	if !strings.Contains(err.Error(), "container_name") || !strings.Contains(err.Error(), "myagent") {
		t.Errorf("the error should name the colliding service and why: %v", err)
	}
}

// TestAddKeployAgent_RefusesAnUnusableServicesSection pins the refusal for a
// `services:` that still cannot hold entries after resolution — an alias to a
// scalar or sequence. Appending there returns nil while the agent service is
// simply absent from the generated file, which is the silent-failure shape this
// whole change exists to remove.
func TestAddKeployAgent_RefusesAnUnusableServicesSection(t *testing.T) {
	idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
	const in = `x-thing: &thing just-a-string
services: *thing
`
	var compose Compose
	if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	err := idc.AddKeployAgentToCompose(&compose, buildAgentOpts())
	if err == nil {
		data, _ := idc.MarshalCompose(&compose)
		t.Fatalf("expected a refusal; instead the agent service was silently "+
			"dropped:\n%s", data)
	}
	if !strings.Contains(err.Error(), "services") {
		t.Errorf("the error should name the section: %v", err)
	}
}

// TestSubKeyAlias_EnvAndVolumesResolve covers an alias one level BELOW the
// service — `environment: *appenv`, `volumes: *appvols` — which is at least as
// common an `x-*` idiom as aliasing a whole service.
//
// Unresolved, the two writers fail in opposite directions and neither says
// anything. addServiceEnvVar's type switch matches neither SequenceNode nor
// MappingNode for an AliasNode, so it falls straight through and keploy's CA and
// JAVA_TOOL_OPTIONS never appear — the app runs uninstrumented.
// addServiceListProperty appends into the alias, so the TLS-cert mount is
// dropped. Both leave a file docker compose accepts.
func TestSubKeyAlias_EnvAndVolumesResolve(t *testing.T) {
	idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
	const in = `x-env: &appenv
  SSL_CERT_FILE: /etc/mine/ca.pem
x-vols: &appvols
  - ./data:/data
services:
  app:
    image: alpine:3.21
    environment: *appenv
    volumes: *appvols
  sidecar:
    image: busybox
    environment: *appenv
    volumes: *appvols
`
	var compose Compose
	if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	node, _, err := idc.findServiceNodeAndName(&compose, "app")
	if err != nil {
		t.Fatalf("findServiceNodeAndName: %v", err)
	}
	idc.addServiceEnvVar(node, "SSL_CERT_FILE", "/tmp/keploy-tls/ca.crt")
	idc.addServiceListProperty(node, "volumes", "keploy-tls-certs:/tmp/keploy-tls")

	data, err := idc.MarshalCompose(&compose)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]interface{}
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatalf("generated compose does not load: %v\n%s", err, data)
	}
	svcs, _ := out["services"].(map[string]interface{})

	app, _ := svcs["app"].(map[string]interface{})
	appEnv, _ := app["environment"].(map[string]interface{})
	if appEnv == nil || appEnv["SSL_CERT_FILE"] != "/tmp/keploy-tls/ca.crt" {
		t.Errorf("keploy's env var was dropped into an alias node and never emitted; "+
			"the app would run without keploy's CA: %v\n%s", app["environment"], data)
	}
	appVols, _ := app["volumes"].([]interface{})
	var mounted bool
	for _, v := range appVols {
		if s, _ := v.(string); strings.Contains(s, "keploy-tls-certs") {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("keploy's TLS mount was dropped: %v\n%s", appVols, data)
	}

	// The sibling shares both fragments and must be untouched — it is not in the
	// agent's network namespace, so keploy's CA would break its outbound TLS.
	sidecar, _ := svcs["sidecar"].(map[string]interface{})
	sideEnv, _ := sidecar["environment"].(map[string]interface{})
	if sideEnv == nil || sideEnv["SSL_CERT_FILE"] != "/etc/mine/ca.pem" {
		t.Errorf("the sibling's own SSL_CERT_FILE was replaced with keploy's: %v", sideEnv)
	}
	sideVols, _ := sidecar["volumes"].([]interface{})
	for _, v := range sideVols {
		if s, _ := v.(string); strings.Contains(s, "keploy-tls-certs") {
			t.Errorf("the sibling inherited keploy's TLS mount: %v", sideVols)
		}
	}
	// And the user's fragments themselves.
	frag, _ := out["x-env"].(map[string]interface{})
	if frag == nil || frag["SSL_CERT_FILE"] != "/etc/mine/ca.pem" {
		t.Errorf("the user's own env fragment was rewritten: %v", frag)
	}
	if vf, _ := out["x-vols"].([]interface{}); len(vf) != 1 {
		t.Errorf("the user's own volumes fragment gained entries: %v", vf)
	}
}

// TestSubKeyAlias_SequenceStyleEnvResolves pins the other legal env shape: a
// sequence of KEY=VALUE strings behind an alias.
func TestSubKeyAlias_SequenceStyleEnvResolves(t *testing.T) {
	idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
	const in = `x-env: &appenv
  - APP_ENV=prod
services:
  app:
    image: alpine:3.21
    environment: *appenv
`
	var compose Compose
	if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	node, _, err := idc.findServiceNodeAndName(&compose, "app")
	if err != nil {
		t.Fatalf("findServiceNodeAndName: %v", err)
	}
	idc.addServiceEnvVar(node, "SSL_CERT_FILE", "/tmp/keploy-tls/ca.crt")

	data, err := idc.MarshalCompose(&compose)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]interface{}
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatalf("reload: %v\n%s", err, data)
	}
	svcs, _ := out["services"].(map[string]interface{})
	app, _ := svcs["app"].(map[string]interface{})
	env, _ := app["environment"].([]interface{})
	var got, kept bool
	for _, e := range env {
		s, _ := e.(string)
		if strings.HasPrefix(s, "SSL_CERT_FILE=") {
			got = true
		}
		if s == "APP_ENV=prod" {
			kept = true
		}
	}
	if !got {
		t.Errorf("keploy's env var was dropped from a sequence-style aliased "+
			"environment: %v\n%s", env, data)
	}
	if !kept {
		t.Errorf("the user's own env var was lost: %v", env)
	}
}

// TestServiceAlias_AliasedContainerNameIsWired is the regression test for the
// loudest-silent failure in this family: keploy records nothing at all.
//
// `container_name: *appname` is legal compose, and findServiceNodeAndName
// already matched it — but modifyAppServiceForKeploy's own scan required the
// value to be a ScalarNode, so an alias missed and the entire wiring block was
// skipped. No `pid:`, no `network_mode:`, no `depends_on:`, no CA variables: the
// app runs completely uninstrumented and the recording comes back empty, with
// nothing in the logs pointing at the compose file.
//
// The same scan stepped by ONE rather than by two, so it also matched
// "container_name" sitting in VALUE position — wiring the wrong service. Both
// shapes are in the table below.
func TestServiceAlias_AliasedContainerNameIsWired(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"alias", `x-name: &appname myapp
x-other: &othername notmyapp
services:
  decoy:
    image: alpine:3.21
    container_name: *othername
  svc:
    image: alpine:3.21
    container_name: *appname
    ports:
      - "8080:8080"
`},
		// A companion that already passed before the fix, kept so the two shapes
		// stay together. It does NOT pin the flatten ordering: serviceKeyThroughMerge
		// reads through `<<` itself, so this passes with the flatten removed.
		{"merge key", `x-naming: &naming
  container_name: myapp
services:
  svc:
    image: alpine:3.21
    <<: *naming
    ports:
      - "8080:8080"
`},
		// The one combination neither this file nor compose_merge_key_test.go
		// exercised: inherited through `<<` AND aliased on the far side.
		{"merge key naming an aliased container_name", `x-name: &appname myapp
x-naming: &naming
  container_name: *appname
services:
  svc:
    image: alpine:3.21
    <<: *naming
    ports:
      - "8080:8080"
`},
		// The OTHER bug the same two lines fix. The old scan stepped by ONE, so
		// it matched the literal string "container_name" in VALUE position when
		// the next node happened to be the app's name — and keploy then wired
		// this service and left the real app untouched.
		{"container_name in value position", `services:
  decoy:
    image: alpine:3.21
    command: container_name
    myapp: whatever
  svc:
    image: alpine:3.21
    container_name: myapp
    ports:
      - "8080:8080"
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
			var compose Compose
			if err := yaml.Unmarshal([]byte(tc.in), &compose); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			_, ports, _, found := idc.findContainerInServices(&compose, "myapp")
			if !found {
				t.Fatalf("the gate did not find the service")
			}
			opts := buildAgentOpts()
			opts.AppPorts = ports
			if err := idc.ModifyComposeForAgent(&compose, opts, "myapp"); err != nil {
				t.Fatalf("ModifyComposeForAgent: %v", err)
			}
			data, err := idc.MarshalCompose(&compose)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var out map[string]interface{}
			if err := yaml.Unmarshal(data, &out); err != nil {
				t.Fatalf("generated compose does not load: %v\n%s", err, data)
			}
			svcs, _ := out["services"].(map[string]interface{})
			svc, _ := svcs["svc"].(map[string]interface{})
			if svc == nil {
				t.Fatalf("service missing: %v", svcs)
			}
			if svc["network_mode"] != "service:keploy-agent" {
				t.Errorf("network_mode=%v: the app never joins the agent's netns, so "+
					"nothing is intercepted and the recording is empty", svc["network_mode"])
			}
			if svc["pid"] != "service:keploy-agent" {
				t.Errorf("pid=%v", svc["pid"])
			}
			if svc["depends_on"] == nil {
				t.Errorf("the app does not wait for keploy-agent to be healthy")
			}
			if svc["environment"] == nil {
				t.Errorf("keploy's CA variables were not added")
			}
			// A service that merely HAS a container_name must not be adopted:
			// without comparing the value, keploy wires whichever service it
			// reaches first and the real app is left alone.
			if decoy, _ := svcs["decoy"].(map[string]interface{}); decoy != nil {
				if decoy["network_mode"] != nil || decoy["pid"] != nil {
					t.Errorf("keploy wired the wrong service: %v", decoy)
				}
			}
		})
	}
}

// TestAddKeployAgent_RefusesACollisionReachedThroughAnAlias covers the shape
// that walked straight past the guard.
//
// `services: {x: *frag}` is an AliasNode, whose Content is nil — and the scan
// used to sit inside a loop over that Content, so it never executed and the
// check was skipped.
//
// What that costs is NOT a rejected file. "keploy-agent" is keploy's internal
// service KEY; the agent's actual container_name is a randomised
// `keploy-v3-<rand>`, so docker sees no clash and accepts the output. The damage
// is silent: findServiceNodeAndName("keploy-agent") matches on container_name
// and reaches the user's service first, so the app's `dns` is MOVED onto that
// unrelated service — overwriting its own — while the agent that owns the
// network namespace gets none. See TestAgentCollision_TheDamageItPrevents.
//
// These subtests call AddKeployAgentToCompose directly, so nothing has resolved
// anything and either ordering reproduces the bug. Ordering is load-bearing only
// through the full pipeline, which the pipeline subtest below covers.
func TestAddKeployAgent_RefusesACollisionReachedThroughAnAlias(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"fragment claims the name", `x-frag: &frag
  image: alpine:3.21
  container_name: keploy-agent
services:
  app:
    image: alpine:3.21
  x: *frag
`},
		{"fragment inherits the name through a merge key", `x-naming: &naming
  container_name: keploy-agent
x-frag: &frag
  image: alpine:3.21
  <<: *naming
services:
  app:
    image: alpine:3.21
  x: *frag
`},
		// Not a regression test for THIS fix — the service is a real mapping, so
		// the old loop ran and the value-side resolve from the previous commit
		// already handled it. Kept for guard-path coverage of that mechanism.
		{"container_name is itself an alias", `x-name: &agentname keploy-agent
services:
  app:
    image: alpine:3.21
  x:
    image: alpine:3.21
    container_name: *agentname
`},
		// The services MAPPING itself inherits the colliding service through a
		// merge key, so a scan over its own Content sees only the key `<<`.
		{"service inherited through a services-level merge key", `x-extra: &extra
  legacy:
    image: alpine:3.21
    container_name: keploy-agent
services:
  <<: *extra
  app:
    image: alpine:3.21
`},
		// Same, colliding on the service KEY rather than container_name. Missing
		// it is worse: keploy appends its own explicit `keploy-agent:` key, which
		// wins over the merge, so the user's service silently disappears.
		{"service KEY inherited through a services-level merge key", `x-extra: &extra
  keploy-agent:
    image: alpine:3.21
services:
  <<: *extra
  app:
    image: alpine:3.21
`},
		// The service key is itself an alias, so its own Value is the anchor name.
		{"service key is an alias", `x-k: &k keploy-agent
services:
  app:
    image: alpine:3.21
  *k :
    image: alpine:3.21
`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
			var compose Compose
			if err := yaml.Unmarshal([]byte(tc.in), &compose); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			err := idc.AddKeployAgentToCompose(&compose, buildAgentOpts())
			if err == nil {
				data, _ := idc.MarshalCompose(&compose)
				t.Fatalf("the collision was not refused; keploy added a second "+
					"service claiming container_name \"keploy-agent\" and docker "+
					"will reject the file:\n%s", data)
			}
			// Either guard is a correct refusal here; they report different
			// collisions (service key vs container_name) and word it differently.
			// What matters is that the message names the clashing name and says
			// what to do, rather than leaving the user with keploy's silent
			// dns mis-wiring.
			if !strings.Contains(err.Error(), "keploy-agent") ||
				!strings.Contains(err.Error(), "rename") {
				t.Errorf("the error does not name the problem: %v", err)
			}
		})
	}
}

// TestAgentCollision_TheDamageItPrevents pins what the guard is actually for,
// because the obvious answer is wrong.
//
// "keploy-agent" is keploy's internal service KEY. The agent's container_name is
// opts.KeployContainer, a randomised `keploy-v3-<rand>` on every docker run — so
// a user service named keploy-agent produces NO container-name clash and docker
// accepts the generated file without complaint.
//
// The damage is in keploy's own lookup. findServiceNodeAndName("keploy-agent")
// matches on container_name too and reaches the user's service first, so the
// app's `dns` is MOVED onto an unrelated service — overwriting the dns that
// service declared for itself — while the agent that owns the network namespace
// gets none. A recording made this way is quietly wrong rather than absent.
//
// Driven through the full pipeline, which is also the only place the ordering
// matters: the lookups that resolve service aliases in place stop as soon as
// they find the app, so a colliding service declared AFTER it is still an
// unexpanded alias when the guard runs.
func TestAgentCollision_TheDamageItPrevents(t *testing.T) {
	const in = `x-frag: &frag
  image: alpine:3.21
  container_name: keploy-agent
  dns:
    - 1.1.1.1
services:
  app:
    image: alpine:3.21
    dns:
      - 8.8.8.8
  x: *frag
`
	idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
	var compose Compose
	if err := yaml.Unmarshal([]byte(in), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Production-shaped: the agent's container_name is randomised, so there is
	// no docker-level clash to save us.
	opts := buildAgentOpts()
	opts.KeployContainer = "keploy-v3-abc123"

	// The gate runs first in production and resolves aliases as it goes — but it
	// stops at the app, so `x` is untouched when the guard runs.
	_, ports, _, _ := idc.findContainerInServices(&compose, "app")
	opts.AppPorts = ports

	err := idc.ModifyComposeForAgent(&compose, opts, "app")
	if err == nil {
		data, _ := idc.MarshalCompose(&compose)
		t.Fatalf("the collision was accepted; the app's dns is now on an unrelated "+
			"service and the agent has none:\n%s", data)
	}
	if !strings.Contains(err.Error(), "keploy-agent") {
		t.Errorf("the error does not name the collision: %v", err)
	}
}

// TestAddKeployAgent_ExplicitServiceOverridesAMergedCollision is the guard
// against the guard over-firing. YAML merge precedence gives an explicitly
// declared key priority over an inherited one, so a `legacy:` service written
// out in full replaces the `legacy:` the fragment supplies — collision and all.
// Refusing here would block a file that is perfectly fine.
func TestAddKeployAgent_ExplicitServiceOverridesAMergedCollision(t *testing.T) {
	idc := &Impl{logger: zap.NewNop(), conf: &config.Config{}}
	var compose Compose
	if err := yaml.Unmarshal([]byte(`x-extra: &extra
  legacy:
    image: alpine:3.21
    container_name: keploy-agent
services:
  <<: *extra
  legacy:
    image: alpine:3.21
    container_name: something-else
  app:
    image: alpine:3.21
`), &compose); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := idc.AddKeployAgentToCompose(&compose, buildAgentOpts()); err != nil {
		t.Errorf("refused a file whose explicit service overrides the merged "+
			"collision: %v", err)
	}
}
