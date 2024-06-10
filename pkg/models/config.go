// Package models provides data models for the keploy.
package models

type TestSet struct {
	PreScript  string            `json:"pre_script" bson:"pre_script" yaml:"pre_script"`
	PostScript string            `json:"post_script" bson:"post_script" yaml:"post_script"`
	Template   map[string]string `json:"template" bson:"template" yaml:"template"`
}