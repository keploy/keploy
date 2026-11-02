package learn

import (
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// sibRolesInsert is a recording of node-jwt's start-up INSERT of a role, on
// connection 2: Sequelize binds the stamp it read from JavaScript's Date (to
// the millisecond) at createdAt and updatedAt, and RETURNING answers both.
func sibRolesInsert(name, id, role, stamp string, at time.Time) *models.Mock {
	return &models.Mock{Name: name, Kind: models.PostgresV3, Spec: models.MockSpec{
		ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
		Metadata: map[string]string{"connID": "2"},
		PostgresV3: &models.PostgresV3Spec{Type: models.PostgresV3TypeQuery, Query: &models.PostgresV3QuerySpec{
			SQLNormalized: `INSERT INTO "roles" ("id","name","createdAt","updatedAt") VALUES ($1,$2,$3,$4) RETURNING "id","name","createdAt","updatedAt"`,
			BindValues:    models.PostgresV3Cells{{Value: id}, {Value: role}, {Value: stamp}, {Value: stamp}},
			Response:      &models.PostgresV3Response{Rows: []models.PostgresV3Cells{{{Value: id}, {Value: role}, {Value: stamp}, {Value: stamp}}}, CommandComplete: "INSERT 0 1"},
		}},
	}}
}

// sibMySQLInsert is a recording of a MySQL prepared INSERT of a role whose
// third parameter is the stamp the application read from its clock, as an
// instant naming its zone; the server answers OK.
func sibMySQLInsert(name, id, role, stamp string, at time.Time) *models.Mock {
	return &models.Mock{Name: name, Kind: models.MySQL, Spec: models.MockSpec{
		ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
		Metadata: map[string]string{"connID": "2"},
		MySQLRequests: []mysql.Request{{PacketBundle: mysql.PacketBundle{
			Header: &mysql.PacketInfo{Type: "COM_STMT_EXECUTE"},
			Message: &mysql.StmtExecutePacket{StatementID: 1, ParameterCount: 3, Parameters: []mysql.Parameter{
				{Type: 3, Value: id}, {Type: 253, Value: role}, {Type: 253, Value: stamp},
			}},
		}}},
		MySQLResponses: []mysql.Response{{PacketBundle: mysql.PacketBundle{Header: &mysql.PacketInfo{Type: "OK"}, Message: &mysql.OKPacket{AffectedRows: 1}}}},
	}}
}

// sibCall classifies the drifted leaves of a later call, each recorded rec
// and live in record's full round, at the call's recorded instant at,
// against the recordings raw before it, with the call's recorded answer
// resp, as a statement's learn step does (ClassifyGiven, no window).
func sibCall(at time.Time, rec, live map[string]string, raw []Raw, resp ...Leaf) map[string]Learned {
	r, l := Shape{Leaves: map[string]Leaf{}}, Shape{Leaves: map[string]Leaf{}}
	var drift []string
	for p, v := range rec {
		r.Leaves[p] = Leaf{Path: p, Type: String, Value: v}
		l.Leaves[p] = Leaf{Path: p, Type: String, Value: live[p]}
		drift = append(drift, p)
	}
	slices.Sort(drift)
	out := map[string]Learned{}
	for _, ln := range ClassifyGiven(clockedDrifts(), drift, l, r, at, nil, raw, resp, nil, nil) {
		out[ln.Recorded.Path] = ln
	}
	return out
}

// Sequelize's stamp layout, its zone after a space, and an ISO instant to
// the millisecond.
const (
	sequelize = "2006-01-02 15:04:05.000 -07:00"
	isoMilli  = "2006-01-02T15:04:05.000Z07:00"
)

// sibLive is the stamp record's full round wrote where the recording wrote
// stamp (in layout): read FullRoundLead and 3 ms later, as the lead shows a
// read of the application's clock.
func sibLive(stamp, layout string) string {
	t, err := time.Parse(layout, stamp)
	if err != nil {
		panic(err)
	}
	return t.Add(FullRoundLead + 3*time.Millisecond).Format(layout)
}

func echoPaths(ln Learned) []string {
	var out []string
	for _, a := range ln.Echo {
		out = append(out, a.Path)
	}
	return out
}

// A field keeps one learned role across its sibling places (sibling.go):
// node-jwt's three role INSERTs, sent one after another on one connection,
// carry the stamp Sequelize read from JavaScript's Date, which resolves to
// the millisecond. Where the first was learned as an echo (RETURNING
// answers the bound stamp) and the second was stamped in the same
// millisecond, the second's stamp is learned as an echo too, as it is when
// stamped in the next millisecond: one verdict either way, whichever
// millisecond the stamps fell in. Without the sibling's role stated, the
// equal stamp is followed as a value carried from the first INSERT, as
// before (fail closed).
func TestAnEqualStampAtASiblingsEchoedBindIsLearnedAsAnEcho(t *testing.T) {
	first := time.Date(2026, 11, 2, 3, 52, 27, 62_000_000, time.UTC)
	at := first.Add(5 * time.Millisecond)
	const same, next = "2026-11-02 03:52:27.056 +00:00", "2026-11-02 03:52:27.057 +00:00"
	sib := RawOf(sibRolesInsert("mock-16", "1", "user", same, first), true).ServedAs(1)
	echoed := sib.Sibling(map[string]SiblingRole{"bind.$3": {Kind: KindEcho, Value: same}, "bind.$4": {Kind: KindEcho, Value: same}})
	resp := func(stamp string) []Leaf {
		return []Leaf{{Path: "row.1.createdAt", Type: String, Value: stamp}, {Path: "row.1.updatedAt", Type: String, Value: stamp}}
	}
	call := func(stamp string, raw Raw) map[string]Learned {
		live := sibLive(stamp, sequelize)
		return sibCall(at, map[string]string{"bind.$3": stamp, "bind.$4": stamp}, map[string]string{"bind.$3": live, "bind.$4": live}, []Raw{raw}, resp(stamp)...)
	}
	want := []string{"row.1.createdAt", "row.1.updatedAt"}
	for _, row := range []struct {
		what  string
		stamp string
		raw   Raw
	}{
		{"the same millisecond, the sibling's echo stated", same, echoed},
		{"the next millisecond, the sibling's echo stated", next, echoed},
		{"the next millisecond, no role stated", next, sib},
	} {
		for p, ln := range call(row.stamp, row.raw) {
			if ln.Kind != KindEcho || !slices.Equal(echoPaths(ln), want) {
				t.Errorf("%s: %s learned %s %v from %q (%s); want an echo answered at %v", row.what, p, ln.Kind, echoPaths(ln), ln.From.Mock, ln.Why, want)
			}
		}
	}
	for p, ln := range call(same, sib) {
		if ln.Kind != KindVariable || ln.From.Mock != "mock-16" {
			t.Errorf("the same millisecond, no role stated: %s learned %s from %q; want carried from mock-16, as before", p, ln.Kind, ln.From.Mock)
		}
	}
}

// The rule is the shared learn step's, whatever the protocol: a MySQL
// prepared INSERT whose earlier sibling's stamp parameter was learned as a
// plain read of the application's clock learns an equal stamp at the same
// parameter as a plain read too, checked in its own call's span at every
// replay, as it does a stamp a millisecond later. Without the role stated
// the equal stamp is carried from the sibling, as before.
func TestAnEqualStampAtASiblingsClockReadParameterIsAClockRead(t *testing.T) {
	first := time.Date(2026, 11, 2, 3, 52, 27, 62_000_000, time.UTC)
	at := first.Add(5 * time.Millisecond)
	const same, next = "2026-11-02T03:52:27.056Z", "2026-11-02T03:52:27.057Z"
	sib := RawOf(sibMySQLInsert("mock-7", "1", "user", same, first), true).ServedAs(1)
	read := sib.Sibling(map[string]SiblingRole{"bind.$3": {Kind: KindClockRead, Value: same}})
	call := func(stamp string, raw Raw) Learned {
		return sibCall(at, map[string]string{"bind.$3": stamp}, map[string]string{"bind.$3": sibLive(stamp, isoMilli)}, []Raw{raw})["bind.$3"]
	}
	for _, row := range []struct {
		what  string
		stamp string
		raw   Raw
	}{
		{"the same millisecond, the sibling's read stated", same, read},
		{"the next millisecond, the sibling's read stated", next, read},
		{"the next millisecond, no role stated", next, sib},
	} {
		if ln := call(row.stamp, row.raw); !plainRead(ln) {
			t.Errorf("%s: learned %s from %q (%s); want a plain read of the clock", row.what, ln.Kind, ln.From.Mock, ln.Why)
		}
	}
	if ln := call(same, sib); ln.Kind != KindVariable || ln.From.Mock != "mock-7" {
		t.Errorf("the same millisecond, no role stated: learned %s from %q; want carried from mock-7, as before", ln.Kind, ln.From.Mock)
	}
}

// The rule passes a sibling's role on only where the sibling learned the
// place as noise, an echo or a plain read of the clock, holding the call's
// value there, and the call's value read with such siblings set aside learns
// that role. Everywhere else the value stays carried from the sibling, as
// before: a sibling's place learned as a carried value or refused, a
// sibling that holds the value at another place than its role's, siblings
// that learned the place two ways, a reading that learns another kind, and
// one that finds another recording holding the value.
func TestASiblingsRoleIsPassedOnOnlyWhereItsRuleHolds(t *testing.T) {
	first := time.Date(2026, 11, 2, 3, 52, 27, 62_000_000, time.UTC)
	at := first.Add(5 * time.Millisecond)
	const token, live = "a7f3c2e9", "9c21e04b"
	const stamp, other = "2026-11-02 03:52:27.056 +00:00", "2026-11-02 03:52:27.012 +00:00"
	insert := func(name, held string) Raw {
		return RawOf(sibRolesInsert(name, "1", held, stamp, first), true).ServedAs(1)
	}
	role := func(k Kind, v string) map[string]SiblingRole {
		return map[string]SiblingRole{"bind.$2": {Kind: k, Value: v}}
	}
	nonce := func(raw []Raw, resp ...Leaf) Learned {
		return sibCall(at, map[string]string{"bind.$2": token}, map[string]string{"bind.$2": live}, raw, resp...)["bind.$2"]
	}
	if ln := nonce([]Raw{insert("mock-16", token).Sibling(role(KindNoise, token))}); ln.Kind != KindNoise {
		t.Errorf("a sibling's place learned as noise, holding the value: learned %s from %q (%s); want noise", ln.Kind, ln.From.Mock, ln.Why)
	}
	echoed := Leaf{Path: "row.1.name", Type: String, Value: token}
	for _, row := range []struct {
		what string
		raw  []Raw
		resp []Leaf
	}{
		{"a sibling's place learned as a carried value", []Raw{insert("mock-16", token).Sibling(role(KindVariable, token))}, nil},
		{"a sibling's place refused", []Raw{insert("mock-16", token).Sibling(role(KindUnaccounted, token))}, nil},
		{"a sibling's place learned as noise of another value", []Raw{insert("mock-16", token).Sibling(role(KindNoise, other))}, nil},
		{"a sibling's role stated at another place only", []Raw{insert("mock-16", token).Sibling(map[string]SiblingRole{"bind.$3": {Kind: KindNoise, Value: token}})}, nil},
		{"siblings that learned the place two ways", []Raw{insert("mock-16", token).Sibling(role(KindNoise, token)), insert("mock-17", token).Sibling(role(KindEcho, token))}, nil},
		{"a reading that learns another kind than the sibling's", []Raw{insert("mock-16", token).Sibling(role(KindNoise, token))}, []Leaf{echoed}},
		{"another recording that holds the value", []Raw{insert("mock-16", token).Sibling(role(KindNoise, token)), insert("mock-9", token)}, nil},
		{"a sibling's place learned as a carried value, another recording holding the value", []Raw{insert("mock-16", token).Sibling(role(KindVariable, token)), insert("mock-9", token)}, nil},
		{"siblings that learned the place two ways, the noise last", []Raw{insert("mock-16", token).Sibling(role(KindEcho, token)), insert("mock-17", token).Sibling(role(KindNoise, token))}, nil},
	} {
		if ln := nonce(row.raw, row.resp...); ln.Kind != KindVariable || ln.From.Mock != "mock-16" {
			t.Errorf("%s: learned %s from %q (%s); want carried from mock-16, as before", row.what, ln.Kind, ln.From.Mock, ln.Why)
		}
	}
}
