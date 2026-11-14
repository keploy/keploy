package models

import (
	"fmt"
	"strings"
	"testing"
)

// The canary of internal/logleak, written out: models' own tests cannot
// import a package that imports models.
const missLogCanary = "kpLogCanary7Qx3vZ9mW2"

// A miss is named in a log line by its shape: no value the call sent, in a
// path segment, a query, a literal or a command, whatever its look.
func TestMissLogShapeNamesNoValue(t *testing.T) {
	const s = missLogCanary
	for _, tc := range []struct {
		r    *MockMismatchReport
		want string
	}{
		{&MockMismatchReport{Protocol: "HTTP", ActualSummary: "GET /v1/reset/" + s + "?access_token=" + s}, "GET /{}/{}/{}"},
		{&MockMismatchReport{Protocol: "HTTP", ActualSummary: "GET //" + s + "/v1/me"}, "GET //{}/{}/{}"},
		{&MockMismatchReport{Protocol: "HTTP", ActualSummary: "POST /users/alice/orders"}, "POST /{}/{}/{}"},
		{&MockMismatchReport{Protocol: "HTTP", ActualSummary: "POST /graphql", Operation: "query GetAppLabels"}, "POST /{} {query GetAppLabels}"},
		{&MockMismatchReport{Protocol: "MySQL", ActualSummary: "COM_QUERY SELECT name FROM users WHERE token = '" + s + "'"}, ""},
		{&MockMismatchReport{Protocol: "gRPC", ActualSummary: "/shop.v1.Orders/Get"}, "/shop.v1.Orders/Get"},
		{&MockMismatchReport{Protocol: "Redis", ActualSummary: "GET session:" + s, Command: "GET"}, "GET"},
		{&MockMismatchReport{Protocol: "Generic", ActualSummary: "AUTH " + s}, ""},
		{nil, ""},
	} {
		got := MissLogShape(tc.r)
		if strings.Contains(got, s[:8]) {
			t.Errorf("MissLogShape = %q holds the call's value", got)
		}
		if tc.want != "" && got != tc.want {
			t.Errorf("MissLogShape = %q, want %q", got, tc.want)
		}
	}
}

// A miss's next step is logged as keploy's own advice, with every field path
// and quoted URL path it names in its shape: a body's key or a path segment
// can be a value the call sent.
func TestMissLogNextStepShapesWhatItNames(t *testing.T) {
	const s = missLogCanary
	r := &MockMismatchReport{
		Protocol: "HTTP",
		NextSteps: `Only values drifted (body.sessions.` + s + `.user): mark it under test.globalNoise.requestBody, e.g. {"body.sessions.` + s + `.user": []}. ` +
			`Or declare it (sessionCalls: [{host: "api.example.com", method: "PUT", pathPrefix: "/sessions/` + s + `"}]).`,
		FieldDiffs: []MockFieldDiff{{Path: "body.sessions." + s + ".user"}},
		Differs:    []string{"header.Authorization"},
	}
	got := MissLogNextStep(r)
	if strings.Contains(got, s[:8]) {
		t.Fatalf("MissLogNextStep holds a value the call sent: %q", got)
	}
	for _, want := range []string{"body.sessions.{}.user", `pathPrefix: "/{}/{}"`, "test.globalNoise.requestBody"} {
		if !strings.Contains(got, want) {
			t.Errorf("MissLogNextStep = %q, want it to say %q", got, want)
		}
	}
	differs := MissLogDiffers(r)
	if len(differs) != 2 || differs[0] != "header.Authorization" || differs[1] != "body.sessions.{}.user" {
		t.Errorf("MissLogDiffers = %q", differs)
	}
}

// A learn round's miss carries the learn step's line for a refused word
// (learn.RefusalLine), which quotes the recorded value and round F's, and
// whose why can quote a value too: a log line names the word's place in its
// shape, its kind, its places and its edits' kinds, never a value or the
// why. Advice's sessionCalls host and method stay. A next step whose quotes
// do not pair is withheld whole: what stands outside them may be a value.
func TestMissLogNextStepWithholdsARefusedWordsValues(t *testing.T) {
	const s = missLogCanary
	refusal := `mock-3 body.filter.` + s + ` word 2: recorded "` + s + `", round F "a\"` + s + `": refused (conflict): "` + s + `" stood whole at mock-1 body.token; round F gave "b` + s + `" there and the value did not follow. Places: mock-1 body.token. Edits: the app changed: fix it; or give it in the test: keploytest.Given(t, filter_token, v). refusal={"kind":"conflict","mock":"mock-3","field":"body.filter.` + s + `","word":2,"places":["mock-1 body.token","mock-2 body.sessions.` + s + `.user"],"edits":[{"kind":"fix-app"},{"kind":"give","name":"filter_token"}]}`
	r := &MockMismatchReport{
		Protocol:  "Mongo",
		NextSteps: "Refused: " + refusal + ` Or declare it (sessionCalls: [{host: "api.example.com", method: "GET", pathPrefix: "/v1/` + s + `"}]).`,
	}
	got := MissLogNextStep(r)
	if strings.Contains(got, s[:8]) {
		t.Fatalf("MissLogNextStep holds a value the call sent: %q", got)
	}
	want := `Refused: mock-3 body.filter.{} word 2: refused (conflict), its recorded and round F values withheld. Places: mock-1 body.token, mock-2 body.sessions.{}.user. Edits: fix-app; or give. Or declare it (sessionCalls: [{host: "api.example.com", method: "GET", pathPrefix: "/{}/{}"}]).`
	if got != want {
		t.Errorf("MissLogNextStep =\n%q, want\n%q", got, want)
	}
	const withheld = "withheld"
	for in, want := range map[string]string{
		// A line with no tail keeps nothing after its where.
		`mock-3 body.x: recorded "` + s + `", round F "` + s + `": refused (pick): ` + s: `mock-3 body.x: refused; its values, why and edits withheld`,
		`host: "` + s + `@evil"`:   `host: "{}"`,
		`set it to "` + s + `"`:    `set it to "{}"`,
		`method: "ABCDEFGHJKLMNP"`: `method: "{}"`,
		`{"header.X-Token": []}`:   `{"header.X-Token": []}`,
		// A host is kept only where a sessionCalls entry names it.
		`the test gave given.host: "` + s + `"`: `the test gave given.host: "{}"`,
		`{host: "api.example.com"}`:             `{host: "api.example.com"}`,
		// A header's name is kept only where it is one.
		`"header.Bearer ` + s + `"`:                        `"header.{}"`,
		`the answer's header "Content-Encoding" is "gzip"`: `the answer's header "Content-Encoding" is "{}"`,
		`"Content-Encoding"`:                               `"{}"`,
		// An English sentence's marks after a quote.
		`the "` + s + `"'s value changed, "` + s + `"!`: `the "{}"'s value changed, "{}"!`,
		// A next step that quotes in backquotes (a YAML error) is withheld.
		"cannot unmarshal `" + s + "` into string": withheld,
		`a "stray quote, then "` + s + `" bare`:    withheld,
		`a "stray quote: x, y " ` + s + `" z`:      withheld,
	} {
		if want == withheld {
			want = fmt.Sprintf("(a next step of %d bytes, withheld: its quotes do not pair, or it holds a backquote; the miss report has it)", len(in))
		}
		if got := MissLogNextStep(&MockMismatchReport{NextSteps: in}); got != want {
			t.Errorf("MissLogNextStep(%q) = %q, want %q", in, got, want)
		}
	}
}

// A call no mock answered is named in a sentence even where its shape cannot
// be told, by its protocol where that is a name, and never by its summary.
func TestUnmatchedCallLogShapeIsNeverEmpty(t *testing.T) {
	const s = missLogCanary
	for _, tc := range []struct {
		c    UnmatchedCall
		want string
	}{
		{UnmatchedCall{Protocol: "HTTP", ActualSummary: "GET /v1/" + s}, "GET /{}/{}"},
		{UnmatchedCall{ActualSummary: "GET /v1/" + s}, "a call"},
		{UnmatchedCall{Protocol: "Generic", ActualSummary: "AUTH " + s}, "a Generic call"},
		{UnmatchedCall{Protocol: s + "@x", ActualSummary: s}, "a call"},
	} {
		if got := UnmatchedCallLogShape(tc.c); got != tc.want {
			t.Errorf("UnmatchedCallLogShape(%s call) = %q, want %q", tc.c.Protocol, got, tc.want)
		}
	}
}

// A refused word's places are read as they are: a mock's name and a field in
// its shape, or {}; a tail the why holds is taken with the why, the line's
// own being the last; a line's full stop is not doubled.
func TestMissLogNextStepReadsARefusedLinesOwnTail(t *testing.T) {
	const s = missLogCanary
	line := `mock-3 body.k: recorded "a", round F "b": refused (pick): ` + s + ` refusal={"kind":"pick"} ` + s + `. Places: x. refusal={"kind":"pick","places":["x\"y.z ` + s + ` w.v","mock-1 body.a ` + s + `"]}. Each other line.`
	got := MissLogNextStep(&MockMismatchReport{NextSteps: line})
	if strings.Contains(got, s[:8]) {
		t.Fatalf("MissLogNextStep holds a value: %q", got)
	}
	if want := `mock-3 body.k: refused (pick), its recorded and round F values withheld. Places: {}, mock-1 body.{}. Each other line.`; got != want {
		t.Errorf("MissLogNextStep =\n%q, want\n%q", got, want)
	}
}

// A field list's words name fields by their shape; mock names and kinds stay.
func TestFieldPathsInTextShaped(t *testing.T) {
	got := FieldPathsInTextShaped([]string{"timer-mock docs.state (noise)", "mock-3 body.sessions." + missLogCanary + ".user (noise)", "variable id bound at body.id"})
	want := []string{"timer-mock docs.state (noise)", "mock-3 body.sessions.{}.user (noise)", "variable id bound at body.id"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("FieldPathsInTextShaped[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}
