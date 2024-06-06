package models

import "encoding/xml"

type UTResult struct {
	Status   string `yaml:"status"`
	Reason   string `yaml:"reason"`
	ExitCode int    `yaml:"exit_code"`
	Stderr   string `yaml:"stderr"`
	Stdout   string `yaml:"stdout"`
	Test     string `yaml:"test"`
}

type UTDetails struct {
	Language      string `yaml:"language"`
	TestSignature string `yaml:"existing_test_function_signature"`
	NewTests      []UT   `yaml:"new_tests"`
}

type UT struct {
	TestBehavior   string `yaml:"test_behavior"`
	TestName       string `yaml:"test_name"`
	TestCode       string `yaml:"test_code"`
	NewImportsCode string `yaml:"new_imports_code"`
	TestsTags      string `yaml:"tests_tags"`
}

type FailedUT struct {
	TestCode string `yaml:"test_code"`
	ErrorMsg string `yaml:"error_msg"`
}

type UTIndentationInfo struct {
	Language         string `yaml:"language"`
	TestingFramework string `yaml:"testing_framework"`
	NumberOfTests    int    `yaml:"number_of_tests"`
	Indentation      int    `yaml:"test_headers_indentation"`
}

type UTInsertionInfo struct {
	Language         string `yaml:"language"`
	TestingFramework string `yaml:"testing_framework"`
	NumberOfTests    int    `yaml:"number_of_tests"`
	Line             int    `yaml:"relevant_line_number_to_insert_after"`
}

type CoverageResult struct {
	LinesCovered  []int
	LinesMissed   []int
	Coverage      float64
	Files         []string
	ReportContent string
}

type Cobertura struct {
	XMLName  xml.Name  `xml:"coverage"`
	Sources  []string  `xml:"sources>source"`
	Packages []Package `xml:"packages>package"`
}

type Package struct {
	Name    string  `xml:"name,attr"`
	Classes []Class `xml:"classes>class"`
}

type Class struct {
	Name     string `xml:"name,attr"`
	FileName string `xml:"filename,attr"`
	Lines    []Line `xml:"lines>line"`
}

type Line struct {
	Number int `xml:"number,attr"`
	Hits   int `xml:"hits,attr"`
}
