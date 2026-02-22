package matcher

import (
	"testing"

	"go.keploy.io/server/v3/config"
)

func TestCompareWithMatchersExactMatch(t *testing.T) {
	matchers := map[string]config.FieldMatcher{
		"user.id": {Type: "exact"},
	}

	expected := []byte(`{"user":{"id":1,"name":"alice"}}`)
	actual := []byte(`{"user":{"id":1,"name":"bob"}}`)

	if err := CompareWithMatchers(expected, actual, matchers); err != nil {
		t.Fatalf("expected match to succeed, got error: %v", err)
	}
}

func TestCompareWithMatchersMissingField(t *testing.T) {
	matchers := map[string]config.FieldMatcher{
		"user.id": {Type: "exact"},
	}

	expected := []byte(`{"user":{"id":1}}`)
	actual := []byte(`{"user":{}}`)

	if err := CompareWithMatchers(expected, actual, matchers); err == nil {
		t.Fatalf("expected missing field error, got nil")
	}
}

func TestCompareWithMatchersNonObjectRoot(t *testing.T) {
	matchers := map[string]config.FieldMatcher{
		"user.id": {Type: "exact"},
	}

	expected := []byte(`[{"user":{"id":1}}]`)
	actual := []byte(`[{"user":{"id":1}}]`)

	if err := CompareWithMatchers(expected, actual, matchers); err == nil {
		t.Fatalf("expected non-object root error, got nil")
	}
}

func TestCompareWithMatchersRegex(t *testing.T) {
	matchers := map[string]config.FieldMatcher{
		"user.id": {Type: "regex", Pattern: `^user-[0-9]+$`},
	}

	expected := []byte(`{"user":{"id":"ignored"}}`)
	actual := []byte(`{"user":{"id":"user-123"}}`)

	if err := CompareWithMatchers(expected, actual, matchers); err != nil {
		t.Fatalf("expected regex match to succeed, got error: %v", err)
	}
}

func TestCompareWithMatchersTolerance(t *testing.T) {
	matchers := map[string]config.FieldMatcher{
		"metrics.latency": {Type: "tolerance", Delta: 2.5},
	}

	expected := []byte(`{"metrics":{"latency":100}}`)
	actual := []byte(`{"metrics":{"latency":101.5}}`)

	if err := CompareWithMatchers(expected, actual, matchers); err != nil {
		t.Fatalf("expected tolerance match to succeed, got error: %v", err)
	}
}
