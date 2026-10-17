package models

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models/mysql"
)

// An HTTP, HTTP/2 or MySQL mock whose recorder stated its lifetime
// (MetadataLifetimeKey) is derived as stated, whatever its type tag: the kind
// fallbacks that make an untagged or "mocks"-tagged data mock of those kinds
// reusable across tests do not apply to it. A mock without the key derives as
// it always did, and so does a mock of any other kind that carries it (the
// Postgres v3 recorder writes the key for its own replayer).
func TestDeriveLifetimeTakesTheStatedLifetime(t *testing.T) {
	stated := func(kind Kind, typ, lifetime, connID string) *Mock {
		md := map[string]string{MetadataLifetimeKey: lifetime}
		if typ != "" {
			md["type"] = typ
		}
		if connID != "" {
			md["connID"] = connID
		}
		return &Mock{Kind: kind, Spec: MockSpec{Metadata: md}}
	}
	for _, tc := range []struct {
		name string
		mock *Mock
		want Lifetime
	}{
		{"HTTP data stated per-test", stated(HTTP, string(HTTPClient), MetadataLifetimePerTest, ""), LifetimePerTest},
		{"untagged HTTP/2 data stated per-test", stated(HTTP2, "", MetadataLifetimePerTest, ""), LifetimePerTest},
		{"MySQL query stated per-test", stated(MySQL, "mocks", MetadataLifetimePerTest, "c1"), LifetimePerTest},
		{"MySQL SET stated session, though typed mocks", stated(MySQL, "mocks", MetadataLifetimeSession, "c1"), LifetimeSession},
		{"MySQL prepare stated connection", stated(MySQL, "connection", MetadataLifetimeConnection, "c1"), LifetimeConnection},
		{"connection stated with no connID is session", stated(MySQL, "connection", MetadataLifetimeConnection, ""), LifetimeSession},
		// A post-record hook tagged it config over its recorder's stated
		// per-test (enterprise's SQS config calls): the tag wins.
		{"HTTP stated per-test, then tagged config", stated(HTTP, "config", MetadataLifetimePerTest, ""), LifetimeSession},
		{"HTTP/2 stated per-test, then tagged config", stated(HTTP2, "config", MetadataLifetimePerTest, ""), LifetimeSession},
		{"MySQL stated per-test, then tagged config", stated(MySQL, "config", MetadataLifetimePerTest, "c1"), LifetimeSession},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.mock.DeriveLifetime()
			if got := tc.mock.TestModeInfo.Lifetime; got != tc.want {
				t.Fatalf("derived %v, want %v", got, tc.want)
			}
			// A stated per-test HTTP mock keeps the stateful cursor, within its
			// test's pool; every other stated mock is reused as recorded.
			want := ConsumeReuse
			if tc.mock.Kind == HTTP && tc.want == LifetimePerTest {
				want = ConsumeCursorSaturate
			}
			if got := tc.mock.TestModeInfo.Consume; got != want {
				t.Fatalf("consume mode %v, want %v", got, want)
			}
		})
	}

	// Without the key, the same mocks derive as they always did: reusable
	// under the lax fallback (KEPLOY_STRICT_MOCK_WINDOW not enabling, the
	// default), per-test with it enabled.
	legacy := LifetimeSession
	if laxKindFallbackDisabled() {
		legacy = LifetimePerTest
	}
	for _, m := range []*Mock{
		{Kind: HTTP, Spec: MockSpec{Metadata: map[string]string{"type": string(HTTPClient)}}},
		{Kind: MySQL, Spec: MockSpec{Metadata: map[string]string{"type": "mocks", "connID": "c1"}}},
		// An unknown value is no statement: the mock derives as one without it.
		stated(HTTP, string(HTTPClient), "someday", ""),
	} {
		m.DeriveLifetime()
		if m.TestModeInfo.Lifetime != legacy {
			t.Fatalf("%s %v without a stated lifetime derived %v, want %v as before", m.Kind, m.Spec.Metadata, m.TestModeInfo.Lifetime, legacy)
		}
	}

	// Other kinds read the key as they always did: not at all. Each derives
	// with it as it does without it.
	for _, tc := range []struct {
		kind  Kind
		typ   string
		value string
	}{
		{PostgresV2, "mocks", "connection"},
		{PostgresV2, "config", "per-test"},
		{Postgres, "mocks", MetadataLifetimeSession},
		{GENERIC, "config", MetadataLifetimePerTest},
		{GENERIC, "mocks", MetadataLifetimeSession},
		{Mongo, "mocks", MetadataLifetimeConnection},
	} {
		with := stated(tc.kind, tc.typ, tc.value, "c1")
		without := stated(tc.kind, tc.typ, tc.value, "c1")
		delete(without.Spec.Metadata, MetadataLifetimeKey)
		with.DeriveLifetime()
		without.DeriveLifetime()
		if with.TestModeInfo.Lifetime != without.TestModeInfo.Lifetime || with.TestModeInfo.Consume != without.TestModeInfo.Consume {
			t.Fatalf("%s typed %q with lifetime %q derived %v/%v, without it %v/%v; want the same", tc.kind, tc.typ, tc.value,
				with.TestModeInfo.Lifetime, with.TestModeInfo.Consume, without.TestModeInfo.Lifetime, without.TestModeInfo.Consume)
		}
		if with.StatesLifetime() {
			t.Fatalf("%s reports a stated lifetime", tc.kind)
		}
	}
}

// A recorder's stated per-test recordings of a request are one stateful
// sequence (StatefulSequenceKey), apart from reusable recordings of the same
// request; a stated recording that is no cursor recording belongs to none.
func TestStatefulSequenceKeyOfStatedRecordings(t *testing.T) {
	call := func(stated, typ, body string) *Mock {
		md := map[string]string{"type": typ}
		if stated != "" {
			md[MetadataLifetimeKey] = stated
		}
		return &Mock{Kind: HTTP, Spec: MockSpec{Metadata: md, HTTPReq: &HTTPReq{Method: "GET", URL: "http://api/counter", Body: body}}}
	}
	perTest, ok := StatefulSequenceKey(call(MetadataLifetimePerTest, "HTTP_CLIENT", ""))
	if !ok || !IsPerTestSequenceKey(perTest) {
		t.Fatalf("a stated per-test recording: key %q ok %v; want a per-test sequence", perTest, ok)
	}
	if again, _ := StatefulSequenceKey(call(MetadataLifetimePerTest, "HTTP_CLIENT", "")); again != perTest {
		t.Fatalf("two recordings of one request have keys %q and %q", perTest, again)
	}
	if other, ok := StatefulSequenceKey(call(MetadataLifetimePerTest, "HTTP_CLIENT", "x")); !ok || other == perTest {
		t.Fatalf("a recording with another body: key %q ok %v; want a sequence of its own", other, ok)
	}
	if !laxKindFallbackDisabled() {
		reusable, ok := StatefulSequenceKey(call("", "HTTP_CLIENT", ""))
		if !ok || IsPerTestSequenceKey(reusable) || reusable == perTest {
			t.Fatalf("a recording made before: key %q ok %v; want a reusable sequence apart from the per-test one", reusable, ok)
		}
	}
	if _, ok := StatefulSequenceKey(call(MetadataLifetimeSession, "config", "")); ok {
		t.Fatal("a recordOne representative (stated session) is no cursor recording")
	}
	if _, ok := StatefulSequenceKey(&Mock{Kind: MySQL, Spec: MockSpec{Metadata: map[string]string{MetadataLifetimeKey: MetadataLifetimePerTest, "connID": "c1"}}}); ok {
		t.Fatal("a stated MySQL mock belongs to no HTTP sequence")
	}
}

// A mock is reusable by its recorder's statement only when the statement
// makes it session or connection where its type tag and command would make
// it per-test.
func TestReusableByStatementOnly(t *testing.T) {
	mysqlMock := func(typ, stated, command string) *Mock {
		md := map[string]string{"type": typ, "connID": "c1"}
		if stated != "" {
			md[MetadataLifetimeKey] = stated
		}
		return &Mock{Kind: MySQL, Spec: MockSpec{Metadata: md, MySQLRequests: []mysql.Request{{PacketBundle: mysql.PacketBundle{Header: &mysql.PacketInfo{Type: command}}}}}}
	}
	for _, tc := range []struct {
		name string
		mock *Mock
		want bool
	}{
		{"SET stated session", mysqlMock("mocks", MetadataLifetimeSession, "COM_QUERY"), true},
		{"CLOSE stated connection", mysqlMock("mocks", MetadataLifetimeConnection, "COM_STMT_CLOSE"), true},
		{"PREPARE, tagged connection", mysqlMock("connection", MetadataLifetimeConnection, "COM_STMT_PREPARE"), false},
		{"handshake, tagged config", mysqlMock("config", MetadataLifetimeSession, "HandshakeResponse41"), false},
		{"COM_PING stated session", mysqlMock("mocks", MetadataLifetimeSession, "COM_PING"), false},
		{"query stated per-test", mysqlMock("mocks", MetadataLifetimePerTest, "COM_QUERY"), false},
		{"recorded before", mysqlMock("mocks", "", "COM_QUERY"), false},
	} {
		if got := tc.mock.ReusableByStatementOnly(); got != tc.want {
			t.Errorf("%s: ReusableByStatementOnly() = %v, want %v", tc.name, got, tc.want)
		}
	}
}
