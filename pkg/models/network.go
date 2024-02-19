package models

import (
	yamlLib "gopkg.in/yaml.v3"
)

type NetworkTrafficDoc struct {
	Version Version      `json:"version" yaml:"version"`
	Kind    Kind         `json:"kind" yaml:"kind"`
	Name    string       `json:"name" yaml:"name"`
	Spec    yamlLib.Node `json:"spec" yaml:"spec"`
	Curl    string       `json:"curl" yaml:"curl,omitempty"`
}

func (nd *NetworkTrafficDoc) GetKind() string {
	return string(nd.Kind)
}
