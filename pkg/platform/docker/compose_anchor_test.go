package docker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// composeWithAnchor is the shape the Compose spec endorses for reuse: a shared
// fragment under an `x-*` extension key, pulled into services with a merge
// alias. Docker accepts it; keploy has to round-trip it without breaking it.
const composeWithAnchor = `x-mysql-common: &mysql-common
  command: --skip-log-bin
  healthcheck:
    test: [ "CMD", "mysqladmin", "ping", "-h", "127.0.0.1", "--protocol=tcp" ]
    start_period: 60s
services:
  mysql-users:
    image: mysql:8.0
    <<: *mysql-common
  mysql-orders:
    image: mysql:8.0
    <<: *mysql-common
`

// TestComposeRoundTrip_PreservesAnchorDefinition is the regression test for the
// generated compose failing to load with:
//
//	go-yaml load error in composer at L15.C21: unknown anchor 'mysql-common' referenced
//
// Compose names only six top-level keys, so `x-mysql-common` was dropped on
// read. Services is a yaml.Node and kept `<<: *mysql-common` verbatim, so the
// file keploy wrote referenced an anchor that was no longer defined and the
// user's app never started.
//
// Asserting the round trip RELOADS is the point: checking only that the anchor
// text survives would still pass if it were emitted after the alias that uses
// it, which YAML rejects.
func TestComposeRoundTrip_PreservesAnchorDefinition(t *testing.T) {
	idc := &Impl{logger: zap.NewNop()}

	dir := t.TempDir()
	src := filepath.Join(dir, "docker-compose.yaml")
	if err := os.WriteFile(src, []byte(composeWithAnchor), 0644); err != nil {
		t.Fatalf("write source compose: %v", err)
	}

	compose, err := idc.ReadComposeFile(src)
	if err != nil {
		t.Fatalf("ReadComposeFile: %v", err)
	}

	out := filepath.Join(dir, "docker-compose-tmp.yaml")
	if err := idc.WriteComposeFile(compose, out); err != nil {
		t.Fatalf("WriteComposeFile: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read generated compose: %v", err)
	}

	// The generated file is what `docker compose -f ... up` parses next.
	var reloaded map[string]interface{}
	if err := yaml.Unmarshal(data, &reloaded); err != nil {
		t.Fatalf("generated compose does not load: %v\n--- generated ---\n%s", err, data)
	}

	services, ok := reloaded["services"].(map[string]interface{})
	if !ok {
		t.Fatalf("generated compose has no services mapping:\n%s", data)
	}
	for _, name := range []string{"mysql-users", "mysql-orders"} {
		svc, ok := services[name].(map[string]interface{})
		if !ok {
			t.Fatalf("service %q missing from generated compose:\n%s", name, data)
		}
		// The merge must still resolve, or the service silently loses the
		// healthcheck that gates everything depending on it.
		if got := svc["command"]; got != "--skip-log-bin" {
			t.Errorf("service %q command = %v, want %q — merge alias did not resolve",
				name, got, "--skip-log-bin")
		}
		if _, ok := svc["healthcheck"].(map[string]interface{}); !ok {
			t.Errorf("service %q lost its healthcheck through the round trip", name)
		}
	}

	// The anchor has to be DEFINED, not merely referenced.
	if !strings.Contains(string(data), "&mysql-common") {
		t.Errorf("anchor definition missing from generated compose:\n%s", data)
	}
}

// TestComposeRoundTrip_KeepsUnknownTopLevelKeys pins the general case behind the
// anchor bug: keys the Compose struct does not name must survive, not just the
// one that happened to carry an anchor.
func TestComposeRoundTrip_KeepsUnknownTopLevelKeys(t *testing.T) {
	idc := &Impl{logger: zap.NewNop()}
	const in = `name: my-project
x-shared-config:
  retries: 3
services:
  app:
    image: alpine:3.21
`
	dir := t.TempDir()
	src := filepath.Join(dir, "docker-compose.yaml")
	if err := os.WriteFile(src, []byte(in), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	compose, err := idc.ReadComposeFile(src)
	if err != nil {
		t.Fatalf("ReadComposeFile: %v", err)
	}
	data, err := idc.MarshalCompose(compose)
	if err != nil {
		t.Fatalf("MarshalCompose: %v", err)
	}
	var reloaded map[string]interface{}
	if err := yaml.Unmarshal(data, &reloaded); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded["name"] != "my-project" {
		t.Errorf("top-level `name` lost: %v\n%s", reloaded["name"], data)
	}
	if _, ok := reloaded["x-shared-config"]; !ok {
		t.Errorf("top-level `x-shared-config` lost:\n%s", data)
	}
}

// TestComposeMarshal_ProgrammaticComposeStillWorks pins the fallback: a Compose
// built in code has no original document, and must still serialise.
func TestComposeMarshal_ProgrammaticComposeStillWorks(t *testing.T) {
	idc := &Impl{logger: zap.NewNop()}
	compose := &Compose{Version: "3.8"}
	compose.Services = yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "app"},
		{Kind: yaml.MappingNode, Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "image"},
			{Kind: yaml.ScalarNode, Value: "alpine:3.21"},
		}},
	}}
	data, err := idc.MarshalCompose(compose)
	if err != nil {
		t.Fatalf("MarshalCompose: %v", err)
	}
	var reloaded map[string]interface{}
	if err := yaml.Unmarshal(data, &reloaded); err != nil {
		t.Fatalf("reload: %v\n%s", err, data)
	}
	services, ok := reloaded["services"].(map[string]interface{})
	if !ok || services["app"] == nil {
		t.Fatalf("programmatic compose lost its service:\n%s", data)
	}
}

// TestComposeRoundTrip_ModifiedServicesAreWritten pins that preserving the
// original document does not come at the cost of DISCARDING the modifications.
//
// keploy's whole reason for rewriting the compose file is to inject its agent
// service and adjust the app service (ModifyComposeForAgent). Those edits land
// on compose.Services, which is decoded separately from the original mapping,
// so they only reach the generated file if that section is spliced back in.
// Without the splice every test above still passes while keploy silently runs
// the user's UNMODIFIED stack -- no agent, nothing recorded.
func TestComposeRoundTrip_ModifiedServicesAreWritten(t *testing.T) {
	idc := &Impl{logger: zap.NewNop()}

	dir := t.TempDir()
	src := filepath.Join(dir, "docker-compose.yaml")
	if err := os.WriteFile(src, []byte(composeWithAnchor), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	compose, err := idc.ReadComposeFile(src)
	if err != nil {
		t.Fatalf("ReadComposeFile: %v", err)
	}

	// Stand in for ModifyComposeForAgent: add a service, and edit an existing one.
	compose.Services.Content = append(compose.Services.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "keploy-agent"},
		&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "image"},
			{Kind: yaml.ScalarNode, Value: "keploy/agent:v3"},
		}})
	for i := 0; i+1 < len(compose.Services.Content); i += 2 {
		if compose.Services.Content[i].Value == "mysql-users" {
			svc := compose.Services.Content[i+1]
			svc.Content = append(svc.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: "container_name"},
				&yaml.Node{Kind: yaml.ScalarNode, Value: "patched-by-keploy"})
		}
	}

	data, err := idc.MarshalCompose(compose)
	if err != nil {
		t.Fatalf("MarshalCompose: %v", err)
	}
	var reloaded map[string]interface{}
	if err := yaml.Unmarshal(data, &reloaded); err != nil {
		t.Fatalf("generated compose does not load: %v\n%s", err, data)
	}
	services, _ := reloaded["services"].(map[string]interface{})
	if services == nil {
		t.Fatalf("no services in generated compose:\n%s", data)
	}
	agent, ok := services["keploy-agent"].(map[string]interface{})
	if !ok {
		t.Fatalf("injected keploy-agent service missing — the modified services "+
			"section was not written:\n%s", data)
	}
	if agent["image"] != "keploy/agent:v3" {
		t.Errorf("keploy-agent image = %v, want keploy/agent:v3", agent["image"])
	}
	appSvc, _ := services["mysql-users"].(map[string]interface{})
	if appSvc == nil || appSvc["container_name"] != "patched-by-keploy" {
		t.Errorf("edit to an existing service was not written: %v\n%s", appSvc, data)
	}
	// ...and the anchor must still resolve for the untouched service.
	if other, _ := services["mysql-orders"].(map[string]interface{}); other == nil ||
		other["command"] != "--skip-log-bin" {
		t.Errorf("merge alias stopped resolving after modification: %v", other)
	}
}

// TestComposeRoundTrip_AbsentSectionsStayAbsent pins the zero-node skip in
// composeDocument, which runs on EVERY compose file keploy rewrites.
//
// Compose names six top-level sections; most files declare two or three. The
// undeclared ones decode to zero yaml.Nodes, and splicing those in unconditionally
// writes `configs: null` / `secrets: null` into the generated file. Real
// `docker compose` rejects that outright ("configs must be a mapping"), so
// dropping the guard breaks every user whose compose omits a section -- which is
// nearly all of them.
func TestComposeRoundTrip_AbsentSectionsStayAbsent(t *testing.T) {
	idc := &Impl{logger: zap.NewNop()}
	const in = `services:
  app:
    image: alpine:3.21
networks:
  default:
    driver: bridge
`
	dir := t.TempDir()
	src := filepath.Join(dir, "docker-compose.yaml")
	if err := os.WriteFile(src, []byte(in), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	compose, err := idc.ReadComposeFile(src)
	if err != nil {
		t.Fatalf("ReadComposeFile: %v", err)
	}
	data, err := idc.MarshalCompose(compose)
	if err != nil {
		t.Fatalf("MarshalCompose: %v", err)
	}

	var reloaded map[string]interface{}
	if err := yaml.Unmarshal(data, &reloaded); err != nil {
		t.Fatalf("reload: %v\n%s", err, data)
	}
	for _, absent := range []string{"configs", "secrets", "volumes", "version"} {
		if v, present := reloaded[absent]; present {
			t.Errorf("section %q was absent from the source but appears as %v in the "+
				"generated file — docker compose rejects a null section:\n%s",
				absent, v, data)
		}
	}
	// The sections that WERE declared must survive.
	if _, ok := reloaded["services"].(map[string]interface{}); !ok {
		t.Errorf("services missing:\n%s", data)
	}
	if _, ok := reloaded["networks"].(map[string]interface{}); !ok {
		t.Errorf("networks missing:\n%s", data)
	}
}

// TestComposeRoundTrip_VersionIsWriteThrough pins that Version behaves like the
// five yaml.Node fields. It is a string, so it needs its own splice; without one
// it is the single named field that silently ignores writes.
func TestComposeRoundTrip_VersionIsWriteThrough(t *testing.T) {
	idc := &Impl{logger: zap.NewNop()}
	const in = `version: "3.8"
services:
  app:
    image: alpine:3.21
`
	dir := t.TempDir()
	src := filepath.Join(dir, "docker-compose.yaml")
	if err := os.WriteFile(src, []byte(in), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	compose, err := idc.ReadComposeFile(src)
	if err != nil {
		t.Fatalf("ReadComposeFile: %v", err)
	}
	compose.Version = "3.9"
	data, err := idc.MarshalCompose(compose)
	if err != nil {
		t.Fatalf("MarshalCompose: %v", err)
	}
	var reloaded map[string]interface{}
	if err := yaml.Unmarshal(data, &reloaded); err != nil {
		t.Fatalf("reload: %v\n%s", err, data)
	}
	if got := reloaded["version"]; got != "3.9" {
		t.Errorf("version = %v, want 3.9 — Version is not write-through:\n%s", got, data)
	}
}

// TestComposeMarshal_DirectYamlMarshalIsAlsoCorrect pins that the fix lives on
// the TYPE, not on WriteComposeFile/MarshalCompose. Callers outside this package
// marshal a *Compose directly (enterprise's --dump-compose does), and if only the
// two wrappers were fixed, the file keploy RUNS and the file it dumps for
// debugging would disagree precisely when this bug bites.
func TestComposeMarshal_DirectYamlMarshalIsAlsoCorrect(t *testing.T) {
	idc := &Impl{logger: zap.NewNop()}
	dir := t.TempDir()
	src := filepath.Join(dir, "docker-compose.yaml")
	if err := os.WriteFile(src, []byte(composeWithAnchor), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	compose, err := idc.ReadComposeFile(src)
	if err != nil {
		t.Fatalf("ReadComposeFile: %v", err)
	}

	direct, err := yaml.Marshal(compose)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	var reloaded map[string]interface{}
	if err := yaml.Unmarshal(direct, &reloaded); err != nil {
		t.Fatalf("a direct yaml.Marshal produced an unloadable document: %v\n%s", err, direct)
	}
	viaHelper, err := idc.MarshalCompose(compose)
	if err != nil {
		t.Fatalf("MarshalCompose: %v", err)
	}
	if string(direct) != string(viaHelper) {
		t.Errorf("yaml.Marshal and MarshalCompose disagree:\n--- direct ---\n%s\n--- helper ---\n%s",
			direct, viaHelper)
	}
}

// TestComposeRoundTrip_UntouchedVersionKeepsItsNode pins that making Version
// write-through did not cost fidelity on the far more common path where nobody
// writes it.
//
// Splicing unconditionally replaces `version:`'s node with a fresh scalar, which
// drops its line comment and re-quotes the value — so `version` would become the
// single key in the document that loses exactly what preserving the original
// mapping exists to keep, on every compose file that declares one.
func TestComposeRoundTrip_UntouchedVersionKeepsItsNode(t *testing.T) {
	idc := &Impl{logger: zap.NewNop()}
	const in = `version: '3.8' # pinned deliberately
services:
  app:
    image: alpine:3.21
`
	dir := t.TempDir()
	src := filepath.Join(dir, "docker-compose.yaml")
	if err := os.WriteFile(src, []byte(in), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	compose, err := idc.ReadComposeFile(src)
	if err != nil {
		t.Fatalf("ReadComposeFile: %v", err)
	}
	// Deliberately do NOT touch compose.Version.
	data, err := idc.MarshalCompose(compose)
	if err != nil {
		t.Fatalf("MarshalCompose: %v", err)
	}
	out := string(data)
	if !strings.Contains(out, "pinned deliberately") {
		t.Errorf("the comment on `version:` was destroyed by an unnecessary splice:\n%s", out)
	}
	if !strings.Contains(out, "'3.8'") {
		t.Errorf("the original scalar style of `version:` was rewritten:\n%s", out)
	}
}

// TestComposeMarshal_ValueReceiverAlsoCorrect pins that the fix is reachable
// through a VALUE, not only a pointer. A pointer-receiver MarshalYAML is absent
// from Compose's method set, so `yaml.Marshal(*compose)` would quietly fall back
// to encoding the struct and emit the unloadable document this exists to fix.
func TestComposeMarshal_ValueReceiverAlsoCorrect(t *testing.T) {
	idc := &Impl{logger: zap.NewNop()}
	dir := t.TempDir()
	src := filepath.Join(dir, "docker-compose.yaml")
	if err := os.WriteFile(src, []byte(composeWithAnchor), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	compose, err := idc.ReadComposeFile(src)
	if err != nil {
		t.Fatalf("ReadComposeFile: %v", err)
	}
	data, err := yaml.Marshal(*compose) // value, not pointer
	if err != nil {
		t.Fatalf("yaml.Marshal(value): %v", err)
	}
	var reloaded map[string]interface{}
	if err := yaml.Unmarshal(data, &reloaded); err != nil {
		t.Fatalf("marshalling a Compose VALUE produced an unloadable document: %v\n%s", err, data)
	}
	services, _ := reloaded["services"].(map[string]interface{})
	svc, _ := services["mysql-users"].(map[string]interface{})
	if svc == nil || svc["command"] != "--skip-log-bin" {
		t.Errorf("merge did not resolve via the value path: %v", svc)
	}
}
