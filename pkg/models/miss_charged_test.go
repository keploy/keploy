package models

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// sessionCallMiss is a call a test made that a recording made outside every
// test answered earlier at the replay, as keploy's HTTP parser reports it
// under the proven switch (a lane's msc-prefix and msc-none sections): its
// next step ends with the repeat note's hint, which quotes the sessionCalls
// entry that would serve the call, its path whole. path is the call's path,
// hint the parser's word on serving it.
func sessionCallMiss(path, hint string) UnmatchedCall {
	return UnmatchedCall{
		Protocol: "HTTP", ActualSummary: "GET " + path, Destination: "127.0.0.1:18094", PathTag: "#1f2e3d4c5b6a",
		ClosestMock: "mock-0", MatchPhase: "no_mocks", Kind: MissInTest,
		ReceivedReq: "GET " + path + " HTTP/1.1\r\nAuthorization: Bearer " + missLogCanary + "\r\n\r\n",
		NextSteps:   "This request is identical to mock-0, a recording made outside every test (in TestMain or at start-up), which already answered a call at this replay. " + hint,
		Repeat:      &RepeatNote{Mock: "mock-0", OutsideTests: true, ReplayGapSeconds: 0.14, Hint: hint},
	}
}

// prefixHint and noneHint are the hints of a lane's msc-prefix and msc-none
// sections: a sessionCalls entry that covers the call only by a prefix, and
// none at all.
func prefixHint(path string) string {
	return `A keploy.yaml sessionCalls entry covers this call only by a path prefix or with no method. Under the proven switch a recording made outside every test (in TestMain or at start-up) is served to every test only for a call an entry names exactly, by its method and whole path, so that no recording is served to a call no one declared: to serve this one so, declare sessionCalls: [{host: "127.0.0.1:18094", method: "GET", pathPrefix: "` + path + `"}]. A recording another test made of it is served to that test only, declared or not. Or record again with 'keploy mock record' so this test records it.`
}

func noneHint(path string) string {
	return `Each test is served the HTTP calls it recorded. If the application made this call outside every test (in TestMain or at start-up) and every test may make it (a token, a shared configuration read), declare it in keploy.yaml to serve that recording to every test (sessionCalls: [{host: "127.0.0.1:18094", method: "GET", pathPrefix: "` + path + `"}]); a recording another test made of it is served to that test only, declared or not. Or record again with 'keploy mock record' so this test records it.`
}

// A miss the agent charged to a test keeps the call and its next step whole
// for the console alone (Charged): in Whole, which no YAML file holds and
// Sanitized never copies, beside its tags; every field a receipt or a log is
// written from is as evidence keeps it, and no full request is kept.
func TestAChargedMissIsWholeForTheConsoleAlone(t *testing.T) {
	path := "/echo/" + missLogCanary
	for _, tc := range []struct{ name, hint string }{
		{"a prefix entry", prefixHint(path)},
		{"no entry", noneHint(path)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := sessionCallMiss(path, tc.hint)
			c := m.Charged()
			if c.Whole == nil || c.Whole.Call != "GET "+path || c.Whole.NextStep != m.NextSteps {
				t.Fatalf("the charged miss's whole call is %+v, want %q and its next step whole", c.Whole, "GET "+path)
			}
			if c.PathTag != m.PathTag || c.ReceivedReq != "" || c.ClosestMockReq != "" {
				t.Errorf("the charged miss keeps tag %q, request %q, closest %q: want its tag and no request", c.PathTag, c.ReceivedReq, c.ClosestMockReq)
			}
			for name, f := range map[string]string{"summary": c.ActualSummary, "next step": c.NextSteps, "hint": c.Repeat.Hint} {
				if strings.Contains(f, missLogCanary[:8]) {
					t.Errorf("the charged miss's %s names the path whole: %q", name, f)
				}
			}
			if !strings.Contains(c.Repeat.Hint, `pathPrefix: "/{}/{}"`) {
				t.Errorf("the charged miss's hint is %q, want the entry's path in its shape", c.Repeat.Hint)
			}
			if again := c.Charged(); again.Whole == nil || *again.Whole != *c.Whole {
				t.Errorf("a charged miss charged again keeps %+v, want %+v", again.Whole, c.Whole)
			}
			y, err := yaml.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(y), missLogCanary[:8]) {
				t.Errorf("a YAML file holds the charged miss's whole call:\n%s", y)
			}
			s := c.Sanitized()
			if s.Whole != nil || s.PathTag != "" || s.HostTag != "" {
				t.Errorf("evidence of a charged miss keeps whole %+v, tags %q %q", s.Whole, s.PathTag, s.HostTag)
			}
			j, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(j), missLogCanary[:8]) {
				t.Errorf("evidence of a charged miss names the path whole: %s", j)
			}
		})
	}
}

// A miss read from the proxy holds its call whole in its own fields: charged,
// its whole call is them; one with no next step has none.
func TestAChargedMissNamesItsOwnFieldsWhole(t *testing.T) {
	m := UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /v1/" + missLogCanary, Operation: "createUser"}
	if c := m.Charged(); c.Whole == nil || c.Whole.Call != "POST /v1/"+missLogCanary+" {createUser}" || c.Whole.NextStep != "" {
		t.Errorf("the charged miss's whole call is %+v", c.Whole)
	}
}
