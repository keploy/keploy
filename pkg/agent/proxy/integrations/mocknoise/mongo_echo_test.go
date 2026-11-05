package mocknoise

import (
	"reflect"
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

func claimServed(t *testing.T, liveReq string) (served, recorded *models.MongoOpMessage) {
	t.Helper()
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
	for name, live := range map[string]string{
		"the same key":                  claimRequest(recKeyID),
		"no key written":                strings.Replace(claimRequest(recKeyID), `"key_id":"`+recKeyID+`",`, "", 1),
		"a key of another class":        claimRequest("key-7"),
		"a random key of another class": claimRequest("0123456789abcdef0123"),
		"several keys written at the place": strings.Replace(claimRequest(recKeyID), `"key_id":"`+recKeyID+`"`,
			`"key_id":["`+liveKeyID+`","0f1e2d3c-4b5a-4968-8776-a5b4c3d2e1f0"]`, 1),
		"not a body section": "",
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

// Only a section framed as the recorder frames a body section is read: bare
// JSON, a body with no closing frame, and a frame with no body (the
// prefix and suffix share its space) are not, and none of them panics.
func TestAMongoSectionNotFramedAsABodyIsNotRead(t *testing.T) {
	for name, section := range map[string]string{
		"bare JSON":                    claimRequest(recKeyID) + mongoMsgSuffix,
		"a body with no closing frame": mongoMsgPrefix + claimRequest(recKeyID),
		"a frame with no body":         "{ SectionSingle msg: }",
	} {
		t.Run(name, func(t *testing.T) {
			if doc := mongoMsgDoc(section); doc != nil {
				t.Fatalf("read %v from %q", doc, section)
			}
		})
	}
	if doc := mongoMsgDoc(mongoMsg(claimRequest(recKeyID))); len(doc["update.$set.key_id"]) != 1 {
		t.Fatalf("a framed body section must be read: %v", doc)
	}
}

// A correlation place names a section by its index and a path inside it;
// any other place is refused.
func TestAMongoCorrelationPlaceIsASectionIndexAndAPath(t *testing.T) {
	if i, path, ok := mongoPlace("section.2.update.$set.key_id"); !ok || i != 2 || path != "update.$set.key_id" {
		t.Fatalf("mongoPlace = %d %q %v", i, path, ok)
	}
	for _, place := range []string{"0.update.$set.key_id", "body.key_id", "section.x.key_id", "section.0", "section.0.", "section.-1.key_id"} {
		if i, path, ok := mongoPlace(place); ok {
			t.Errorf("mongoPlace(%q) = %d %q, want refused", place, i, path)
		}
	}
}

// A correlation the render cannot place or read (one carried from elsewhere
// with no recorded value, a place it cannot parse, a section the live
// request does not have) leaves the answer as recorded.
func TestARecordedUpsertIsServedAsRecordedForACorrelationItCannotPlace(t *testing.T) {
	m := mongoMock(claimRequest(recKeyID), claimAnswer(recKeyID))
	MaterializeCorrelations(m)
	recorded := m.Spec.MongoResponses[0].Message.(*models.MongoOpMessage)
	live := []string{mongoMsg(claimRequest(liveKeyID))}
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
