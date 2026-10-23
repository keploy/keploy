package models

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models/mysql"
)

// fileSource is a PayloadSource whose file holds the mocks recorded: it hands
// back a fresh decode of the mock named by at, as a read of the file would.
type fileSource struct {
	recorded map[string]func() *Mock
	mutate   PayloadMutator
	reads    int
	err      error
}

func (s *fileSource) LoadPayload(_ context.Context, at PayloadAt) (*Mock, error) {
	s.reads++
	if s.err != nil {
		return nil, s.err
	}
	return s.recorded[at.Name](), nil
}
func (s *fileSource) SetPayloadMutator(m PayloadMutator) { s.mutate = m }
func (s *fileSource) PayloadMutator() PayloadMutator     { return s.mutate }

var payloadAt = time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)

// payloadMocks builds one mock of each shape whose payload a replay reads
// before it sends the mock: the reports' summary and target, a MySQL
// command's lifetime and protocol phase, and the registered predicates.
func payloadMocks() map[string]func() *Mock {
	mysqlOf := func(name, cmd string) func() *Mock {
		return func() *Mock {
			return &Mock{Version: "api.keploy.io/v1beta1", Name: name, Kind: MySQL, Spec: MockSpec{
				Metadata:         map[string]string{"type": "mocks", "destAddr": "mysql:3306", "requestOperation": cmd},
				MySQLRequests:    []mysql.Request{{PacketBundle: mysql.PacketBundle{Header: &mysql.PacketInfo{Type: cmd}}}},
				MySQLResponses:   []mysql.Response{{PacketBundle: mysql.PacketBundle{Header: &mysql.PacketInfo{Type: "OK"}}}},
				ReqTimestampMock: payloadAt, ResTimestampMock: payloadAt.Add(time.Millisecond),
			}}
		}
	}
	return map[string]func() *Mock{
		"http": func() *Mock {
			return &Mock{Version: "api.keploy.io/v1beta1", Name: "http", Kind: HTTP, ConnectionID: "c1", Noise: []string{"tok-.*"}, Spec: MockSpec{
				Metadata:         map[string]string{"type": "mocks", "operation": "GET"},
				HTTPReq:          &HTTPReq{Method: "GET", URL: "http://orders.internal:8080/v1/orders?id=7"},
				HTTPResp:         &HTTPResp{StatusCode: 200, Body: strings.Repeat("x", 4096)},
				Created:          7,
				ReqTimestampMock: payloadAt, ResTimestampMock: payloadAt.Add(2 * time.Millisecond),
			}}
		},
		"idle-poll": func() *Mock {
			return &Mock{Version: "api.keploy.io/v1beta1", Name: "idle-poll", Kind: HTTP, Spec: MockSpec{
				Metadata:         map[string]string{"type": "mocks"},
				HTTPReq:          &HTTPReq{Method: "POST", URL: "http://sqs.internal/?Action=ReceiveMessage"},
				HTTPResp:         &HTTPResp{StatusCode: 200, Body: "<ReceiveMessageResult/>"},
				ReqTimestampMock: payloadAt, ResTimestampMock: payloadAt.Add(time.Second),
			}}
		},
		"mongo": func() *Mock {
			return &Mock{Version: "api.keploy.io/v1beta1", Name: "mongo", Kind: Mongo, Spec: MockSpec{
				Metadata:         map[string]string{"type": "mocks", "operation": `{"find":"apps","filter":{}}`},
				MongoRequests:    []MongoRequest{{ReadDelay: 3}},
				MongoResponses:   []MongoResponse{{ReadDelay: 4}},
				ReqTimestampMock: payloadAt, ResTimestampMock: payloadAt.Add(time.Millisecond),
			}}
		},
		// Its summary and target, which carry the whole recorded command, are
		// longer than a mock whose payload is on disk keeps: they are worked
		// out again from the payload read back.
		"mongo-long": func() *Mock {
			return &Mock{Version: "api.keploy.io/v1beta1", Name: "mongo-long", Kind: Mongo, Spec: MockSpec{
				Metadata:         map[string]string{"type": "mocks", "operation": `{"insert":"apps","documents":["` + strings.Repeat("d", 4096) + `"]}`},
				MongoRequests:    []MongoRequest{{ReadDelay: 3}},
				MongoResponses:   []MongoResponse{{ReadDelay: 4}},
				ReqTimestampMock: payloadAt, ResTimestampMock: payloadAt.Add(time.Millisecond),
			}}
		},
		"mysql-prepare": mysqlOf("mysql-prepare", "COM_STMT_PREPARE"),
		"mysql-ping":    mysqlOf("mysql-ping", "COM_PING"),
		"mysql-query":   mysqlOf("mysql-query", "COM_QUERY"),
		"dns": func() *Mock {
			return &Mock{Version: "api.keploy.io/v1beta1", Name: "dns", Kind: DNS, Spec: MockSpec{
				Metadata:         map[string]string{"type": "config"},
				DNSReq:           &DNSReq{Name: "orders.internal."},
				ReqTimestampMock: payloadAt, ResTimestampMock: payloadAt.Add(time.Millisecond),
			}}
		},
	}
}

// answers is everything a replay asks of a mock before it sends it.
type answers struct {
	Summary, Target                         string
	ProtocolPhase, StatementLifecycle       bool
	ReusableByStatementOnly, StatesLifetime bool
	Idle, AnswerPlaced, CarryOver, InFlight bool
	Staged, MarksOutOfWindow                bool
	Place                                   time.Time
	Windowed                                WindowedMock
	Lifetime                                Lifetime
	Kind                                    Kind
	Name, Type                              string
}

func answersOf(m *Mock) answers {
	return answers{
		Summary: MockSummaryFromSpec(m), Target: MockTargetFromSpec(m),
		ProtocolPhase: m.IsProtocolPhase(), StatementLifecycle: m.IsMySQLStatementLifecycle(),
		ReusableByStatementOnly: m.ReusableByStatementOnly(), StatesLifetime: m.StatesLifetime(),
		Idle: IsIdleAnswer(m), AnswerPlaced: IsAnswerPlaced(m), CarryOver: IsCarryOver(m), InFlight: InFlightEligible(m),
		Staged: IsStagedAtStaging(m), MarksOutOfWindow: MarksOutOfWindow(m),
		Place: PlaceOf(m), Windowed: NewWindowedMock(m),
		Lifetime: m.TestModeInfo.Lifetime, Kind: m.Kind, Name: m.Name, Type: m.Spec.Metadata["type"],
	}
}

// registerPayloadPredicates registers a predicate on every registry whose
// predicates read a mock's payload, each matching one mock by its payload.
func registerPayloadPredicates(t *testing.T) {
	t.Helper()
	url := func(m *Mock, sub string) bool {
		return m.Spec.HTTPReq != nil && strings.Contains(m.Spec.HTTPReq.URL, sub)
	}
	for _, unregister := range []func(){
		RegisterIdleAnswer(HTTP, func(m *Mock) bool {
			return m.Spec.HTTPResp != nil && m.Spec.HTTPResp.Body == "<ReceiveMessageResult/>"
		}),
		RegisterAnswerPlaced(HTTP, func(m *Mock) bool { return url(m, "ReceiveMessage") }),
		RegisterCarryOver(HTTP, func(m *Mock) bool { return url(m, "/v1/orders") }),
		RegisterStagingNeed(Mongo, StagingNeed{Use: StagingWithStartup, Match: func(m *Mock) bool { return len(m.Spec.MongoRequests) == 1 }}),
		RegisterStagingNeed(DNS, StagingNeed{Use: StagingRelease, Match: func(m *Mock) bool { return m.Spec.DNSReq != nil }}),
	} {
		t.Cleanup(unregister)
	}
	RegisterSessionReusable(MySQL, func(m *Mock) bool {
		return len(m.Spec.MySQLRequests) == 1 && m.Spec.MySQLRequests[0].Header.Type == "COM_QUERY"
	})
	t.Cleanup(sessionReusableHooks.reset)
}

// A mock whose payload is left on disk answers every question a replay asks
// of it before sending it as it did whole, and holds none of its payload.
func TestLeavePayloadOnDiskAnswersAsTheWholeMock(t *testing.T) {
	registerPayloadPredicates(t)
	recorded := payloadMocks()
	src := &fileSource{recorded: recorded}
	for name, build := range recorded {
		t.Run(name, func(t *testing.T) {
			whole := build()
			whole.DeriveLifetime()
			want := answersOf(whole)

			m := build()
			m.DeriveLifetime()
			m.LeavePayloadOnDisk(src, PayloadAt{Offset: 10, Length: 20, Name: name})
			if !m.PayloadOnDisk() {
				t.Fatal("the mock does not say its payload is on disk")
			}
			if got := answersOf(m); !reflect.DeepEqual(got, want) {
				t.Fatalf("the mock with its payload on disk answers\n%+v\nwhole it answered\n%+v", got, want)
			}
			for _, kept := range []struct {
				field string
				ok    bool
			}{
				{"HTTPReq", m.Spec.HTTPReq == nil}, {"HTTPResp", m.Spec.HTTPResp == nil},
				{"MongoRequests", m.Spec.MongoRequests == nil}, {"MongoResponses", m.Spec.MongoResponses == nil},
				{"MySQLRequests", m.Spec.MySQLRequests == nil}, {"MySQLResponses", m.Spec.MySQLResponses == nil},
				{"DNSReq", m.Spec.DNSReq == nil}, {"Noise", m.Noise == nil},
				{"operation", m.Spec.Metadata["operation"] == ""},
			} {
				if !kept.ok {
					t.Errorf("the mock still holds its %s", kept.field)
				}
			}
			if m.Spec.ReqTimestampMock != whole.Spec.ReqTimestampMock || m.Spec.ResTimestampMock != whole.Spec.ResTimestampMock || m.Spec.Created != whole.Spec.Created {
				t.Error("the mock lost its request and response times")
			}
			if c := m.DeepCopy(); !c.PayloadOnDisk() || !reflect.DeepEqual(answersOf(c), want) {
				t.Error("a deep copy of the mock lost where its payload is, or what it answers")
			}
		})
	}
}

// A summary longer than a mock whose payload is on disk keeps is read from
// the file as recorded, without the source's mutator; one that cannot be read
// back is the mock's kind, as a mock with nothing identifying it is summed up.
func TestALongSummaryIsReadBackAsRecorded(t *testing.T) {
	src := &fileSource{recorded: payloadMocks()}
	src.SetPayloadMutator(func(_ context.Context, m *Mock) error {
		m.Spec.Metadata["operation"] = "decrypted"
		return nil
	})
	whole := src.recorded["mongo-long"]()
	m := src.recorded["mongo-long"]()
	m.LeavePayloadOnDisk(src, PayloadAt{Name: "mongo-long"})
	if m.onDisk.summary != "" || !m.onDisk.longSummary {
		t.Fatalf("the mock kept a %d-byte summary", len(m.onDisk.summary))
	}
	if got, want := MockSummaryFromSpec(m), MockSummaryFromSpec(whole); got != want {
		t.Fatalf("the summary read back is %.40q...; want the recorded %.40q...", got, want)
	}
	src.err = ErrPayloadFileReplaced
	if got := MockSummaryFromSpec(m); got != string(Mongo) {
		t.Fatalf("a summary that cannot be read back is %q; want the kind", got)
	}
	if got := MockTargetFromSpec(m); got != "" {
		t.Fatalf("a target that cannot be read back is %q; want none", got)
	}
}

// WithPayload is the mock as recorded, with the pool fields a replay gave it
// since it was read, and changed by the source's mutator; the mock it is read
// for keeps no payload.
func TestWithPayloadReadsThePayloadBack(t *testing.T) {
	recorded := payloadMocks()
	src := &fileSource{recorded: recorded}
	m := recorded["http"]()
	m.DeriveLifetime()
	m.LeavePayloadOnDisk(src, PayloadAt{Name: "http"})
	m.TestModeInfo.IsFiltered = true
	m.TestModeInfo.SortOrder = 4
	src.SetPayloadMutator(func(_ context.Context, w *Mock) error {
		w.Spec.HTTPResp.Body = "decrypted"
		return nil
	})

	whole, err := m.WithPayload(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := recorded["http"]()
	if !reflect.DeepEqual(whole.Spec.HTTPReq, want.Spec.HTTPReq) || whole.Spec.Metadata["operation"] != "GET" || !reflect.DeepEqual(whole.Noise, want.Noise) {
		t.Fatalf("the payload read back is %+v; want the recorded one", whole.Spec)
	}
	if whole.Spec.HTTPResp.Body != "decrypted" {
		t.Fatalf("the source's mutator did not change the payload read back: %q", whole.Spec.HTTPResp.Body)
	}
	if !whole.TestModeInfo.IsFiltered || whole.TestModeInfo.SortOrder != 4 || whole.TestModeInfo.Lifetime != m.TestModeInfo.Lifetime {
		t.Fatalf("the mock read back lost its test-mode info: %+v", whole.TestModeInfo)
	}
	if whole.PayloadOnDisk() || !m.PayloadOnDisk() || m.Spec.HTTPReq != nil {
		t.Fatal("the payload read back is held by the mock it was read for")
	}

	whole2, err := m.WithPayload(context.Background())
	if err != nil || whole2 == whole || src.reads != 2 {
		t.Fatalf("a second send did not read the payload back again: %v, reads %d", err, src.reads)
	}

	plain := recorded["dns"]()
	if got, err := plain.WithPayload(context.Background()); err != nil || got != plain {
		t.Fatal("a mock whose payload is in memory is not sent as it is")
	}
}

// A payload that cannot be read back fails the send, with the read's error.
func TestWithPayloadFailsAPayloadItCannotReadBack(t *testing.T) {
	src := &fileSource{recorded: payloadMocks(), err: ErrPayloadFileReplaced}
	m := src.recorded["mongo"]()
	m.LeavePayloadOnDisk(src, PayloadAt{Name: "mongo"})
	if _, err := m.WithPayload(context.Background()); !errors.Is(err, ErrPayloadFileReplaced) {
		t.Fatalf("WithPayload returned %v; want the read's ErrPayloadFileReplaced", err)
	}
}

// A payload hook may change a mock's payload only: one that changes what
// decides its pool, or its place in it, fails the mock.
func TestMutatePayloadRefusesAHookThatMovesTheMock(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Mock)
		ok     bool
	}{
		{"its payload", func(m *Mock) { m.Spec.HTTPResp.Body = "plain" }, true},
		{"its type", func(m *Mock) { m.Spec.Metadata["type"] = "config" }, false},
		{"its lifetime", func(m *Mock) { m.TestModeInfo.Lifetime = LifetimeSession }, false},
		{"its name", func(m *Mock) { m.Name = "other" }, false},
		{"its request time", func(m *Mock) { m.Spec.ReqTimestampMock = m.Spec.ReqTimestampMock.Add(time.Second) }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := payloadMocks()["http"]()
			err := MutatePayload(context.Background(), m, func(_ context.Context, m *Mock) error {
				tc.change(m)
				return nil
			})
			if tc.ok && err != nil {
				t.Fatalf("a hook that changed %s failed: %v", tc.name, err)
			}
			if !tc.ok && !errors.Is(err, ErrPayloadMutatorChangedMock) {
				t.Fatalf("a hook that changed %s returned %v; want ErrPayloadMutatorChangedMock", tc.name, err)
			}
		})
	}
	if err := MutatePayload(context.Background(), payloadMocks()["http"](), nil); err != nil {
		t.Fatalf("no mutator: %v", err)
	}
}
