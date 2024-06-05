package models

type UnitTestResult struct {
	Status   string `yaml:"status"`
	Reason   string `yaml:"reason"`
	ExitCode int    `yaml:"exit_code"`
	Stderr   string `yaml:"stderr"`
	Stdout   string `yaml:"stdout"`
	Test     string `yaml:"test"`
}

type UnitTestsDetails struct {
	Language                       string     `yaml:"language"`
	ExistingTestsFunctionSignature string     `yaml:"existing_test_function_signature"`
	NewTests                       []UnitTest `yaml:"new_tests"`
}

type UnitTest struct {
	TestBehavior   string `yaml:"test_behavior"`
	TestName       string `yaml:"test_name"`
	TestCode       string `yaml:"test_code"`
	NewImportsCode string `yaml:"new_imports_code"`
	TestsTags      string `yaml:"tests_tags"`
}

type FailedUnitTest struct {
	TestCode string `yaml:"test_code"`
	ErrorMsg string `yaml:"error_msg"`
}

type UnitTestsIndentation struct {
	Language               string `yaml:"language"`
	TestingFramework       string `yaml:"testing_framework"`
	NumberOfTests          int    `yaml:"number_of_tests"`
	TestHeadersIndentation int    `yaml:"test_headers_indentation"`
}

type UnitTestInsertionDetails struct {
	Language                        string `yaml:"language"`
	TestingFramework                string `yaml:"testing_framework"`
	NumberOfTests                   int    `yaml:"number_of_tests"`
	RelevantLineNumberToInsertAfter int    `yaml:"relevant_line_number_to_insert_after"`
}
