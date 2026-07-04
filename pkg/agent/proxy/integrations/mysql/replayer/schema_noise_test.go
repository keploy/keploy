package replayer

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/schemanoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// noiseCapturingDb wraps fakeMockDb to capture the mock state handed to the
// consume/update paths, so tests can observe the ReqBodyNoise that updateMock
// merged onto the copies (the learn carry-out).
type noiseCapturingDb struct {
	*fakeMockDb
	captured []models.Mock
}

func (d *noiseCapturingDb) DeleteFilteredMock(m models.Mock) bool {
	d.captured = append(d.captured, m)
	return d.fakeMockDb.DeleteFilteredMock(m)
}

func (d *noiseCapturingDb) UpdateUnFilteredMock(old, updated *models.Mock) bool {
	d.captured = append(d.captured, *updated)
	return d.fakeMockDb.UpdateUnFilteredMock(old, updated)
}

// capturedNoiseFor returns the ReqBodyNoise captured for a mock name, if any.
func (d *noiseCapturingDb) capturedNoiseFor(name string) map[string][]string {
	for _, m := range d.captured {
		if m.Name == name && len(m.Spec.ReqBodyNoise) > 0 {
			return m.Spec.ReqBodyNoise
		}
	}
	return nil
}

// execMock builds a session-tier COM_STMT_EXECUTE data mock with a single
// string parameter (mirrors readbackMock's lax-promoted shape).
func execMock(name, paramValue string) *models.Mock {
	m := &models.Mock{Name: name, Kind: models.MySQL}
	m.TestModeInfo.Lifetime = models.LifetimeSession
	m.Spec.Metadata = map[string]string{"type": "mocks"}
	m.Spec.MySQLRequests = []mysql.Request{{PacketBundle: execBundle(paramValue)}}
	m.Spec.MySQLResponses = []mysql.Response{{
		PacketBundle: mysql.PacketBundle{
			Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 32, SequenceID: 1}, Type: "BinaryProtocolResultSet"},
			Message: &mysql.BinaryProtocolResultSet{},
		},
	}}
	return m
}

// sldMockOf builds a COM_STMT_SEND_LONG_DATA mock carrying the given chunk.
func sldMockOf(name string, data []byte) *models.Mock {
	m := &models.Mock{Name: name, Kind: models.MySQL}
	m.TestModeInfo.Lifetime = models.LifetimeSession
	m.Spec.Metadata = map[string]string{"type": "mocks"}
	m.Spec.MySQLRequests = []mysql.Request{{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: uint32(len(data) + 7), SequenceID: 0}, Type: "COM_STMT_SEND_LONG_DATA"},
		Message: &mysql.StmtSendLongDataPacket{Status: 0x18, StatementID: 1, ParameterID: 1, Data: data},
	}}}
	return m
}

func sldReq(data []byte) mysql.Request {
	return mysql.Request{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: uint32(len(data) + 7), SequenceID: 0}, Type: "COM_STMT_SEND_LONG_DATA"},
		Message: &mysql.StmtSendLongDataPacket{Status: 0x18, StatementID: 7, ParameterID: 1, Data: data},
	}}
}

// TestMySQLRequestBodyJSON locks in the canonical JSON vocabulary the noise
// engine diffs: per-packet field paths, index-keyed parameters (arrays would
// collapse to a single "[]" bucket in the JSON differ), and ok=false for
// packets with no drift-capable body.
func TestMySQLRequestBodyJSON(t *testing.T) {
	t.Run("COM_QUERY -> literal-split query shape", func(t *testing.T) {
		bundle := comQueryReq("SELECT 'drift-sentinel'").PacketBundle
		b, ok := mysqlRequestBodyJSON(&bundle)
		if !ok {
			t.Fatal("expected ok for COM_QUERY")
		}
		var doc struct {
			Query struct {
				Template string            `json:"template"`
				Literals map[string]string `json:"literals"`
			} `json:"query"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if doc.Query.Template == "" || strings.Contains(doc.Query.Template, "drift-sentinel") {
			t.Errorf("template must replace literals with placeholders, got %q", doc.Query.Template)
		}
		if doc.Query.Literals["0"] != "drift-sentinel" {
			t.Errorf("literals.0 = %v", doc.Query.Literals)
		}
	})

	t.Run("COM_QUERY unparseable SQL falls back to whole text", func(t *testing.T) {
		bundle := comQueryReq("%%% definitely not sql %%%").PacketBundle
		b, ok := mysqlRequestBodyJSON(&bundle)
		if !ok {
			t.Fatal("expected ok even for unparseable SQL")
		}
		var doc map[string]any
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if doc["query"] != "%%% definitely not sql %%%" {
			t.Errorf("unparseable SQL must stay a plain string, got %v", doc["query"])
		}
	})

	t.Run("COM_STMT_EXECUTE -> index-keyed parameters", func(t *testing.T) {
		bundle := execBundle("uuid-1")
		b, ok := mysqlRequestBodyJSON(&bundle)
		if !ok {
			t.Fatal("expected ok for COM_STMT_EXECUTE")
		}
		var doc struct {
			Parameters map[string]map[string]any `json:"parameters"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		p0, ok := doc.Parameters["0"]
		if !ok {
			t.Fatalf("parameters must be keyed by index, got %v", doc.Parameters)
		}
		if p0["value"] != "uuid-1" {
			t.Errorf("parameters.0.value = %v", p0["value"])
		}
	})

	t.Run("SEND_LONG_DATA json chunk embeds raw for field-level paths", func(t *testing.T) {
		bundle := sldReq([]byte(`{"id":"a","pad":"x"}`)).PacketBundle
		b, ok := mysqlRequestBodyJSON(&bundle)
		if !ok {
			t.Fatal("expected ok for SLD")
		}
		var doc struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(b, &doc); err != nil {
			t.Fatalf("json chunk must embed as raw JSON: %v", err)
		}
		if doc.Data["id"] != "a" {
			t.Errorf("data.id = %v", doc.Data["id"])
		}
	})

	t.Run("SEND_LONG_DATA binary chunk becomes opaque string", func(t *testing.T) {
		bundle := sldReq([]byte{0x00, 0x01, 0xff}).PacketBundle
		b, ok := mysqlRequestBodyJSON(&bundle)
		if !ok {
			t.Fatal("expected ok for SLD")
		}
		var doc struct {
			Data string `json:"data"`
		}
		if err := json.Unmarshal(b, &doc); err != nil || doc.Data == "" {
			t.Fatalf("binary chunk must serialize to a base64 string, got %s (err %v)", b, err)
		}
	})

	t.Run("utility packet has no body", func(t *testing.T) {
		bundle := mysql.PacketBundle{
			Header:  &mysql.PacketInfo{Header: &mysql.Header{PayloadLength: 1}, Type: "COM_PING"},
			Message: &mysql.PingPacket{Command: 0x0e},
		}
		if _, ok := mysqlRequestBodyJSON(&bundle); ok {
			t.Error("COM_PING must have no diffable body")
		}
	})
}

// TestMySQLNoiseAdapter_DetectExecParamDrift exercises the adapter through the
// shared engine: a drifted bound parameter must surface as its per-position
// field path, and an unchanged one must not.
func TestMySQLNoiseAdapter_DetectExecParamDrift(t *testing.T) {
	eng := schemanoise.New(mysqlNoiseAdapter{}, true /*detection*/, false)
	mock := execMock("m1", "recorded-uuid")
	liveBundle := execBundle("replay-uuid")
	live, _ := mysqlRequestBodyJSON(&liveBundle)

	drift, comparable := eng.Detect(mock, live, nil)
	if !comparable {
		t.Fatal("canonical JSON bodies must be comparable")
	}
	if _, ok := drift["body.parameters.0.value"]; !ok {
		t.Fatalf("expected drift on body.parameters.0.value, got %v", drift)
	}

	// Same params -> nothing to learn.
	sameBundle := execBundle("recorded-uuid")
	liveSame, _ := mysqlRequestBodyJSON(&sameBundle)
	drift, _ = eng.Detect(mock, liveSame, nil)
	if len(drift) != 0 {
		t.Fatalf("no drift expected for identical params, got %v", drift)
	}
}

// TestMatchCommand_ExecStrict is the end-to-end strict contract for
// COM_STMT_EXECUTE through matchCommand: a candidate whose bound parameter
// drifted is served leniently, rejected under strict, and served again under
// strict once the drifting path is learned as req_body_noise.
func TestMatchCommand_ExecStrict(t *testing.T) {
	logger := zap.NewNop()
	req := mysql.Request{PacketBundle: execBundle("replay-uuid")}

	newDb := func(m *models.Mock) *noiseCapturingDb {
		return &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{m}}}
	}

	t.Run("lenient serves the drifted candidate", func(t *testing.T) {
		db := newDb(execMock("m1", "recorded-uuid"))
		_, ok, _, err := matchCommand(context.Background(), logger, req, db, newDecodeCtx(), nil, nil)
		if err != nil || !ok {
			t.Fatalf("lenient path must serve the score-based candidate (ok=%v err=%v)", ok, err)
		}
	})

	t.Run("strict rejects unmarked param drift", func(t *testing.T) {
		db := newDb(execMock("m1", "recorded-uuid"))
		eng := schemanoise.New(mysqlNoiseAdapter{}, false, true /*strict*/)
		_, ok, miss, err := matchCommand(context.Background(), logger, req, db, newDecodeCtx(), eng, nil)
		if err == nil && ok {
			t.Fatal("strict must reject a candidate whose param drifted outside noise")
		}
		// The miss diagnostics must name the closest EXECUTE candidate (which
		// used to be empty for EXEC) and the drifted param path with values,
		// so the mismatch report can render a FIELD | EXPECTED | RECEIVED row.
		if miss == nil || miss.closestMock != "m1" {
			t.Fatalf("expected closest mock m1 for EXEC rejection, got %+v", miss)
		}
		foundParamDiff := false
		for _, d := range miss.fieldDiffs {
			if d.Path == "body.parameters.0.value" && d.Expected == "recorded-uuid" && d.Actual == "replay-uuid" {
				foundParamDiff = true
			}
		}
		if !foundParamDiff {
			t.Errorf("miss must carry the drifted param diff with values, got %+v", miss.fieldDiffs)
		}
	})

	t.Run("strict allows drift covered by learned noise", func(t *testing.T) {
		m := execMock("m1", "recorded-uuid")
		m.Spec.ReqBodyNoise = map[string][]string{"body.parameters.0.value": {}}
		db := newDb(m)
		eng := schemanoise.New(mysqlNoiseAdapter{}, false, true /*strict*/)
		_, ok, _, err := matchCommand(context.Background(), logger, req, db, newDecodeCtx(), eng, nil)
		if err != nil || !ok {
			t.Fatalf("strict must serve a candidate whose only drift is learned noise (ok=%v err=%v)", ok, err)
		}
	})

	t.Run("strict allows drift covered by user requestBody noise", func(t *testing.T) {
		db := newDb(execMock("m1", "recorded-uuid"))
		eng := schemanoise.New(mysqlNoiseAdapter{}, false, true /*strict*/)
		user := map[string][]string{"parameters.0.value": {}}
		_, ok, _, err := matchCommand(context.Background(), logger, req, db, newDecodeCtx(), eng, user)
		if err != nil || !ok {
			t.Fatalf("strict must honour user-configured request-body noise (ok=%v err=%v)", ok, err)
		}
	})
}

// TestMatchCommand_ExecDetectionLearns verifies the learn carry-out: with
// detection on, serving a param-drifted EXECUTE candidate must merge the
// drifted path onto the mock copy handed to the mock DB.
func TestMatchCommand_ExecDetectionLearns(t *testing.T) {
	logger := zap.NewNop()
	req := mysql.Request{PacketBundle: execBundle("replay-uuid")}
	db := &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{execMock("m1", "recorded-uuid")}}}
	eng := schemanoise.New(mysqlNoiseAdapter{}, true /*detection*/, false)

	_, ok, _, err := matchCommand(context.Background(), logger, req, db, newDecodeCtx(), eng, nil)
	if err != nil || !ok {
		t.Fatalf("detection replay must still serve leniently (ok=%v err=%v)", ok, err)
	}
	noise := db.capturedNoiseFor("m1")
	if _, learned := noise["body.parameters.0.value"]; !learned {
		t.Fatalf("expected body.parameters.0.value learned on the updated mock, got %v", noise)
	}
	// The shared pooled mock must never be mutated (copy-on-learn).
	if len(db.session) > 0 && len(db.session[0].Spec.ReqBodyNoise) != 0 {
		t.Fatalf("pooled mock was mutated in place: %v", db.session[0].Spec.ReqBodyNoise)
	}
}

// TestMatchCommand_ComQueryStrict covers the text-protocol path: two INSERTs
// with the same AST structure but drifted inlined literals. Lenient serves the
// structure-matched candidate by score; strict rejects it until body.query is
// noise.
func TestMatchCommand_ComQueryStrict(t *testing.T) {
	logger := zap.NewNop()
	const recorded = "INSERT INTO events (id, name) VALUES ('recorded-uuid', 'e')"
	const live = "INSERT INTO events (id, name) VALUES ('replay-uuid', 'e')"
	req := comQueryReq(live)

	t.Run("lenient serves structure-matched candidate", func(t *testing.T) {
		db := &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{readbackMock("q1", recorded, "row-1", zeroTime())}}}
		_, ok, _, err := matchCommand(context.Background(), logger, req, db, newDecodeCtx(), nil, nil)
		if err != nil || !ok {
			t.Fatalf("lenient path must serve the structure-matched candidate (ok=%v err=%v)", ok, err)
		}
	})

	t.Run("strict rejects literal drift, reports closest mock", func(t *testing.T) {
		db := &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{readbackMock("q1", recorded, "row-1", zeroTime())}}}
		eng := schemanoise.New(mysqlNoiseAdapter{}, false, true)
		_, ok, miss, err := matchCommand(context.Background(), logger, req, db, newDecodeCtx(), eng, nil)
		if err == nil && ok {
			t.Fatal("strict must reject unmarked literal drift in COM_QUERY text")
		}
		if miss == nil || miss.closestMock != "q1" {
			t.Fatalf("mismatch report should still name the closest mock, got %+v", miss)
		}
		if miss.strictRejected == 0 {
			t.Error("miss must count strict-rejected candidates")
		}
		foundLiteralDiff := false
		for _, d := range miss.fieldDiffs {
			if d.Path == "body.query.literals.0" && d.Expected == "recorded-uuid" && d.Actual == "replay-uuid" {
				foundLiteralDiff = true
			}
		}
		if !foundLiteralDiff {
			t.Errorf("miss must carry the drifted literal diff with values, got %+v", miss.fieldDiffs)
		}
	})

	t.Run("strict-rejected exact-text candidate still names closest query", func(t *testing.T) {
		// Identical SQL text (exact match) but a drifting query attribute
		// (CLIENT_QUERY_ATTRIBUTES) — strict must reject, and the miss must
		// carry the candidate's query so the report diff isn't blank.
		m := readbackMock("q1", recorded, "row-1", zeroTime())
		m.Spec.MySQLRequests[0].PacketBundle.Message.(*mysql.QueryPacket).Parameters =
			[]mysql.Parameter{{Type: 254 /* MYSQL_TYPE_STRING */, Value: "recorded-attr"}}
		exactReq := comQueryReq(recorded)
		exactReq.PacketBundle.Message.(*mysql.QueryPacket).Parameters =
			[]mysql.Parameter{{Type: 254, Value: "replay-attr"}}
		db := &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{m}}}
		eng := schemanoise.New(mysqlNoiseAdapter{}, false, true)
		_, ok, miss, err := matchCommand(context.Background(), logger, exactReq, db, newDecodeCtx(), eng, nil)
		if err == nil && ok {
			t.Fatal("strict must reject unmarked query-attribute drift even on an exact-text match")
		}
		if miss == nil || miss.closestMock != "q1" {
			t.Fatalf("mismatch report should name the strict-rejected exact candidate, got %+v", miss)
		}
		if miss.closestQuery != recorded {
			t.Errorf("miss must carry the exact candidate's query as closest, got %q", miss.closestQuery)
		}
		if miss.strictRejected == 0 {
			t.Error("miss must count strict-rejected candidates")
		}
	})

	t.Run("strict allows learned body.query noise", func(t *testing.T) {
		m := readbackMock("q1", recorded, "row-1", zeroTime())
		m.Spec.ReqBodyNoise = map[string][]string{"body.query": {}}
		db := &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{m}}}
		eng := schemanoise.New(mysqlNoiseAdapter{}, false, true)
		_, ok, _, err := matchCommand(context.Background(), logger, req, db, newDecodeCtx(), eng, nil)
		if err != nil || !ok {
			t.Fatalf("strict must serve when body.query is learned noise (ok=%v err=%v)", ok, err)
		}
	})

	t.Run("detection learns only the drifting literal position", func(t *testing.T) {
		db := &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{readbackMock("q1", recorded, "row-1", zeroTime())}}}
		eng := schemanoise.New(mysqlNoiseAdapter{}, true, false)
		_, ok, _, err := matchCommand(context.Background(), logger, req, db, newDecodeCtx(), eng, nil)
		if err != nil || !ok {
			t.Fatalf("detection replay must serve leniently (ok=%v err=%v)", ok, err)
		}
		noise := db.capturedNoiseFor("q1")
		if _, has := noise["body.query.literals.0"]; !has {
			t.Fatalf("expected body.query.literals.0 in learned noise, got %v", noise)
		}
		// The stable 'e' literal (position 1) and the template must NOT be
		// flagged — per-position granularity is the point of the shape split.
		if _, has := noise["body.query.literals.1"]; has {
			t.Fatalf("stable literal must not be learned as noise, got %v", noise)
		}
		if _, has := noise["body.query.template"]; has {
			t.Fatalf("template must not be learned for literal-only drift, got %v", noise)
		}
	})

	t.Run("strict allows learned per-literal noise", func(t *testing.T) {
		m := readbackMock("q1", recorded, "row-1", zeroTime())
		m.Spec.ReqBodyNoise = map[string][]string{"body.query.literals.0": {}}
		db := &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{m}}}
		eng := schemanoise.New(mysqlNoiseAdapter{}, false, true)
		_, ok, _, err := matchCommand(context.Background(), logger, req, db, newDecodeCtx(), eng, nil)
		if err != nil || !ok {
			t.Fatalf("strict must serve when only the noised literal drifts (ok=%v err=%v)", ok, err)
		}
	})

	t.Run("strict keeps serving an exact match", func(t *testing.T) {
		db := &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{readbackMock("q1", live, "row-1", zeroTime())}}}
		eng := schemanoise.New(mysqlNoiseAdapter{}, false, true)
		_, ok, _, err := matchCommand(context.Background(), logger, req, db, newDecodeCtx(), eng, nil)
		if err != nil || !ok {
			t.Fatalf("strict must not affect an exact-text match (ok=%v err=%v)", ok, err)
		}
	})
}

// TestMatchCommand_SendLongDataStrict covers the streamed-blob path, which
// historically had no content comparison at all: lenient consumes the first
// recorded chunk regardless of drift (and detection names it); strict rejects
// unmarked drift instead of silently passing.
func TestMatchCommand_SendLongDataStrict(t *testing.T) {
	logger := zap.NewNop()
	recorded := []byte(`{"id":"recorded","pad":"x"}`)
	live := sldReq([]byte(`{"id":"replay","pad":"x"}`))

	t.Run("lenient consumes and detection learns field path inside chunk", func(t *testing.T) {
		db := &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{sldMockOf("sld1", recorded)}}}
		eng := schemanoise.New(mysqlNoiseAdapter{}, true, false)
		_, ok, _, err := matchCommand(context.Background(), logger, live, db, newDecodeCtx(), eng, nil)
		if err != nil || !ok {
			t.Fatalf("lenient SLD must be accepted (ok=%v err=%v)", ok, err)
		}
		noise := db.capturedNoiseFor("sld1")
		if _, has := noise["body.data.id"]; !has {
			t.Fatalf("expected body.data.id learned for the JSON chunk, got %v", noise)
		}
	})

	t.Run("strict rejects unmarked chunk drift", func(t *testing.T) {
		db := &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{sldMockOf("sld1", recorded)}}}
		eng := schemanoise.New(mysqlNoiseAdapter{}, false, true)
		_, ok, miss, err := matchCommand(context.Background(), logger, live, db, newDecodeCtx(), eng, nil)
		if err == nil && ok {
			t.Fatal("strict must reject SLD data drift outside noise")
		}
		if miss == nil || miss.closestMock != "sld1" {
			t.Fatalf("expected closest mock sld1 in report, got %+v", miss)
		}
		foundDataDiff := false
		for _, d := range miss.fieldDiffs {
			if d.Path == "body.data.id" {
				foundDataDiff = true
			}
		}
		if !foundDataDiff {
			t.Errorf("miss must carry a body.data.id field diff, got %+v", miss.fieldDiffs)
		}
	})

	t.Run("strict allows learned chunk noise", func(t *testing.T) {
		m := sldMockOf("sld1", recorded)
		m.Spec.ReqBodyNoise = map[string][]string{"body.data.id": {}}
		db := &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{m}}}
		eng := schemanoise.New(mysqlNoiseAdapter{}, false, true)
		_, ok, _, err := matchCommand(context.Background(), logger, live, db, newDecodeCtx(), eng, nil)
		if err != nil || !ok {
			t.Fatalf("strict must consume when the chunk drift is learned noise (ok=%v err=%v)", ok, err)
		}
	})

	t.Run("no recorded SLD mocks stays a graceful accept even under strict", func(t *testing.T) {
		db := &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{readbackMock("q1", "SELECT 1", "row", zeroTime())}}}
		eng := schemanoise.New(mysqlNoiseAdapter{}, false, true)
		_, ok, _, err := matchCommand(context.Background(), logger, live, db, newDecodeCtx(), eng, nil)
		if err != nil || !ok {
			t.Fatalf("SLD with no recorded candidates must stay a graceful no-response accept (ok=%v err=%v)", ok, err)
		}
	})
}

// zeroTime keeps the readbackMock helper usable with no active test window.
func zeroTime() time.Time { return time.Time{} }

// TestExtractQueryShape locks in the literal-split contract: static values
// come out per-position, the template carries placeholders instead, and
// statement comments (per-request trace ids) live on their own field so they
// drift independently of the template.
func TestExtractQueryShape(t *testing.T) {
	t.Run("literals extracted per position", func(t *testing.T) {
		shape := extractQueryShape("INSERT INTO events (id, name, created_at) VALUES ('u-42', 'text-event', '2026-07-02 08:09:49')")
		if !shape.ok {
			t.Fatal("expected parseable SQL")
		}
		want := []string{"u-42", "text-event", "2026-07-02 08:09:49"}
		if len(shape.literals) != len(want) {
			t.Fatalf("literals = %v, want %v", shape.literals, want)
		}
		for i, w := range want {
			if shape.literals[i] != w {
				t.Errorf("literals[%d] = %q, want %q", i, shape.literals[i], w)
			}
		}
		for _, w := range want {
			if strings.Contains(shape.template, w) {
				t.Errorf("template still contains literal %q: %s", w, shape.template)
			}
		}
	})

	t.Run("comments split out of the template", func(t *testing.T) {
		a := extractQueryShape("SELECT /* traceparent=aaa */ customer FROM orders WHERE amount > 50")
		b := extractQueryShape("SELECT /* traceparent=bbb */ customer FROM orders WHERE amount > 50")
		if !a.ok || !b.ok {
			t.Fatal("expected parseable SQL")
		}
		if a.template != b.template {
			t.Errorf("templates must be identical when only the comment drifts:\n  a=%s\n  b=%s", a.template, b.template)
		}
		if a.comments == b.comments || a.comments == "" {
			t.Errorf("comments must carry the drifting trace id, got a=%q b=%q", a.comments, b.comments)
		}
	})

	t.Run("placeholders are not literals", func(t *testing.T) {
		shape := extractQueryShape("SELECT customer FROM orders WHERE id = ? AND amount > 100")
		if !shape.ok {
			t.Fatal("expected parseable SQL")
		}
		if len(shape.literals) != 1 || shape.literals[0] != "100" {
			t.Errorf("only the inline 100 is a literal (? is a bind arg), got %v", shape.literals)
		}
	})

	t.Run("unparseable SQL reports ok=false", func(t *testing.T) {
		if shape := extractQueryShape("%%% not sql %%%"); shape.ok {
			t.Errorf("expected ok=false, got %+v", shape)
		}
	})
}

// TestMatchCommand_TraceCommentDrift covers the per-request trace-comment
// case end-to-end: identical query except the comment. Detection must learn
// body.query.comments (not the template, not a literal), and strict must then
// serve it.
func TestMatchCommand_TraceCommentDrift(t *testing.T) {
	logger := zap.NewNop()
	const recorded = "SELECT /* traceparent=recorded */ customer FROM orders WHERE amount > 50"
	const live = "SELECT /* traceparent=replay */ customer FROM orders WHERE amount > 50"
	req := comQueryReq(live)

	t.Run("detection learns body.query.comments only", func(t *testing.T) {
		db := &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{readbackMock("q1", recorded, "row-1", zeroTime())}}}
		eng := schemanoise.New(mysqlNoiseAdapter{}, true, false)
		_, ok, _, err := matchCommand(context.Background(), logger, req, db, newDecodeCtx(), eng, nil)
		if err != nil || !ok {
			t.Fatalf("detection replay must serve leniently (ok=%v err=%v)", ok, err)
		}
		noise := db.capturedNoiseFor("q1")
		if _, has := noise["body.query.comments"]; !has {
			t.Fatalf("expected body.query.comments learned, got %v", noise)
		}
		if _, has := noise["body.query.template"]; has {
			t.Fatalf("template must not be noise for comment-only drift, got %v", noise)
		}
	})

	t.Run("strict allows learned comment noise", func(t *testing.T) {
		m := readbackMock("q1", recorded, "row-1", zeroTime())
		m.Spec.ReqBodyNoise = map[string][]string{"body.query.comments": {}}
		db := &noiseCapturingDb{fakeMockDb: &fakeMockDb{session: []*models.Mock{m}}}
		eng := schemanoise.New(mysqlNoiseAdapter{}, false, true)
		_, ok, _, err := matchCommand(context.Background(), logger, req, db, newDecodeCtx(), eng, nil)
		if err != nil || !ok {
			t.Fatalf("strict must serve when only the noised comment drifts (ok=%v err=%v)", ok, err)
		}
	})
}
