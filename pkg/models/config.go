// Package models provides data models for the keploy.
package models

type TestSet struct {
	PreScript  string                 `json:"pre_script" bson:"pre_script" yaml:"pre_script,omitempty"`
	PostScript string                 `json:"post_script" bson:"post_script" yaml:"post_script,omitempty"`
	Template   map[string]interface{} `json:"template" bson:"template" yaml:"template,omitempty"`
	AppCmd     string                 `json:"appCmd" bson:"app_cmd" yaml:"appCmd,omitempty"`
}
