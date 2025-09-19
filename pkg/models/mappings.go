package models

import (
	"encoding/json"
	"strings"

	"gopkg.in/yaml.v3"
)

const MappingKind = "TestMocksMapping"

// MocksArray is a custom type that serializes as a comma-separated string
type MocksArray []string

// MarshalJSON implements json.Marshaler interface
func (m MocksArray) MarshalJSON() ([]byte, error) {
	return json.Marshal(strings.Join(m, ","))
}

// UnmarshalJSON implements json.Unmarshaler interface
func (m *MocksArray) UnmarshalJSON(data []byte) error {
	var str string
	if err := json.Unmarshal(data, &str); err != nil {
		return err
	}
	if str == "" {
		*m = MocksArray{}
		return nil
	}
	*m = MocksArray(strings.Split(str, ","))
	return nil
}

// MarshalYAML implements yaml.Marshaler interface
func (m MocksArray) MarshalYAML() (interface{}, error) {
	return strings.Join(m, ","), nil
}

// UnmarshalYAML implements yaml.Unmarshaler interface
func (m *MocksArray) UnmarshalYAML(node *yaml.Node) error {
	var str string
	if err := node.Decode(&str); err != nil {
		return err
	}
	if str == "" {
		*m = MocksArray{}
		return nil
	}
	*m = MocksArray(strings.Split(str, ","))
	return nil
}

// ToSlice returns the underlying string slice
func (m MocksArray) ToSlice() []string {
	return []string(m)
}

// FromSlice creates a MocksArray from a string slice
func FromSlice(slice []string) MocksArray {
	return MocksArray(slice)
}

type Mapping struct {
	Version   string `json:"version" yaml:"version"`
	Kind      string `json:"kind" yaml:"kind"`
	TestSetID string `json:"testSetId" yaml:"test_set_id"`
	Tests     []Test `json:"tests" yaml:"tests"`
}

type Test struct {
	ID    string     `json:"id" yaml:"id"`
	Mocks MocksArray `json:"mocks" yaml:"mocks"`
}
