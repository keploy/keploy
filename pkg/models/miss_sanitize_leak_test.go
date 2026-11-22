package models

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Evidence keeps a miss with no value of the call (UnmatchedCall.Sanitized):
// not a URL path its next step quotes (an HTTP/2 session-call hint's
// pathPrefix, or the one a repeat note's hint quotes), not a learn step's
// refused values in its next step, and not
// the value a MongoDB command that runs on no collection names (createUser's
// user), which a shape read as the collection.
func TestEvidenceKeepsNoValueOfTheCall(t *testing.T) {
	const s = missLogCanary
	for _, tc := range []struct {
		name string
		m    UnmatchedCall
	}{
		{"an HTTP/2 session-call hint", UnmatchedCall{Protocol: "HTTP2", ActualSummary: "POST /v1/" + s,
			NextSteps: `declare it (sessionCalls: [{host: "api.example.com", method: "POST", pathPrefix: "/v1/` + s + `"}])`}},
		{"a learn round's refused word", UnmatchedCall{Protocol: "MongoDB", ActualSummary: "find on shop.users",
			NextSteps: `mock-3 body.filter.token: recorded "` + s + `", round F "a` + s + `": refused (conflict): "` + s + `" stood whole at mock-1 body.token. Places: mock-1 body.token. refusal={"kind":"conflict","mock":"mock-3","field":"body.filter.token","places":["mock-1 body.token"]}`}},
		{"a createUser", UnmatchedCall{Protocol: "MongoDB", ActualSummary: "createUser", ClosestMock: "mock-1",
			ReceivedReq: `{"cmd":{"createUser":"useralice","pwd":"` + s + `","roles":[],"$db":"admin"}}`}},
		{"a createUser named by its summary", UnmatchedCall{Protocol: "MongoDB", ActualSummary: "createUser on admin." + "useralice"}},
		{"a repeat note's session-call hint, by a prefix entry", sessionCallMiss("/echo/"+s, prefixHint("/echo/"+s))},
		{"a repeat note's session-call hint, with no entry", sessionCallMiss("/echo/"+s, noneHint("/echo/"+s))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.m.Sanitized()
			// The miss as a receipt writes it, every field.
			written, err := yaml.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range []string{got.ActualSummary, got.NextSteps, MissShape(tc.m), string(written)} {
				if strings.Contains(f, s[:8]) || strings.Contains(f, "useralice") {
					t.Errorf("evidence keeps a value of the call: %q", f)
				}
			}
		})
	}
}
