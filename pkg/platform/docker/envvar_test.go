package docker

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

const (
	testTrustStorePath = "/tmp/keploy-tls/truststore.jks"
	testTrustStoreFlag = "-Djavax.net.ssl.trustStore=" + testTrustStorePath
	testTrustPassFlag  = "-Djavax.net.ssl.trustStorePassword=changeit"
	testJavaOpts       = testTrustStoreFlag + " " + testTrustPassFlag
	testCACertPath     = "/tmp/keploy-tls/ca.crt"
)

func newTestImpl() *Impl {
	return &Impl{
		logger: zap.NewNop(),
	}
}

// buildSequenceEnvNode creates a YAML service node with sequence-style
// environment: ["KEY=VAL", ...]
func buildSequenceEnvNode(envs map[string]string) *yaml.Node {
	envItems := []*yaml.Node{}
	for k, v := range envs {
		envItems = append(envItems, &yaml.Node{
			Kind:  yaml.ScalarNode,
			Value: fmt.Sprintf("%s=%s", k, v),
		})
	}
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "environment"},
			{Kind: yaml.SequenceNode, Content: envItems},
		},
	}
}

// buildMappingEnvNode creates a YAML service node with mapping-style
// environment: { KEY: VAL, ... }
func buildMappingEnvNode(envs map[string]string) *yaml.Node {
	envItems := []*yaml.Node{}
	for k, v := range envs {
		envItems = append(envItems,
			&yaml.Node{Kind: yaml.ScalarNode, Value: k},
			&yaml.Node{Kind: yaml.ScalarNode, Value: v},
		)
	}
	return &yaml.Node{
		Kind: yaml.MappingNode,
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Value: "environment"},
			{Kind: yaml.MappingNode, Content: envItems},
		},
	}
}

// getEnvFromSequence extracts the value of a KEY=VAL entry from a
// sequence-style environment node.
func getEnvFromSequence(serviceNode *yaml.Node, key string) (string, bool) {
	prefix := key + "="
	for i := 0; i < len(serviceNode.Content); i += 2 {
		if serviceNode.Content[i].Value == "environment" {
			envNode := serviceNode.Content[i+1]
			for _, item := range envNode.Content {
				if strings.HasPrefix(item.Value, prefix) {
					return strings.TrimPrefix(item.Value, prefix), true
				}
			}
		}
	}
	return "", false
}

// getEnvFromMapping extracts the value from a mapping-style environment node.
func getEnvFromMapping(serviceNode *yaml.Node, key string) (string, bool) {
	for i := 0; i < len(serviceNode.Content); i += 2 {
		if serviceNode.Content[i].Value == "environment" {
			envNode := serviceNode.Content[i+1]
			for j := 0; j < len(envNode.Content)-1; j += 2 {
				if envNode.Content[j].Value == key {
					return envNode.Content[j+1].Value, true
				}
			}
		}
	}
	return "", false
}

// countEnvOccurrences counts how many times a key appears in the environment.
func countEnvOccurrences(serviceNode *yaml.Node, key string) int {
	count := 0
	prefix := key + "="
	for i := 0; i < len(serviceNode.Content); i += 2 {
		if serviceNode.Content[i].Value == "environment" {
			envNode := serviceNode.Content[i+1]
			if envNode.Kind == yaml.SequenceNode {
				for _, item := range envNode.Content {
					if strings.HasPrefix(item.Value, prefix) {
						count++
					}
				}
			}
			if envNode.Kind == yaml.MappingNode {
				for j := 0; j < len(envNode.Content)-1; j += 2 {
					if envNode.Content[j].Value == key {
						count++
					}
				}
			}
		}
	}
	return count
}

// --- addServiceEnvVar Tests ---

func TestAddServiceEnvVar_Sequence_NewKey(t *testing.T) {
	idc := newTestImpl()
	node := buildSequenceEnvNode(map[string]string{})

	idc.addServiceEnvVar(node, "SSL_CERT_FILE", testCACertPath)

	val, found := getEnvFromSequence(node, "SSL_CERT_FILE")
	require.True(t, found)
	assert.Equal(t, testCACertPath, val)
}

func TestAddServiceEnvVar_Sequence_OverwritesExistingKey(t *testing.T) {
	idc := newTestImpl()
	node := buildSequenceEnvNode(map[string]string{
		"SSL_CERT_FILE": "/old/path/ca.crt",
	})

	idc.addServiceEnvVar(node, "SSL_CERT_FILE", testCACertPath)

	val, found := getEnvFromSequence(node, "SSL_CERT_FILE")
	require.True(t, found)
	assert.Equal(t, testCACertPath, val)
	assert.Equal(t, 1, countEnvOccurrences(node, "SSL_CERT_FILE"),
		"must not create duplicate entries")
}

func TestAddServiceEnvVar_Sequence_NoDuplicateOnRepeatedCalls(t *testing.T) {
	idc := newTestImpl()
	node := buildSequenceEnvNode(map[string]string{})

	idc.addServiceEnvVar(node, "SSL_CERT_FILE", testCACertPath)
	idc.addServiceEnvVar(node, "SSL_CERT_FILE", testCACertPath)
	idc.addServiceEnvVar(node, "SSL_CERT_FILE", testCACertPath)

	assert.Equal(t, 1, countEnvOccurrences(node, "SSL_CERT_FILE"),
		"repeated calls must not create duplicate entries")
}

func TestAddServiceEnvVar_Mapping_NewKey(t *testing.T) {
	idc := newTestImpl()
	node := buildMappingEnvNode(map[string]string{})

	idc.addServiceEnvVar(node, "SSL_CERT_FILE", testCACertPath)

	val, found := getEnvFromMapping(node, "SSL_CERT_FILE")
	require.True(t, found)
	assert.Equal(t, testCACertPath, val)
}

func TestAddServiceEnvVar_Mapping_OverwritesExistingKey(t *testing.T) {
	idc := newTestImpl()
	node := buildMappingEnvNode(map[string]string{
		"SSL_CERT_FILE": "/old/path/ca.crt",
	})

	idc.addServiceEnvVar(node, "SSL_CERT_FILE", testCACertPath)

	val, found := getEnvFromMapping(node, "SSL_CERT_FILE")
	require.True(t, found)
	assert.Equal(t, testCACertPath, val)
	assert.Equal(t, 1, countEnvOccurrences(node, "SSL_CERT_FILE"),
		"must not create duplicate entries")
}

func TestAddServiceEnvVar_CreatesEnvironmentIfMissing(t *testing.T) {
	idc := newTestImpl()
	node := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{}}

	idc.addServiceEnvVar(node, "SSL_CERT_FILE", testCACertPath)

	val, found := getEnvFromSequence(node, "SSL_CERT_FILE")
	require.True(t, found)
	assert.Equal(t, testCACertPath, val)
}

func TestAddServiceEnvVar_Sequence_BareKeyUpdated(t *testing.T) {
	// Docker Compose allows bare "KEY" (no =) to inherit from host.
	// addServiceEnvVar must detect this and update it in place.
	idc := newTestImpl()
	node := buildSequenceEnvNode(map[string]string{})
	// Manually add a bare key (no "=")
	for i := 0; i < len(node.Content); i += 2 {
		if node.Content[i].Value == "environment" {
			node.Content[i+1].Content = append(node.Content[i+1].Content, &yaml.Node{
				Kind:  yaml.ScalarNode,
				Value: "SSL_CERT_FILE",
			})
		}
	}

	idc.addServiceEnvVar(node, "SSL_CERT_FILE", testCACertPath)

	val, found := getEnvFromSequence(node, "SSL_CERT_FILE")
	require.True(t, found)
	assert.Equal(t, testCACertPath, val)
	assert.Equal(t, 1, countEnvOccurrences(node, "SSL_CERT_FILE"),
		"bare KEY must be updated in place, not duplicated")
}

func TestAppendJavaToolOptions_Sequence_BareKeyUpdated(t *testing.T) {
	// If compose has bare "JAVA_TOOL_OPTIONS" (inheriting from host),
	// appendJavaToolOptions must detect it and set the truststore flags.
	idc := newTestImpl()
	node := buildSequenceEnvNode(map[string]string{})
	for i := 0; i < len(node.Content); i += 2 {
		if node.Content[i].Value == "environment" {
			node.Content[i+1].Content = append(node.Content[i+1].Content, &yaml.Node{
				Kind:  yaml.ScalarNode,
				Value: "JAVA_TOOL_OPTIONS",
			})
		}
	}

	idc.appendJavaToolOptions(node, testJavaOpts)

	val, found := getEnvFromSequence(node, "JAVA_TOOL_OPTIONS")
	require.True(t, found)
	assert.Contains(t, val, testTrustStoreFlag)
	assert.Equal(t, 1, countEnvOccurrences(node, "JAVA_TOOL_OPTIONS"),
		"bare KEY must be updated in place, not duplicated")
}

// --- appendJavaToolOptions Tests ---

func TestAppendServiceEnvVar_Sequence_AppendsToExistingValue(t *testing.T) {
	existing := "-Dhttps.proxyHost=proxy.corp.com -Dhttps.proxyPort=3128"
	idc := newTestImpl()
	node := buildSequenceEnvNode(map[string]string{
		"JAVA_TOOL_OPTIONS": existing,
	})

	idc.appendJavaToolOptions(node, testJavaOpts)

	val, found := getEnvFromSequence(node, "JAVA_TOOL_OPTIONS")
	require.True(t, found)

	assert.Contains(t, val, existing, "original flags must be preserved")
	assert.Contains(t, val, testTrustStoreFlag, "truststore flag must be appended")
	assert.Contains(t, val, testTrustPassFlag, "truststore password must be appended")
	assert.Equal(t, 1, countEnvOccurrences(node, "JAVA_TOOL_OPTIONS"),
		"must not create duplicate JAVA_TOOL_OPTIONS entries")
}

func TestAppendServiceEnvVar_Sequence_CreatesWhenMissing(t *testing.T) {
	idc := newTestImpl()
	node := buildSequenceEnvNode(map[string]string{})

	idc.appendJavaToolOptions(node, testJavaOpts)

	val, found := getEnvFromSequence(node, "JAVA_TOOL_OPTIONS")
	require.True(t, found)
	assert.Equal(t, testJavaOpts, val)
}

func TestAppendServiceEnvVar_Sequence_NoDuplicateWhenTrustStorePresent(t *testing.T) {
	existing := "-Dhttps.proxyHost=proxy.corp.com " + testJavaOpts
	idc := newTestImpl()
	node := buildSequenceEnvNode(map[string]string{
		"JAVA_TOOL_OPTIONS": existing,
	})

	idc.appendJavaToolOptions(node, testJavaOpts)

	val, found := getEnvFromSequence(node, "JAVA_TOOL_OPTIONS")
	require.True(t, found)
	assert.Equal(t, existing, val, "value must not be modified when truststore already present")
	assert.Equal(t, 1, strings.Count(val, "-Djavax.net.ssl.trustStore="),
		"truststore flag must appear exactly once")
}

func TestAppendServiceEnvVar_Sequence_EmptyExistingValue(t *testing.T) {
	idc := newTestImpl()
	node := buildSequenceEnvNode(map[string]string{
		"JAVA_TOOL_OPTIONS": "",
	})

	idc.appendJavaToolOptions(node, testJavaOpts)

	val, found := getEnvFromSequence(node, "JAVA_TOOL_OPTIONS")
	require.True(t, found)
	assert.Equal(t, testJavaOpts, val, "empty value should be replaced, not get leading space")
}

func TestAppendServiceEnvVar_Mapping_AppendsToExistingValue(t *testing.T) {
	existing := "-Dhttps.proxyHost=proxy.corp.com"
	idc := newTestImpl()
	node := buildMappingEnvNode(map[string]string{
		"JAVA_TOOL_OPTIONS": existing,
	})

	idc.appendJavaToolOptions(node, testJavaOpts)

	val, found := getEnvFromMapping(node, "JAVA_TOOL_OPTIONS")
	require.True(t, found)
	assert.Contains(t, val, existing)
	assert.Contains(t, val, testTrustStoreFlag)
	assert.Equal(t, 1, countEnvOccurrences(node, "JAVA_TOOL_OPTIONS"))
}

func TestAppendServiceEnvVar_Mapping_NoDuplicateWhenTrustStorePresent(t *testing.T) {
	existing := "-Dhttps.proxyHost=proxy.corp.com " + testJavaOpts
	idc := newTestImpl()
	node := buildMappingEnvNode(map[string]string{
		"JAVA_TOOL_OPTIONS": existing,
	})

	idc.appendJavaToolOptions(node, testJavaOpts)

	val, found := getEnvFromMapping(node, "JAVA_TOOL_OPTIONS")
	require.True(t, found)
	assert.Equal(t, existing, val)
}

func TestAppendServiceEnvVar_NoEnvironmentSection(t *testing.T) {
	idc := newTestImpl()
	node := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{}}

	idc.appendJavaToolOptions(node, testJavaOpts)

	val, found := getEnvFromSequence(node, "JAVA_TOOL_OPTIONS")
	require.True(t, found)
	assert.Equal(t, testJavaOpts, val)
}

// --- Integration-style test: simulates full modifyAppServiceForKeploy flow ---

func TestAddServiceEnvVar_AllEnvVars_NoDuplicatesOnRepeatedMutation(t *testing.T) {
	idc := newTestImpl()
	node := buildSequenceEnvNode(map[string]string{
		"JAVA_TOOL_OPTIONS": "-Dhttps.proxyHost=proxy.corp.com",
	})

	// Simulate two rounds of mutation (e.g., re-running keploy setup)
	for range 2 {
		idc.addServiceEnvVar(node, "NODE_EXTRA_CA_CERTS", testCACertPath)
		idc.addServiceEnvVar(node, "REQUESTS_CA_BUNDLE", testCACertPath)
		idc.addServiceEnvVar(node, "SSL_CERT_FILE", testCACertPath)
		idc.addServiceEnvVar(node, "CARGO_HTTP_CAINFO", testCACertPath)
		idc.appendJavaToolOptions(node, testJavaOpts)
	}

	// Each env var must appear exactly once
	for _, key := range []string{"NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "SSL_CERT_FILE", "CARGO_HTTP_CAINFO", "JAVA_TOOL_OPTIONS"} {
		assert.Equal(t, 1, countEnvOccurrences(node, key),
			"%s must appear exactly once after repeated mutations", key)
	}

	// JAVA_TOOL_OPTIONS must have original + truststore
	val, _ := getEnvFromSequence(node, "JAVA_TOOL_OPTIONS")
	assert.Contains(t, val, "-Dhttps.proxyHost=proxy.corp.com")
	assert.Contains(t, val, testTrustStoreFlag)
	assert.Equal(t, 1, strings.Count(val, "-Djavax.net.ssl.trustStore="))
}
