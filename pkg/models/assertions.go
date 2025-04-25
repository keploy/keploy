package models

// AssertionType defines a custom type for supported assertion keys.
type AssertionType string

const (
	NoiseAssertion  AssertionType = "noise"
	StatusCode      AssertionType = "status_code"
	StatusCodeClass AssertionType = "status_code_class"
	StatusCodeIn    AssertionType = "status_code_in"
	HeaderEqual     AssertionType = "header_equal"
	HeaderContains  AssertionType = "header_contains"
	HeaderExists    AssertionType = "header_exists"
	HeaderMatches   AssertionType = "header_matches"
	JsonEqual       AssertionType = "json_equal"
	JsonContains    AssertionType = "json_contains"
)
