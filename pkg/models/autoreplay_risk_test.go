package models

import "testing"

func TestIsAutoReplayHighRisk(t *testing.T) {
	t.Run("zero value", func(t *testing.T) {
		if IsAutoReplayHighRisk(FailureInfo{}) {
			t.Fatal("zero-value failure is high risk")
		}
	})

	// Cover every current risk/category combination, including unknown values.
	// Categories alone other than schema changes must not widen this policy.
	risks := []RiskLevel{"", None, Low, Medium, High, "UNKNOWN"}
	categories := []FailureCategory{
		"", SchemaUnchanged, SchemaAdded, SchemaBroken, StatusCodeChanged,
		HeaderChanged, InternalFailure, AppConnectionError, DependencyMissing,
		CallUnmatched, CaptureIncompleteCategory, TemplateVariable,
		TestSetRefused, TestCaseNotFound, "UNKNOWN",
	}
	for _, risk := range risks {
		for _, category := range categories {
			t.Run(string(risk)+"/"+string(category), func(t *testing.T) {
				info := FailureInfo{Risk: risk, Category: []FailureCategory{category}}
				want := risk == High || category == SchemaAdded || category == SchemaBroken
				if got := IsAutoReplayHighRisk(info); got != want {
					t.Fatalf("IsAutoReplayHighRisk(%+v) = %v, want %v", info, got, want)
				}
			})
		}
	}

	for _, schema := range []FailureCategory{SchemaAdded, SchemaBroken} {
		for _, tc := range []struct {
			name       string
			categories []FailureCategory
		}{
			{name: "first", categories: []FailureCategory{schema, HeaderChanged}},
			{name: "second", categories: []FailureCategory{HeaderChanged, schema}},
			{name: "last", categories: []FailureCategory{SchemaUnchanged, HeaderChanged, schema}},
			{name: "duplicate", categories: []FailureCategory{schema, schema}},
		} {
			t.Run("mixed/"+string(schema)+"/"+tc.name, func(t *testing.T) {
				info := FailureInfo{Risk: Low, Category: tc.categories}
				if !IsAutoReplayHighRisk(info) {
					t.Fatalf("schema change in %+v was not protected", info)
				}
			})
		}
	}

	t.Run("multiple non-schema categories at non-high risk", func(t *testing.T) {
		info := FailureInfo{
			Risk:     Medium,
			Category: []FailureCategory{SchemaUnchanged, HeaderChanged, DependencyMissing},
		}
		if IsAutoReplayHighRisk(info) {
			t.Fatalf("non-schema categories at medium risk %+v should not be high risk", info)
		}
	})

	for _, tc := range []struct {
		name       string
		categories []FailureCategory
	}{
		{name: "nil", categories: nil},
		{name: "empty", categories: []FailureCategory{}},
		{name: "non-schema", categories: []FailureCategory{SchemaUnchanged, HeaderChanged}},
	} {
		t.Run("high risk without schema change/"+tc.name, func(t *testing.T) {
			info := FailureInfo{Risk: High, Category: tc.categories}
			if !IsAutoReplayHighRisk(info) {
				t.Fatalf("high risk in %+v was not protected", info)
			}
		})
	}
}
