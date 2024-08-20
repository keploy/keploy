// Package models provides data models for the keploy.
package models

type TestSet struct {
	PreScript    string                 `json:"pre_script" bson:"pre_script" yaml:"preScript"`
	PostScript   string                 `json:"post_script" bson:"post_script" yaml:"postScript"`
	Template     map[string]interface{} `json:"template" bson:"template" yaml:"template"`
	MockRegistry *MockRegistry          `yaml:"mockRegistry" bson:"mock_registry" json:"mockRegistry,omitempty"`
}

type MockRegistry struct {
	Mock string `json:"mock" bson:"mock" yaml:"mock,omitempty"`
	App  string `json:"app" bson:"app" yaml:"app,omitempty"`
	User string `json:"user" bson:"user" yaml:"user,omitempty"`
}
