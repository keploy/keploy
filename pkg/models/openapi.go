package models

type HTTPSchema2 struct {
	Version string     `json:"version" yaml:"version"`
	Kind    string     `json:"kind" yaml:"kind"`
	Name    string     `json:"name" yaml:"name"`
	Spec    HTTPSchema `json:"spec" yaml:"spec"`
}

type OpenAPI struct {
	OpenAPI    string                 `json:"openapi" yaml:"openapi"`
	Info       Info                   `json:"info" yaml:"info"`
	Servers    []map[string]string    `json:"servers" yaml:"servers"`
	Paths      map[string]PathItem    `json:"paths" yaml:"paths"`
	Components map[string]interface{} `json:"components" yaml:"components"`
}

type Info struct {
	Title       string `json:"title" yaml:"title"`
	Version     string `json:"version" yaml:"version"`
	Description string `json:"description" yaml:"description"`
}

type PathItem struct {
	Get    *Operation `json:"get,omitempty" yaml:"get,omitempty"`
	Post   *Operation `json:"post,omitempty" yaml:"post,omitempty"`
	Put    *Operation `json:"put,omitempty" yaml:"put,omitempty"`
	Delete *Operation `json:"delete,omitempty" yaml:"delete,omitempty"`
	Patch  *Operation `json:"patch,omitempty" yaml:"patch,omitempty"`
}

type Operation struct {
	Summary     string                  `json:"summary" yaml:"summary"`
	Description string                  `json:"description" yaml:"description"`
	Parameters  []Parameter             `json:"parameters" yaml:"parameters"`
	OperationID string                  `json:"operationId" yaml:"operationId"`
	RequestBody *RequestBody            `json:"requestBody,omitempty" yaml:"requestBody,omitempty"`
	Responses   map[string]ResponseItem `json:"responses" yaml:"responses"`
}

type Parameter struct {
	Name     string      `json:"name" yaml:"name"`
	In       string      `json:"in" yaml:"in"`
	Required bool        `json:"required" yaml:"required"`
	Schema   ParamSchema `json:"schema" yaml:"schema"`
	Example  string      `json:"example" yaml:"example"`
}

type RequestBody struct {
	Content map[string]MediaType `json:"content" yaml:"content"`
}

type MediaType struct {
	Schema  Schema                 `json:"schema" yaml:"schema"`
	Example map[string]interface{} `json:"example" yaml:"example"`
}

type ResponseItem struct {
	Description string               `json:"description" yaml:"description"`
	Content     map[string]MediaType `json:"content" yaml:"content"`
}

type Schema struct {
	Type       string                            `json:"type" yaml:"type"`
	Properties map[string]map[string]interface{} `json:"properties" yaml:"properties"`
}

type ParamSchema struct {
	Type string `json:"type" yaml:"type"`
}
