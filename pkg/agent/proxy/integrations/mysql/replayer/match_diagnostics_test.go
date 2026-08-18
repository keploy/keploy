package replayer

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/schemanoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// TestMatchCommand_MissNamesTheNearestRecordingInsteadOfClaimingNoneExists pins
// the diagnostic that made this class of failure so hard to read.
//
// A query that drifted from its recording produces no scoring candidate. That
// used to leave closest_mock empty, and query.go turned an empty closest_mock
// into "REPLAY-ORPHAN: mock NEVER RECORDED for this query (lost at record
// time)". So an operator whose recording was sitting right there, intact, was
// told to go looking for a lost mock. The candidate count and match phase the
// report printed alongside it were never assigned either — a hardcoded zero
// that read as "the pool was empty".
//
// The miss must therefore carry a MEASURED candidate count and phase, and must
// name the nearest recorded statement.
func TestMatchCommand_MissNamesTheNearestRecordingInsteadOfClaimingNoneExists(t *testing.T) {
	logger := zap.NewNop()

	const recorded = "SELECT `id`, `name` FROM `invoices` WHERE `token` = 'recorded-token'"
	const live = "SELECT `id`, `name` FROM `couriers` WHERE `token` = 'live-token'"

	db := &fakeMockDb{session: []*models.Mock{
		readbackMock("m-invoices", recorded, "row-1", zeroTime()),
	}}
	eng := schemanoise.New(mysqlNoiseAdapter{}, false, false)

	_, ok, miss, err := matchCommand(context.Background(), logger, comQueryReq(live), db, newDecodeCtx(), eng, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("a different table must not match")
	}
	if miss == nil {
		t.Fatal("a miss must carry a report")
	}
	if miss.candidateCount != 1 {
		t.Errorf("candidateCount = %d, want 1 — the count must be measured, not left at its zero value "+
			"(zero is what made a drifted query look like an empty mock pool)", miss.candidateCount)
	}
	if miss.matchPhase != models.MatchPhaseExhausted {
		t.Errorf("matchPhase = %q, want %q", miss.matchPhase, models.MatchPhaseExhausted)
	}
	if miss.matchPhase == models.MatchPhaseNoMocks {
		t.Error("mocks were present, so the report must not claim the pool was empty — " +
			"that is the condition query.go turns into REPLAY-ORPHAN")
	}
	if miss.closestMock != "m-invoices" {
		t.Errorf("closestMock = %q, want the nearest recorded statement's mock", miss.closestMock)
	}
}

// TestMatchCommand_DBMCommentEndToEnd drives the whole matchCommand path with a
// traced service's traffic shape: the recorded statement behind a
// writer-endpoint comment, the live one behind a reader-endpoint comment with a
// fresh traceparent.
//
// A DECOY of a different statement sits first in the pool, and the test asserts
// WHICH recorded response comes back — not merely that something did. Both mocks
// declare the same wire payload length, which is what the matcher used to score
// on, so under the old behaviour the decoy wins on pool order and the assertion
// fails. Asserting only "ok" would pass either way and prove nothing.
func TestMatchCommand_DBMCommentEndToEnd(t *testing.T) {
	logger := zap.NewNop()

	const body = "SELECT `id`, `name` FROM `invoices` WHERE `id` = 1"
	const decoy = "SELECT `id`, `name` FROM `couriers` WHERE `id` = 9"

	db := &fakeMockDb{session: []*models.Mock{
		readbackMock("m-decoy", dbmWriter+decoy, "decoy-row", zeroTime()),
		readbackMock("m-target", dbmWriter+body, "target-row", zeroTime()),
	}}
	eng := schemanoise.New(mysqlNoiseAdapter{}, false, false)

	resp, ok, _, err := matchCommand(context.Background(), logger, comQueryReq(dbmReader+body), db, newDecodeCtx(), eng, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("the same statement behind a different-length DBM comment must be served")
	}
	if resp == nil {
		t.Fatal("a match must carry a response")
	}
	if resp.Payload != "target-row" {
		t.Errorf("served %q, want \"target-row\" — a different statement's recorded response was returned", resp.Payload)
	}
}

// TestMatchCommand_NoMocksPhaseStillReachable keeps the REPLAY-ORPHAN
// diagnostic alive.
//
// query.go raises "no data mock available for this query" only on
// MatchPhaseNoMocks, which is derived from the candidate count — so WHERE that
// count is incremented decides whether the diagnostic can ever fire. Count it
// before the lifetime filter and a lone handshake mock keeps it non-zero
// forever, silently retiring the message.
//
// Here the pool holds only a session-lifetime handshake mock, which the command
// phase skips. Nothing was compared, so the phase must say so.
func TestMatchCommand_NoMocksPhaseStillReachable(t *testing.T) {
	handshake := &models.Mock{Name: "hs", Kind: models.MySQL}
	handshake.TestModeInfo.Lifetime = models.LifetimeSession
	handshake.Spec.Metadata = map[string]string{"type": "config"}
	handshake.Spec.MySQLRequests = []mysql.Request{{
		PacketBundle: mysql.PacketBundle{
			Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 32}, Type: "HandshakeResponse41"},
			Message: &mysql.HandshakeResponse41Packet{},
		},
	}}

	db := &fakeMockDb{session: []*models.Mock{handshake}}
	eng := schemanoise.New(mysqlNoiseAdapter{}, false, false)

	_, ok, miss, err := matchCommand(context.Background(), zap.NewNop(),
		comQueryReq("SELECT 1"), db, newDecodeCtx(), eng, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Fatal("a handshake mock must not answer a COM_QUERY")
	}
	if miss.candidateCount != 0 {
		t.Errorf("candidateCount = %d, want 0 — a handshake mock is skipped by the command "+
			"phase and must not be counted as a candidate", miss.candidateCount)
	}
	if miss.matchPhase != models.MatchPhaseNoMocks {
		t.Errorf("matchPhase = %q, want %q — otherwise the REPLAY-ORPHAN diagnostic can never fire",
			miss.matchPhase, models.MatchPhaseNoMocks)
	}
}
