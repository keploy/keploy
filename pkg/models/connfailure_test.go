package models

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	yamlLib "gopkg.in/yaml.v3"
)

var connFailureT0 = time.Date(2026, 10, 8, 10, 21, 46, 511876000, time.UTC)

func connFailureMock(phase ConnFailurePhase, outcome ConnFailureOutcome) *Mock {
	return &Mock{
		Version: GetVersion(),
		Kind:    ConnectionFailure,
		Name:    "mock-cf",
		Spec: MockSpec{
			Metadata: map[string]string{"type": "mocks"},
			ConnFailure: &ConnFailureSpec{
				Address: "127.0.0.1:59999",
				Host:    "db.example.com",
				Phase:   phase,
				Outcome: outcome,
				Cause:   "dial tcp 127.0.0.1:59999: connect: connection refused",
			},
			ReqTimestampMock: connFailureT0,
			ResTimestampMock: connFailureT0.Add(time.Millisecond),
		},
	}
}

// The no-hyphen rule is load-bearing (see the comment on ConnectionFailure in
// mock.go): every released keploy skips a hyphenated kind at Debug level
// before its unknown-kind ERROR, so a hyphen would make an older keploy drop
// these mocks without a word. The mockdb test
// TestConnectionFailureKindReachesTheUnknownKindError shows the two paths.
func TestConnectionFailureKindHasNoHyphen(t *testing.T) {
	if strings.Contains(string(ConnectionFailure), "-") {
		t.Fatalf("kind %q contains a hyphen: every keploy since 2023 skips such a kind at Debug level as "+
			"enterprise-only, before the unknown-kind ERROR, so an older keploy would drop these mocks silently "+
			"instead of saying it cannot read them", ConnectionFailure)
	}
}

func TestConnFailureSpecValidate(t *testing.T) {
	valid := []struct {
		phase    ConnFailurePhase
		outcomes []ConnFailureOutcome
	}{
		{ConnFailurePhaseConnect, []ConnFailureOutcome{ConnFailureRefused, ConnFailureHostUnreachable, ConnFailureNetUnreachable, ConnFailureTimeout}},
		{ConnFailurePhaseAccepted, []ConnFailureOutcome{ConnFailureClosed}},
		{ConnFailurePhaseTLS, []ConnFailureOutcome{ConnFailureClosed}},
		{ConnFailurePhaseRequest, []ConnFailureOutcome{ConnFailureClosed}},
	}
	for _, v := range valid {
		for _, o := range v.outcomes {
			s := connFailureMock(v.phase, o).Spec.ConnFailure
			if err := s.Validate(); err != nil {
				t.Errorf("phase %q outcome %q is in the format, Validate = %v", v.phase, o, err)
			}
		}
	}
	for _, addr := range []string{"[2001:db8::10]:443", "10.0.3.7:65535"} {
		s := connFailureMock(ConnFailurePhaseConnect, ConnFailureNetUnreachable).Spec.ConnFailure
		s.Address = addr
		if err := s.Validate(); err != nil {
			t.Errorf("address %q: Validate = %v", addr, err)
		}
	}

	const upgrade = ", which this keploy does not support; upgrade keploy"
	for _, tc := range []struct {
		name string
		edit func(*ConnFailureSpec)
		want string
	}{
		// What a newer keploy may write: named, with what to do about it.
		{"unknown phase", func(s *ConnFailureSpec) { s.Phase = "handshake" }, `uses phase "handshake"` + upgrade},
		{"unknown outcome", func(s *ConnFailureSpec) { s.Outcome = "reset" }, `uses outcome "reset"` + upgrade},
		{"unknown outcome in a later phase", func(s *ConnFailureSpec) {
			s.Phase, s.Outcome = ConnFailurePhaseTLS, "tls-alert"
		}, `uses outcome "tls-alert"` + upgrade},
		{"known outcome in a phase that does not have it", func(s *ConnFailureSpec) {
			s.Phase, s.Outcome = ConnFailurePhaseConnect, ConnFailureClosed
		}, `uses outcome "closed" in phase "connect"` + upgrade},
		{"a connect outcome after the connection was up", func(s *ConnFailureSpec) {
			s.Phase, s.Outcome = ConnFailurePhaseTLS, ConnFailureRefused
		}, `uses outcome "refused" in phase "tls"` + upgrade},
		// A newer phase or outcome decides what the address looks like, so it
		// is named before the address is judged: such a document gets the
		// upgrade advice, not "malformed".
		{"a later phase with no address", func(s *ConnFailureSpec) {
			s.Phase, s.Outcome, s.Address = "dns", "nxdomain", ""
		}, `uses phase "dns"` + upgrade},
		{"a later phase with a name for an address", func(s *ConnFailureSpec) {
			s.Phase, s.Outcome, s.Address = "dns", "nxdomain", "db.example.com"
		}, `uses phase "dns"` + upgrade},
		{"a later phase with a socket path", func(s *ConnFailureSpec) {
			s.Phase, s.Address = "unix", "/var/run/docker.sock"
		}, `uses phase "unix"` + upgrade},
		{"a later outcome with no address", func(s *ConnFailureSpec) {
			s.Outcome, s.Address = "reset", ""
		}, `uses outcome "reset"` + upgrade},
		{"a pair this keploy does not know, with no address", func(s *ConnFailureSpec) {
			s.Outcome, s.Address = ConnFailureClosed, ""
		}, `uses outcome "closed" in phase "connect"` + upgrade},
		// What no keploy writes: malformed.
		{"no address", func(s *ConnFailureSpec) { s.Address = "" }, "has no address"},
		{"address without a port", func(s *ConnFailureSpec) { s.Address = "127.0.0.1" }, "is not host:port"},
		{"port zero", func(s *ConnFailureSpec) { s.Address = "127.0.0.1:0" }, "has no valid port"},
		{"port out of range", func(s *ConnFailureSpec) { s.Address = "127.0.0.1:70000" }, "has no valid port"},
		{"no phase", func(s *ConnFailureSpec) { s.Phase = "" }, "has no phase"},
		{"no outcome", func(s *ConnFailureSpec) { s.Outcome = "" }, "has no outcome"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := connFailureMock(ConnFailurePhaseConnect, ConnFailureRefused).Spec.ConnFailure
			tc.edit(s)
			err := s.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v, want an error containing %q", err, tc.want)
			}
			if strings.HasSuffix(tc.want, upgrade) != strings.Contains(err.Error(), "upgrade keploy") {
				t.Fatalf("only a value a newer keploy may write says to upgrade; got %q", err)
			}
		})
	}

	var nilSpec *ConnFailureSpec
	if err := nilSpec.Validate(); err == nil {
		t.Fatal("a connection failure with no details must not validate")
	}
}

// The format is closed: CheckConnFailureFields knows exactly the fields
// ConnFailureSchema writes, in both file formats, and refuses any other with
// the upgrade advice. A field added to the struct without being added to the
// list (or the reverse) fails here, before a reader refuses its own keploy's
// documents or accepts a field it would drop.
func TestConnFailureFieldsAreTheSchemas(t *testing.T) {
	full := ConnFailureSchema{
		Metadata:         map[string]string{"type": "mocks"},
		ConnFailureSpec:  *connFailureMock(ConnFailurePhaseConnect, ConnFailureRefused).Spec.ConnFailure,
		ReqTimestampMock: connFailureT0,
		ResTimestampMock: connFailureT0.Add(time.Millisecond),
	}
	j, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	var jsonKeys map[string]json.RawMessage
	if err := json.Unmarshal(j, &jsonKeys); err != nil {
		t.Fatal(err)
	}
	y, err := yamlLib.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	var yamlKeys map[string]yamlLib.Node
	if err := yamlLib.Unmarshal(y, &yamlKeys); err != nil {
		t.Fatal(err)
	}
	for format, keys := range map[string][]string{"json": mapKeys(jsonKeys), "yaml": mapKeys(yamlKeys)} {
		if len(keys) != len(connFailureFields) {
			t.Fatalf("%s writes the fields %v; the format knows %d", format, keys, len(connFailureFields))
		}
		if err := CheckConnFailureFields(keys); err != nil {
			t.Fatalf("%s: a field this keploy writes is refused: %v", format, err)
		}
	}

	for _, newer := range []string{"count", "acceptedBefore", "Address"} {
		err := CheckConnFailureFields([]string{"address", newer, "phase"})
		want := `the connection failure uses field "` + newer + `", which this keploy does not support; upgrade keploy`
		if err == nil || err.Error() != want {
			t.Fatalf("CheckConnFailureFields(%q) = %v, want %q", newer, err, want)
		}
	}
}

func mapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A connection failure is only ever replayed in the test window its request
// time falls in. Without both times it can be placed in none, and the filters
// would serve an untimed per-test mock to every test.
func TestValidateConnFailureNeedsBothTimesInOrder(t *testing.T) {
	ok := connFailureMock(ConnFailurePhaseConnect, ConnFailureRefused)
	if err := ok.ValidateConnFailure(); err != nil {
		t.Fatalf("a valid mock: %v", err)
	}
	for _, tc := range []struct {
		name string
		edit func(*Mock)
		want string
	}{
		{"no request time", func(m *Mock) { m.Spec.ReqTimestampMock = time.Time{} }, "cannot be placed"},
		{"no response time", func(m *Mock) { m.Spec.ResTimestampMock = time.Time{} }, "cannot be placed"},
		{"response before request", func(m *Mock) { m.Spec.ResTimestampMock = m.Spec.ReqTimestampMock.Add(-time.Millisecond) }, "is before its reqTimestampMock"},
		{"no spec", func(m *Mock) { m.Spec.ConnFailure = nil }, "has no details"},
		{"invalid spec", func(m *Mock) { m.Spec.ConnFailure.Outcome = "reset" }, "upgrade keploy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := connFailureMock(ConnFailurePhaseConnect, ConnFailureRefused)
			tc.edit(m)
			if err := m.ValidateConnFailure(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateConnFailure = %v, want an error containing %q", err, tc.want)
			}
		})
	}
}

// Whatever its metadata says, and whatever an out-of-tree hook registers for
// it, a connection failure is per-test: every reusable lifetime would serve a
// failure recorded in one test to others.
func TestDeriveLifetimeKeepsConnectionFailuresPerTest(t *testing.T) {
	t.Cleanup(sessionReusableHooks.reset)
	sessionReusableHooks.reset()
	RegisterSessionReusable(ConnectionFailure, func(*Mock) bool { return true })

	for _, tag := range []string{"", "mocks", "config", "connection", "HTTP_CLIENT"} {
		t.Run("tag="+tag, func(t *testing.T) {
			m := connFailureMock(ConnFailurePhaseConnect, ConnFailureRefused)
			m.Spec.Metadata = map[string]string{"connID": "c1"}
			if tag != "" {
				m.Spec.Metadata["type"] = tag
			}
			m.DeriveLifetime()
			if m.TestModeInfo.Lifetime != LifetimePerTest || !m.TestModeInfo.LifetimeDerived {
				t.Fatalf("Lifetime = %v (derived %v), want per-test", m.TestModeInfo.Lifetime, m.TestModeInfo.LifetimeDerived)
			}
		})
	}

	// Control: the same tag on an ordinary kind is reusable, so the cases
	// above are not passing for want of a rule that could promote them.
	cfg := &Mock{Kind: GENERIC, Spec: MockSpec{Metadata: map[string]string{"type": "config"}}}
	cfg.DeriveLifetime()
	if cfg.TestModeInfo.Lifetime != LifetimeSession {
		t.Fatalf("control: a config-tagged Generic mock is %v, want session", cfg.TestModeInfo.Lifetime)
	}
}

// A registration for the kind cannot make a connection failure carry over to
// a later window: that is serving it in a test it does not belong to.
func TestConnectionFailuresNeverCarryOver(t *testing.T) {
	unregister := RegisterCarryOver(ConnectionFailure, func(*Mock) bool { return true })
	t.Cleanup(unregister)

	m := connFailureMock(ConnFailurePhaseConnect, ConnFailureRefused)
	m.DeriveLifetime()
	if IsCarryOver(m) {
		t.Fatal("a connection failure must never be carry-over")
	}
	if CarryOverKind(ConnectionFailure) {
		t.Fatal("ConnectionFailure must never be a carry-over kind")
	}

	// Control: the same registration does carry over another kind.
	other := RegisterCarryOver(brokerKind, func(*Mock) bool { return true })
	t.Cleanup(other)
	b := brokerMock("mocks", "SEND")
	b.DeriveLifetime()
	if !IsCarryOver(b) {
		t.Fatal("control: a registered per-test broker mock is carry-over")
	}
}

func TestWindowBound(t *testing.T) {
	if !WindowBound(ConnectionFailure) {
		t.Fatal("ConnectionFailure is window-bound")
	}
	for _, k := range []Kind{HTTP, HTTP2, GENERIC, MySQL, Postgres, PostgresV2, PostgresV3, GRPC_EXPORT, Mongo, DNS, REDIS, KAFKA, Aerospike} {
		if WindowBound(k) {
			t.Fatalf("%s must not be window-bound: that would drop its out-of-window mocks in lax mode", k)
		}
	}
}

func TestDeepCopyDetachesTheConnFailureSpec(t *testing.T) {
	m := connFailureMock(ConnFailurePhaseConnect, ConnFailureRefused)
	c := m.DeepCopy()
	if c.Spec.ConnFailure == m.Spec.ConnFailure {
		t.Fatal("DeepCopy shares the ConnFailure pointer")
	}
	if *c.Spec.ConnFailure != *m.Spec.ConnFailure {
		t.Fatalf("DeepCopy changed the spec: %+v, want %+v", *c.Spec.ConnFailure, *m.Spec.ConnFailure)
	}
	c.Spec.ConnFailure.Outcome = ConnFailureTimeout
	if m.Spec.ConnFailure.Outcome != ConnFailureRefused {
		t.Fatal("editing the copy edited the original")
	}
}

func TestExcludedFromDependencyAssertion(t *testing.T) {
	for k, want := range map[Kind]bool{
		DNS: true, ConnectionFailure: true,
		HTTP: false, HTTP2: false, GENERIC: false, MySQL: false, PostgresV2: false, PostgresV3: false,
		GRPC_EXPORT: false, Mongo: false, REDIS: false, KAFKA: false, Kind(""): false,
	} {
		if got := ExcludedFromDependencyAssertion(k); got != want {
			t.Errorf("ExcludedFromDependencyAssertion(%q) = %v, want %v", k, got, want)
		}
	}
}

// AgentBound is what the agent holds: every mock but the connection failures,
// in order, as a new slice when it drops any (the caller's is left as it was),
// and the same slice when it drops none. A nil entry passes through.
func TestAgentBound(t *testing.T) {
	cf := connFailureMock(ConnFailurePhaseConnect, ConnFailureRefused)
	http := &Mock{Name: "mock-h", Kind: HTTP}
	dns := &Mock{Name: "mock-d", Kind: DNS}
	in := []*Mock{http, cf, nil, dns, cf}
	got := AgentBound(in)
	if len(got) != 3 || got[0] != http || got[1] != nil || got[2] != dns {
		t.Fatalf("AgentBound = %v, want [mock-h <nil> mock-d]", got)
	}
	if in[1] != cf || in[4] != cf || len(in) != 5 {
		t.Fatal("AgentBound changed the caller's slice")
	}
	none := []*Mock{http, nil, dns}
	if got := AgentBound(none); len(got) != 3 || &got[0] != &none[0] {
		t.Fatal("with nothing to drop, AgentBound returns the slice it was given")
	}
}
