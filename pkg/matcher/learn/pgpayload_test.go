package learn

import (
	"math"
	"math/big"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"go.keploy.io/server/v3/pkg/models"
	yaml "gopkg.in/yaml.v3"
)

// reloaded is q as the learn step reads it: written to mocks.yaml and
// loaded back (the agent learns from the mocks it loads).
func reloaded(t *testing.T, q *models.PostgresV3QuerySpec) *models.PostgresV3QuerySpec {
	t.Helper()
	body, err := yaml.Marshal(q)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back models.PostgresV3QuerySpec
	if err := yaml.Unmarshal(body, &back); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	return &back
}

// pgQueryMock is an earlier Postgres recording of q, recorded at at, as the
// recorder writes one since integrations#229: stating it kept the bound
// values as the bytes the client sent.
func pgQueryMock(name string, at time.Time, q *models.PostgresV3QuerySpec) *models.Mock {
	return &models.Mock{Name: name, Kind: models.PostgresV3, Spec: models.MockSpec{
		Metadata:         map[string]string{"bindEncoding": "wire"},
		ReqTimestampMock: at,
		ResTimestampMock: at.Add(time.Millisecond),
		PostgresV3:       &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: q},
	}}
}

// classifyBind is the kind a bound value learns as when it was recorded and
// changed to live in record's full round, at a call made at at with earlier
// places raw.
func classifyBind(t *testing.T, at time.Time, raw []Raw, recorded, live string) Learned {
	t.Helper()
	rec := Shape{Leaves: map[string]Leaf{"bind.$8": {Path: "bind.$8", Value: recorded}}}
	got := Shape{Leaves: map[string]Leaf{"bind.$8": {Path: "bind.$8", Value: live}}}
	out := Classify(NewDriftCache(), []string{"bind.$8"}, got, rec, at, nil, raw, nil)
	if len(out) != 1 {
		t.Fatalf("recorded %s, live %s: %+v", recorded, live, out)
	}
	return out[0]
}

// pgjdbc sends an int4, int8 or bool in binary, and the recorder keeps each
// bound value as the bytes the client sent (bindEncoding: wire): Flyway's
// history INSERT binds its installed_rank, checksum and execution_time so
// (formats 1,0,0,0,0,1,0,1,0). Read as its JSON, such a value is the base64
// of its bytes ("AAAAIQ==" for 33), which is no word the application had, and
// a later value the application copied from it has no visible source: the
// drift is learned as noise and goes unchecked at replay. Each is read as its
// type's text, a text-format one as the text it was sent as, and a cell the
// recorder kept as bytes as its column's type; no base64 or format code is.
func TestAPostgresRecordingsBinaryValuesAreReadAsTheirValues(t *testing.T) {
	at := time.Date(2026, 10, 24, 18, 34, 48, 617828595, time.UTC)
	q := &models.PostgresV3QuerySpec{
		SQLNormalized: `INSERT INTO "public"."t" ("id", "n", "name", "ok", "big") VALUES ($1, $2, $3, $4, $5)`,
		ParamOIDs:     []uint32{23, 23, 1043, 16, 20},
		BindValues: models.PostgresV3Cells{
			{Value: []byte{0, 0, 0, 0x21}},
			{Value: []byte("4711")},
			{Value: []byte("customer360")},
			{Value: []byte{1}},
			{Value: []byte{0, 0, 0, 0, 0x07, 0x5b, 0xcd, 0x15}},
		},
		BindFormats: []int{1, 0, 0, 1, 1},
		Response: &models.PostgresV3Response{
			RowDescription: []models.PostgresV3ColumnDescriptor{{Name: "id", TypeOID: 23, Format: 1}, {Name: "tag", TypeOID: 25}},
			Rows: []models.PostgresV3Cells{{
				{Value: models.PostgresV3CellRaw{Format: 1, Bytes: []byte{0, 0, 0x02, 0x9a}}},
				{Value: models.PostgresV3CellRaw{Format: 0, Bytes: []byte("blue")}},
			}},
			CommandComplete: "INSERT 0 7",
		},
	}
	before := *q
	r := RawOf(pgQueryMock("mock-62", at, q), false)
	if r.Unread != "" {
		t.Fatalf("a recording of known types is read whole, got unread %q", r.Unread)
	}
	words := map[string]bool{}
	for _, w := range strings.Fields(r.Text) {
		words[w] = true
	}
	for _, v := range []string{"33", "4711", "customer360", "t", "123456789", "666", "blue"} {
		if !words[v] {
			t.Errorf("the value %s is not read as a word of the recording: %q", v, r.Text)
		}
	}
	for _, enc := range []string{"AAAAIQ==", "NDcxMQ==", "AAAAAAdbzRU=", "AAACmg==", "$pgtype", "format", "bytes"} {
		if strings.Contains(r.Text, enc) {
			t.Errorf("the recording is read with %q, an encoding of its values or keploy's word for one: %q", enc, r.Text)
		}
	}
	if !reflect.DeepEqual(*q, before) {
		t.Fatalf("RawOf changed the recording: %+v, was %+v", *q, before)
	}

	// The false green: execution_time copied from the earlier binary bind 33
	// and changed in record's full round is a variable from it, not noise.
	raw := []Raw{RawOf(pgQueryMock("mock-62", at, q), false)}
	if got := classifyBind(t, at.Add(time.Second), raw, "33", "3"); got.Kind != KindVariable || got.From.Mock != "mock-62" {
		t.Errorf("a value an earlier binary bind holds: %s from %+v, want a variable from mock-62", got.Kind, got.From)
	}
	if got := classifyBind(t, at.Add(time.Second), raw, "666", "7"); got.Kind != KindVariable {
		t.Errorf("a value an earlier binary cell holds: %s, want a variable", got.Kind)
	}
	// A value only the wire's typing held (a format code 1) has no source
	// there, and one the command tag's row count holds does.
	if got := classifyBind(t, at.Add(time.Second), raw, "1", "2"); got.Kind != KindNoise {
		t.Errorf("a value only a format code holds: %s (%s), want noise", got.Kind, got.Why)
	}
	if got := classifyBind(t, at.Add(time.Second), raw, "7", "8"); got.Kind != KindVariable {
		t.Errorf("a value the command tag's row count holds: %s, want a variable", got.Kind)
	}
}

// A binary value of a type keploy cannot read (a type of the app's own, an
// extension's) may hold any word: a later drift that may have come from it
// is refused with that reason, never learned as noise. So is one sent in a
// format keploy cannot tell. A drift of a call made before the recording is
// not.
func TestADriftAfterAPostgresValueKeployCannotReadIsRefused(t *testing.T) {
	at := time.Date(2026, 10, 24, 18, 34, 48, 0, time.UTC)
	cases := map[string]*models.PostgresV3QuerySpec{
		"a bind of an unknown type": {
			SQLNormalized: "SELECT f($1)", ParamOIDs: []uint32{99999},
			BindValues: models.PostgresV3Cells{{Value: []byte{0, 0, 0, 3}}}, BindFormats: []int{1},
		},
		"a bind in binary with no type": {
			SQLNormalized: "SELECT f($1)",
			BindValues:    models.PostgresV3Cells{{Value: []byte{0, 0, 0, 3}}}, BindFormats: []int{1},
		},
		"a bind in a format keploy cannot tell": {
			SQLNormalized: "SELECT f($1, $2, $3)", ParamOIDs: []uint32{23, 23, 23},
			BindValues:  models.PostgresV3Cells{{Value: []byte("1")}, {Value: []byte("2")}, {Value: []byte("3")}},
			BindFormats: []int{0, 1},
		},
		"a cell of an unknown type": {
			SQLNormalized: "SELECT g()",
			Response: &models.PostgresV3Response{
				RowDescription: []models.PostgresV3ColumnDescriptor{{Name: "g", TypeOID: 99999}},
				Rows:           []models.PostgresV3Cells{{{Value: models.PostgresV3CellRaw{Format: 1, Bytes: []byte{0, 0, 0, 3}}}}},
			},
		},
	}
	for name, q := range cases {
		t.Run(name, func(t *testing.T) {
			m := pgQueryMock("mock-7", at, q)
			dc := NewDriftCache()
			raw := []Raw{dc.RawOf(m, false)}
			if raw[0].Unread == "" {
				t.Fatalf("the recording is read whole: %q", raw[0].Text)
			}
			if again := dc.RawOf(m, true); again.Unread != raw[0].Unread || !again.Served {
				t.Fatalf("the cache lost why the recording is unread, or how it was served: %+v", again)
			}
			got := classifyBind(t, at.Add(time.Second), raw, "33", "3")
			if got.Kind != KindUnaccounted || !strings.Contains(got.Why, "mock-7") || !strings.Contains(got.Why, raw[0].Unread) {
				t.Fatalf("a drift after it: %s (%s), want refused naming mock-7 and why", got.Kind, got.Why)
			}
			if got := classifyBind(t, at.Add(-time.Second), raw, "33", "3"); got.Kind != KindNoise {
				t.Fatalf("a drift of a call made before it: %s (%s), want noise", got.Kind, got.Why)
			}
		})
	}
	if why := RawOf(pgQueryMock("mock-7", at, cases["a bind of an unknown type"]), false).Unread; !strings.Contains(why, "type 99999") {
		t.Errorf("the reason does not name the type: %q", why)
	}
}

// A Postgres session recording (the connection's start-up) holds the
// protocol version (3.0), the server's answer to the SSL request and the
// authentication mode keploy observed: what the driver and keploy said to
// each other, no word the application was given. Read as payload, Flyway's
// execution_time of 3 ms in record's full round was refused for the "3" of
// "3.0". The server's version, its parameters and the backend's process id
// are what the server told the client, which the application can read: a
// value one holds still has a source there.
func TestAPostgresSessionsProtocolWordsAreNotPayloadButTheServersAre(t *testing.T) {
	at := time.Date(2026, 10, 24, 18, 34, 47, 0, time.UTC)
	m := &models.Mock{Name: "mock-0", Kind: models.PostgresV3, Spec: models.MockSpec{
		ReqTimestampMock: at,
		PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeSession, Session: &models.PostgresV3SessionSpec{
			ProtocolVersion:  "3.0",
			SSLResponse:      "N",
			ServerVersion:    "15.2 (Debian 15.2-1.pgdg110+1)",
			ParameterStatus:  map[string]string{"TimeZone": "Etc/UTC", "integer_datetimes": "on", "max_identifier_length": "63"},
			BackendProcessID: 81,
			BackendSecretKey: 4242,
			ObservedAuthMode: "md5",
		}},
	}}
	before := *m.Spec.PostgresV3.Session
	raw := []Raw{RawOf(m, false)}
	for _, protocol := range []string{"3", "0", "N", "md5"} {
		if got := classifyBind(t, at.Add(time.Second), raw, protocol, protocol+"9"); got.Kind != KindNoise {
			t.Errorf("a value %s only the session's protocol words hold: %s (%s), want noise", protocol, got.Kind, got.Why)
		}
	}
	for _, server := range []string{"15", "2", "Debian", "81", "4242", "63"} {
		if got := classifyBind(t, at.Add(time.Second), raw, server, server+"9"); got.Kind != KindVariable {
			t.Errorf("a value %s the server told the client: %s, want a variable", server, got.Kind)
		}
	}
	if !reflect.DeepEqual(*m.Spec.PostgresV3.Session, before) {
		t.Fatalf("RawOf changed the recording")
	}
}

// words is r's text as words.
func words(r Raw) map[string]bool {
	w := map[string]bool{}
	for _, f := range strings.Fields(r.Text) {
		w[f] = true
	}
	return w
}

// A bytea the application bound is its bytes: pgjdbc's setBytes, pgx and
// asyncpg send one in binary, and a text one is the hex Postgres reads. A
// bytea cell likewise. A token copied from one has a source there.
func TestAPostgresByteaIsReadAsItsBytes(t *testing.T) {
	at := time.Date(2026, 10, 24, 18, 0, 0, 0, time.UTC)
	cases := map[string]*models.PostgresV3QuerySpec{
		"a bind in binary": {
			SQLNormalized: "SELECT f($1)", ParamOIDs: []uint32{17},
			BindValues: models.PostgresV3Cells{{Value: []byte("tok_9f8e7d6c5b4a")}}, BindFormats: []int{1},
		},
		"a bind in text": {
			SQLNormalized: "SELECT f($1)", ParamOIDs: []uint32{17},
			BindValues: models.PostgresV3Cells{{Value: []byte(`\x746f6b5f396638653764366335623461`)}},
		},
		"a cell": {
			SQLNormalized: "SELECT g()",
			Response: &models.PostgresV3Response{
				RowDescription: []models.PostgresV3ColumnDescriptor{{Name: "g", TypeOID: 17}},
				Rows:           []models.PostgresV3Cells{{{Value: []byte("tok_9f8e7d6c5b4a")}}},
			},
		},
	}
	for name, q := range cases {
		t.Run(name, func(t *testing.T) {
			raw := []Raw{RawOf(pgQueryMock("mock-3", at, q), false)}
			if raw[0].Unread != "" || !words(raw[0])["tok_9f8e7d6c5b4a"] {
				t.Fatalf("the bytes are not read: unread %q, text %q", raw[0].Unread, raw[0].Text)
			}
			if got := classifyBind(t, at.Add(time.Second), raw, "tok_9f8e7d6c5b4a", "tok_1a2b3c4d5e6f"); got.Kind != KindVariable {
				t.Fatalf("a token copied from it: %s (%s), want a variable", got.Kind, got.Why)
			}
		})
	}
}

// A value keploy keeps in a form of its own (a numeric as its digits and
// exponent, an interval as its parts) is read as its type's text, in an
// answer, a table's data and its migration history: a numeric 12345e-2 is
// 123.45, the word an application copies, and none of keploy's words for
// the form is read. A bound value sent with no format codes is text.
func TestAPostgresValueKeployKeepsInItsOwnFormIsReadAsItsText(t *testing.T) {
	at := time.Date(2026, 10, 24, 18, 0, 0, 0, time.UTC)
	price := pgtype.Numeric{Int: big.NewInt(12345), Exp: -2, Valid: true}
	wait := pgtype.Interval{Microseconds: 90_000_000, Valid: true}
	specs := map[string]*models.PostgresV3Spec{
		"an answer": {Query: &models.PostgresV3QuerySpec{
			SQLNormalized: "SELECT price, wait FROM t WHERE sku = $1",
			BindValues:    models.PostgresV3Cells{{Value: []byte("4711")}},
			Response: &models.PostgresV3Response{
				RowDescription: []models.PostgresV3ColumnDescriptor{{Name: "price", TypeOID: 1700}, {Name: "wait", TypeOID: 1186}},
				Rows:           []models.PostgresV3Cells{{{Value: price}, {Value: wait}}},
			},
		}},
		"a table's data": {Data: &models.PostgresV3DataSpec{Table: "t", Columns: []string{"price", "wait"},
			Rows: []models.PostgresV3Cells{{{Value: price}, {Value: wait}}}}},
		"a migration history": {Catalog: &models.PostgresV3CatalogSpec{MigrationState: []models.PostgresV3MigrationTable{{
			Name: "flyway_schema_history", Columns: []string{"price", "wait"},
			Rows: []models.PostgresV3Cells{{{Value: price}, {Value: wait}}}}}}},
	}
	for name, pg := range specs {
		t.Run(name, func(t *testing.T) {
			m := &models.Mock{Name: "mock-4", Kind: models.PostgresV3, Spec: models.MockSpec{ReqTimestampMock: at, PostgresV3: pg}}
			raw := []Raw{RawOf(m, false)}
			w := words(raw[0])
			if raw[0].Unread != "" || !w["123.45"] || !w["00:01:30"] {
				t.Fatalf("the values are not read as their text: unread %q, text %q", raw[0].Unread, raw[0].Text)
			}
			for _, own := range []string{"$pgtype", "exp", "infinitymodifier", "microseconds", "12345"} {
				if w[own] {
					t.Errorf("keploy's form %q is read: %q", own, raw[0].Text)
				}
			}
			if got := classifyBind(t, at.Add(time.Second), raw, "123.45", "99.10"); got.Kind != KindVariable {
				t.Errorf("a price copied from it: %s (%s), want a variable", got.Kind, got.Why)
			}
			if name == "an answer" && (!w["4711"] || strings.Contains(raw[0].Text, "NDcxMQ==")) {
				t.Errorf("a bind sent with no format codes is not read as its text: %q", raw[0].Text)
			}
		})
	}
}

// A binary jsonb is its version byte and its text, and a json its text: read
// through Go's numbers, an id past what a float holds was a word nobody sent
// and the real id was missed. An empty text sent in binary is an empty
// value, read whole.
func TestAPostgresJSONAndEmptyBinaryValuesAreReadAsSent(t *testing.T) {
	at := time.Date(2026, 10, 24, 18, 0, 0, 0, time.UTC)
	q := &models.PostgresV3QuerySpec{
		SQLNormalized: "SELECT f($1, $2, $3)", ParamOIDs: []uint32{3802, 114, 25},
		BindValues: models.PostgresV3Cells{
			{Value: append([]byte{1}, `{"id": 12345678901234567891}`...)},
			{Value: []byte(`{"ref": "ord-5521"}`)},
			{Value: []byte{}},
		},
		BindFormats: []int{1},
	}
	raw := []Raw{RawOf(pgQueryMock("mock-5", at, q), false)}
	if raw[0].Unread != "" {
		t.Fatalf("unread %q", raw[0].Unread)
	}
	for _, v := range []string{"12345678901234567891", `"ord-5521"}`} {
		if !strings.Contains(raw[0].Text, v) {
			t.Errorf("%s is not read: %q", v, raw[0].Text)
		}
	}
	// A jsonb that is a number is read as that number, without its version
	// byte, which is the wire's typing; an empty bytea as nothing, not as
	// the x of its hex text's \x.
	more := &models.PostgresV3QuerySpec{SQLNormalized: "SELECT f($1, $2)", ParamOIDs: []uint32{3802, 17},
		BindValues: models.PostgresV3Cells{{Value: []byte{1, '3', '3'}}, {Value: []byte{}}}, BindFormats: []int{1}}
	r := RawOf(pgQueryMock("mock-5", at, more), false)
	if tok := tokensBy(r.Text, wordByte); r.Unread != "" || !tok["33"] || strings.ContainsRune(r.Text, 1) || tok["x"] {
		t.Errorf("unread %q, text %q", r.Unread, r.Text)
	}
	if got := classifyBind(t, at.Add(time.Second), raw, "12345678901234567891", "12345678901234567892"); got.Kind != KindVariable {
		t.Errorf("an id copied from the jsonb: %s (%s), want a variable", got.Kind, got.Why)
	}
}

// A binary value whose codec fails on its bytes (an int4[] whose header
// counts 5 elements and holds 1, as the recorder keeps a cell its own codec
// could not read) leaves the recording unread, with why, and does not stop
// the learn step.
func TestAPostgresValueWhoseBytesDoNotReadLeavesTheRecordingUnread(t *testing.T) {
	at := time.Date(2026, 10, 24, 18, 0, 0, 0, time.UTC)
	short := []byte{0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0x17, 0, 0, 0, 5, 0, 0, 0, 1, 0, 0, 0, 4, 0, 0, 0, 7}
	for name, q := range map[string]*models.PostgresV3QuerySpec{
		"a bind": {SQLNormalized: "SELECT f($1)", ParamOIDs: []uint32{1007}, BindValues: models.PostgresV3Cells{{Value: short}}, BindFormats: []int{1}},
		"a cell": {SQLNormalized: "SELECT g()", Response: &models.PostgresV3Response{
			RowDescription: []models.PostgresV3ColumnDescriptor{{Name: "g", TypeOID: 1007}},
			Rows:           []models.PostgresV3Cells{{{Value: models.PostgresV3CellRaw{Format: 1, Bytes: short}}}},
		}},
		"a bind in a format no int16 holds": {SQLNormalized: "SELECT f($1)", ParamOIDs: []uint32{23}, BindValues: models.PostgresV3Cells{{Value: []byte("3")}}, BindFormats: []int{70000}},
	} {
		t.Run(name, func(t *testing.T) {
			r := RawOf(pgQueryMock("mock-6", at, q), false)
			if r.Unread == "" {
				t.Fatalf("read whole: %q", r.Text)
			}
			if name != "a bind in a format no int16 holds" && !strings.Contains(r.Unread, "cannot read it as one") {
				t.Errorf("the reason does not say its bytes do not read: %q", r.Unread)
			}
		})
	}
	no := RawOf(pgQueryMock("mock-6", at, &models.PostgresV3QuerySpec{SQLNormalized: "SELECT f($1)",
		BindValues: models.PostgresV3Cells{{Value: []byte{0, 0, 0, 3}}}, BindFormats: []int{1}}), false)
	if !strings.Contains(no.Unread, "no type stated") {
		t.Errorf("a binary bind of no type: %q", no.Unread)
	}
	bad := pgQueryMock("mock-6", at, &models.PostgresV3QuerySpec{SQLNormalized: "SELECT 1",
		Response: &models.PostgresV3Response{Rows: []models.PostgresV3Cells{{{Value: make(chan int)}}}}})
	if r := RawOf(bad, false); !strings.Contains(r.Unread, "JSON") {
		t.Errorf("a recording keploy cannot write as JSON: %q", r.Unread)
	}
}

// The refusal for an unreadable earlier recording is only for a drift no
// readable place holds: a value found whole in another is a variable from
// there, asserted against it. It is for a recording that record's full
// round served before the call, even one recorded after the call, and for
// an echo, and
// never for an empty recorded value, which carries nothing to follow. A
// parked recording read back keeps why it is unread.
func TestTheRefusalForAnUnreadRecordingIsOnlyWhereNothingElseHoldsTheValue(t *testing.T) {
	at := time.Date(2026, 10, 24, 18, 0, 0, 0, time.UTC)
	good := pgQueryMock("mock-1", at, &models.PostgresV3QuerySpec{SQLNormalized: "SELECT f($1)", BindValues: models.PostgresV3Cells{{Value: []byte("order-77812")}}})
	bad := pgQueryMock("mock-2", at, &models.PostgresV3QuerySpec{SQLNormalized: "SELECT f($1)", ParamOIDs: []uint32{99999},
		BindValues: models.PostgresV3Cells{{Value: []byte{0, 0, 0, 3}}}, BindFormats: []int{1}})
	raw := []Raw{RawOf(good, false), RawOf(bad, false)}
	if got := classifyBind(t, at.Add(time.Second), raw, "order-77812", "order-90001"); got.Kind != KindVariable || got.From.Mock != "mock-1" {
		t.Errorf("a value a readable place holds whole: %s (%s), want a variable from mock-1", got.Kind, got.Why)
	}
	if got := classifyBind(t, at.Add(time.Second), raw, "", "x"); got.Kind == KindUnaccounted {
		t.Errorf("an empty recorded value: refused (%s)", got.Why)
	}
	later := RawOf(bad, true)
	later.At = at.Add(time.Hour)
	if got := classifyBind(t, at.Add(time.Second), []Raw{later}, "33", "3"); got.Kind != KindUnaccounted {
		t.Errorf("a drift after a served unreadable recording, recorded later: %s, want refused", got.Kind)
	}
	parked := pgQueryMock("mock-2", at, bad.Spec.PostgresV3.Query)
	parked.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) { return nil, nil, nil })
	dc := NewDriftCache()
	for i, served := range []bool{false, true} {
		r, err := dc.ReadRaw(parked, served)
		if err != nil || r.Unread == "" || r.Served != served {
			t.Fatalf("read %d of a parked recording: err %v, %+v", i, err, r)
		}
	}
}

// An answer's columns are read by their names only: a descriptor keploy
// keeps with its name alone no longer writes a type 0 the server never
// sent, and a 0 the application copied from nowhere has no source there.
func TestAPostgresAnswersColumnsAreReadByNameOnly(t *testing.T) {
	at := time.Date(2026, 10, 24, 18, 0, 0, 0, time.UTC)
	raw := []Raw{RawOf(pgQueryMock("mock-8", at, &models.PostgresV3QuerySpec{SQLNormalized: "SELECT id FROM t",
		Response: &models.PostgresV3Response{
			RowDescription:  []models.PostgresV3ColumnDescriptor{{Name: "id", TypeOID: 23, TypeSize: 4}},
			Rows:            []models.PostgresV3Cells{{{Value: int64(5)}}},
			CommandComplete: "SELECT 1",
		}}), false)}
	if w := words(raw[0]); w["0"] || w["typeOid"] || w["23"] || w["4"] || !w["id"] {
		t.Fatalf("the answer's columns are read with their typing: %q", raw[0].Text)
	}
	if got := classifyBind(t, at.Add(time.Second), raw, "0", "4"); got.Kind != KindNoise {
		t.Errorf("a 0 copied from nowhere: %s (%s), want noise", got.Kind, got.Why)
	}
}

// More of what a Postgres recording holds, each read as its value or, where
// keploy cannot read it, leaving the recording unread: an array of uuids
// (by its column's type, or, in a table's data, as uuids), a range's bounds,
// a NaN, a date by its column's type, a binary json, a text COPY's data, a
// fastpath call's text argument, and none of the COPY's or the call's
// formats.
func TestMorePostgresValuesAreReadAsTheirValues(t *testing.T) {
	at := time.Date(2026, 10, 24, 18, 0, 0, 0, time.UTC)
	id := [16]uint8{0x9f, 0x8e, 0x7d, 0x6c, 0x5b, 0x4a, 0x41, 0x2b, 0x8c, 0x3d, 0x1e, 0x0f, 0xa1, 0xb2, 0xc3, 0xd4}
	day := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	q := &models.PostgresV3QuerySpec{
		SQLNormalized: "SELECT f($1)", ParamOIDs: []uint32{114},
		BindValues: models.PostgresV3Cells{{Value: []byte(`{"id": 12345678901234567891}`)}}, BindFormats: []int{1},
		Response: &models.PostgresV3Response{
			RowDescription: []models.PostgresV3ColumnDescriptor{{Name: "ids", TypeOID: 2951}, {Name: "d", TypeOID: 1082}, {Name: "f", TypeOID: 701}},
			Rows:           []models.PostgresV3Cells{{{Value: []any{id}}, {Value: day}, {Value: math.NaN()}}},
			CopyIn:         &models.PostgresV3CopyInPayload{OverallFormat: 1, ColumnFormatCodes: []uint16{1, 1}},
			CopyOut:        &models.PostgresV3CopyOutPayload{ColumnFormatCodes: []uint16{0, 0}, Data: [][]byte{[]byte("4711\tord-5521\n")}},
			FunctionCall:   &models.PostgresV3FunctionCall{OID: 957, ArgFormats: []int16{0}, Args: []models.PostgresV3Cell{{Value: []byte("8812")}}},
		},
	}
	raw := []Raw{RawOf(pgQueryMock("mock-9", at, q), false)}
	w := words(raw[0])
	if raw[0].Unread != "" {
		t.Fatalf("unread %q", raw[0].Unread)
	}
	for _, v := range []string{"12345678901234567891", "9f8e7d6c-5b4a-412b-8c3d-1e0fa1b2c3d4", "NaN", "2026-10-24", "ord-5521", "8812", "957"} {
		if !strings.Contains(raw[0].Text, v) {
			t.Errorf("%s is not read: %q", v, raw[0].Text)
		}
	}
	// The fastpath call's format code 0 is no word, nor the COPY's.
	for _, own := range []string{"$pgtype", "1", "0", "argFormats", "columnFormatCodes"} {
		if w[own] {
			t.Errorf("%q is read: %q", own, raw[0].Text)
		}
	}
	// The date by its column's type, and as Go's JSON wrote it.
	if !w["2026-10-24"] || !w["2026-10-24T00:00:00Z"] {
		t.Errorf("the date is not read as its type and as RFC 3339 write it: %q", raw[0].Text)
	}
	if got := classifyBind(t, at.Add(time.Second), raw, "9f8e7d6c-5b4a-412b-8c3d-1e0fa1b2c3d4", "00000000-5b4a-412b-8c3d-1e0fa1b2c3d4"); got.Kind != KindVariable {
		t.Errorf("a uuid copied from the uuid[]: %s (%s), want a variable", got.Kind, got.Why)
	}
	data := &models.Mock{Name: "mock-10", Kind: models.PostgresV3, Spec: models.MockSpec{ReqTimestampMock: at, PostgresV3: &models.PostgresV3Spec{
		Data: &models.PostgresV3DataSpec{Table: "t", Columns: []string{"ids", "span", "mac"}, Rows: []models.PostgresV3Cells{{
			{Value: []any{id}},
			{Value: pgtype.Range[any]{Lower: int64(17), Upper: int64(29), LowerType: pgtype.Inclusive, UpperType: pgtype.Exclusive, Valid: true}},
			{Value: net.HardwareAddr{0x08, 0x00, 0x2b, 0x01, 0x02, 0x03}},
		}}},
	}}}
	d := RawOf(data, false)
	dw := words(d)
	if d.Unread != "" || !strings.Contains(d.Text, "9f8e7d6c-5b4a-412b-8c3d-1e0fa1b2c3d4") || !dw["17"] || !dw["29"] || dw["105"] || dw["101"] {
		t.Errorf("a table's uuid and range are not read as their values: unread %q, text %q", d.Unread, d.Text)
	}
	// A table's data states no column types: a mac address is read as one,
	// not as the base64 of its bytes.
	if !strings.Contains(d.Text, "08:00:2b:01:02:03") || strings.Contains(d.Text, "CAArAQID") {
		t.Errorf("a table's mac address is not read as its value: %q", d.Text)
	}
	echoRaw := []Raw{RawOf(pgQueryMock("mock-2", at, &models.PostgresV3QuerySpec{SQLNormalized: "SELECT f($1)", ParamOIDs: []uint32{99999},
		BindValues: models.PostgresV3Cells{{Value: []byte{0, 0, 0, 3}}}, BindFormats: []int{1}}), false)}
	rec := Shape{Leaves: map[string]Leaf{"bind.$8": {Path: "bind.$8", Value: "ord-7781", Type: String}}}
	live := Shape{Leaves: map[string]Leaf{"bind.$8": {Path: "bind.$8", Value: "ord-9001", Type: String}}}
	resp := []Leaf{{Path: "row.ref", Value: "ord-7781", Type: String}}
	if out := Classify(NewDriftCache(), []string{"bind.$8"}, live, rec, at.Add(time.Second), nil, nil, resp); out[0].Kind != KindEcho {
		t.Fatalf("the premise: an echo with no unread recording is %s (%s)", out[0].Kind, out[0].Why)
	}
	if out := Classify(NewDriftCache(), []string{"bind.$8"}, live, rec, at.Add(time.Second), nil, echoRaw, resp); out[0].Kind != KindUnaccounted {
		t.Errorf("an echo after an unread recording: %s, want refused", out[0].Kind)
	}
}

// What keploy cannot read leaves the recording unread, with why: a value
// the recorder kept only the start of (with how to record it whole), a
// jsonb of another version, a format code that is neither text nor binary,
// one no Bind can carry, and a binary COPY or fastpath argument, whose
// types no message states.
func TestMorePostgresValuesKeployCannotReadLeaveTheRecordingUnread(t *testing.T) {
	at := time.Date(2026, 10, 24, 18, 0, 0, 0, time.UTC)
	cut := []byte("keploy-truncated:300000:7b22")
	for name, c := range map[string]struct {
		q   *models.PostgresV3QuerySpec
		why string
	}{
		"a text value cut short": {&models.PostgresV3QuerySpec{SQLNormalized: "SELECT f($1)", ParamOIDs: []uint32{25}, BindValues: models.PostgresV3Cells{{Value: cut}}}, "KEPLOY_PG_V3_MAX_CAPTURED_VALUE_BYTES"},
		"a jsonb cut short":      {&models.PostgresV3QuerySpec{SQLNormalized: "SELECT f($1)", ParamOIDs: []uint32{3802}, BindValues: models.PostgresV3Cells{{Value: cut}}, BindFormats: []int{1}}, "300000 bytes"},
		"a jsonb of version 2":   {&models.PostgresV3QuerySpec{SQLNormalized: "SELECT f($1)", ParamOIDs: []uint32{3802}, BindValues: models.PostgresV3Cells{{Value: []byte("\x02{}")}}, BindFormats: []int{1}}, "version 1"},
		"format code 2":          {&models.PostgresV3QuerySpec{SQLNormalized: "SELECT f($1)", ParamOIDs: []uint32{23}, BindValues: models.PostgresV3Cells{{Value: []byte{0, 0, 0, 3}}}, BindFormats: []int{2}}, "neither text nor binary"},
		"format code 65536":      {&models.PostgresV3QuerySpec{SQLNormalized: "SELECT f($1)", ParamOIDs: []uint32{23}, BindValues: models.PostgresV3Cells{{Value: []byte("3")}}, BindFormats: []int{65536}}, "no code the Bind can carry"},
		"two values keploy cannot read: the first is named": {&models.PostgresV3QuerySpec{SQLNormalized: "SELECT f($1, $2)", ParamOIDs: []uint32{99999, 99998},
			BindValues: models.PostgresV3Cells{{Value: []byte{0, 0, 0, 3}}, {Value: []byte{0, 0, 0, 4}}}, BindFormats: []int{1}}, "Postgres type 99999,"},
		"a binary COPY": {&models.PostgresV3QuerySpec{SQLNormalized: "COPY t TO STDOUT", Response: &models.PostgresV3Response{
			CopyOut: &models.PostgresV3CopyOutPayload{OverallFormat: 1, Data: [][]byte{{0, 0, 0, 0x21}}}}}, "binary COPY"},
		"a binary fastpath argument": {&models.PostgresV3QuerySpec{Response: &models.PostgresV3Response{
			FunctionCall: &models.PostgresV3FunctionCall{OID: 957, ArgFormats: []int16{1}, Args: []models.PostgresV3Cell{{Value: []byte{0, 0, 0, 0x21}}}}}}, "no type stated"},
		"a binary fastpath result": {&models.PostgresV3QuerySpec{Response: &models.PostgresV3Response{
			FunctionCall: &models.PostgresV3FunctionCall{OID: 957, ResultFormat: 1, Result: models.PostgresV3Cell{Value: []byte{0, 0, 0, 0x21}}}}}, "its function call's result was sent in binary with no type stated"},
		"a fastpath call with 2 format codes for 3 arguments": {&models.PostgresV3QuerySpec{Response: &models.PostgresV3Response{
			FunctionCall: &models.PostgresV3FunctionCall{OID: 957, ArgFormats: []int16{0, 0}, Args: []models.PostgresV3Cell{{Value: []byte("1")}, {Value: []byte("2")}, {Value: []byte("3")}}}}}, "the call gives 2 format codes for its 3 values"},
		"a text cell cut short": {&models.PostgresV3QuerySpec{SQLNormalized: "SELECT g()", Response: &models.PostgresV3Response{
			RowDescription: []models.PostgresV3ColumnDescriptor{{Name: "g", TypeOID: 25}}, Rows: []models.PostgresV3Cells{{{Value: string(cut)}}}}}, "row 1, column \"g\" of its answer was 300000 bytes long"},
		"a bytea cell cut short": {&models.PostgresV3QuerySpec{SQLNormalized: "SELECT g()", Response: &models.PostgresV3Response{
			RowDescription: []models.PostgresV3ColumnDescriptor{{Name: "g", TypeOID: 17}}, Rows: []models.PostgresV3Cells{{{Value: cut}}}}}, "row 1, column \"g\" of its answer was 300000 bytes long"},
	} {
		t.Run(name, func(t *testing.T) {
			r := RawOf(pgQueryMock("mock-11", at, c.q), false)
			if !strings.Contains(r.Unread, c.why) {
				t.Fatalf("unread %q, want it to say %q (text %q)", r.Unread, c.why, r.Text)
			}
			// What keploy cannot read is not read as text either.
			if strings.ContainsRune(r.Text, 0) || strings.Contains(r.Text, "keploy-truncated") {
				t.Errorf("the unread value is read as text: %q", r.Text)
			}
		})
	}
}

// A bytea is read in each form an application has bytes in: Go's
// encoding/json and Jackson write a []byte or byte[] as its standard base64,
// hex.EncodeToString and Python's bytes.hex() as its hex, with no `\x`
// glued on. A token copied from a bytea cell, or from a bytea bound in
// binary or in text, in either form, has its source there; bytes that are
// no UTF-8 are no text of it.
func TestAPostgresByteaIsReadInTheFormsAnApplicationHasBytesIn(t *testing.T) {
	at := time.Date(2026, 10, 24, 18, 0, 0, 0, time.UTC)
	b := []byte{0xab, 0xcd, 0xef, 0x12, 0x34, 0x56, 0x78, 0x90, 0xfe, 0xdc, 0xba, 0x98, 0x76, 0x54, 0x32, 0x10}
	sources := map[string]*models.PostgresV3QuerySpec{
		"a cell": {SQLNormalized: "SELECT g()", Response: &models.PostgresV3Response{
			RowDescription: []models.PostgresV3ColumnDescriptor{{Name: "g", TypeOID: 17}},
			Rows:           []models.PostgresV3Cells{{{Value: b}}},
		}},
		"a bind in binary": {SQLNormalized: "SELECT f($1)", ParamOIDs: []uint32{17}, BindValues: models.PostgresV3Cells{{Value: b}}, BindFormats: []int{1}},
		"a bind in text": {SQLNormalized: "SELECT f($1)", ParamOIDs: []uint32{17},
			BindValues: models.PostgresV3Cells{{Value: []byte(`\xABCDEF1234567890FEDCBA9876543210`)}}},
	}
	copies := map[string][2]string{
		"base64": {"q83vEjRWeJD+3LqYdlQyEA==", "EjRWeJCrze/+3LqYdlQyEA=="},
		"hex":    {"abcdef1234567890fedcba9876543210", "1234567890abcdeffedcba9876543210"},
	}
	for name, q := range sources {
		for form, c := range copies {
			t.Run(name+", copied as "+form, func(t *testing.T) {
				raw := []Raw{RawOf(pgQueryMock("mock-3", at, q), false)}
				if raw[0].Unread != "" || strings.ContainsRune(raw[0].Text, '�') {
					t.Fatalf("unread %q, text %q", raw[0].Unread, raw[0].Text)
				}
				if got := classifyBind(t, at.Add(time.Second), raw, c[0], c[1]); got.Kind != KindVariable {
					t.Fatalf("a %s copy of it: %s (%s), want a variable (text %q)", form, got.Kind, got.Why, raw[0].Text)
				}
			})
		}
	}
}

// An array is read as its elements, each by the array's element type: a
// bytea[] as its bytes, not as the hex its Postgres text writes them in, in
// a cell and in a bound value sent in binary or in text.
func TestAPostgresArrayIsReadAsItsElementsByTheirType(t *testing.T) {
	at := time.Date(2026, 10, 24, 18, 0, 0, 0, time.UTC)
	tok := []byte("tok_9f8e7d6c5b4a")
	m := pgtype.NewMap()
	bin, err := m.Encode(pgtype.ByteaArrayOID, pgtype.BinaryFormatCode, [][]byte{tok}, nil)
	if err != nil {
		t.Fatal(err)
	}
	txt, err := m.Encode(pgtype.ByteaArrayOID, pgtype.TextFormatCode, [][]byte{tok}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, q := range map[string]*models.PostgresV3QuerySpec{
		"a cell": {SQLNormalized: "SELECT g()", Response: &models.PostgresV3Response{
			RowDescription: []models.PostgresV3ColumnDescriptor{{Name: "g", TypeOID: pgtype.ByteaArrayOID}},
			Rows:           []models.PostgresV3Cells{{{Value: []any{tok}}}},
		}},
		"a bind in binary": {SQLNormalized: "SELECT f($1)", ParamOIDs: []uint32{pgtype.ByteaArrayOID}, BindValues: models.PostgresV3Cells{{Value: bin}}, BindFormats: []int{1}},
		"a bind in text":   {SQLNormalized: "SELECT f($1)", ParamOIDs: []uint32{pgtype.ByteaArrayOID}, BindValues: models.PostgresV3Cells{{Value: txt}}},
	} {
		t.Run(name, func(t *testing.T) {
			raw := []Raw{RawOf(pgQueryMock("mock-3", at, q), false)}
			if raw[0].Unread != "" || !words(raw[0])["tok_9f8e7d6c5b4a"] && !strings.Contains(raw[0].Text, "tok_9f8e7d6c5b4a") {
				t.Fatalf("the elements are not read: unread %q, text %q", raw[0].Unread, raw[0].Text)
			}
			if got := classifyBind(t, at.Add(time.Second), raw, "tok_9f8e7d6c5b4a", "tok_1a2b3c4d5e6f"); got.Kind != KindVariable {
				t.Fatalf("a token copied from it: %s (%s), want a variable", got.Kind, got.Why)
			}
		})
	}
}

// A recording made before the recorder kept the bytes sent (no
// bindEncoding: wire) kept a jsonb, json, xml or bytea bound value decoded:
// asyncpg's binary jsonb {"ref":"ord-5521"} is that JSON text, read as it,
// not refused as no jsonb of version 1. One that states it kept the bytes
// sent is read as those bytes.
func TestAPostgresRecordingFromBeforeTheBytesSentKeepsItsDecodedBinds(t *testing.T) {
	at := time.Date(2026, 10, 24, 18, 0, 0, 0, time.UTC)
	q := &models.PostgresV3QuerySpec{SQLNormalized: "SELECT f($1)", ParamOIDs: []uint32{pgtype.JSONBOID},
		BindValues: models.PostgresV3Cells{{Value: []byte(`{"ref":"ord-5521"}`)}}, BindFormats: []int{1}}
	old := pgQueryMock("mock-5", at, q)
	old.Spec.Metadata = nil
	raw := []Raw{RawOf(old, false)}
	if raw[0].Unread != "" || !strings.Contains(raw[0].Text, "ord-5521") {
		t.Fatalf("the decoded jsonb is not read: unread %q, text %q", raw[0].Unread, raw[0].Text)
	}
	if got := classifyBind(t, at.Add(time.Second), raw, "ord-5521", "ord-9001"); got.Kind != KindVariable {
		t.Errorf("a ref copied from it: %s (%s), want a variable", got.Kind, got.Why)
	}
	if r := RawOf(pgQueryMock("mock-5", at, q), false); !strings.Contains(r.Unread, "version 1") {
		t.Errorf("stated as the bytes sent, a jsonb not of version 1: unread %q", r.Unread)
	}
	// Unstated, a binary jsonb that is neither of version 1 nor a JSON
	// text is no value keploy can read.
	odd := pgQueryMock("mock-5", at, &models.PostgresV3QuerySpec{SQLNormalized: "SELECT f($1)", ParamOIDs: []uint32{pgtype.JSONBOID},
		BindValues: models.PostgresV3Cells{{Value: []byte("\x02{}")}}, BindFormats: []int{1}})
	odd.Spec.Metadata = nil
	if r := RawOf(odd, false); !strings.Contains(r.Unread, "version 1") {
		t.Errorf("unstated, a jsonb of version 2: unread %q", r.Unread)
	}
	// Unstated, an xml bound in binary was kept as its text, and a bytea
	// the recorder kept decoded and only the start of cannot be read.
	xml := pgQueryMock("mock-5", at, &models.PostgresV3QuerySpec{SQLNormalized: "SELECT f($1)", ParamOIDs: []uint32{142},
		BindValues: models.PostgresV3Cells{{Value: []byte("<ref>ord-5521</ref>")}}, BindFormats: []int{1}})
	xml.Spec.Metadata = nil
	if r := RawOf(xml, false); r.Unread != "" || !strings.Contains(r.Text, "ord-5521") {
		t.Errorf("unstated, a decoded xml in a binary Bind is not read as its text: unread %q, text %q", r.Unread, r.Text)
	}
	cut := pgQueryMock("mock-5", at, &models.PostgresV3QuerySpec{SQLNormalized: "SELECT f($1)", ParamOIDs: []uint32{pgtype.ByteaOID},
		BindValues: models.PostgresV3Cells{{Value: []byte("keploy-truncated:300000:7b22")}}})
	cut.Spec.Metadata = nil
	if r := RawOf(cut, false); !strings.Contains(r.Unread, "300000 bytes") {
		t.Errorf("unstated, a decoded bytea cut short: unread %q", r.Unread)
	}
	odd2 := pgQueryMock("mock-5", at, &models.PostgresV3QuerySpec{SQLNormalized: "SELECT f($1, $2, $3)", ParamOIDs: []uint32{17, 17, 17},
		BindValues: models.PostgresV3Cells{{Value: []byte("a")}, {Value: []byte("b")}, {Value: []byte("c")}}, BindFormats: []int{0, 1}})
	odd2.Spec.Metadata = nil
	if r := RawOf(odd2, false); !strings.Contains(r.Unread, "the Bind gives 2 format codes for its 3 values") {
		t.Errorf("unstated, 2 format codes for 3 values: unread %q", r.Unread)
	}
	bytea := pgQueryMock("mock-6", at, &models.PostgresV3QuerySpec{SQLNormalized: "SELECT f($1)", ParamOIDs: []uint32{pgtype.ByteaOID},
		BindValues: models.PostgresV3Cells{{Value: []byte{0xab, 0xcd, 0xef, 0x12}}}})
	bytea.Spec.Metadata = nil
	if r := RawOf(bytea, false); r.Unread != "" || !words(r)["abcdef12"] {
		t.Errorf("a decoded bytea in a text Bind is not read as its bytes: unread %q, text %q", r.Unread, r.Text)
	}
}

// A range is read as its bounds, each by the range's subtype, in a cell, in
// a multirange, in an array of any depth and in a value sent in binary: a
// tsrange's bound as the timestamp Postgres writes, 2026-10-24 10:30:45,
// whose 45 is a word of its own, never as the timestamptz 10:30:45Z it
// would be written as with no type stated.
func TestAPostgresRangeIsReadAsItsBoundsByItsSubtype(t *testing.T) {
	at := time.Date(2026, 10, 24, 18, 0, 0, 0, time.UTC)
	from := time.Date(2026, 10, 24, 10, 30, 45, 0, time.UTC)
	span := pgtype.Range[any]{Lower: from, Upper: from.Add(time.Hour), LowerType: pgtype.Inclusive, UpperType: pgtype.Exclusive, Valid: true}
	bin, err := pgtype.NewMap().Encode(pgtype.TsrangeOID, pgtype.BinaryFormatCode, pgtype.Range[pgtype.Timestamp]{
		Lower: pgtype.Timestamp{Time: from, Valid: true}, Upper: pgtype.Timestamp{Time: from.Add(time.Hour), Valid: true},
		LowerType: pgtype.Inclusive, UpperType: pgtype.Exclusive, Valid: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cell := func(oid uint32, v any) *models.PostgresV3QuerySpec {
		return &models.PostgresV3QuerySpec{SQLNormalized: "SELECT g()", Response: &models.PostgresV3Response{
			RowDescription: []models.PostgresV3ColumnDescriptor{{Name: "spans", TypeOID: oid}},
			Rows:           []models.PostgresV3Cells{{{Value: v}}},
		}}
	}
	for name, q := range map[string]*models.PostgresV3QuerySpec{
		"a tsrange":                   cell(pgtype.TsrangeOID, span),
		"a tsmultirange":              cell(pgtype.TsmultirangeOID, pgtype.Multirange[pgtype.Range[any]]{span}),
		"a tsrange[]":                 cell(pgtype.TsrangeArrayOID, []any{span}),
		"a two-dimensional tsrange[]": cell(pgtype.TsrangeArrayOID, []any{[]any{span}}),
		"a tsrange bound in binary": {SQLNormalized: "SELECT f($1)", ParamOIDs: []uint32{pgtype.TsrangeOID},
			BindValues: models.PostgresV3Cells{{Value: bin}}, BindFormats: []int{1}},
	} {
		for from, q := range map[string]*models.PostgresV3QuerySpec{"in memory": q, "from mocks.yaml": reloaded(t, q)} {
			t.Run(name+" "+from, func(t *testing.T) {
				r := RawOf(pgQueryMock("mock-12", at, q), false)
				if tok := tokensBy(r.Text, wordByte); r.Unread != "" || !tok["45"] || !strings.Contains(r.Text, "2026-10-24 11:30:45") {
					t.Errorf("unread %q, text %q", r.Unread, r.Text)
				}
				if w := words(r); w["105"] || w["101"] || strings.Contains(strings.ToLower(r.Text), "lowertype") {
					t.Errorf("keploy's form is read: %q", r.Text)
				}
			})
		}
	}
}

// A multirange is read as its ranges' bounds, never as keploy's words for
// their kinds (105 for inclusive, 101 for exclusive); a float4 as Postgres
// writes it, 0.1, not as its widening to a float8.
func TestAPostgresMultirangeAndFloat4AreReadAsTheirValues(t *testing.T) {
	at := time.Date(2026, 10, 24, 18, 0, 0, 0, time.UTC)
	span := pgtype.Multirange[pgtype.Range[any]]{{Lower: int64(3), Upper: int64(9), LowerType: pgtype.Inclusive, UpperType: pgtype.Exclusive, Valid: true}}
	q := &models.PostgresV3QuerySpec{SQLNormalized: "SELECT g()", Response: &models.PostgresV3Response{
		RowDescription: []models.PostgresV3ColumnDescriptor{{Name: "span", TypeOID: pgtype.Int4multirangeOID}, {Name: "ratio", TypeOID: pgtype.Float4OID},
			{Name: "count", TypeOID: pgtype.Float4OID}, {Name: "top", TypeOID: pgtype.Float4OID}},
		Rows: []models.PostgresV3Cells{{{Value: span}, {Value: float32(0.1)}, {Value: float32(1234567)}, {Value: float32(math.Inf(1))}}},
	}}
	if r := RawOf(pgQueryMock("mock-7", at, reloaded(t, q)), false); r.Unread != "" || !words(r)["3"] || !words(r)["9"] ||
		words(r)["105"] || strings.Contains(strings.ToLower(r.Text), "lowertype") {
		t.Errorf("the multirange from mocks.yaml: unread %q, text %q", r.Unread, r.Text)
	}
	raw := []Raw{RawOf(pgQueryMock("mock-7", at, q), false)}
	w := words(raw[0])
	if raw[0].Unread != "" || !w["3"] || !w["9"] || strings.Contains(raw[0].Text, "0.10000000149011612") || !strings.Contains(raw[0].Text, "0.1") {
		t.Fatalf("unread %q, text %q", raw[0].Unread, raw[0].Text)
	}
	// A float4 of 1234567 as Postgres writes it and as a driver gives it;
	// an infinite one as Postgres writes it.
	if tok := tokensBy(raw[0].Text, wordByte); !tok["1234567"] || !strings.Contains(raw[0].Text, "1.234567e+06") || !tok["Infinity"] || strings.Contains(raw[0].Text, "+Inf") {
		t.Errorf("a float4 is not read as Postgres and a driver write it: %q", raw[0].Text)
	}
	if w["105"] || w["101"] || w["$pgtype"] || strings.Contains(strings.ToLower(raw[0].Text), "lowertype") {
		t.Errorf("keploy's form is read: %q", raw[0].Text)
	}
	if got := classifyBind(t, at.Add(time.Second), raw, "0.1", "0.7"); got.Kind != KindVariable {
		t.Errorf("a ratio copied from the float4: %s (%s), want a variable", got.Kind, got.Why)
	}
}

// A text array is read as it was sent beside its elements: pgjdbc's text
// timestamptz[] {"2026-10-24 10:30:45+00"} holds 45 as a word of its own,
// which the element pgtype writes back, 10:30:45Z, does not.
func TestAPostgresTextArrayIsReadAsSentAndAsItsElements(t *testing.T) {
	at := time.Date(2026, 10, 24, 18, 0, 0, 0, time.UTC)
	q := &models.PostgresV3QuerySpec{SQLNormalized: "SELECT f($1)", ParamOIDs: []uint32{pgtype.TimestamptzArrayOID},
		BindValues: models.PostgresV3Cells{{Value: []byte(`{"2026-10-24 10:30:45+00"}`)}}}
	r := RawOf(pgQueryMock("mock-13", at, q), false)
	if tok := tokensBy(r.Text, wordByte); r.Unread != "" || !tok["45"] || !tok["45Z"] {
		t.Errorf("unread %q, text %q", r.Unread, r.Text)
	}
}

// A numeric[], interval[] or time[] column's elements are read as their
// values from mocks.yaml as in memory (123.45, 00:01:30, 10:30:45), never as
// keploy's words for their fields (int 12345, exp -2, microseconds): a price
// copied from the numeric has its source.
func TestAPostgresArrayOfNumericsIntervalsAndTimesIsReadAsItsValues(t *testing.T) {
	at := time.Date(2026, 10, 24, 18, 0, 0, 0, time.UTC)
	q := &models.PostgresV3QuerySpec{SQLNormalized: "SELECT g()", Response: &models.PostgresV3Response{
		RowDescription: []models.PostgresV3ColumnDescriptor{{Name: "prices", TypeOID: pgtype.NumericArrayOID},
			{Name: "waits", TypeOID: pgtype.IntervalArrayOID}, {Name: "opens", TypeOID: pgtype.TimeArrayOID}},
		Rows: []models.PostgresV3Cells{{{Value: []any{pgtype.Numeric{Int: big.NewInt(12345), Exp: -2, Valid: true}}},
			{Value: []any{pgtype.Interval{Microseconds: 90e6, Valid: true}}}, {Value: []any{[]any{pgtype.Time{Microseconds: 37845e6, Valid: true}}}}}},
	}}
	for from, q := range map[string]*models.PostgresV3QuerySpec{"in memory": q, "from mocks.yaml": reloaded(t, q)} {
		t.Run(from, func(t *testing.T) {
			raw := []Raw{RawOf(pgQueryMock("mock-14", at, q), false)}
			if raw[0].Unread != "" || !strings.Contains(raw[0].Text, "123.45") || !strings.Contains(raw[0].Text, "00:01:30") || !strings.Contains(raw[0].Text, "10:30:45") {
				t.Fatalf("unread %q, text %q", raw[0].Unread, raw[0].Text)
			}
			if w := words(raw[0]); w["12345"] || strings.Contains(raw[0].Text, "microseconds") || strings.Contains(raw[0].Text, "infinitymodifier") {
				t.Errorf("keploy's form is read: %q", raw[0].Text)
			}
			if got := classifyBind(t, at.Add(time.Second), raw, "123.45", "99.10"); got.Kind != KindVariable {
				t.Errorf("a price copied from the numeric: %s (%s), want a variable", got.Kind, got.Why)
			}
		})
	}
}
