package schema

import "testing"

// TestSameSchemaType covers the cross-version case: contracts generated before
// integer inference type every JSON number as "number", and `keploy contract
// download` copies another service's generated schema verbatim rather than
// regenerating it. A provider on an older keploy and a consumer on a newer one
// therefore hold both spellings of the same field, and scoring that as a
// mismatch reports the mock as MISSED for a single-property schema.
func TestSameSchemaType(t *testing.T) {
	tests := []struct {
		name string
		a, b interface{}
		want bool
	}{
		{name: "identical", a: "string", b: "string", want: true},
		{name: "integer vs number", a: "integer", b: "number", want: true},
		{name: "number vs integer", a: "number", b: "integer", want: true},
		{name: "integer vs integer", a: "integer", b: "integer", want: true},
		{name: "number vs string is still a mismatch", a: "number", b: "string", want: false},
		{name: "integer vs string is still a mismatch", a: "integer", b: "string", want: false},
		{name: "integer vs boolean is still a mismatch", a: "integer", b: "boolean", want: false},
		{name: "object vs array is still a mismatch", a: "object", b: "array", want: false},
		{name: "missing types compare equal", a: nil, b: nil, want: true},
		{name: "missing vs number is a mismatch", a: nil, b: "number", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sameSchemaType(tt.a, tt.b); got != tt.want {
				t.Errorf("sameSchemaType(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
