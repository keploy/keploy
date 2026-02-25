package models

import (
	"encoding/json"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

const MappingKind = "TestMocksMapping"

// MockEntry represents a single mock entry with its name and kind.
type MockEntry struct {
	Name string `json:"name" yaml:"name"`
	Kind string `json:"kind" yaml:"kind"`
}

// Mapping represents the top-level structure of a mappings.yaml file.
type Mapping struct {
	Version   string `json:"version" yaml:"version"`
	Kind      string `json:"kind" yaml:"kind"`
	TestSetID string `json:"testSetId" yaml:"test_set_id"`
	Tests     []Test `json:"tests" yaml:"tests"`
}

// Test represents a single test case and its associated mocks.
type Test struct {
	ID    string      `json:"id" yaml:"id"`
	Mocks []MockEntry `json:"mocks" yaml:"mocks"`
}

// UnmarshalYAML provides backward compatibility for the old comma-separated
// string format while supporting the new array-of-objects format.
//
// Old format:
//
//	tests:
//	  - id: test-1
//	    mocks: "mock-0,mock-1,mock-2"
//
// New format:
//
//	tests:
//	  - id: test-1
//	    mocks:
//	      - name: mock-0
//	        kind: HTTP
//	      - name: mock-1
//	        kind: Redis
func (t *Test) UnmarshalYAML(node *yaml.Node) error {
	// Try new format first (mocks as array of objects)
	type NewFormat struct {
		ID    string      `yaml:"id"`
		Mocks []MockEntry `yaml:"mocks"`
	}
	var nf NewFormat
	if err := node.Decode(&nf); err == nil && len(nf.Mocks) > 0 {
		t.ID = nf.ID
		t.Mocks = nf.Mocks
		return nil
	}

	// Fall back to old format (mocks as comma-separated string)
	type OldFormat struct {
		ID    string `yaml:"id"`
		Mocks string `yaml:"mocks"`
	}
	var of OldFormat
	if err := node.Decode(&of); err == nil {
		t.ID = of.ID
		if of.Mocks != "" {
			for _, name := range strings.Split(of.Mocks, ",") {
				name = strings.TrimSpace(name)
				if name != "" {
					t.Mocks = append(t.Mocks, MockEntry{Name: name})
				}
			}
		}
		return nil
	}

	return fmt.Errorf("failed to unmarshal Test from YAML")
}

// UnmarshalJSON provides backward compatibility for the old comma-separated
// string format while supporting the new array-of-objects format in JSON.
func (t *Test) UnmarshalJSON(data []byte) error {
	// Try new format first (mocks as array of objects)
	type NewFormat struct {
		ID    string      `json:"id"`
		Mocks []MockEntry `json:"mocks"`
	}
	var nf NewFormat
	if err := json.Unmarshal(data, &nf); err == nil && len(nf.Mocks) > 0 {
		t.ID = nf.ID
		t.Mocks = nf.Mocks
		return nil
	}

	// Try old format (mocks as comma-separated string)
	type OldFormat struct {
		ID    string `json:"id"`
		Mocks string `json:"mocks"`
	}
	var of OldFormat
	if err := json.Unmarshal(data, &of); err == nil {
		t.ID = of.ID
		if of.Mocks != "" {
			for _, name := range strings.Split(of.Mocks, ",") {
				name = strings.TrimSpace(name)
				if name != "" {
					t.Mocks = append(t.Mocks, MockEntry{Name: name})
				}
			}
		}
		return nil
	}

	return fmt.Errorf("failed to unmarshal Test from JSON")
}

// MockNames returns the names of all mocks as a string slice (for backward compatibility).
func (t *Test) MockNames() []string {
	names := make([]string, len(t.Mocks))
	for i, m := range t.Mocks {
		names[i] = m.Name
	}
	return names
}
