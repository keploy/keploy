package models

type Mock struct {
	Version string     `json:"version" bson:"version,omitempty" yaml:"version"`
	Kind    string     `json:"kind" bson:"kind,omitempty" yaml:"kind"`
	Name    string     `json:"name" bson:"name,omitempty" yaml:"name"`
	Spec    SpecSchema `json:"spec" bson:"spec,omitempty" yaml:"spec"`
}

type SpecSchema struct {
	Type     string            `json:"type" bson:"type,omitempty" yaml:"type"`
	Metadata map[string]string `json:"metadata" bson:"metadata,omitempty" yaml:"metadata"`
	Request  HttpReq           `json:"req" bson:"req,omitempty" yaml:"req"`
	Response HttpResp          `json:"resp" bson:"resp,omitempty" yaml:"resp"`
	Objects  []Object          `json:"objects" bson:"objects,omitempty" yaml:"objects"`
}

type Object struct {
	Type string `json:"type" bson:"type,omitempty" yaml:"type"`
	Data string `json:"data" bson:"data,omitempty" yaml:"data"`
}
