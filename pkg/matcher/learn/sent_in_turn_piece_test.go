package learn

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/wiremessage"
)

// stepIDMock is an earlier call of the test that sends a step id of its own
// (a v4 UUID minted in each run) at its "step_id" field, asked at req and
// answered at res: api-server's TestSeededSuitesRunOnClient, whose steps
// send a fresh X-Keploy-Test-Step-Id each.
func stepIDMock(name, stepID string, req, res time.Time) *models.Mock {
	doc := `{ SectionSingle identifier: documents , msgs: [ {"step_id":"` + stepID + `","kind":"STEP"} ] }`
	return &models.Mock{Name: name, Kind: models.Mongo, Spec: models.MockSpec{
		MongoRequests: []models.MongoRequest{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
			`{ SectionSingle msg: {"insert":"steps","ordered":true,"$db":"api-server"} }`, doc}}}},
		MongoResponses: []models.MongoResponse{{Header: &models.MongoHeader{Opcode: wiremessage.OpMsg}, Message: &models.MongoOpMessage{Sections: []string{
			`{ SectionSingle msg: {"n":{"$numberInt":"1"},"ok":{"$numberDouble":"1.0"}} }`}}}},
		ReqTimestampMock: req, ResTimestampMock: res,
	}}
}

// The suite run's id is sent at two calls at once (sentInTurnMock's
// eventsV2 insert and the call), in the other order in each run, so each of
// its long words (95ad0386, 618e3aa21789 as recorded) is bound at the other
// call in either order. Its 16-bit group 71d9 (1c2e in round F) is read at
// every place too, and round F's step id of an earlier call of the test
// holds round F's group by chance, inside a UUID of its own that shows none
// of the run id's long words. keploy mock binds the group with the id's
// long words, in either order at the other call: the id is one the
// application made at the call (no place gave the application any of its
// long words before the call; the other call's request gives it nothing),
// so a place holding a piece of it alone holds it by chance (the founder's
// minted default). Before, the group was refused as unknown at the step's
// place ("both values hold it, or only part of a word"), so the call missed
// at replay (TestSeededSuitesRunOnClient's eventsV2 insert, its
// test_suite_run_id's group, against an earlier step's id).
// keploy test reads the piece as before.
func TestAPieceOfAValueSentInTheOtherOrderIsBoundWithItsLongWords(t *testing.T) {
	us := time.Microsecond
	req, res := oAt.Add(-8*us), oAt.Add(220*us)
	inTurn := RawOf(sentInTurnMock("mock-1784", sentInTurnRec, req, res), true).ServedAs(6).ServedAsRecorded()
	inTurn.servedAfter = true
	stepAt, stepRes := oAt.Add(-2*time.Second), oAt.Add(-2*time.Second+time.Millisecond)
	const recStep = "385fbd9c-6427-4c24-a8f1-20999f93288d"
	step := func(liveStep string) Raw {
		return RawOf(stepIDMock("mock-1702", recStep, stepAt, stepRes), true).ServedAs(3).
			ServedLive(RawOf(stepIDMock("mock-1702", liveStep, stepAt, stepRes), true))
	}
	decide := func(drifts *DriftCache, r, f string, raw ...Raw) Decision {
		in := oInput(sentInTurnPath, sentInTurnRec, sentInTurnLive, nil, raw)
		in.Scope = func(string) Scope { return ScopeTest("test-2") }
		if drifts != nil {
			in.Drifts = drifts
		}
		return oWord(t, Decide(in), r, f)
	}
	// Round F's step id holds round F's group (1c2e) by chance.
	chance := step("9a0e5b17-1c2e-4f6d-b3a8-c41d27e95f60")
	for _, w := range [][2]string{{"95ad0386", "4fbd76ad"}, {"618e3aa21789", "7e8f9a0b1c2d"}} {
		if d := decide(mockDrifts(), w[0], w[1], inTurn, chance); d.Outcome != OutcomeValue || !d.EitherOrder || d.From.Mock != "mock-1784" {
			t.Fatalf("FALSE RED: long word %s of the id sent in the other order: %s (either order %v); want bound to mock-1784 in either order", w[0], oDescribe([]Decision{d}), d.EitherOrder)
		}
	}
	if d := decide(mockDrifts(), "71d9", "1c2e", inTurn, chance); d.Outcome != OutcomeValue || !d.EitherOrder || d.From.Mock != "mock-1784" {
		t.Errorf("FALSE RED: the id's group stands by chance in round F's step id of an earlier call: %s (either order %v); want bound to mock-1784 in either order, with the id's long words", oDescribe([]Decision{d}), d.EitherOrder)
	}
	// Round F's step id holds the recorded group (71d9) by chance: alike.
	if d := decide(mockDrifts(), "71d9", "1c2e", inTurn, step("c41d2e95-71d9-4f60-9a0e-5b17b3a8f6d2")); d.Outcome != OutcomeValue || !d.EitherOrder || d.From.Mock != "mock-1784" {
		t.Errorf("FALSE RED: the recorded group stands by chance in round F's step id of an earlier call: %s (either order %v); want bound to mock-1784 in either order", oDescribe([]Decision{d}), d.EitherOrder)
	}

	// Controls. keploy test reads the piece as before: refused at the step.
	oRefused(t, "keploy test: the group stands in round F's step id", decide(nil, "71d9", "1c2e", inTurn, chance), RefusalUnknown, "mock-1702")
	// The step id shows a long word of the run id too (round F's 32-bit
	// word, whole or glued): the place may be where the application took
	// the id from, so the group stays refused.
	for _, live := range []string{"4fbd76ad-1c2e-4f6d-b3a8-c41d27e95f60", "a4fbd76adb-1c2e-4f6d-b3a8-c41d27e95f60"} {
		if d := decide(mockDrifts(), "71d9", "1c2e", inTurn, step(live)); d.Outcome == OutcomeValue || d.Outcome == OutcomeFresh || d.EitherOrder {
			t.Errorf("FALSE GREEN? round F's step id %s holds the id's 32-bit word too: %s; want the group refused", live, oDescribe([]Decision{d}))
		}
	}
	// The id's long words bound at a source (round F gave the other call as
	// recorded before the call: a recording whose request holds the id gave
	// the application nothing, but a source decides first) leave no id the
	// call made, so the group is read as for any word.
	gaveRecorded := RawOf(sentInTurnMock("mock-1784", sentInTurnRec, req, res), true).ServedAs(6).ServedAsRecorded()
	if d := decide(mockDrifts(), "71d9", "1c2e", gaveRecorded, chance); d.EitherOrder {
		t.Errorf("round F served the other call before the call: %s; want no value in either order", oDescribe([]Decision{d}))
	}
}
