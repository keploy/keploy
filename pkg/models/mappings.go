package models

type Mapping struct {
	Version string `json:"version" yaml:"version"`
	Id      string `json:"id" yaml:"id"`
	Tests   []Test `json:"tests" yaml:"tests"`
}

type Test struct {
	ID    string   `json:"id" yaml:"id"`
	Mocks []string `json:"mocks" yaml:"mocks"`
}
