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
	Name      string `json:"name" yaml:"name" bson:"name"`
	Kind      string `json:"kind" yaml:"kind" bson:"kind"`
	Timestamp int64  `json:"timestamp,omitempty" yaml:"timestamp,omitempty" bson:"timestamp,omitempty"`
}

type Mapping struct {
	Version   string `json:"version" yaml:"version" bson:"version"`
	Kind      string `json:"kind" yaml:"kind" bson:"kind"`
	TestSetID string `json:"testSetId" yaml:"test_set_id" bson:"test_set_id"`
	Tests     []Test `json:"tests" yaml:"tests" bson:"tests"`
}

type Test struct {
	ID    string      `json:"id" yaml:"id" bson:"id"`
	Mocks []MockEntry `json:"mocks" yaml:"mocks" bson:"mocks"`
}

type compatTestFormat struct {
	ID          string      `json:"id" yaml:"id"`
	Mocks       string      `json:"mocks" yaml:"mocks"`
	MockEntries []MockEntry `json:"mock_entries,omitempty" yaml:"mock_entries,omitempty"`
}

type structuredTestFormat struct {
	ID    string      `json:"id" yaml:"id"`
	Mocks []MockEntry `json:"mocks" yaml:"mocks"`
}

type stringSliceTestFormat struct {
	ID    string   `json:"id" yaml:"id"`
	Mocks []string `json:"mocks" yaml:"mocks"`
}

func (t Test) MarshalYAML() (interface{}, error) {
	return t.compatFormat(), nil
}

func (t Test) MarshalJSON() ([]byte, error) {
	return json.Marshal(t.compatFormat())
}

// UnmarshalYAML provides backward compatibility for the old comma-separated
// string format while supporting the structured formats used by newer builds.
//
// Old format:
//
//	tests:
//	  - id: test-1
//	    mocks: "mock-0,mock-1,mock-2"
//
// Structured format:
//
//	tests:
//	  - id: test-1
//	    mocks:
//	      - name: mock-0
//	        kind: HTTP
//	      - name: mock-1
//	        kind: Redis
func (t *Test) UnmarshalYAML(node *yaml.Node) error {
	// Prefer the backward-compatible persisted format first.
	var compat compatTestFormat
	if err := node.Decode(&compat); err == nil {
		t.applyDecodedMocks(compat.ID, compat.MockEntries, compat.Mocks)
		return nil
	}

	var structured structuredTestFormat
	if err := node.Decode(&structured); err == nil {
		t.applyDecodedMocks(structured.ID, structured.Mocks, "")
		return nil
	}

	// Accept a string slice too so intermediate/custom fixtures remain readable.
	var stringSlice stringSliceTestFormat
	if err := node.Decode(&stringSlice); err == nil {
		t.applyDecodedMocks(stringSlice.ID, mockEntriesFromNames(stringSlice.Mocks), "")
		return nil
	}

	return fmt.Errorf("failed to unmarshal Test from YAML")
}

// UnmarshalJSON provides backward compatibility for the old comma-separated
// string format while supporting the structured formats used by newer builds.
func (t *Test) UnmarshalJSON(data []byte) error {
	var compat compatTestFormat
	if err := json.Unmarshal(data, &compat); err == nil {
		t.applyDecodedMocks(compat.ID, compat.MockEntries, compat.Mocks)
		return nil
	}

	var structured structuredTestFormat
	if err := json.Unmarshal(data, &structured); err == nil {
		t.applyDecodedMocks(structured.ID, structured.Mocks, "")
		return nil
	}

	var stringSlice stringSliceTestFormat
	if err := json.Unmarshal(data, &stringSlice); err == nil {
		t.applyDecodedMocks(stringSlice.ID, mockEntriesFromNames(stringSlice.Mocks), "")
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

func (t Test) compatFormat() compatTestFormat {
	return compatTestFormat{
		ID:          t.ID,
		Mocks:       strings.Join(t.MockNames(), ","),
		MockEntries: append([]MockEntry(nil), t.Mocks...),
	}
}

func (t *Test) applyDecodedMocks(id string, mockEntries []MockEntry, legacyMocks string) {
	t.ID = id
	t.Mocks = nil
	if len(mockEntries) > 0 {
		t.Mocks = append([]MockEntry(nil), mockEntries...)
		return
	}
	t.Mocks = parseLegacyMocks(legacyMocks)
}

func parseLegacyMocks(mocks string) []MockEntry {
	if mocks == "" {
		return nil
	}

	var entries []MockEntry
	for _, name := range strings.Split(mocks, ",") {
		name = strings.TrimSpace(name)
		if name != "" {
			entries = append(entries, MockEntry{Name: name})
		}
	}
	return entries
}

func mockEntriesFromNames(names []string) []MockEntry {
	if len(names) == 0 {
		return nil
	}

	entries := make([]MockEntry, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name != "" {
			entries = append(entries, MockEntry{Name: name})
		}
	}
	return entries
}
