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

type RefactorDetails struct {
	Language             string `yaml:"language" json:"language"`
	RefactoredSourceCode string `yaml:"refactored_source_code" json:"refactored_source_code"`
}

type UT struct {
	TestBehavior            string `yaml:"test_behavior"`
	TestName                string `yaml:"test_name"`
	TestCode                string `yaml:"test_code"`
	NewImportsCode          string `yaml:"new_imports_code"`
	LibraryInstallationCode string `yaml:"library_installation_code"`
	TestsTags               string `yaml:"tests_tags"`
}

type FailedUT struct {
	TestCode                string `yaml:"test_code"`
	ErrorMsg                string `yaml:"error_msg"`
	NewImportsCode          string `yaml:"imports_code"`
	LibraryInstallationCode string `yaml:"library_installation_code"`
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

type Jacoco struct {
	Name        string          `xml:"name,attr"`
	XMLName     xml.Name        `xml:"report"`
	Packages    []JacocoPackage `xml:"package"`
	SessionInfo []SessionInfo   `xml:"sessioninfo"`
}

type SessionInfo struct {
	ID    string `xml:"id,attr"`
	Start string `xml:"start,attr"`
	Dump  string `xml:"dump,attr"`
}

type JacocoPackage struct {
	Name        string             `xml:"name,attr"`
	Classes     []JacocoClass      `xml:"class"`
	Counters    []Counter          `xml:"counter"`
	SourceFiles []JacocoSourceFile `xml:"sourcefile"` // Adding this field to capture source files
}

type JacocoSourceFile struct {
	Name     string       `xml:"name,attr"`
	Lines    []JacocoLine `xml:"line"`
	Counters []Counter    `xml:"counter"`
}

type JacocoClass struct {
	Name       string         `xml:"name,attr"`
	SourceFile string         `xml:"sourcefilename,attr"`
	Methods    []JacocoMethod `xml:"method"`
	Lines      []JacocoLine   `xml:"line"` // This is where JacocoLine is used
}

type JacocoMethod struct {
	Name       string    `xml:"name,attr"`
	Descriptor string    `xml:"desc,attr"`
	Line       string    `xml:"line,attr"`
	Counters   []Counter `xml:"counter"`
}

type JacocoLine struct {
	Number              string `xml:"nr,attr"`
	MissedInstructions  string `xml:"mi,attr"`
	CoveredInstructions string `xml:"ci,attr"`
	MissedBranches      string `xml:"mb,attr"`
	CoveredBranches     string `xml:"cb,attr"`
}

type Counter struct {
	Type    string `xml:"type,attr"`
	Missed  string `xml:"missed,attr"`
	Covered string `xml:"covered,attr"`
}
