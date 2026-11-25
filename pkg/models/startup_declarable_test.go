package models

import "testing"

// A plain startup: recording is declarable (StartupDeclarable) when a
// keploy.yaml sessionCalls entry can name its call: an HTTP or HTTP/2
// recording that states its lifetime, unless its API names each call in a
// header or the body behind one method and path (AWS JSON, AWS Query,
// GraphQL, JSON-RPC), each told by its precise marker alone; any recording
// of a registered kind but those; nothing else.
// Mutation: drop or loosen any marker, read a header name in one case only,
// declare an unstated lifetime, or ignore a registration.
func TestStartupDeclarableIsWhereASessionCallsEntryCanNameTheCall(t *testing.T) {
	stated := map[string]string{"type": string(HTTPClient), MetadataLifetimeKey: MetadataLifetimePerTest}
	h1 := func(method, ct, body string, extra map[string]string) *Mock {
		h := map[string]string{"Host": "api.example.com"}
		if ct != "" {
			h["content-type"] = ct
		}
		for k, v := range extra {
			h[k] = v
		}
		return &Mock{Kind: HTTP, Spec: MockSpec{Metadata: stated, HTTPReq: &HTTPReq{Method: Method(method), URL: "/", Header: h, Body: body}}}
	}
	form := "application/x-www-form-urlencoded; charset=utf-8"
	for _, tc := range []struct {
		name string
		m    *Mock
		want bool
	}{
		{"plain GET", h1("GET", "", "", nil), true},
		{"AWS JSON", h1("POST", "application/x-amz-json-1.0", `{"TableName":"t"}`, map[string]string{"X-Amz-Target": "DynamoDB_20120810.GetItem"}), false},
		{"AWS JSON, header in lower case", h1("POST", "", "{}", map[string]string{"x-amz-target": "AmazonSQS.ReceiveMessage"}), false},
		{"AWS Query", h1("POST", form, "Action=ReceiveMessage&Version=2012-11-05&QueueUrl=q", nil), false},
		{"a form with Action but no Version", h1("POST", form, "Action=login&user=a", nil), true},
		{"Action and Version, not a form", h1("POST", "text/plain", "Action=ReceiveMessage&Version=2012-11-05", nil), true},
		{"GraphQL", h1("POST", "application/json", ` {"query":"{ rates { id } }","variables":{}}`, nil), false},
		{"a JSON body whose query is no string", h1("POST", "application/json", `{"query":{"id":1}}`, nil), true},
		{"JSON-RPC", h1("POST", "application/json", `{"jsonrpc":"2.0","method":"eth_blockNumber","id":1}`, nil), false},
		{"a JSON array", h1("POST", "application/json", `[{"jsonrpc":"2.0"}]`, nil), true},
		{"a JSON object of neither", h1("POST", "application/json", `{"id":1,"name":"q"}`, nil), true},
		{"lifetime unstated", &Mock{Kind: HTTP, Spec: MockSpec{HTTPReq: &HTTPReq{Method: "GET", URL: "/"}}}, false},
		{"HTTP/2, stated", &Mock{Kind: HTTP2, Spec: MockSpec{Metadata: stated, HTTP2Req: &HTTP2Req{Method: "GET", URL: "/"}}}, true},
		{"HTTP/2, AWS JSON", &Mock{Kind: HTTP2, Spec: MockSpec{Metadata: stated, HTTP2Req: &HTTP2Req{Method: "POST", URL: "/", Headers: map[string]string{"x-amz-target": "Kinesis_20131202.PutRecord"}}}}, false},
		{"Mongo, unregistered", &Mock{Kind: Mongo}, false},
		{"nil", nil, false},
	} {
		if got := StartupDeclarable(tc.m); got != tc.want {
			t.Errorf("%s: StartupDeclarable = %v, want %v", tc.name, got, tc.want)
		}
	}

	unregister := RegisterStartupDeclarableKinds(Mongo, HTTP)
	defer unregister()
	if !StartupDeclarable(&Mock{Kind: Mongo}) {
		t.Error("a registered kind's recording is not declarable")
	}
	if StartupDeclarable(h1("POST", "", "{}", map[string]string{"X-Amz-Target": "DynamoDB_20120810.GetItem"})) {
		t.Error("an AWS JSON recording is declarable once HTTP is registered")
	}
	if !StartupDeclarable(&Mock{Kind: HTTP, Spec: MockSpec{HTTPReq: &HTTPReq{Method: "GET", URL: "/"}}}) {
		t.Error("an unstated HTTP recording is not declarable once HTTP is registered")
	}
}
