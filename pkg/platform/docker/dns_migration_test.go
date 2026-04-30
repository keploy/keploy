package docker

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// buildServiceKeyValueNodes returns a (key, value) pair suitable for appending
// into a service MappingNode's Content.
func buildServiceKeyValueNodes(key string, value *yaml.Node) []*yaml.Node {
	return []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: key},
		value,
	}
}

// buildSequenceNode builds a sequence of scalar strings.
func buildSequenceNode(values ...string) *yaml.Node {
	content := make([]*yaml.Node, 0, len(values))
	for _, v := range values {
		content = append(content, &yaml.Node{Kind: yaml.ScalarNode, Value: v})
	}
	return &yaml.Node{Kind: yaml.SequenceNode, Content: content}
}

// buildMappingNode builds a mapping node from key/value string pairs.
func buildMappingNode(kvs ...string) *yaml.Node {
	content := make([]*yaml.Node, 0, len(kvs))
	for i := 0; i+1 < len(kvs); i += 2 {
		content = append(content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: kvs[i]},
			&yaml.Node{Kind: yaml.ScalarNode, Value: kvs[i+1]},
		)
	}
	return &yaml.Node{Kind: yaml.MappingNode, Content: content}
}

// buildComposeForDNSMigration returns a Compose with a keploy-agent service
// and an app service that carries the given DNS-related nodes.
func buildComposeForDNSMigration(appName string, dnsNodes map[string]*yaml.Node) *Compose {
	appContent := []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: "image"},
		{Kind: yaml.ScalarNode, Value: "app:latest"},
	}
	// Preserve a deterministic key order for test assertions.
	for _, key := range []string{"dns", "dns_search", "dns_opt"} {
		if node, ok := dnsNodes[key]; ok {
			appContent = append(appContent, buildServiceKeyValueNodes(key, node)...)
		}
	}

	appService := &yaml.Node{Kind: yaml.MappingNode, Content: appContent}
	keployAgentService := &yaml.Node{
		Kind:    yaml.MappingNode,
		Content: []*yaml.Node{},
	}

	compose := &Compose{}
	compose.Services = yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "keploy-agent"},
			keployAgentService,
			{Kind: yaml.ScalarNode, Value: appName},
			appService,
		},
	}
	return compose
}

// findServiceByName returns the MappingNode value for the given service name
// under compose.Services, or nil if not found.
func findServiceByName(compose *Compose, name string) *yaml.Node {
	for i := 0; i+1 < len(compose.Services.Content); i += 2 {
		if compose.Services.Content[i].Value == name {
			return compose.Services.Content[i+1]
		}
	}
	return nil
}

// mappingValueByKey looks up a property in a service mapping by key.
func mappingValueByKey(node *yaml.Node, key string) *yaml.Node {
	if node == nil {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

func TestModifyAppServiceForKeploy_MovesSequenceDNSNodes(t *testing.T) {
	dns := buildSequenceNode("8.8.8.8", "1.1.1.1")
	dnsSearch := buildSequenceNode("example.com", "corp.local")
	dnsOpt := buildSequenceNode("ndots:2", "timeout:3")

	compose := buildComposeForDNSMigration("app", map[string]*yaml.Node{
		"dns":        dns,
		"dns_search": dnsSearch,
		"dns_opt":    dnsOpt,
	})

	if err := newImpl().modifyAppServiceForKeploy(compose, "app"); err != nil {
		t.Fatalf("modifyAppServiceForKeploy returned error: %v", err)
	}

	appSvc := findServiceByName(compose, "app")
	if appSvc == nil {
		t.Fatalf("app service disappeared from compose")
	}
	for _, key := range []string{"dns", "dns_search", "dns_opt"} {
		if mappingValueByKey(appSvc, key) != nil {
			t.Fatalf("expected %q removed from app service", key)
		}
	}

	agentSvc := findServiceByName(compose, "keploy-agent")
	if agentSvc == nil {
		t.Fatalf("keploy-agent service disappeared from compose")
	}

	cases := []struct {
		key    string
		values []string
	}{
		{"dns", []string{"8.8.8.8", "1.1.1.1"}},
		{"dns_search", []string{"example.com", "corp.local"}},
		{"dns_opt", []string{"ndots:2", "timeout:3"}},
	}
	for _, c := range cases {
		got := mappingValueByKey(agentSvc, c.key)
		if got == nil {
			t.Fatalf("expected %q on keploy-agent", c.key)
		}
		if got.Kind != yaml.SequenceNode {
			t.Fatalf("expected %q to remain a SequenceNode, got kind %v", c.key, got.Kind)
		}
		if len(got.Content) != len(c.values) {
			t.Fatalf("expected %d values for %q, got %d", len(c.values), c.key, len(got.Content))
		}
		for i, v := range c.values {
			if got.Content[i].Value != v {
				t.Fatalf("%q[%d] = %q, want %q", c.key, i, got.Content[i].Value, v)
			}
		}
	}
}

func TestModifyAppServiceForKeploy_MovesScalarAndMappingDNSNodes(t *testing.T) {
	// dns_search can be a scalar in some compose files; dns_opt can be a mapping.
	dnsScalar := &yaml.Node{Kind: yaml.ScalarNode, Value: "8.8.8.8"}
	dnsSearchScalar := &yaml.Node{Kind: yaml.ScalarNode, Value: "example.com"}
	dnsOptMapping := buildMappingNode("ndots", "2", "timeout", "3")

	compose := buildComposeForDNSMigration("app", map[string]*yaml.Node{
		"dns":        dnsScalar,
		"dns_search": dnsSearchScalar,
		"dns_opt":    dnsOptMapping,
	})

	if err := newImpl().modifyAppServiceForKeploy(compose, "app"); err != nil {
		t.Fatalf("modifyAppServiceForKeploy returned error: %v", err)
	}

	agentSvc := findServiceByName(compose, "keploy-agent")
	if agentSvc == nil {
		t.Fatalf("keploy-agent service disappeared")
	}

	if got := mappingValueByKey(agentSvc, "dns"); got == nil || got.Kind != yaml.ScalarNode || got.Value != "8.8.8.8" {
		t.Fatalf("dns scalar not preserved on keploy-agent: %#v", got)
	}
	if got := mappingValueByKey(agentSvc, "dns_search"); got == nil || got.Kind != yaml.ScalarNode || got.Value != "example.com" {
		t.Fatalf("dns_search scalar not preserved on keploy-agent: %#v", got)
	}
	got := mappingValueByKey(agentSvc, "dns_opt")
	if got == nil || got.Kind != yaml.MappingNode {
		t.Fatalf("dns_opt mapping not preserved on keploy-agent: %#v", got)
	}
	if len(got.Content) != 4 {
		t.Fatalf("dns_opt mapping has %d children, want 4", len(got.Content))
	}
}

func TestModifyAppServiceForKeploy_SkipsMigrationWhenNoDNSSettings(t *testing.T) {
	compose := buildComposeForDNSMigration("app", nil)

	if err := newImpl().modifyAppServiceForKeploy(compose, "app"); err != nil {
		t.Fatalf("modifyAppServiceForKeploy returned error: %v", err)
	}

	agentSvc := findServiceByName(compose, "keploy-agent")
	if agentSvc == nil {
		t.Fatalf("keploy-agent service disappeared")
	}
	for _, key := range []string{"dns", "dns_search", "dns_opt"} {
		if mappingValueByKey(agentSvc, key) != nil {
			t.Fatalf("unexpected %q on keploy-agent when app had none", key)
		}
	}
}

func TestModifyAppServiceForKeploy_CloneDoesNotAliasOriginalNode(t *testing.T) {
	dns := buildSequenceNode("8.8.8.8")
	compose := buildComposeForDNSMigration("app", map[string]*yaml.Node{"dns": dns})

	if err := newImpl().modifyAppServiceForKeploy(compose, "app"); err != nil {
		t.Fatalf("modifyAppServiceForKeploy returned error: %v", err)
	}

	agentSvc := findServiceByName(compose, "keploy-agent")
	got := mappingValueByKey(agentSvc, "dns")
	if got == nil {
		t.Fatalf("dns missing on keploy-agent")
	}
	// Mutating the original node must not affect the clone on keploy-agent.
	dns.Content[0].Value = "MUTATED"
	if got.Content[0].Value == "MUTATED" {
		t.Fatalf("keploy-agent dns clone aliases original; expected deep clone")
	}
}
