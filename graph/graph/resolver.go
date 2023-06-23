package graph
//go:generate go run github.com/99designs/gqlgen generate
// This file will not be regenerated automatically.
//
// It serves as dependency injection for your app, add any dependencies you require here.

import (
	"go.keploy.io/server/pkg/service/record"
	"go.keploy.io/server/pkg/service/test"
	"go.keploy.io/server/pkg/platform/yaml"
)

func NewResolver(test test.Tester, record record.Recorder) *Resolver {
	return &Resolver{
		Test:     test,
		Record:  record,
		Yaml: 	yaml.Yaml{},
		TestReportID: yaml.TestReport{},
	}
}

type Resolver struct {
	Test     test.Tester
	Record  record.Recorder
	Yaml 	yaml.Yaml
	TestReportID		yaml.TestReport
}
