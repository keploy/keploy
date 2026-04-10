package docker

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// newSeqServiceNode builds a service MappingNode with a sequence-style
// environment block: environment: ["K1=V1", "K2=V2", ...].
func newSeqServiceNode(envPairs ...string) *yaml.Node {
	envContent := make([]*yaml.Node, 0, len(envPairs))
	for _, p := range envPairs {
		envContent = append(envContent, &yaml.Node{Kind: yaml.ScalarNode, Value: p})
	}
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "environment"},
			{Kind: yaml.SequenceNode, Content: envContent},
		},
	}
}

// newMapServiceNode builds a service MappingNode with a mapping-style
// environment block: environment: { K1: V1, K2: V2, ... }.
func newMapServiceNode(kvs ...string) *yaml.Node {
	envContent := make([]*yaml.Node, 0, len(kvs))
	for i := 0; i < len(kvs)-1; i += 2 {
		envContent = append(envContent,
			&yaml.Node{Kind: yaml.ScalarNode, Value: kvs[i]},
			&yaml.Node{Kind: yaml.ScalarNode, Value: kvs[i+1]},
		)
	}
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "environment"},
			{Kind: yaml.MappingNode, Content: envContent},
		},
	}
}

// newEmptyServiceNode builds a service MappingNode with no environment block.
func newEmptyServiceNode() *yaml.Node {
	return &yaml.Node{
		Kind:    yaml.MappingNode,
		Content: []*yaml.Node{},
	}
}

// seqEnvValues returns all "KEY=VAL" entries from a sequence-style env node.
func seqEnvValues(svc *yaml.Node) []string {
	for i := 0; i < len(svc.Content); i += 2 {
		if svc.Content[i].Value == "environment" {
			node := svc.Content[i+1]
			out := make([]string, len(node.Content))
			for j, n := range node.Content {
				out[j] = n.Value
			}
			return out
		}
	}
	return nil
}

// mapEnvValue returns the value for a key in a mapping-style env node.
func mapEnvValue(svc *yaml.Node, key string) (string, bool) {
	for i := 0; i < len(svc.Content); i += 2 {
		if svc.Content[i].Value == "environment" {
			node := svc.Content[i+1]
			for j := 0; j < len(node.Content)-1; j += 2 {
				if node.Content[j].Value == key {
					return node.Content[j+1].Value, true
				}
			}
		}
	}
	return "", false
}

func newImpl() *Impl {
	return &Impl{}
}

// ---------- getOrCreateEnvNode ----------

func TestGetOrCreateEnvNode_ExistingEnv(t *testing.T) {
	svc := newSeqServiceNode("FOO=bar")
	idc := newImpl()
	env := idc.getOrCreateEnvNode(svc)
	if env.Kind != yaml.SequenceNode {
		t.Fatalf("expected SequenceNode, got %v", env.Kind)
	}
	if len(env.Content) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(env.Content))
	}
}

func TestGetOrCreateEnvNode_CreatesWhenMissing(t *testing.T) {
	svc := newEmptyServiceNode()
	idc := newImpl()
	env := idc.getOrCreateEnvNode(svc)
	if env.Kind != yaml.SequenceNode {
		t.Fatalf("expected new SequenceNode, got %v", env.Kind)
	}
	if len(env.Content) != 0 {
		t.Fatalf("expected empty env node, got %d entries", len(env.Content))
	}
	// Calling again should return the same node, not create another.
	env2 := idc.getOrCreateEnvNode(svc)
	if env2 != env {
		t.Fatal("expected same node pointer on second call")
	}
}

// ---------- addServiceEnvVar (sequence style) ----------

func TestAddServiceEnvVar_Seq_NewKey(t *testing.T) {
	svc := newSeqServiceNode("EXISTING=val")
	newImpl().addServiceEnvVar(svc, "NEW_KEY", "new_val")

	vals := seqEnvValues(svc)
	if len(vals) != 2 {
		t.Fatalf("expected 2 entries, got %d: %v", len(vals), vals)
	}
	if vals[1] != "NEW_KEY=new_val" {
		t.Fatalf("unexpected entry: %s", vals[1])
	}
}

func TestAddServiceEnvVar_Seq_UpsertExistingKey(t *testing.T) {
	svc := newSeqServiceNode("MY_KEY=old_val", "OTHER=keep")
	newImpl().addServiceEnvVar(svc, "MY_KEY", "new_val")

	vals := seqEnvValues(svc)
	if len(vals) != 2 {
		t.Fatalf("expected 2 entries (no duplicate), got %d: %v", len(vals), vals)
	}
	if vals[0] != "MY_KEY=new_val" {
		t.Fatalf("expected upsert, got: %s", vals[0])
	}
}

func TestAddServiceEnvVar_Seq_NoDuplicateOnRepeat(t *testing.T) {
	svc := newSeqServiceNode()
	idc := newImpl()
	idc.addServiceEnvVar(svc, "JAVA_TOOL_OPTIONS", "-javaagent:/agent.jar")
	idc.addServiceEnvVar(svc, "JAVA_TOOL_OPTIONS", "-javaagent:/agent.jar")

	vals := seqEnvValues(svc)
	if len(vals) != 1 {
		t.Fatalf("expected exactly 1 entry after double add, got %d: %v", len(vals), vals)
	}
}

// ---------- addServiceEnvVar (mapping style) ----------

func TestAddServiceEnvVar_Map_NewKey(t *testing.T) {
	svc := newMapServiceNode("EXISTING", "val")
	newImpl().addServiceEnvVar(svc, "NEW_KEY", "new_val")

	v, ok := mapEnvValue(svc, "NEW_KEY")
	if !ok {
		t.Fatal("NEW_KEY not found")
	}
	if v != "new_val" {
		t.Fatalf("unexpected value: %s", v)
	}
}

func TestAddServiceEnvVar_Map_UpsertExistingKey(t *testing.T) {
	svc := newMapServiceNode("MY_KEY", "old_val", "OTHER", "keep")
	newImpl().addServiceEnvVar(svc, "MY_KEY", "new_val")

	v, ok := mapEnvValue(svc, "MY_KEY")
	if !ok {
		t.Fatal("MY_KEY not found")
	}
	if v != "new_val" {
		t.Fatalf("expected upsert, got: %s", v)
	}
	// Ensure no duplicate keys.
	envNode := svc.Content[1]
	count := 0
	for i := 0; i < len(envNode.Content)-1; i += 2 {
		if envNode.Content[i].Value == "MY_KEY" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected 1 MY_KEY entry, got %d", count)
	}
}

// ---------- addServiceEnvVar (no existing env block) ----------

func TestAddServiceEnvVar_CreatesEnvBlock(t *testing.T) {
	svc := newEmptyServiceNode()
	newImpl().addServiceEnvVar(svc, "APP_PORT", "8080")

	vals := seqEnvValues(svc)
	if len(vals) != 1 || vals[0] != "APP_PORT=8080" {
		t.Fatalf("unexpected env after creation: %v", vals)
	}
}

// ---------- appendServiceEnvVar (sequence style) ----------

func TestAppendServiceEnvVar_Seq_AppendsToExisting(t *testing.T) {
	svc := newSeqServiceNode("JAVA_TOOL_OPTIONS=-Xmx512m")
	newImpl().appendServiceEnvVar(svc, "JAVA_TOOL_OPTIONS", "-javaagent:/agent.jar")

	vals := seqEnvValues(svc)
	if len(vals) != 1 {
		t.Fatalf("expected 1 entry, got %d: %v", len(vals), vals)
	}
	if vals[0] != "JAVA_TOOL_OPTIONS=-Xmx512m -javaagent:/agent.jar" {
		t.Fatalf("unexpected value: %s", vals[0])
	}
}

func TestAppendServiceEnvVar_Seq_SkipsDuplicateToken(t *testing.T) {
	svc := newSeqServiceNode("JAVA_TOOL_OPTIONS=-javaagent:/agent.jar")
	newImpl().appendServiceEnvVar(svc, "JAVA_TOOL_OPTIONS", "-javaagent:/agent.jar")

	vals := seqEnvValues(svc)
	if len(vals) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(vals))
	}
	// Should NOT have the agent flag twice.
	if strings.Count(vals[0], "-javaagent:/agent.jar") != 1 {
		t.Fatalf("agent flag duplicated: %s", vals[0])
	}
}

func TestAppendServiceEnvVar_Seq_AppendsOnlyMissingTokens(t *testing.T) {
	svc := newSeqServiceNode("JAVA_TOOL_OPTIONS=-Dflag1 -Dflag2")
	newImpl().appendServiceEnvVar(svc, "JAVA_TOOL_OPTIONS", "-Dflag2 -Dflag3")

	vals := seqEnvValues(svc)
	if len(vals) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(vals))
	}
	// -Dflag2 should appear only once, -Dflag3 should be appended.
	if strings.Count(vals[0], "-Dflag2") != 1 {
		t.Fatalf("-Dflag2 duplicated: %s", vals[0])
	}
	if !strings.Contains(vals[0], "-Dflag3") {
		t.Fatalf("-Dflag3 missing: %s", vals[0])
	}
}

func TestAppendServiceEnvVar_Seq_CreatesNewWhenMissing(t *testing.T) {
	svc := newSeqServiceNode("OTHER=val")
	newImpl().appendServiceEnvVar(svc, "JAVA_TOOL_OPTIONS", "-javaagent:/agent.jar")

	vals := seqEnvValues(svc)
	if len(vals) != 2 {
		t.Fatalf("expected 2 entries, got %d: %v", len(vals), vals)
	}
	if vals[1] != "JAVA_TOOL_OPTIONS=-javaagent:/agent.jar" {
		t.Fatalf("unexpected new entry: %s", vals[1])
	}
}

func TestAppendServiceEnvVar_Seq_NoSubstringFalsePositive(t *testing.T) {
	// "-Dfoo" should NOT be treated as already present when only "-Dfoobar" exists.
	svc := newSeqServiceNode("JAVA_TOOL_OPTIONS=-Dfoobar")
	newImpl().appendServiceEnvVar(svc, "JAVA_TOOL_OPTIONS", "-Dfoo")

	vals := seqEnvValues(svc)
	if !strings.Contains(vals[0], "-Dfoo") || !strings.Contains(vals[0], "-Dfoobar") {
		t.Fatalf("expected both -Dfoobar and -Dfoo: %s", vals[0])
	}
	if strings.Count(vals[0], "-Dfoo") != 2 {
		// "-Dfoobar" contains "-Dfoo" as a substring, but token match should treat them differently.
		// We expect: "JAVA_TOOL_OPTIONS=-Dfoobar -Dfoo"
		t.Fatalf("expected -Dfoo appended as separate token: %s", vals[0])
	}
}

// ---------- appendServiceEnvVar (mapping style) ----------

func TestAppendServiceEnvVar_Map_AppendsToExisting(t *testing.T) {
	svc := newMapServiceNode("JAVA_TOOL_OPTIONS", "-Xmx512m")
	newImpl().appendServiceEnvVar(svc, "JAVA_TOOL_OPTIONS", "-javaagent:/agent.jar")

	v, ok := mapEnvValue(svc, "JAVA_TOOL_OPTIONS")
	if !ok {
		t.Fatal("JAVA_TOOL_OPTIONS not found")
	}
	if v != "-Xmx512m -javaagent:/agent.jar" {
		t.Fatalf("unexpected value: %s", v)
	}
}

func TestAppendServiceEnvVar_Map_SkipsDuplicateToken(t *testing.T) {
	svc := newMapServiceNode("JAVA_TOOL_OPTIONS", "-javaagent:/agent.jar")
	newImpl().appendServiceEnvVar(svc, "JAVA_TOOL_OPTIONS", "-javaagent:/agent.jar")

	v, _ := mapEnvValue(svc, "JAVA_TOOL_OPTIONS")
	if strings.Count(v, "-javaagent:/agent.jar") != 1 {
		t.Fatalf("agent flag duplicated: %s", v)
	}
}

func TestAppendServiceEnvVar_Map_AppendsOnlyMissingTokens(t *testing.T) {
	svc := newMapServiceNode("JAVA_TOOL_OPTIONS", "-Dflag1 -Dflag2")
	newImpl().appendServiceEnvVar(svc, "JAVA_TOOL_OPTIONS", "-Dflag2 -Dflag3")

	v, _ := mapEnvValue(svc, "JAVA_TOOL_OPTIONS")
	if strings.Count(v, "-Dflag2") != 1 {
		t.Fatalf("-Dflag2 duplicated: %s", v)
	}
	if !strings.Contains(v, "-Dflag3") {
		t.Fatalf("-Dflag3 missing: %s", v)
	}
}

func TestAppendServiceEnvVar_Map_CreatesNewWhenMissing(t *testing.T) {
	svc := newMapServiceNode("OTHER", "val")
	newImpl().appendServiceEnvVar(svc, "JAVA_TOOL_OPTIONS", "-javaagent:/agent.jar")

	v, ok := mapEnvValue(svc, "JAVA_TOOL_OPTIONS")
	if !ok {
		t.Fatal("JAVA_TOOL_OPTIONS not found")
	}
	if v != "-javaagent:/agent.jar" {
		t.Fatalf("unexpected value: %s", v)
	}
}

// ---------- Regression: repeated replay must not duplicate ----------

func TestRepeatedReplay_Seq_NoDuplication(t *testing.T) {
	svc := newSeqServiceNode()
	idc := newImpl()

	// Simulate two replay runs injecting the same agent flag.
	idc.appendServiceEnvVar(svc, "JAVA_TOOL_OPTIONS", "-javaagent:/keploy/agent.jar")
	idc.appendServiceEnvVar(svc, "JAVA_TOOL_OPTIONS", "-javaagent:/keploy/agent.jar")

	vals := seqEnvValues(svc)
	if len(vals) != 1 {
		t.Fatalf("expected 1 entry, got %d: %v", len(vals), vals)
	}
	if strings.Count(vals[0], "-javaagent:/keploy/agent.jar") != 1 {
		t.Fatalf("agent flag duplicated after repeated replay: %s", vals[0])
	}
}

func TestRepeatedReplay_Map_NoDuplication(t *testing.T) {
	svc := newMapServiceNode()
	idc := newImpl()

	idc.appendServiceEnvVar(svc, "JAVA_TOOL_OPTIONS", "-javaagent:/keploy/agent.jar")
	idc.appendServiceEnvVar(svc, "JAVA_TOOL_OPTIONS", "-javaagent:/keploy/agent.jar")

	v, ok := mapEnvValue(svc, "JAVA_TOOL_OPTIONS")
	if !ok {
		t.Fatal("JAVA_TOOL_OPTIONS not found")
	}
	if strings.Count(v, "-javaagent:/keploy/agent.jar") != 1 {
		t.Fatalf("agent flag duplicated after repeated replay: %s", v)
	}
}
