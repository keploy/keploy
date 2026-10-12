package mismatch

import (
	"strings"
	"testing"
)

func TestExtractReqIdentity(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"apollo graphql poll",
			`{"operationName":"GetAllGenJobs","variables":{"cid":"keploy.io"},"query":"query GetAllGenJobs {...}"}`,
			"operationName=GetAllGenJobs"},
		{"operationName last",
			`{"variables":{},"query":"query X {...}","operationName":"MyNotifications"}`,
			"operationName=MyNotifications"},
		{"no identity key", `{"variables":{},"query":"query {...}"}`, ""},
		{"not json", `<html>nope</html>`, ""},
		{"empty", ``, ""},
		{"null operationName", `{"operationName":null,"query":"q"}`, ""},
		{"json-rpc", `{"method":"eth_call","params":[]}`, "method=eth_call"},
		{"non-string value is skipped", `{"operationName":42}`, ""},
		{"array body", `[{"operationName":"Batch"}]`, ""},
	}
	for _, c := range cases {
		if got := ExtractReqIdentity([]byte(c.body)); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// TestBuilderCarriesReqIdentity pins the WIRING, not the extractor. The
// extractor passed its own unit test while the builder call was missing from
// the main report path, so every real miss reported no identity at all --
// exactly the case this exists to catch.
func TestBuilderCarriesReqIdentity(t *testing.T) {
	body := []byte(`{"operationName":"GetAllGenJobs","variables":{}}`)
	r := NewReport(ProtocolHTTP, "POST /query").
		WithReqIdentity(ExtractReqIdentity(body)).
		WithDestination("localhost:8083").
		Build()
	if r.ReqIdentity != "operationName=GetAllGenJobs" {
		t.Fatalf("report lost ReqIdentity: got %q", r.ReqIdentity)
	}
}

// A body past 64 KB has its identity too. The HTTP matcher's identity gate asks
// it, and a 134 KB GenerateAPITests mutation that had none was fuzzy-matched
// against every GraphQL operation in the pool: 77 s for one request.
func TestExtractReqIdentity_LargeBody(t *testing.T) {
	big := strings.Repeat("x", 200<<10)
	cases := []struct{ name, body, want string }{
		{"identity before a big value",
			`{"operationName":"GenerateAPITests","variables":{"schema":"` + big + `"},"query":"mutation GenerateAPITests { generateAPITests }"}`,
			"operationName=GenerateAPITests"},
		{"identity after a big value",
			`{"variables":{"schema":"` + big + `"},"operationName":"GenerateAPITests"}`,
			"operationName=GenerateAPITests"},
		{"a nested operationName is not the call's",
			`{"variables":{"operationName":"Inner","schema":"` + big + `"}}`,
			""},
		{"a later duplicate wins, as json.Unmarshal's does",
			`{"operationName":"First","operationName":"Second"}`,
			"operationName=Second"},
		{"a non-string duplicate clears it",
			`{"operationName":"First","operationName":7}`,
			""},
		{"trailing bytes are not JSON", `{"operationName":"X"} junk`, ""},
		{"cut short", `{"operationName":"X","variables":{"schema":"` + big, ""},
	}
	for _, c := range cases {
		if got := ExtractReqIdentity([]byte(c.body)); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}
