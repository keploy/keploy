package learn

import (
	"slices"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/wiremessage"
)

// sectionTokens are a poll's token as the recording sent it (rec) and as
// record's full round sent it (live): 72-bit words.
const (
	sectionTokenRec  = "4f1c9a7e2b6d4e8f90"
	sectionTokenLive = "b7d24e9a1c3f5e6d81"
)

// sectionFind is a find of the call's own test by ask, asked two seconds
// before the call, whose answer's first batch holds a document for each
// token, in order.
func sectionFind(name, ask string, tokens ...string) *models.Mock {
	docs := make([]string, len(tokens))
	for i, tk := range tokens {
		docs[i] = `{"token":"` + tk + `"}`
	}
	return &models.Mock{Name: name, Kind: models.Mongo, Spec: models.MockSpec{
		MongoRequests: []models.MongoRequest{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
			`{ SectionSingle msg: {"find":"orders","filter":{"_id":"` + ask + `"},"$db":"app"} }`}}}},
		MongoResponses: []models.MongoResponse{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
			`{ SectionSingle msg: {"cursor":{"firstBatch":[` + strings.Join(docs, ",") + `],"id":{"$numberLong":"0"},"ns":"app.orders"},"ok":{"$numberDouble":"1.0"}} }`}}}},
		ReqTimestampMock: oAt.Add(-2 * time.Second), ResTimestampMock: oAt.Add(-2*time.Second + time.Millisecond),
	}}
}

// sectionGet is sectionFind's HTTP twin: a GET of ask answered with a JSON
// list of the tokens, in order.
func sectionGet(name, ask string, tokens ...string) *models.Mock {
	docs := make([]string, len(tokens))
	for i, tk := range tokens {
		docs[i] = `{"token":"` + tk + `"}`
	}
	return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{
		HTTPReq:          &models.HTTPReq{Method: "GET", URL: "http://orders.internal/open/" + ask},
		HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: `{"items":[` + strings.Join(docs, ",") + `]}`},
		ReqTimestampMock: oAt.Add(-2 * time.Second), ResTimestampMock: oAt.Add(-2*time.Second + time.Millisecond),
	}}
}

// sectionFields are the fields eachField reads of m's payload, each as
// "path=text" ("path(key)=text" for a key).
func sectionFields(m *models.Mock) []string {
	b := RawOf(m, true).payload()
	var out []string
	eachField(b.text, b.fields, func(f fieldAt, text string) {
		k := ""
		if f.key {
			k = "(key)"
		}
		out = append(out, f.path+k+"="+text)
	})
	return out
}

// hasFieldEnding reports whether one of fields is at a path ending in
// suffix and holds text.
func hasFieldEnding(fields []string, suffix, text string) bool {
	return slices.ContainsFunc(fields, func(f string) bool { return strings.HasSuffix(f, suffix+"="+text) })
}

// A recorded Mongo section is read leaf by leaf, as an HTTP JSON body is:
// a body section's document below "msg", and a document sequence's
// identifier and each of its documents, by index, below "msgs". Before, the
// learn step read a section's text as one field, since it is not JSON (its
// frame is the recorder's), so a word that moved between two documents of
// one answer stood "at one field" in both runs. A section whose document
// is not wholly JSON, or holds no leaf, or whose frame is cut, is one field
// as before; a key is never read as JSON.
func TestAMongoSectionIsReadLeafByLeaf(t *testing.T) {
	find := sectionFields(sectionFind("mock-31", "q-1", sectionTokenRec, sectionTokenLive))
	for _, want := range [][2]string{
		{".msg.cursor.firstBatch[0].token", sectionTokenRec},
		{".msg.cursor.firstBatch[1].token", sectionTokenLive},
		{".msg.cursor.ns", "app.orders"},
		{".msg.find", "orders"},
	} {
		if !hasFieldEnding(find, want[0], want[1]) {
			t.Errorf("a body section: no field %s=%s in %q", want[0], want[1], find)
		}
	}
	if !hasFieldEnding(find, ".msg.cursor(key)", "cursor") {
		t.Errorf("a body section: its document's key cursor is not a key at .msg.cursor in %q", find)
	}
	for _, f := range find {
		if strings.Contains(f, "SectionSingle") {
			t.Errorf("a body section read whole: %q", f)
		}
	}

	seq := &models.Mock{Name: "mock-32", Kind: models.Mongo, Spec: models.MockSpec{
		MongoRequests: []models.MongoRequest{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
			`{ SectionSingle msg: {"insert":"orders","$db":"app"} }`,
			`{ SectionSingle identifier: documents , msgs: [ {"token":"` + sectionTokenRec + `"}, {"token":"` + sectionTokenLive + `"} ] }`}}}},
	}}
	got := sectionFields(seq)
	for _, want := range [][2]string{
		{".identifier", "documents"},
		{".msgs[0].token", sectionTokenRec},
		{".msgs[1].token", sectionTokenLive},
		{".msg.insert", "orders"},
	} {
		if !hasFieldEnding(got, want[0], want[1]) {
			t.Errorf("a document sequence: no field %s=%s in %q", want[0], want[1], got)
		}
	}

	// A document that is one scalar stands at "msg" itself.
	scalar := sectionFields(&models.Mock{Name: "mock-36", Kind: models.Mongo, Spec: models.MockSpec{
		MongoRequests: []models.MongoRequest{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
			`{ SectionSingle msg: "` + sectionTokenRec + `" }`}}}},
	}})
	if !slices.ContainsFunc(scalar, func(f string) bool { return strings.HasSuffix(f, ".msg="+sectionTokenRec) }) {
		t.Errorf("a body section of one scalar: not at .msg: %q", scalar)
	}

	// Each text eachSectionLeaf does not read is one field, whole.
	for name, section := range map[string]string{
		"a body section that is not JSON":         `{ SectionSingle msg: {"token": ` + sectionTokenRec + ` }`,
		"a body section of two JSON values":       `{ SectionSingle msg: {"a":"` + sectionTokenRec + `"} {"b":"c"} }`,
		"a body section with no leaf":             `{ SectionSingle msg: {} }`,
		"a body section with no closing frame":    `{ SectionSingle msg: {"token":"` + sectionTokenRec + `"}`,
		"a sequence with no closing frame":        `{ SectionSingle identifier: documents , msgs: [ {"token":"` + sectionTokenRec + `"} ]`,
		"a sequence with no documents' list":      `{ SectionSingle identifier: documents {"token":"` + sectionTokenRec + `"} ] }`,
		"a sequence cut inside its documents":     `{ SectionSingle identifier: documents , msgs: [ ] }`,
		"a sequence whose documents are not JSON": `{ SectionSingle identifier: documents , msgs: [ {"token": ` + sectionTokenRec + `} ] }`,
		"a text of no section form":               `SectionSingle msg: {"token":"` + sectionTokenRec + `"}`,
	} {
		m := &models.Mock{Name: "mock-33", Kind: models.Mongo, Spec: models.MockSpec{
			MongoRequests: []models.MongoRequest{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{section}}}},
		}}
		if got := sectionFields(m); !slices.ContainsFunc(got, func(f string) bool { return strings.HasSuffix(f, "="+section) }) {
			t.Errorf("%s: not one field whole: %q", name, got)
		}
	}
	if got := sectionFields(&models.Mock{Name: "mock-34", Kind: models.Mongo, Spec: models.MockSpec{
		MongoRequests: []models.MongoRequest{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
			`{ SectionSingle identifier: documents , msgs: [  ] }`}}}},
	}}); !slices.ContainsFunc(got, func(f string) bool { return strings.HasSuffix(f, "=[  ]") || strings.Contains(f, "msgs: [  ]") }) {
		t.Errorf("an empty sequence holds no leaf, so it is one field whole: %q", got)
	}

	// A key whose name reads as a section is a key, never a document.
	keyed := sectionFields(&models.Mock{Name: "mock-35", Kind: models.HTTP, Spec: models.MockSpec{
		HTTPReq: &models.HTTPReq{Method: "GET", URL: "http://orders.internal/open", Header: map[string]string{`{ SectionSingle msg: {"a":"b"} }`: "v"}},
	}})
	if !slices.ContainsFunc(keyed, func(f string) bool { return strings.HasSuffix(f, `(key)={ SectionSingle msg: {"a":"b"} }`) }) {
		t.Errorf("a key that reads as a section is not one key, whole: %q", keyed)
	}
}

// api-server's poll (the re-serve case): the test's own find answers a list
// of two documents, the token the recording's later update sent at the
// first and the token round F's update sent at the second, and round F
// served the find as recorded. The application read the second document in
// round F, so nothing ties the token to the first document's place: in one
// answer, the two runs' words stand at two leaves. Before, the learn step
// read the answer's section as one field, found both words "at one field"
// and bound the token to the first document's token, where a replay reads
// the recorded token while the application sends the second's: red at
// every replay, the test kept on a bind its own round F contradicts. Now
// the word is refused as moved (check "moved"), naming the leaf round F
// gave it at, in both KEPLOY_PROVEN modes. A find whose answer gave each
// run's token at its one document is still a source (the variable binds at
// that leaf). An HTTP JSON body, which was read leaf by leaf before, is
// decided the same both ways.
func TestAWordMovedBetweenDocumentsOfOneMongoAnswerIsRefusedAsMoved(t *testing.T) {
	const p = "body.u.token"
	scope := func(in Input) Input {
		in.Scope = func(string) Scope { return ScopeTest("test-2") }
		return in
	}
	for _, mode := range []string{"", "1"} {
		t.Run("proven="+mode, func(t *testing.T) {
			t.Setenv(provenswitch.Env, mode)
			for _, c := range []struct {
				name string
				of   func(name, ask string, tokens ...string) *models.Mock
				at   string // where round F gave its word
			}{
				{"a Mongo find", sectionFind, ".msg.cursor.firstBatch[1].token"},
				{"an HTTP GET", sectionGet, "items[1].token"},
			} {
				// Round F's view of the find, as the Mongo adapter states a
				// re-serve: round F's own ask, answered as recorded.
				both := RawOf(c.of("mock-31", "q-rec", sectionTokenRec, sectionTokenLive), true).ServedAs(3).ServedLive(RawOf(c.of("mock-31", "q-live", sectionTokenRec, sectionTokenLive), true))
				d := oWord(t, Decide(scope(oInput(p, sectionTokenRec, sectionTokenLive, nil, []Raw{both}))), sectionTokenRec, sectionTokenLive)
				oRefused(t, c.name+": the two runs' tokens at two documents of one answer", d, RefusalUnknown, "mock-31", c.at)
				if d.Refusal != nil && d.Refusal.Check != "moved" {
					t.Errorf("%s: refused by check %q; want moved", c.name, d.Refusal.Check)
				}

				one := RawOf(c.of("mock-31", "q-rec", sectionTokenRec), true).ServedAs(3).ServedLive(RawOf(c.of("mock-31", "q-live", sectionTokenLive), true))
				d = oWord(t, Decide(scope(oInput(p, sectionTokenRec, sectionTokenLive, nil, []Raw{one}))), sectionTokenRec, sectionTokenLive)
				if d.Outcome != OutcomeValue || d.From.Mock != "mock-31" {
					t.Errorf("FALSE RED: %s: each run's token at the one document of its answer: %s; want a value from mock-31", c.name, oDescribe([]Decision{d}))
				}
			}
		})
	}
}
