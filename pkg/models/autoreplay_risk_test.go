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
		HeaderChanged, InternalFailure, AppConnectionError, DependencyMissing, "UNKNOWN",
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
		for _, categories := range [][]FailureCategory{
			{schema, HeaderChanged},
			{HeaderChanged, schema},
			{SchemaUnchanged, HeaderChanged, schema},
			{schema, schema},
		} {
			t.Run("mixed/"+string(schema), func(t *testing.T) {
				info := FailureInfo{Risk: Low, Category: categories}
				if !IsAutoReplayHighRisk(info) {
					t.Fatalf("schema change in %+v was not protected", info)
				}
			})
		}
	}

	for _, categories := range [][]FailureCategory{nil, {}, {SchemaUnchanged, HeaderChanged}} {
		t.Run("high risk without schema change", func(t *testing.T) {
			info := FailureInfo{Risk: High, Category: categories}
			if !IsAutoReplayHighRisk(info) {
				t.Fatalf("high risk in %+v was not protected", info)
			}
		})
	}
}
