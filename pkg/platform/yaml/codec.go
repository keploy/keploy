package yaml

import (
	"encoding/json"
	"fmt"

	"go.keploy.io/server/v3/pkg/models"
	yamlLib "gopkg.in/yaml.v3"
)

// Format represents the serialization format for test data files.
type Format string

const (
	FormatYAML Format = "yaml"
	FormatJSON Format = "json"
)

// ParseFormat parses a string into a Format, defaulting to YAML.
func ParseFormat(s string) Format {
	switch s {
	case "json", "JSON":
		return FormatJSON
	default:
		return FormatYAML
	}
}

// FileExtension returns the file extension (without dot) for the format.
func (f Format) FileExtension() string {
	switch f {
	case FormatJSON:
		return "json"
	default:
		return "yaml"
	}
}

// NetworkTrafficDocJSON is the JSON-friendly version of NetworkTrafficDoc.
// It uses json.RawMessage instead of yaml.Node for the polymorphic Spec field.
type NetworkTrafficDocJSON struct {
	Version      models.Version      `json:"version"`
	Kind         models.Kind         `json:"kind"`
	Name         string              `json:"name"`
	Spec         json.RawMessage     `json:"spec"`
	Noise        []string            `json:"noise,omitempty"`
	LastUpdated  *models.LastUpdated `json:"last_updated,omitempty"`
	Curl         string              `json:"curl,omitempty"`
	ConnectionID string              `json:"connectionId,omitempty"`
}

// docToJSON converts a NetworkTrafficDoc to its JSON-friendly representation.
func docToJSON(doc *NetworkTrafficDoc) (*NetworkTrafficDocJSON, error) {
	var specData interface{}
	if err := doc.Spec.Decode(&specData); err != nil {
		return nil, fmt.Errorf("failed to decode yaml.Node spec for JSON conversion: %w", err)
	}
	specBytes, err := json.Marshal(specData)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal spec to JSON: %w", err)
	}
	return &NetworkTrafficDocJSON{
		Version:      doc.Version,
		Kind:         doc.Kind,
		Name:         doc.Name,
		Spec:         specBytes,
		Noise:        doc.Noise,
		LastUpdated:  doc.LastUpdated,
		Curl:         doc.Curl,
		ConnectionID: doc.ConnectionID,
	}, nil
}

// MarshalDoc serializes a NetworkTrafficDoc to bytes in the specified format.
// For YAML, it marshals the doc directly (using yaml.Node for Spec).
// For JSON, it converts yaml.Node Spec to json.RawMessage via a round-trip.
func MarshalDoc(format Format, doc *NetworkTrafficDoc) ([]byte, error) {
	switch format {
	case FormatJSON:
		jsonDoc, err := docToJSON(doc)
		if err != nil {
			return nil, err
		}
		return json.Marshal(jsonDoc)
	default:
		return yamlLib.Marshal(doc)
	}
}

// MarshalDocIndent is like MarshalDoc but produces indented output for JSON.
func MarshalDocIndent(format Format, doc *NetworkTrafficDoc) ([]byte, error) {
	switch format {
	case FormatJSON:
		jsonDoc, err := docToJSON(doc)
		if err != nil {
			return nil, err
		}
		return json.MarshalIndent(jsonDoc, "", "  ")
	default:
		return yamlLib.Marshal(doc)
	}
}

// UnmarshalDoc deserializes bytes into a NetworkTrafficDoc from the specified format.
// For YAML, it unmarshals directly.
// For JSON, it unmarshals to NetworkTrafficDocJSON, then converts Spec to yaml.Node.
func UnmarshalDoc(format Format, data []byte) (*NetworkTrafficDoc, error) {
	switch format {
	case FormatJSON:
		var jsonDoc NetworkTrafficDocJSON
		if err := json.Unmarshal(data, &jsonDoc); err != nil {
			return nil, fmt.Errorf("failed to unmarshal JSON doc: %w", err)
		}
		return jsonDocToYamlDoc(&jsonDoc)
	default:
		var doc NetworkTrafficDoc
		if err := yamlLib.Unmarshal(data, &doc); err != nil {
			return nil, fmt.Errorf("failed to unmarshal YAML doc: %w", err)
		}
		return &doc, nil
	}
}

// jsonDocToYamlDoc converts a NetworkTrafficDocJSON to NetworkTrafficDoc
// by converting the json.RawMessage Spec to a yaml.Node.
func jsonDocToYamlDoc(jsonDoc *NetworkTrafficDocJSON) (*NetworkTrafficDoc, error) {
	doc := &NetworkTrafficDoc{
		Version:      jsonDoc.Version,
		Kind:         jsonDoc.Kind,
		Name:         jsonDoc.Name,
		Noise:        jsonDoc.Noise,
		LastUpdated:  jsonDoc.LastUpdated,
		Curl:         jsonDoc.Curl,
		ConnectionID: jsonDoc.ConnectionID,
	}

	// Convert json.RawMessage to a generic interface, then encode into yaml.Node
	if len(jsonDoc.Spec) > 0 {
		var specData interface{}
		if err := json.Unmarshal(jsonDoc.Spec, &specData); err != nil {
			return nil, fmt.Errorf("failed to unmarshal JSON spec: %w", err)
		}
		if err := doc.Spec.Encode(specData); err != nil {
			return nil, fmt.Errorf("failed to encode spec into yaml.Node: %w", err)
		}
	}

	return doc, nil
}

// MarshalGeneric serializes any value to bytes in the specified format.
func MarshalGeneric(format Format, v interface{}) ([]byte, error) {
	switch format {
	case FormatJSON:
		return json.MarshalIndent(v, "", "  ")
	default:
		return yamlLib.Marshal(v)
	}
}

// UnmarshalGeneric deserializes bytes into v using the specified format.
func UnmarshalGeneric(format Format, data []byte, v interface{}) error {
	switch format {
	case FormatJSON:
		return json.Unmarshal(data, v)
	default:
		return yamlLib.Unmarshal(data, v)
	}
}
