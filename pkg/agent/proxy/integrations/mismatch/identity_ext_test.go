package mismatch

import "testing"

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
