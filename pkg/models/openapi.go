package models

// SchemaMatchMode defines the possible modes for schema matching.
type SchemaMatchMode int

const (
	// IdentifyMode is used for identifying schema.
	IdentifyMode SchemaMatchMode = iota

	// CompareMode is used for comparing schemas.
	CompareMode
)

// String returns the string representation of the SchemaMatchMode.
func (s SchemaMatchMode) String() string {
	return [...]string{"IDENTIFYMODE", "COMPAREMODE"}[s]
}

// DrivenMode defines the possible modes for driven contexts.
type DrivenMode int

const (
	// ConsumerMode is used for a consumer context.
	ConsumerMode DrivenMode = iota

	// ProviderMode is used for a provider context.
	ProviderMode
)

// String returns the string representation of the DrivenMode.
func (d DrivenMode) String() string {
	return [...]string{"consumer", "provider"}[d]
}

type HTTPDoc struct {
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
type SchemaInfo struct {
	Service   string  `json:"service" yaml:"service"`
	TestSetID string  `json:"testSetId" yaml:"testSetId"`
	Name      string  `json:"name" yaml:"name"`
	Score     float64 `json:"score" yaml:"score"`
	Data      OpenAPI `json:"data" yaml:"data"`
}
type Summary struct {
	ServicesSummary []ServiceSummary `json:"servicesSummary" yaml:"servicesSummary"`
}
type ServiceSummary struct {
	Service     string            `json:"service" yaml:"service"`
	TestSets    map[string]Status `json:"testSets" yaml:"testSets"`
	PassedCount int               `json:"passedCount" yaml:"passedCount"`
	FailedCount int               `json:"failedCount" yaml:"failedCount"`
	MissedCount int               `json:"missedCount" yaml:"missedCount"`
}
type Status struct {
	Failed []string `json:"failed" yaml:"failed"`
	Passed []string `json:"passed" yaml:"passed"`
	Missed []string `json:"missed" yaml:"missed"`
}

type MockMapping struct {
	Service   string     `json:"service" yaml:"service"`
	TestSetID string     `json:"testSetId" yaml:"testSetId"`
	Mocks     []*OpenAPI `json:"mocks" yaml:"mocks"`
}
