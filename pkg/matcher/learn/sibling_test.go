package learn

import (
	"slices"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/provenswitch"
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
	return sibCallBy(ClassifyGiven, at, rec, live, raw, resp...)
}

// sibCallBy is sibCall by classify: ClassifyGiven, or ClassifyFollowing.
func sibCallBy(classify func(*DriftCache, []string, Shape, Shape, time.Time, []Earlier, []Raw, []Leaf, *Window, GivenEarlier) []Learned, at time.Time, rec, live map[string]string, raw []Raw, resp ...Leaf) map[string]Learned {
	r, l := Shape{Leaves: map[string]Leaf{}}, Shape{Leaves: map[string]Leaf{}}
	var drift []string
	for p, v := range rec {
		r.Leaves[p] = Leaf{Path: p, Type: String, Value: v}
		l.Leaves[p] = Leaf{Path: p, Type: String, Value: live[p]}
		drift = append(drift, p)
	}
	slices.Sort(drift)
	out := map[string]Learned{}
	for _, ln := range classify(clockedDrifts(), drift, l, r, at, nil, raw, resp, nil, nil) {
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
		{"a sibling's place refused, another recording holding the value F saw", []Raw{insert("mock-16", token).Sibling(role(KindUnaccounted, token)), insert("mock-9", live)}, nil},
		{"siblings that learned the place two ways, the noise last", []Raw{insert("mock-16", token).Sibling(role(KindEcho, token)), insert("mock-17", token).Sibling(role(KindNoise, token))}, nil},
	} {
		if ln := nonce(row.raw, row.resp...); ln.Kind != KindVariable || ln.From.Mock != "mock-16" {
			t.Errorf("%s: learned %s from %q (%s); want carried from mock-16, as before", row.what, ln.Kind, ln.From.Mock, ln.Why)
		}
	}
}

// The value is read again with the siblings set aside, and the moments
// they hold go with them: a sibling whose other places hold the moment F
// saw in the call's stamp is no earlier place of that reading's echo, and
// the stamp is learned in the sibling's role. A stamp found carried from
// another recording (by the moment it was recorded with, written there in
// another layout) stays carried from there, whatever a sibling learned.
func TestASiblingSetAsideTakesItsMomentsWithIt(t *testing.T) {
	first := time.Date(2026, 11, 2, 3, 52, 27, 62_000_000, time.UTC)
	at := first.Add(5 * time.Millisecond)
	const stamp, iso = "2026-11-02 03:52:27.056 +00:00", "2026-11-02T03:52:27.056Z"
	live := sibLive(stamp, sequelize)
	// The sibling bound the stamp at $2, which RETURNING answers, and
	// the moment F saw at $3 and $4.
	sibling := RawOf(sibRolesInsert("mock-16", "1", stamp, live, first), true).ServedAs(1).
		Sibling(map[string]SiblingRole{"bind.$2": {Kind: KindEcho, Value: stamp}})
	other := RawOf(sibRolesInsert("mock-9", "1", iso, iso, first.Add(-time.Millisecond)), true).ServedAs(1)
	answer := Leaf{Path: "row.0.1", Type: String, Value: stamp}
	call := func(raw ...Raw) Learned {
		return sibCall(at, map[string]string{"bind.$2": stamp}, map[string]string{"bind.$2": live}, raw, answer)["bind.$2"]
	}
	if ln := call(sibling); ln.Kind != KindEcho || !slices.Equal(echoPaths(ln), []string{"row.0.1"}) {
		t.Errorf("a sibling holding the moment F saw elsewhere: learned %s from %q echoing %v (%s); want the sibling's echo of row.0.1", ln.Kind, ln.From.Mock, echoPaths(ln), ln.Why)
	}
	if ln := call(other, sibling); ln.Kind != KindVariable || ln.From.Mock != "mock-9" {
		t.Errorf("a stamp found carried from another recording: learned %s from %q (%s); want carried from mock-9, as before", ln.Kind, ln.From.Mock, ln.Why)
	}
}

// node-jwt sends its three role INSERTs at start-up at once, and Sequelize's
// pool sends them on two connections or more: record's full round (F) may
// take a later recorded one first, before it served its earlier siblings,
// which its learn step has then not read yet. Under the rule a copy follows
// its source (ClassifyFollowing) such a sibling's place is no source of the
// stamp (F had not served it: the application sent the call first), and the
// echo RETURNING gives stands beside it: the verdict is one in any order F
// took the statements in. Before, the echo was refused beside a source set
// aside, or, beside a sibling learned as an echo, the stamp was carried
// from that sibling. It fails closed: an echo beside an unserved recording
// that states no role, one that holds another value, one learned in another
// role, or any other recording set aside, is refused as before.
func TestAnEqualStampBesideASiblingFHadNotServedIsAnEcho(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	first := time.Date(2026, 11, 2, 3, 52, 27, 62_000_000, time.UTC)
	at := first.Add(5 * time.Millisecond)
	const same, other = "2026-11-02 03:52:27.056 +00:00", "2026-11-02 03:52:27.051 +00:00"
	role := func(kind Kind, v string) map[string]SiblingRole {
		return map[string]SiblingRole{"bind.$3": {Kind: kind, Value: v}, "bind.$4": {Kind: kind, Value: v}}
	}
	// A sibling's recorded answer holds its timestamptz cells in Postgres's
	// own spelling; the call's answer is handed to the classifier as its
	// protocol's learn step reads an echo, written back from the bind.
	const cell = "2026-11-02 03:52:27.056+00"
	insert := func(name, id, role string, at time.Time) *models.Mock {
		m := sibRolesInsert(name, id, role, same, at)
		row := m.Spec.PostgresV3.Query.Response.Rows[0]
		row[2].Value, row[3].Value = cell, cell
		return m
	}
	served := RawOf(insert("mock-16", "1", "user", first), true).ServedAs(1)
	unserved := RawOf(insert("mock-17", "2", "moderator", first.Add(2*time.Millisecond)), false)
	unserved16 := RawOf(insert("mock-16", "1", "user", first), false)
	resp := []Leaf{{Path: "row.1.createdAt", Type: String, Value: same}, {Path: "row.1.updatedAt", Type: String, Value: same}}
	live := sibLive(same, sequelize)
	call := func(raw ...Raw) map[string]Learned {
		return sibCallBy(ClassifyFollowing, at, map[string]string{"bind.$3": same, "bind.$4": same}, map[string]string{"bind.$3": live, "bind.$4": live}, raw, resp...)
	}
	want := []string{"row.1.createdAt", "row.1.updatedAt"}
	for _, row := range []struct {
		what string
		raw  []Raw
	}{
		{"the sibling F served learned as an echo, the other unread", []Raw{served.Sibling(role(KindEcho, same)), unserved.Sibling(role("", same))}},
		{"one sibling, unread", []Raw{unserved.Sibling(role("", same))}},
		{"two siblings, both unread", []Raw{unserved16.Sibling(role("", same)), unserved.Sibling(role("", same))}},
		{"one sibling F had not served, learned as an echo already", []Raw{unserved.Sibling(role(KindEcho, same))}},
	} {
		got := call(row.raw...)
		for _, p := range []string{"bind.$3", "bind.$4"} {
			if ln := got[p]; ln.Kind != KindEcho || !slices.Equal(echoPaths(ln), want) {
				t.Errorf("%s: %s learned %s %v from %q (%s); want an echo answered at %v", row.what, p, ln.Kind, echoPaths(ln), ln.From.Mock, ln.Why, want)
			}
		}
	}
	for _, row := range []struct {
		what string
		raw  []Raw
	}{
		{"no role stated", []Raw{unserved}},
		{"stated unread with another value", []Raw{unserved.Sibling(role("", other))}},
		{"learned as noise", []Raw{unserved.Sibling(role(KindNoise, same))}},
		{"beside an unread sibling, an unserved recording that is none", []Raw{unserved16, unserved.Sibling(role("", same))}},
	} {
		for p, ln := range call(row.raw...) {
			if ln.Kind != KindUnaccounted || !strings.Contains(ln.Why, "an echo keploy cannot answer beside a source it set aside") {
				t.Errorf("FALSE GREEN? %s: %s learned %s from %q (%s); want the echo refused beside a source set aside, as before", row.what, p, ln.Kind, ln.From.Mock, ln.Why)
			}
		}
	}
}
