// Package models provides data models for the keploy.
package models

type TestSet struct {
	PreScript    string                 `json:"pre_script" bson:"pre_script" yaml:"preScript"`
	PostScript   string                 `json:"post_script" bson:"post_script" yaml:"postScript"`
	Template     map[string]interface{} `json:"template" bson:"template" yaml:"template"`
	Secret       map[string]interface{} `json:"secret" bson:"secret" yaml:"secret,omitempty"`
	MockRegistry *MockRegistry          `yaml:"mockRegistry" bson:"mock_registry" json:"mockRegistry,omitempty"`
	Metadata     map[string]interface{} `json:"metadata" bson:"metadata" yaml:"metadata,omitempty"`
}

type MockRegistry struct {
	Mock string `json:"mock" bson:"mock" yaml:"mock,omitempty"`
	App  string `json:"app" bson:"app" yaml:"app,omitempty"`
	User string `json:"user" bson:"user" yaml:"user,omitempty"`
}

type Plan struct {
	Type   string `json:"type"`
	Status string `json:"status"`
	KUnits int    `json:"kunits,omitempty"`
}
