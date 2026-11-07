package mocknoise

import (
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

const (
	recKeyID  = "5b6f1c2e-8d3a-4f7b-9e21-0c4d5e6f7a8b"
	liveKeyID = "e2a4c6f8-1b3d-4e5f-8a7c-9d0e1f2a3b4c"
)

func mongoMsg(doc string) string { return "{ SectionSingle msg: " + doc + " }" }

func mongoMock(req, resp string) *models.Mock {
	return &models.Mock{
		Name: "mock-claim",
		Kind: models.Mongo,
		Spec: models.MockSpec{
			MongoRequests:  []models.MongoRequest{{Message: &models.MongoOpMessage{Sections: []string{mongoMsg(req)}}}},
			MongoResponses: []models.MongoResponse{{Message: &models.MongoOpMessage{Sections: []string{mongoMsg(resp)}}}},
		},
	}
}

// claimRequest is the shape a session registry claims a slot with: a
// findAndModify upsert keyed by the session, writing the key the application
// minted on this call (key_id), its owner, scopes and clock-read times.
func claimRequest(keyID string) string {
	return `{"findAndModify":"web_session_tokens","query":{"_id":"cid-1:user-1:jti-1","expires_at":{"$lte":{"$date":{"$numberLong":"1793337012000"}}}},` +
		`"update":{"$set":{"cid":"cid-1","created_by":"user-1","expires_at":{"$date":{"$numberLong":"1793340612000"}},"key_id":"` + keyID + `","scopes":["read","write","admin"],"updated_at":{"$date":{"$numberLong":"1793337012000"}}}},` +
		`"upsert":true,"new":true,"$db":"api"}`
}

// claimAnswer is the upsert's answer: the document it wrote, key_id included.
func claimAnswer(keyID string) string {
	return `{"lastErrorObject":{"n":1,"updatedExisting":false,"upserted":"cid-1:user-1:jti-1"},` +
		`"value":{"_id":"cid-1:user-1:jti-1","cid":"cid-1","created_by":"user-1","key_id":"` + keyID + `","scopes":["read","write","admin"]},"ok":1}`
}

// A findAndModify upsert that claims a slot with a key the application minted
// answers with the document it wrote. The minted key is the one value seen
// twice, written and answered, so it is the one correlation; the filter's
// key, the clock-read times, the owner and the scopes are not.
func TestAMongoUpsertsEchoedMintedKeyIsACorrelation(t *testing.T) {
	got := CorrelationsFromMock(mongoMock(claimRequest(recKeyID), claimAnswer(recKeyID)))
	want := []models.FieldCorrelation{{
		RequestPath:   "section.0.update.$set.key_id",
		ResponsePaths: []string{"section.0.value.key_id"},
		RecordedValue: recKeyID,
		ValueClass:    "uuid",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("correlations = %+v\nwant %+v", got, want)
	}
}

// A value seen once is not a variable: a minted key the answer does not echo,
// a key only the answer holds, and a key in the filter (the key of a lookup,
// which another key's lookup must not be answered with) give no correlation.
func TestAMongoValueSeenOnceOrInAFilterIsNotACorrelation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		req, resp string
	}{
		{"written, not echoed", claimRequest(recKeyID), `{"lastErrorObject":{"n":1,"updatedExisting":false},"ok":1}`},
		{"answered, not written", claimRequest("not-a-minted-key"), claimAnswer(recKeyID)},
		{"a lookup's filter key echoed", `{"find":"web_session_tokens","filter":{"key_id":"` + recKeyID + `"},"$db":"api"}`,
			`{"cursor":{"firstBatch":[{"key_id":"` + recKeyID + `"}],"id":0,"ns":"api.web_session_tokens"},"ok":1}`},
		{"findAndModify's query key echoed", `{"findAndModify":"web_session_tokens","query":{"key_id":"` + recKeyID + `"},"remove":true,"$db":"api"}`,
			`{"value":{"key_id":"` + recKeyID + `"},"ok":1}`},
		{"a written word that is not random", `{"findAndModify":"t","query":{"_id":1},"update":{"$set":{"state":"ACTIVE"}},"upsert":true,"new":true}`,
			`{"value":{"_id":1,"state":"ACTIVE"},"ok":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CorrelationsFromMock(mongoMock(tc.req, tc.resp)); len(got) != 0 {
				t.Fatalf("want no correlation, got %+v", got)
			}
		})
	}
}

// An update command's update document and an inserted document are written
// places too; the update's filter (q) is not.
func TestAMongoUpdateAndInsertWritePlacesAreRead(t *testing.T) {
	upd := mongoMock(`{"update":"t","updates":[{"q":{"_id":"k"},"u":{"$set":{"token":"`+recKeyID+`"}},"upsert":true}]}`,
		`{"n":1,"upserted":[{"index":0,"_id":"`+recKeyID+`"}],"ok":1}`)
	if got := CorrelationsFromMock(upd); len(got) != 1 || got[0].RequestPath != "section.0.updates.u.$set.token" {
		t.Fatalf("update: correlations = %+v, want one at section.0.updates.u.$set.token", got)
	}
	ins := mongoMock(`{"insert":"t","documents":[{"_id":"`+recKeyID+`"}]}`, `{"n":1,"inserted":"`+recKeyID+`","ok":1}`)
	if got := CorrelationsFromMock(ins); len(got) != 1 || got[0].RequestPath != "section.0.documents._id" {
		t.Fatalf("insert: correlations = %+v, want one at section.0.documents._id", got)
	}
	q := mongoMock(`{"update":"t","updates":[{"q":{"_id":"`+recKeyID+`"},"u":{"$set":{"n":1}}}]}`,
		`{"n":1,"upserted":[{"index":0,"_id":"`+recKeyID+`"}],"ok":1}`)
	if got := CorrelationsFromMock(q); len(got) != 0 {
		t.Fatalf("update filter: want no correlation, got %+v", got)
	}
}

// A place that holds several values (an update of two statements, each
// writing its own minted key) is not read: which live value goes with which
// recorded one would rest on the statements' order.
func TestAMongoPlaceWithSeveralValuesIsNotACorrelation(t *testing.T) {
	const other = "0f1e2d3c-4b5a-4968-8776-a5b4c3d2e1f0"
	m := mongoMock(`{"update":"t","updates":[{"q":{"_id":"a"},"u":{"$set":{"token":"`+recKeyID+`"}},"upsert":true},`+
		`{"q":{"_id":"b"},"u":{"$set":{"token":"`+other+`"}},"upsert":true}]}`,
		`{"n":2,"upserted":[{"index":0,"_id":"`+recKeyID+`"},{"index":1,"_id":"`+other+`"}],"ok":1}`)
	if got := CorrelationsFromMock(m); len(got) != 0 {
		t.Fatalf("want no correlation, got %+v", got)
	}
	// Every value at the place counts, whatever it is and wherever it comes.
	list := mongoMock(`{"findAndModify":"t","query":{"_id":"k"},"update":{"$set":{"keys":[5,"`+recKeyID+`"]}},"new":true}`,
		`{"value":{"keys":[5,"`+recKeyID+`"]},"ok":1}`)
	if got := CorrelationsFromMock(list); len(got) != 0 {
		t.Fatalf("a number before the key: want no correlation, got %+v", got)
	}
}

// A mock that is not one OP_MSG request and answer, or whose sections are not
// body sections, gives no correlation.
func TestAMongoMockOfAnotherShapeHasNoCorrelation(t *testing.T) {
	two := mongoMock(claimRequest(recKeyID), claimAnswer(recKeyID))
	two.Spec.MongoRequests = append(two.Spec.MongoRequests, two.Spec.MongoRequests[0])
	seq := mongoMock(claimRequest(recKeyID), claimAnswer(recKeyID))
	seq.Spec.MongoRequests[0].Message = &models.MongoOpMessage{Sections: []string{
		"{ SectionSingle identifier: documents , msgs: [ {\"_id\":\"" + recKeyID + "\"} ] }"}}
	twoAnswers := mongoMock(claimRequest(recKeyID), claimAnswer(recKeyID))
	twoAnswers.Spec.MongoResponses = append(twoAnswers.Spec.MongoResponses, twoAnswers.Spec.MongoResponses[0])
	query := mongoMock(claimRequest(recKeyID), claimAnswer(recKeyID))
	query.Spec.MongoRequests[0].Message = &models.MongoOpQuery{}
	for name, m := range map[string]*models.Mock{"two requests": two, "two answers": twoAnswers, "a document sequence": seq, "an OP_QUERY": query} {
		if got := CorrelationsFromMock(m); len(got) != 0 {
			t.Errorf("%s: want no correlation, got %+v", name, got)
		}
	}
}

// switchOff pins the proven switch off: these rows read correlations the way
// the agent materializes them, which it does only with the switch off.
func switchOff(t *testing.T) { t.Setenv(provenswitch.Env, "") }

func claimServed(t *testing.T, liveReq string) (served, recorded *models.MongoOpMessage) {
	t.Helper()
	switchOff(t)
	m := mongoMock(claimRequest(recKeyID), claimAnswer(recKeyID))
	MaterializeCorrelations(m)
	if len(m.Spec.Correlations) != 1 {
		t.Fatalf("materialized correlations = %+v, want one", m.Spec.Correlations)
	}
	recorded = m.Spec.MongoResponses[0].Message.(*models.MongoOpMessage)
	return RenderMongoEchoes(m.Spec.Correlations, []string{mongoMsg(liveReq)}, recorded), recorded
}

// The recorded upsert, served to a run that minted another key, answers with
// that key, so the application reads that it won its claim. The pooled
// recording keeps the recorded key.
func TestARecordedUpsertAnswersWithTheKeyTheLiveRequestWrote(t *testing.T) {
	served, recorded := claimServed(t, claimRequest(liveKeyID))
	if served == recorded {
		t.Fatal("served the recorded answer itself; want a copy with the live key")
	}
	if want := mongoMsg(claimAnswer(liveKeyID)); len(served.Sections) != 1 || served.Sections[0] != want {
		t.Fatalf("served = %q\nwant %q", served.Sections, want)
	}
	if recorded.Sections[0] != mongoMsg(claimAnswer(recKeyID)) {
		t.Fatalf("the pooled recording changed: %q", recorded.Sections[0])
	}
}

// Where the live request does not write one value of the recorded class at
// every place the recording wrote it, the answer is served as recorded, as it
// was before correlations.
func TestARecordedUpsertIsServedAsRecordedWhenTheLiveWriteDoesNotBind(t *testing.T) {
	switchOff(t)
	for name, live := range map[string]string{
		"the same key":                  claimRequest(recKeyID),
		"no key written":                strings.Replace(claimRequest(recKeyID), `"key_id":"`+recKeyID+`",`, "", 1),
		"a key of another class":        claimRequest("key-7"),
		"a random key of another class": claimRequest("0123456789abcdef0123"),
		"several keys written at the place": strings.Replace(claimRequest(recKeyID), `"key_id":"`+recKeyID+`"`,
			`"key_id":["`+liveKeyID+`","0f1e2d3c-4b5a-4968-8776-a5b4c3d2e1f0"]`, 1),
		// JSON writes the newline escaped, so the live key's text is not
		// the key, and written raw into the answer it would break it.
		"a key with a newline after it": claimRequest(liveKeyID + `\n`),
		"not a body section":            "",
	} {
		t.Run(name, func(t *testing.T) {
			m := mongoMock(claimRequest(recKeyID), claimAnswer(recKeyID))
			MaterializeCorrelations(m)
			recorded := m.Spec.MongoResponses[0].Message.(*models.MongoOpMessage)
			sections := []string{mongoMsg(live)}
			if live == "" {
				sections = []string{"{ SectionSingle identifier: documents , msgs: [ ] }"}
			}
			if got := RenderMongoEchoes(m.Spec.Correlations, sections, recorded); got != recorded {
				t.Fatalf("served %q; want the recorded answer itself", got.Sections)
			}
		})
	}
}

// A recorded value written at two places binds only when the live request
// writes one value at both.
func TestARecordedValueWrittenTwiceBindsOnlyOneLiveValue(t *testing.T) {
	switchOff(t)
	two := func(a, b string) string {
		return `{"findAndModify":"t","query":{"_id":"k"},"update":{"$set":{"key_id":"` + a + `","alias":"` + b + `"}},"upsert":true,"new":true}`
	}
	m := mongoMock(two(recKeyID, recKeyID), `{"value":{"key_id":"`+recKeyID+`"},"ok":1}`)
	MaterializeCorrelations(m)
	if len(m.Spec.Correlations) != 2 {
		t.Fatalf("correlations = %+v, want two", m.Spec.Correlations)
	}
	recorded := m.Spec.MongoResponses[0].Message.(*models.MongoOpMessage)
	if got := RenderMongoEchoes(m.Spec.Correlations, []string{mongoMsg(two(liveKeyID, recKeyID))}, recorded); got != recorded {
		t.Fatalf("two live values bound: served %q", got.Sections)
	}
	// Refused at one place, the value stays refused: neither live key is
	// served.
	if got := RenderMongoEchoes(m.Spec.Correlations, []string{mongoMsg(two(liveKeyID, "0f1e2d3c-4b5a-4968-8776-a5b4c3d2e1f0"))}, recorded); got != recorded {
		t.Fatalf("two new live values bound: served %q", got.Sections)
	}
	got := RenderMongoEchoes(m.Spec.Correlations, []string{mongoMsg(two(liveKeyID, liveKeyID))}, recorded)
	if want := mongoMsg(`{"value":{"key_id":"` + liveKeyID + `"},"ok":1}`); got.Sections[0] != want {
		t.Fatalf("served %q, want %q", got.Sections[0], want)
	}
}

// Under the proven switch what a value looks like decides nothing: a Mongo
// mock gets no correlation at ingest, and its answer is served as recorded.
func TestAMongoEchoIsNotDetectedUnderTheProvenSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	m := mongoMock(claimRequest(recKeyID), claimAnswer(recKeyID))
	MaterializeCorrelations(m)
	if len(m.Spec.Correlations) != 0 {
		t.Fatalf("correlations under the switch = %+v, want none", m.Spec.Correlations)
	}
}

// Only a section framed as the recorder frames a body section, around valid
// JSON, is read: bare JSON, a body with no closing frame, a frame with no
// body (the prefix and suffix share its space) and a body with text after its
// document are not, and none of them panics.
func TestAMongoSectionNotFramedAsABodyIsNotRead(t *testing.T) {
	for name, section := range map[string]string{
		"bare JSON":                    claimRequest(recKeyID) + mongoMsgSuffix,
		"a body with no closing frame": mongoMsgPrefix + claimRequest(recKeyID),
		"a frame with no body":         "{ SectionSingle msg: }",
		"text after the document":      mongoMsg(claimRequest(recKeyID) + ` x`),
	} {
		t.Run(name, func(t *testing.T) {
			if doc, ok := mongoMsgJSON(section); ok {
				t.Fatalf("read %q from %q", doc, section)
			}
		})
	}
	if doc, ok := mongoMsgJSON(mongoMsg(claimRequest(recKeyID))); !ok || doc != claimRequest(recKeyID) {
		t.Fatalf("a framed body section must be read: %q %v", doc, ok)
	}
}

// A section that is not valid JSON is not read on either side, nor in the
// live request: here a document with text after it, whose document alone
// would correlate.
func TestAMongoSectionThatIsNotValidJSONIsNotRead(t *testing.T) {
	switchOff(t)
	if got := CorrelationsFromMock(mongoMock(claimRequest(recKeyID)+` x`, claimAnswer(recKeyID))); len(got) != 0 {
		t.Errorf("a request that is not JSON: correlations = %+v, want none", got)
	}
	if got := CorrelationsFromMock(mongoMock(claimRequest(recKeyID), claimAnswer(recKeyID)+` x`)); len(got) != 0 {
		t.Errorf("an answer that is not JSON: correlations = %+v, want none", got)
	}
	if served, recorded := claimServed(t, claimRequest(liveKeyID)+` x`); served != recorded {
		t.Errorf("a live request that is not JSON: served %q, want the recorded answer itself", served.Sections)
	}
}

// A correlation binds only at a place named as the detector names one: a
// section's index and, inside it, the path of a place a write sets. A place
// named any other way, and a filter's place, are served as recorded.
func TestARecordedUpsertBindsOnlyAtAWrittenPlaceOfASection(t *testing.T) {
	switchOff(t)
	m := mongoMock(claimRequest(recKeyID), claimAnswer(recKeyID))
	MaterializeCorrelations(m)
	if len(m.Spec.Correlations) != 1 {
		t.Fatalf("correlations = %+v, want the claim's one", m.Spec.Correlations)
	}
	recorded := m.Spec.MongoResponses[0].Message.(*models.MongoOpMessage)
	live := []string{mongoMsg(claimRequest(liveKeyID))}
	if got := RenderMongoEchoes(m.Spec.Correlations, live, recorded); got == recorded || !strings.Contains(got.Sections[0], liveKeyID) {
		t.Fatalf("the claim's own place must bind: served %q", got.Sections)
	}
	for _, place := range []string{"0.update.$set.key_id", "body.update.$set.key_id", "section.x.update.$set.key_id", "section.0",
		"section.0.", "section.-1.update.$set.key_id", "section.00.update.$set.key_id", "section.0.update.$set.key_id.x"} {
		c := models.FieldCorrelation{RequestPath: place, RecordedValue: recKeyID, ValueClass: "uuid"}
		if got := RenderMongoEchoes([]models.FieldCorrelation{c}, live, recorded); got != recorded {
			t.Errorf("place %q: served %q, want the recorded answer itself", place, got.Sections)
		}
	}
	filter := models.FieldCorrelation{RequestPath: "section.0.query.key_id", RecordedValue: recKeyID, ValueClass: "uuid"}
	lookup := []string{mongoMsg(`{"findAndModify":"web_session_tokens","query":{"key_id":"` + liveKeyID + `"},"update":{"$set":{"n":1}},"$db":"api"}`)}
	if got := RenderMongoEchoes([]models.FieldCorrelation{filter}, lookup, recorded); got != recorded {
		t.Errorf("a filter's place: served %q, want the recorded answer itself", got.Sections)
	}
}

// A correlation the render cannot place or read (one carried from elsewhere
// with no recorded value, a place it cannot parse, a section the live
// request does not have) leaves the answer as recorded.
func TestARecordedUpsertIsServedAsRecordedForACorrelationItCannotPlace(t *testing.T) {
	switchOff(t)
	m := mongoMock(claimRequest(recKeyID), claimAnswer(recKeyID))
	MaterializeCorrelations(m)
	recorded := m.Spec.MongoResponses[0].Message.(*models.MongoOpMessage)
	live := []string{mongoMsg(claimRequest(liveKeyID))}
	if len(m.Spec.Correlations) != 1 {
		t.Fatalf("correlations = %+v, want the claim's one", m.Spec.Correlations)
	}
	good := m.Spec.Correlations[0]
	for name, tc := range map[string]struct {
		c    models.FieldCorrelation
		live []string
	}{
		"no recorded value": {models.FieldCorrelation{RequestPath: good.RequestPath, RecordedValue: "", ValueClass: "uuid"}, live},
		// A body that is one value flattens to the empty path, which is
		// what an unparsed place would name.
		"an unparsed place":   {models.FieldCorrelation{RequestPath: "body.key_id", RecordedValue: recKeyID, ValueClass: "uuid"}, []string{mongoMsg(`"` + liveKeyID + `"`)}},
		"a section not there": {models.FieldCorrelation{RequestPath: "section.1.update.$set.key_id", RecordedValue: recKeyID, ValueClass: "uuid"}, live},
	} {
		t.Run(name, func(t *testing.T) {
			if got := RenderMongoEchoes([]models.FieldCorrelation{tc.c}, tc.live, recorded); got != recorded {
				t.Fatalf("served %q; want the recorded answer itself", got.Sections)
			}
		})
	}
	if got := RenderMongoEchoes([]models.FieldCorrelation{good}, live, nil); got != nil {
		t.Fatalf("no recorded answer: served %v, want nil", got)
	}
}

// writeTwice is a claim that writes one minted key at two places, key_id and
// alias (a JSON value; none when empty), and answers with key_id.
func writeTwice(keyID, alias string) string {
	set := `"key_id":"` + keyID + `"`
	if alias != "" {
		set = `"alias":` + alias + `,` + set
	}
	return `{"findAndModify":"t","query":{"_id":"k"},"update":{"$set":{` + set + `}},"upsert":true,"new":true}`
}

// A recorded value that cannot bind at one of the places the recording wrote
// it is served as recorded, though another place binds: the live request has
// no value there, several, one that is not random, one of another class, or
// the place cannot be read. Each is checked with the refused place read both
// before and after the place that binds.
func TestARecordedValueRefusedAtOnePlaceIsNotBoundAtAnother(t *testing.T) {
	switchOff(t)
	m := mongoMock(writeTwice(recKeyID, `"`+recKeyID+`"`), `{"value":{"key_id":"`+recKeyID+`"},"ok":1}`)
	MaterializeCorrelations(m)
	if len(m.Spec.Correlations) != 2 || m.Spec.Correlations[0].RequestPath != "section.0.update.$set.alias" {
		t.Fatalf("correlations = %+v, want alias and key_id", m.Spec.Correlations)
	}
	recorded := m.Spec.MongoResponses[0].Message.(*models.MongoOpMessage)
	alias, key := m.Spec.Correlations[0], m.Spec.Correlations[1]
	for name, aliasJSON := range map[string]string{
		"no value":                 ``,
		"several values":           `["` + liveKeyID + `","` + liveKeyID + `"]`,
		"a value not random":       `"key-7"`,
		"a value of another class": `"0123456789abcdef0123"`,
	} {
		live := []string{mongoMsg(writeTwice(liveKeyID, aliasJSON))}
		for order, corrs := range map[string][]models.FieldCorrelation{"refused first": {alias, key}, "refused last": {key, alias}} {
			if got := RenderMongoEchoes(corrs, live, recorded); got != recorded {
				t.Errorf("%s, %s: served %q; want the recorded answer itself", name, order, got.Sections)
			}
		}
	}
	unread := alias
	unread.RequestPath = "body.alias"
	live := []string{mongoMsg(writeTwice(liveKeyID, `"`+liveKeyID+`"`))}
	for order, corrs := range map[string][]models.FieldCorrelation{"refused first": {unread, key}, "refused last": {key, unread}} {
		if got := RenderMongoEchoes(corrs, live, recorded); got != recorded {
			t.Errorf("a place it cannot read, %s: served %q; want the recorded answer itself", order, got.Sections)
		}
	}
	// The same live key at both places binds.
	if got := RenderMongoEchoes([]models.FieldCorrelation{alias, key}, live, recorded); got == recorded || !strings.Contains(got.Sections[0], liveKeyID) {
		t.Fatalf("one live key at both places must bind: served %q", got.Sections)
	}
}

// A minted key of each class binds: a hex token and a letters-and-digits
// nonce are answered with the live key as a uuid is.
func TestAMintedHexOrNonceKeyIsAnsweredWithTheLiveKey(t *testing.T) {
	switchOff(t)
	for class, keys := range map[string][2]string{
		"hex":   {"0123456789abcdef0123", "fedcba9876543210fedc"},
		"nonce": {"AbC123dEf456GhI789jKl0", "ZyX987wVu654TsR321qPo0"},
	} {
		m := mongoMock(claimRequest(keys[0]), claimAnswer(keys[0]))
		MaterializeCorrelations(m)
		if len(m.Spec.Correlations) != 1 || m.Spec.Correlations[0].ValueClass != class {
			t.Fatalf("%s: correlations = %+v, want one of class %s", class, m.Spec.Correlations, class)
		}
		recorded := m.Spec.MongoResponses[0].Message.(*models.MongoOpMessage)
		got := RenderMongoEchoes(m.Spec.Correlations, []string{mongoMsg(claimRequest(keys[1]))}, recorded)
		if want := mongoMsg(claimAnswer(keys[1])); got.Sections[0] != want {
			t.Errorf("%s: served %q\nwant %q", class, got.Sections[0], want)
		}
	}
}

// A minted key with space around it is not a correlation: JSON escapes a tab
// or a newline, so the answer's text does not hold the key as it is.
func TestAMintedKeyWithSpaceAroundItIsNotACorrelation(t *testing.T) {
	for name, key := range map[string]string{"a newline after it": recKeyID + `\n`, "a space before it": " " + recKeyID} {
		if got := CorrelationsFromMock(mongoMock(claimRequest(key), claimAnswer(key))); len(got) != 0 {
			t.Errorf("%s: correlations = %+v, want none", name, got)
		}
	}
}

// manyDocs is a JSON array of n documents, each with a uuid.
func manyDocs(n int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"_id":{"$oid":"65a1b2c3d4e5f6a7b8c9%04x"},"key_id":"5b6f1c2e-8d3a-4f7b-9e21-%012x","name":"user %d","scopes":["read","write"]}`, i%65536, i, i)
	}
	b.WriteByte(']')
	return b.String()
}

// ingestCost is what one CorrelationsFromMock of m allocates, in allocations
// and bytes, and the correlations it gives.
func ingestCost(m *models.Mock) (allocs, bytes uint64, got []models.FieldCorrelation) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got = CorrelationsFromMock(m)
	runtime.ReadMemStats(&after)
	return after.Mallocs - before.Mallocs, after.TotalAlloc - before.TotalAlloc, got
}

// checkIngestCost fails when reading m at ingest gives a correlation, or
// costs more than a thousand allocations or 256 KB: a parse of these
// 0.8-2.7 MB sections costs hundreds of thousands of allocations and over
// 30 MB, and keeping their tens of thousands of values over 1 MB.
func checkIngestCost(t *testing.T, name string, m *models.Mock) {
	t.Helper()
	allocs, bytes, got := ingestCost(m)
	if len(got) != 0 {
		t.Errorf("%s: correlations = %+v, want none", name, got)
	}
	if allocs > 1000 || bytes > 256<<10 {
		t.Errorf("%s: %d allocations and %d bytes to read a mock that echoes no written value; a large side was parsed or its values kept", name, allocs, bytes)
	}
}

// A read writes no value, so its answer is never parsed when the agent takes
// the mocks in, however large; nor is a write's answer that does not hold the
// value it wrote (a findAndModify that answers the document as it was before
// the write), whose values are only looked up, not kept.
func TestAMongoReadsLargeAnswerIsNotParsedAtIngest(t *testing.T) {
	docs := manyDocs(20000)
	cursor := `{"cursor":{"firstBatch":` + docs + `,"id":{"$numberLong":"0"},"ns":"api.t"},"ok":1}`
	for name, tc := range map[string]struct{ req, answer string }{
		"a find":       {`{"find":"t","filter":{"cid":"c1","key_id":"5b6f1c2e-8d3a-4f7b-9e21-000000000007"},"$db":"api"}`, cursor},
		"an aggregate": {`{"aggregate":"t","pipeline":[{"$match":{"cid":"c1"}}],"cursor":{},"$db":"api"}`, cursor},
		"a getMore":    {`{"getMore":{"$numberLong":"7"},"collection":"t","$db":"api"}`, cursor},
		"a write answered with the document before it": {claimRequest(liveKeyID),
			`{"lastErrorObject":{"n":1,"updatedExisting":true},"value":{"_id":"cid-1:user-1:jti-1","history":` + docs + `},"ok":1}`},
		// The answer echoes the filter's key, so the request is read, but
		// not the key the write set, so the answer is not parsed.
		"a write answered with its filter's key, not the key it set": {
			`{"findAndModify":"t","query":{"_id":"5b6f1c2e-8d3a-4f7b-9e21-000000000007"},"update":{"$set":{"key_id":"` + liveKeyID + `"}},"$db":"api"}`,
			`{"value":{"_id":"5b6f1c2e-8d3a-4f7b-9e21-000000000007","history":` + docs + `},"ok":1}`},
	} {
		checkIngestCost(t, name, mongoMock(tc.req, tc.answer))
	}
	// An answer's size is all its sections': a large first section and a
	// small last one make a large answer.
	two := mongoMock(claimRequest(liveKeyID), "")
	two.Spec.MongoResponses[0].Message = &models.MongoOpMessage{Sections: []string{
		mongoMsg(`{"value":{"_id":"cid-1:user-1:jti-1","history":` + docs + `}}`), mongoMsg(`{"ok":1}`)}}
	checkIngestCost(t, "a write answered in two sections, the large one first", two)
}

// A write that sets two minted values, of which its answer holds one (an
// update whose answer echoes only the upserted _id, a findAndModify with a
// projection), correlates exactly that one. The written values are read in
// map order, so each case is read many times.
func TestAWriteOfTwoMintedValuesCorrelatesOnlyTheOneItsAnswerHolds(t *testing.T) {
	req := `{"findAndModify":"t","query":{"_id":"k"},"update":{"$set":{"k1":"` + recKeyID + `","k2":"` + liveKeyID + `"}},"upsert":true,"new":true}`
	for _, answered := range []string{recKeyID, liveKeyID} {
		m := mongoMock(req, `{"value":{"x":"`+answered+`"},"ok":1}`)
		for i := 0; i < 200; i++ {
			if got := CorrelationsFromMock(m); len(got) != 1 || got[0].RecordedValue != answered {
				t.Fatalf("answer holds %s, ingest %d: correlations = %+v, want that one value", answered, i, got)
			}
		}
	}
}

// A correlation lists the answer's places in one order on every ingest, however
// the answer's document is read.
func TestACorrelationListsTheAnswersPlacesInOneOrder(t *testing.T) {
	m := mongoMock(claimRequest(recKeyID), `{"lastErrorObject":{"n":1,"updatedExisting":false,"upserted":"`+recKeyID+`"},`+
		`"value":{"_id":"cid-1:user-1:jti-1","key_id":"`+recKeyID+`","alias":"`+recKeyID+`"},"ok":1}`)
	want := []string{"section.0.lastErrorObject.upserted", "section.0.value.alias", "section.0.value.key_id"}
	for i := 0; i < 50; i++ {
		if got := CorrelationsFromMock(m); len(got) != 1 || !reflect.DeepEqual(got[0].ResponsePaths, want) {
			t.Fatalf("ingest %d: correlations = %+v, want one answered at %v", i, got, want)
		}
	}
}

// Every field that holds a place mongoWritten reads gets its request read,
// whatever else the section names.
func TestARequestIsReadForEveryFieldAWriteSets(t *testing.T) {
	for name, req := range map[string]string{
		"update":    `{"update":{"$set":{"token":"` + recKeyID + `"}}}`,
		"updates":   `{"updates":[{"q":{"_id":"k"},"u":{"$set":{"token":"` + recKeyID + `"}}}]}`,
		"documents": `{"documents":[{"token":"` + recKeyID + `"}]}`,
		// An update may be an aggregation pipeline (MongoDB 4.2+).
		"a pipeline update": `{"update":[{"$set":{"token":"` + recKeyID + `"}}]}`,
	} {
		if got := CorrelationsFromMock(mongoMock(req, `{"value":{"token":"`+recKeyID+`"},"ok":1}`)); len(got) != 1 {
			t.Errorf("%s: correlations = %+v, want one", name, got)
		}
	}
}

// manyIDs is a JSON list of n quoted uuids, the ids manyDocs holds.
func manyIDs(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"5b6f1c2e-8d3a-4f7b-9e21-%012x"`, i)
	}
	return b.String()
}

// A request is parsed at ingest only when it names a place a write sets and
// it shares a quoted value with its answer. A find's filter, however large, is
// not parsed, even when its answer echoes the ids it filters on; nor is an
// insert's list of documents when its answer is a count, or a count and a
// replica set's election id, which no document holds.
func TestALargeMongoRequestIsNotParsedUnlessItWritesAValueItsAnswerEchoes(t *testing.T) {
	ins := `{"insert":"t","documents":` + manyDocs(20000) + `,"$db":"api"}`
	for name, tc := range map[string]struct{ req, answer string }{
		"a find with a large $in": {`{"find":"t","filter":{"key_id":{"$in":[` + manyIDs(20000) + `]}},"$db":"api"}`,
			`{"cursor":{"firstBatch":[{"_id":"x","key_id":"5b6f1c2e-8d3a-4f7b-9e21-000000000007"}],"id":{"$numberLong":"0"},"ns":"api.t"},"ok":1}`},
		"an insert answered with a count": {ins, `{"n":{"$numberInt":"20000"},"ok":{"$numberDouble":"1.0"}}`},
		"an insert answered with a count and an election id": {ins,
			`{"n":{"$numberInt":"20000"},"electionId":{"$oid":"7fffffff0000000000000001"},"ok":{"$numberDouble":"1.0"}}`},
	} {
		checkIngestCost(t, name, mongoMock(tc.req, tc.answer))
	}
}

// A key is found after a quote escaped in a value before it, which shifts
// which quotes open a string.
func TestAKeyAfterAnEscapedQuoteIsACorrelation(t *testing.T) {
	answer := strings.Replace(claimAnswer(recKeyID), `"key_id":`, `"note":"a 5\" screen","key_id":`, 1)
	if got := CorrelationsFromMock(mongoMock(claimRequest(recKeyID), answer)); len(got) != 1 || got[0].RecordedValue != recKeyID {
		t.Fatalf("correlations = %+v, want the key", got)
	}
}

// The key is read in whichever answer section holds it, and named by that
// section's index, whether the answer is smaller than its request (its values
// are the ones classified) or larger (its values are only looked up).
func TestAKeyAnsweredInALaterSectionIsACorrelation(t *testing.T) {
	for name, first := range map[string]string{
		"a small answer": `{"ok":1}`,
		"a large answer": `{"ok":1,"note":"` + strings.Repeat("x", 4096) + `"}`,
	} {
		m := mongoMock(claimRequest(recKeyID), "")
		m.Spec.MongoResponses[0].Message = &models.MongoOpMessage{Sections: []string{mongoMsg(first), mongoMsg(claimAnswer(recKeyID))}}
		got := CorrelationsFromMock(m)
		if len(got) != 1 || !reflect.DeepEqual(got[0].ResponsePaths, []string{"section.1.value.key_id"}) {
			t.Errorf("%s: correlations = %+v, want the key answered at section.1.value.key_id", name, got)
		}
	}
}

// A key written in a later section of the request is named by that section's
// index, and only the live request's same section binds it.
func TestAKeyWrittenInALaterRequestSectionBindsOnlyThere(t *testing.T) {
	switchOff(t)
	first := mongoMsg(`{"ok":1}`)
	m := mongoMock(claimRequest(recKeyID), claimAnswer(recKeyID))
	m.Spec.MongoRequests[0].Message = &models.MongoOpMessage{Sections: []string{first, mongoMsg(claimRequest(recKeyID))}}
	MaterializeCorrelations(m)
	if len(m.Spec.Correlations) != 1 || m.Spec.Correlations[0].RequestPath != "section.1.update.$set.key_id" {
		t.Fatalf("correlations = %+v, want the key at section.1.update.$set.key_id", m.Spec.Correlations)
	}
	recorded := m.Spec.MongoResponses[0].Message.(*models.MongoOpMessage)
	if got := RenderMongoEchoes(m.Spec.Correlations, []string{first, mongoMsg(claimRequest(liveKeyID))}, recorded); got == recorded || !strings.Contains(got.Sections[0], liveKeyID) {
		t.Fatalf("the live key in section 1 must bind: served %q", got.Sections)
	}
	if got := RenderMongoEchoes(m.Spec.Correlations, []string{mongoMsg(claimRequest(liveKeyID)), first}, recorded); got != recorded {
		t.Fatalf("the live key in section 0 must not bind: served %q", got.Sections)
	}
}
