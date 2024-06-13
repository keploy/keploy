package models

type HTTPSchema2 struct {
	Version string     `json:"version" yaml:"version"`
	Kind    string     `json:"kind" yaml:"kind"`
	Name    string     `json:"name" yaml:"name"`
	Spec    HTTPSchema `json:"spec" yaml:"spec"`
}

type OpenAPI struct {
	OpenAPI    string                 `json:"openapi"`
	Info       Info                   `json:"info"`
	Paths      map[string]PathItem    `json:"paths"`
	Components map[string]interface{} `json:"components"`
}

type Info struct {
	Title   string `json:"title"`
	Version string `json:"version"`
}

type PathItem struct {
	Get  *Operation `json:"get,omitempty"`
	Post *Operation `json:"post,omitempty"`
}

type Operation struct {
	Summary     string                  `json:"summary"`
	Description string                  `json:"description"`
	OperationID string                  `json:"operationId"`
	Parameters  []Parameter             `json:"parameters"`
	RequestBody *RequestBody            `json:"requestBody,omitempty"`
	Responses   map[string]ResponseItem `json:"responses"`
}

type Parameter struct {
	Name     string      `json:"name"`
	In       string      `json:"in"`
	Required bool        `json:"required"`
	Schema   ParamSchema `json:"schema"`
	Example  string      `json:"example"`
}

type RequestBody struct {
	Content map[string]MediaType `json:"content"`
}

type MediaType struct {
	Schema  Schema `json:"schema"`
	Example string `json:"example"`
}

type ResponseItem struct {
	Description string               `json:"description"`
	Content     map[string]MediaType `json:"content"`
}

type Schema struct {
	Type       string                       `json:"type"`
	Properties map[string]map[string]string `json:"properties"`
}

type ParamSchema struct {
	Type string `json:"type"`
}
