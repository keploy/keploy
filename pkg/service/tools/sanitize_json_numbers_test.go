package tools

import (
	"encoding/json"
	"strings"
	"testing"
)

// A body that holds a secret is decoded and written back, and everything in
// it that is not the secret must come back as it was: a 64-bit id, a decimal
// with a trailing zero and the characters "<", ">" and "&" are not secrets.
func TestRedactJSONStringKeepsWhatIsNotASecret(t *testing.T) {
	secret := "fake-secret-value-for-tests-0001"
	secretSet := map[string]struct{}{secret: {}}

	tests := []struct {
		name string
		in   string
		// keep are the substrings the output must still hold, byte for byte.
		keep []string
	}{
		{
			name: "integer above 2^53",
			in:   `{"id":12345678901234567890,"n":9007199254740993,"token":"` + secret + `"}`,
			keep: []string{`"id":12345678901234567890`, `"n":9007199254740993`},
		},
		{
			name: "decimal and exponent literals",
			in:   `{"price":10.50,"rate":1.0,"big":1e3,"token":"` + secret + `"}`,
			keep: []string{`"price":10.50`, `"rate":1.0`, `"big":1e3`},
		},
		{
			name: "html characters in a value",
			in:   `{"html":"<b>a & b</b>","token":"` + secret + `"}`,
			keep: []string{`"html":"<b>a & b</b>"`},
		},
		{
			name: "numbers inside arrays and nested objects",
			in:   `{"items":[{"id":9223372036854775807}],"token":"` + secret + `"}`,
			keep: []string{`"id":9223372036854775807`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			changed, out, repls := redactJSONString(tc.in, "body", secretSet, map[string]string{}, map[string]string{}, "test-1")
			if !changed {
				t.Fatalf("a body holding a secret was not redacted: %s", out)
			}
			if strings.Contains(out, secret) {
				t.Fatalf("the secret is still in the body: %s", out)
			}
			if len(repls) != 1 {
				t.Fatalf("want one replacement, got %d", len(repls))
			}
			if !json.Valid([]byte(out)) {
				t.Fatalf("the redacted body is not JSON: %s", out)
			}
			for _, k := range tc.keep {
				if !strings.Contains(out, k) {
					t.Errorf("the redacted body lost %s:\n%s", k, out)
				}
			}
		})
	}
}

// A string that holds more than one JSON document is not one JSON value;
// it was left as it was, and still is, with its trailing bytes.
func TestRedactJSONStringLeavesTrailingDataAlone(t *testing.T) {
	secret := "fake-secret-value-for-tests-0001"
	secretSet := map[string]struct{}{secret: {}}
	in := `{"token":"` + secret + `"} {"x":1}`
	changed, out, _ := redactJSONString(in, "body", secretSet, map[string]string{}, map[string]string{}, "test-1")
	if changed || out != in {
		t.Fatalf("want the string untouched, got changed=%v %q", changed, out)
	}
}
