package docker

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// envSeqValue returns the value of KEY in a service's list-form `environment`
// ("KEY=value"), or "" if absent.
func envSeqValue(serviceNode *yaml.Node, key string) string {
	env := mappingValueByKey(serviceNode, "environment")
	if env == nil || env.Kind != yaml.SequenceNode {
		return ""
	}
	prefix := key + "="
	for _, n := range env.Content {
		if n.Kind == yaml.ScalarNode && strings.HasPrefix(n.Value, prefix) {
			return strings.TrimPrefix(n.Value, prefix)
		}
	}
	return ""
}

// The per-service ComposeServiceHook contract: modifyAppServiceForKeploy must
// invoke the hook for the RECORDED APP service (not only keploy-agent), and it
// must do so AFTER it has set the app's JAVA_TOOL_OPTIONS truststore — so a
// downstream hook (the enterprise low-latency hook) can append the deterministic
// JVM -javaagent to an already-populated JAVA_TOOL_OPTIONS. This is what replaces
// jattach for compose Java TLS capture.
func TestModifyAppServiceForKeploy_InvokesHookForAppServiceAfterTrustStore(t *testing.T) {
	var seenNames []string
	var appNode *yaml.Node
	var jtoAtHookTime string

	prev := ComposeServiceHook
	t.Cleanup(func() { ComposeServiceHook = prev })
	ComposeServiceHook = func(serviceName string, serviceNode *yaml.Node) {
		seenNames = append(seenNames, serviceName)
		if serviceName == "app" {
			appNode = serviceNode
			jtoAtHookTime = envSeqValue(serviceNode, "JAVA_TOOL_OPTIONS")
		}
	}

	compose := buildComposeForDNSMigration("app", nil)
	if err := newImpl().modifyAppServiceForKeploy(compose, "app"); err != nil {
		t.Fatalf("modifyAppServiceForKeploy returned error: %v", err)
	}

	// The hook fired for the app service exactly once.
	appHookCalls := 0
	for _, n := range seenNames {
		if n == "app" {
			appHookCalls++
		}
	}
	if appHookCalls != 1 {
		t.Fatalf("expected the hook called once for the app service, saw %v", seenNames)
	}
	// It received the actual app service node (so it can mutate it in place).
	if appNode != findServiceByName(compose, "app") {
		t.Fatalf("hook received a node that is not the app service node")
	}
	// Ordering: the truststore JAVA_TOOL_OPTIONS was already set when the hook ran,
	// so the enterprise hook's -javaagent append composes onto it rather than
	// clobbering it.
	if !strings.Contains(jtoAtHookTime, "trustStore") {
		t.Fatalf("hook must run AFTER the truststore is set; JAVA_TOOL_OPTIONS at hook time = %q", jtoAtHookTime)
	}
}

// The identifier the hook receives for the app service must be the
// --container-name value the downstream matches on, NOT the compose map key.
// When a service is selected by its `container_name` (different from its key),
// passing the key would make the enterprise app hook silently miss and Java TLS
// go uncaptured.
func TestModifyAppServiceForKeploy_HookIdentifierIsContainerNameNotKey(t *testing.T) {
	var appIdentifier string
	prev := ComposeServiceHook
	t.Cleanup(func() { ComposeServiceHook = prev })
	ComposeServiceHook = func(serviceIdentifier string, _ *yaml.Node) {
		appIdentifier = serviceIdentifier // only the app hook fires in modifyAppServiceForKeploy
	}

	// Service KEY is "web", but container_name is "my-app"; recorded with
	// --container-name my-app.
	webSvc := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "image"},
		{Kind: yaml.ScalarNode, Value: "app:latest"},
		{Kind: yaml.ScalarNode, Value: "container_name"},
		{Kind: yaml.ScalarNode, Value: "my-app"},
	}}
	compose := &Compose{}
	compose.Services = yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "keploy-agent"},
		{Kind: yaml.MappingNode, Content: []*yaml.Node{}},
		{Kind: yaml.ScalarNode, Value: "web"},
		webSvc,
	}}

	if err := newImpl().modifyAppServiceForKeploy(compose, "my-app"); err != nil {
		t.Fatalf("modifyAppServiceForKeploy returned error: %v", err)
	}
	if appIdentifier != "my-app" {
		t.Fatalf("hook received %q, want the --container-name value \"my-app\" (not the service key \"web\")", appIdentifier)
	}
}

// A nil ComposeServiceHook must be a no-op (OSS-only builds have no enterprise
// hook registered) — modifyAppServiceForKeploy must still succeed.
func TestModifyAppServiceForKeploy_NilHookIsNoOp(t *testing.T) {
	prev := ComposeServiceHook
	t.Cleanup(func() { ComposeServiceHook = prev })
	ComposeServiceHook = nil

	compose := buildComposeForDNSMigration("app", nil)
	if err := newImpl().modifyAppServiceForKeploy(compose, "app"); err != nil {
		t.Fatalf("modifyAppServiceForKeploy with nil hook returned error: %v", err)
	}
	// The app still gets its truststore JAVA_TOOL_OPTIONS from OSS.
	appSvc := findServiceByName(compose, "app")
	if !strings.Contains(envSeqValue(appSvc, "JAVA_TOOL_OPTIONS"), "trustStore") {
		t.Fatalf("app service missing truststore JAVA_TOOL_OPTIONS")
	}
}
