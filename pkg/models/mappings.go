package models

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const MappingKind = "TestMocksMapping"

// MockEntry represents a single mock entry with its name and kind.
type MockEntry struct {
	Name             string `json:"name" yaml:"name" bson:"name"`
	Kind             string `json:"kind" yaml:"kind" bson:"kind"`
	Timestamp        int64  `json:"timestamp,omitempty" yaml:"timestamp,omitempty" bson:"timestamp,omitempty"`
	ReqTimestampMock string `json:"reqTimestampMock,omitempty" yaml:"reqTimestampMock,omitempty" bson:"req_timestamp_mock,omitempty"`
	ResTimestampMock string `json:"resTimestampMock,omitempty" yaml:"resTimestampMock,omitempty" bson:"res_timestamp_mock,omitempty"`
}

func FormatMockTimestamp(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	return ts.Format(time.RFC3339Nano)
}

type Mapping struct {
	Version   string           `json:"version" yaml:"version" bson:"version"`
	Kind      string           `json:"kind" yaml:"kind" bson:"kind"`
	TestSetID string           `json:"testSetId" yaml:"test_set_id" bson:"test_set_id"`
	TestCases []MappedTestCase `json:"tests" yaml:"tests" bson:"tests"`
}

type MappedTestCase struct {
	ID    string      `json:"id" yaml:"id" bson:"id"`
	Mocks []MockEntry `json:"mocks" yaml:"mocks" bson:"mocks"`
}

type persistedTestFormat struct {
	ID          string      `json:"id" yaml:"id"`
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

func (tc MappedTestCase) MarshalYAML() (interface{}, error) {
	return tc.persistedFormat(), nil
}

func (tc MappedTestCase) MarshalJSON() ([]byte, error) {
	return json.Marshal(tc.persistedFormat())
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
func (tc *MappedTestCase) UnmarshalYAML(node *yaml.Node) error {
	// Prefer the backward-compatible persisted format first.
	var persisted persistedTestFormat
	if err := node.Decode(&persisted); err == nil && nodeHasField(node, "mock_entries") {
		tc.applyDecodedMocks(persisted.ID, persisted.MockEntries, "")
		return nil
	}

	type legacyTestFormat struct {
		ID    string `yaml:"id"`
		Mocks string `yaml:"mocks"`
	}
	var legacy legacyTestFormat
	if err := node.Decode(&legacy); err == nil {
		tc.applyDecodedMocks(legacy.ID, nil, legacy.Mocks)
		return nil
	}

	var structured structuredTestFormat
	if err := node.Decode(&structured); err == nil {
		tc.applyDecodedMocks(structured.ID, structured.Mocks, "")
		return nil
	}

	// Accept a string slice too so intermediate/custom fixtures remain readable.
	var stringSlice stringSliceTestFormat
	if err := node.Decode(&stringSlice); err == nil {
		tc.applyDecodedMocks(stringSlice.ID, mockEntriesFromNames(stringSlice.Mocks), "")
		return nil
	}

	return fmt.Errorf("failed to unmarshal mapped test case from YAML")
}

// UnmarshalJSON provides backward compatibility for the old comma-separated
// string format while supporting the structured formats used by newer builds.
func (tc *MappedTestCase) UnmarshalJSON(data []byte) error {
	var persisted persistedTestFormat
	if err := json.Unmarshal(data, &persisted); err == nil && jsonContainsField(data, "mock_entries") {
		tc.applyDecodedMocks(persisted.ID, persisted.MockEntries, "")
		return nil
	}

	type legacyTestFormat struct {
		ID    string `json:"id"`
		Mocks string `json:"mocks"`
	}
	var legacy legacyTestFormat
	if err := json.Unmarshal(data, &legacy); err == nil {
		tc.applyDecodedMocks(legacy.ID, nil, legacy.Mocks)
		return nil
	}

	var structured structuredTestFormat
	if err := json.Unmarshal(data, &structured); err == nil {
		tc.applyDecodedMocks(structured.ID, structured.Mocks, "")
		return nil
	}

	var stringSlice stringSliceTestFormat
	if err := json.Unmarshal(data, &stringSlice); err == nil {
		tc.applyDecodedMocks(stringSlice.ID, mockEntriesFromNames(stringSlice.Mocks), "")
		return nil
	}

	return fmt.Errorf("failed to unmarshal mapped test case from JSON")
}

// MockNames returns the names of all mocks as a string slice (for backward compatibility).
func (tc *MappedTestCase) MockNames() []string {
	names := make([]string, len(tc.Mocks))
	for i, m := range tc.Mocks {
		names[i] = m.Name
	}
	return names
}

func (tc MappedTestCase) persistedFormat() persistedTestFormat {
	return persistedTestFormat{
		ID:          tc.ID,
		MockEntries: append([]MockEntry(nil), tc.Mocks...),
	}
}

func (tc *MappedTestCase) applyDecodedMocks(id string, mockEntries []MockEntry, legacyMocks string) {
	tc.ID = id
	tc.Mocks = nil
	if len(mockEntries) > 0 {
		tc.Mocks = append([]MockEntry(nil), mockEntries...)
		return
	}
	tc.Mocks = parseLegacyMocks(legacyMocks)
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

func nodeHasField(node *yaml.Node, field string) bool {
	if node == nil || node.Kind != yaml.MappingNode {
		return false
	}

	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == field {
			return true
		}
	}
	return false
}

func jsonContainsField(data []byte, field string) bool {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	_, ok := raw[field]
	return ok
}
