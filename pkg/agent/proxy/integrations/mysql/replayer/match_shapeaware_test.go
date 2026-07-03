package replayer

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/schemanoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// comQueryMockLen builds a session-tier COM_QUERY data mock whose header
// payload length tracks the SQL text (len+1 for the command byte), unlike
// readbackMock's fixed length — needed to exercise the length-sensitive
// scoring paths the shape-aware refinements exist for.
func comQueryMockLen(name, sql string) *models.Mock {
	m := &models.Mock{Name: name, Kind: models.MySQL}
	m.TestModeInfo.Lifetime = models.LifetimeSession
	m.Spec.Metadata = map[string]string{"type": "mocks"}
	m.Spec.MySQLRequests = []mysql.Request{{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: uint32(len(sql) + 1), SequenceID: 0}, Type: "COM_QUERY"},
		Message: &mysql.QueryPacket{Query: sql},
	}}}
	m.Spec.MySQLResponses = []mysql.Response{{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 32, SequenceID: 1}, Type: "TextResultSet"},
		Message: &mysql.TextResultSet{},
	}}}
	return m
}

func comQueryReqLen(sql string) mysql.Request {
	return mysql.Request{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: uint32(len(sql) + 1), SequenceID: 0}, Type: "COM_QUERY"},
		Message: &mysql.QueryPacket{Query: sql},
	}}
}

// stmtPrepareMockLen builds a COM_STMT_PREPARE mock with text-tracking
// payload length and a minimal PREPARE_OK response.
func stmtPrepareMockLen(name, sql string) *models.Mock {
	m := &models.Mock{Name: name, Kind: models.MySQL}
	m.TestModeInfo.Lifetime = models.LifetimeSession
	m.Spec.Metadata = map[string]string{"type": "mocks"}
	m.Spec.MySQLRequests = []mysql.Request{{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: uint32(len(sql) + 1), SequenceID: 0}, Type: "COM_STMT_PREPARE"},
		Message: &mysql.StmtPreparePacket{Command: 0x16, Query: sql},
	}}}
	m.Spec.MySQLResponses = []mysql.Response{{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 12, SequenceID: 1}, Type: mysql.COM_STMT_PREPARE_OK},
		Message: &mysql.StmtPrepareOkPacket{StatementID: 1, NumParams: 2},
	}}}
	return m
}

// TestExtractQueryShape_Placeholders locks in the parsed-placeholder contract:
// real `?` bind args are counted, '?' bytes inside string literals are not,
// and the literal-replacement naming can never collide with vitess's own
// v1/v2/… placeholder arguments.
func TestExtractQueryShape_Placeholders(t *testing.T) {
	t.Run("literal '?' is not a placeholder", func(t *testing.T) {
		shape := extractQueryShape("INSERT INTO msgs (body) VALUES ('Are you there?')")
		if !shape.ok {
			t.Fatal("expected parseable SQL")
		}
		if shape.placeholders != 0 {
			t.Errorf("placeholders = %d, want 0 ('?' inside a string literal)", shape.placeholders)
		}
	})

	t.Run("real bind args are counted", func(t *testing.T) {
		shape := extractQueryShape("SELECT c FROM t WHERE a = ? AND b IN (?, ?) AND note = 'why?'")
		if !shape.ok {
			t.Fatal("expected parseable SQL")
		}
		if shape.placeholders != 3 {
			t.Errorf("placeholders = %d, want 3", shape.placeholders)
		}
	})

	t.Run("swapped placeholder/literal positions yield DIFFERENT templates", func(t *testing.T) {
		a := extractQueryShape("SELECT c FROM t WHERE a = ? AND b = 5")
		b := extractQueryShape("SELECT c FROM t WHERE a = 5 AND b = ?")
		if !a.ok || !b.ok {
			t.Fatal("expected parseable SQL")
		}
		if a.template == b.template {
			t.Fatalf("templates must differ (arg-name collision): %q", a.template)
		}
	})
}

// TestMatchCommand_ShapeAware_NonDMLLengthDrift is the row-1 fix contract: a
// SELECT whose drifted literal CHANGES the query's byte length is unmatchable
// in normal replay (non-DML earns no structure rescue and misses the
// payload-length point), but under schema-noise the template-equal rescue
// serves it, detection learns the literal path, and strict gates it.
func TestMatchCommand_ShapeAware_NonDMLLengthDrift(t *testing.T) {
	logger := zap.NewNop()
	const recorded = "SELECT COUNT(*) FROM events WHERE amount > 99.5"
	const live = "SELECT COUNT(*) FROM events WHERE amount > 105.75"
	req := comQueryReqLen(live)

	newDb := func(m *models.Mock) *noiseCapturingDb {
		return &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{m}}}
	}

	t.Run("normal replay: rejected (unchanged behavior)", func(t *testing.T) {
		db := newDb(comQueryMockLen("q1", recorded))
		_, ok, _, err := matchCommand(context.Background(), logger, req, db, newDecodeCtx(), nil, nil)
		if err == nil && ok {
			t.Fatal("normal replay must NOT match a length-changed non-DML drift")
		}
	})

	t.Run("detection: served and literal learned", func(t *testing.T) {
		db := newDb(comQueryMockLen("q1", recorded))
		eng := schemanoise.New(mysqlNoiseAdapter{}, true, false)
		_, ok, _, err := matchCommand(context.Background(), logger, req, db, newDecodeCtx(), eng, nil)
		if err != nil || !ok {
			t.Fatalf("detection must serve the template-equal SELECT (ok=%v err=%v)", ok, err)
		}
		noise := db.capturedNoiseFor("q1")
		if _, has := noise["body.query.literals.0"]; !has {
			t.Fatalf("expected body.query.literals.0 learned, got %v", noise)
		}
	})

	t.Run("strict: rejected with the literal named", func(t *testing.T) {
		db := newDb(comQueryMockLen("q1", recorded))
		eng := schemanoise.New(mysqlNoiseAdapter{}, false, true)
		_, ok, miss, err := matchCommand(context.Background(), logger, req, db, newDecodeCtx(), eng, nil)
		if err == nil && ok {
			t.Fatal("strict must reject unmarked literal drift")
		}
		found := false
		for _, d := range miss.fieldDiffs {
			if d.Path == "body.query.literals.0" {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected body.query.literals.0 in field diffs, got %+v", miss.fieldDiffs)
		}
	})

	t.Run("strict + learned noise: served", func(t *testing.T) {
		m := comQueryMockLen("q1", recorded)
		m.Spec.ReqBodyNoise = map[string][]string{"body.query.literals.0": {}}
		db := newDb(m)
		eng := schemanoise.New(mysqlNoiseAdapter{}, false, true)
		_, ok, _, err := matchCommand(context.Background(), logger, req, db, newDecodeCtx(), eng, nil)
		if err != nil || !ok {
			t.Fatalf("strict must serve when the drifting literal is learned noise (ok=%v err=%v)", ok, err)
		}
	})

	t.Run("different table never rescued even under detection", func(t *testing.T) {
		db := newDb(comQueryMockLen("q1", "SELECT COUNT(*) FROM orders WHERE amount > 99.5"))
		eng := schemanoise.New(mysqlNoiseAdapter{}, true, false)
		_, ok, _, err := matchCommand(context.Background(), logger, req, db, newDecodeCtx(), eng, nil)
		if err == nil && ok {
			t.Fatal("template includes table names: a different-table SELECT must not match")
		}
	})
}

// TestMatchCommand_ShapeAware_QuestionMarkInLiteral is the row-2 measurement
// fix: a '?' byte inside a drifting string literal is value content. Normal
// replay keeps the historical byte-count rejection; schema-noise counts
// parsed bind args and treats it as ordinary learnable literal drift.
func TestMatchCommand_ShapeAware_QuestionMarkInLiteral(t *testing.T) {
	logger := zap.NewNop()
	const recorded = "INSERT INTO msgs (body) VALUES ('Are you there?')"
	const live = "INSERT INTO msgs (body) VALUES ('ok, on my way')"
	req := comQueryReqLen(live)

	t.Run("normal replay: rejected by byte-count gate (unchanged)", func(t *testing.T) {
		db := &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{comQueryMockLen("q1", recorded)}}}
		_, ok, _, err := matchCommand(context.Background(), logger, req, db, newDecodeCtx(), nil, nil)
		if err == nil && ok {
			t.Fatal("normal replay must keep the historical byte-count rejection")
		}
	})

	t.Run("detection: served and literal learned", func(t *testing.T) {
		db := &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{comQueryMockLen("q1", recorded)}}}
		eng := schemanoise.New(mysqlNoiseAdapter{}, true, false)
		_, ok, _, err := matchCommand(context.Background(), logger, req, db, newDecodeCtx(), eng, nil)
		if err != nil || !ok {
			t.Fatalf("detection must treat literal '?' as value content (ok=%v err=%v)", ok, err)
		}
		noise := db.capturedNoiseFor("q1")
		if _, has := noise["body.query.literals.0"]; !has {
			t.Fatalf("expected body.query.literals.0 learned, got %v", noise)
		}
	})
}

// TestMatchQuery_ShapeAware_RealArityStillRejected pins the row-2 principle:
// a genuine placeholder-arity change is structural and stays a hard reject at
// the matcher in EVERY mode — schema-noise refinements must not soften it.
// (Asserted on matchPreparePacket directly: at the matchCommand level an
// unmatched PREPARE is answered with a synthetic PREPARE_OK by design, so the
// recorded mock not being matched/consumed is the observable outcome there.)
func TestMatchQuery_ShapeAware_RealArityStillRejected(t *testing.T) {
	logger := zap.NewNop()
	recordedBundle := stmtPrepareMockLen("p1", "SELECT id FROM orders WHERE seq IN (?, ?)").Spec.MySQLRequests[0].PacketBundle
	liveBundle := stmtPrepareReq("SELECT id FROM orders WHERE seq IN (?, ?, ?)").PacketBundle

	for _, tc := range []struct {
		name       string
		shapeAware bool
	}{
		{"normal replay", false},
		{"schema-noise", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ok, score := matchPreparePacket(context.Background(), logger, recordedBundle, liveBundle, tc.shapeAware)
			if ok || score != 0 {
				t.Fatalf("placeholder arity change must hard-reject with score 0, got ok=%v score=%d", ok, score)
			}
		})
	}

	// And at the matchCommand level: the recorded mock must never be
	// matched/consumed for the arity-changed live PREPARE (the returned OK is
	// the synthetic orphan-PREPARE response, not the mock).
	db := &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{stmtPrepareMockLen("p1", "SELECT id FROM orders WHERE seq IN (?, ?)")}}}
	eng := schemanoise.New(mysqlNoiseAdapter{}, true, false)
	_, ok, _, err := matchCommand(context.Background(), zap.NewNop(), stmtPrepareReq("SELECT id FROM orders WHERE seq IN (?, ?, ?)"), db, newDecodeCtx(), eng, nil)
	if err != nil || !ok {
		t.Fatalf("unmatched PREPARE is answered synthetically by design (ok=%v err=%v)", ok, err)
	}
	if len(db.captured) != 0 {
		t.Fatalf("recorded mock must not be consumed for an arity-changed PREPARE, captured %d", len(db.captured))
	}
}
