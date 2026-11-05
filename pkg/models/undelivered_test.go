package models

import (
	"reflect"
	"testing"
)

// undeliveredMock is a data-plane answer recorded at at (ms past wm0), tagged
// as one the client never read when tagged is true.
func undeliveredMock(name string, at int, tagged bool) *Mock {
	m := &Mock{Name: name, Kind: Kind("Kafka"), Spec: MockSpec{
		Metadata:         map[string]string{"type": "mocks"},
		ReqTimestampMock: wmAt(at),
		ResTimestampMock: wmAt(at + 1),
	}}
	if tagged {
		m.MarkUndelivered()
	}
	return m
}

// Only the tag's own value says an answer went unread: no tag, another value,
// and a nil mock are answers the client read.
func TestOnlyTheUndeliveredTagMarksAnAnswerTheClientNeverRead(t *testing.T) {
	if !undeliveredMock("unread", 1000, true).IsUndelivered() {
		t.Fatal("an answer tagged undelivered was read as delivered")
	}
	bare := &Mock{Name: "bare", Kind: Kind("Kafka")}
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("MarkUndelivered on a mock with no metadata panicked: %v", p)
			}
		}()
		bare.MarkUndelivered()
	}()
	if !bare.IsUndelivered() {
		t.Fatal("MarkUndelivered on a mock with no metadata did not tag it")
	}
	// No test's and no worker scope's, but still the data-plane answer it is
	// to every site that serves or matches: no path that serves
	// protocol-phase mocks takes it for one.
	for _, m := range []*Mock{undeliveredMock("unread", 1000, true), bare} {
		if !m.OutsideTestScopes() {
			t.Fatalf("%s, an answer the client never read, was read as one a test or the worker scope may require", m.Name)
		}
		if m.IsProtocolPhase() {
			t.Fatalf("%s, an answer the client never read, was read as protocol-phase", m.Name)
		}
	}
	other := undeliveredMock("read", 1000, false)
	other.Spec.Metadata[MetadataUndeliveredKey] = "false"
	for _, m := range []*Mock{undeliveredMock("read", 1000, false), other, nil, {Name: "no-metadata"}} {
		if m.IsUndelivered() {
			t.Fatalf("%+v was read as undelivered", m)
		}
		if m != nil && m.Name != "no-metadata" && m.OutsideTestScopes() {
			t.Fatalf("%+v, an answer the client read, was read as outside every test's scope", m)
		}
	}
	// A protocol-phase mock is outside them as before.
	if cfg := (&Mock{Name: "cfg", Kind: Kind("Kafka"), Spec: MockSpec{Metadata: map[string]string{"type": "config"}}}); !cfg.OutsideTestScopes() {
		t.Fatal("a protocol-phase mock was read as one a test may require")
	}
}

// An answer the client never read is in no mapping: no test owns it, wherever
// it was recorded, and the worker scope does not list it, while the answer
// the client read beside it is mapped as before. Run at record (keploy
// record's mappings.yaml) and at replay (a set with no mappings.yaml) alike.
func TestAnUndeliveredAnswerIsInNoTestsMappingNorTheWorkerScope(t *testing.T) {
	windows := []TestWindow{wmWindow("test-1", 1000, 1100), wmWindow("test-2", 2000, 2100)}
	var mocks []WindowedMock
	for _, m := range []*Mock{
		undeliveredMock("unread-at-boot", 500, true),
		undeliveredMock("read-in-test-1", 1040, false),
		undeliveredMock("unread-in-test-1", 1050, true),
		undeliveredMock("unread-in-gap", 1600, true),
		undeliveredMock("read-in-gap", 1700, false),
		undeliveredMock("unread-in-test-2", 2050, true),
	} {
		w := NewWindowedMock(m)
		if w.TestMock == m.IsUndelivered() {
			t.Fatalf("NewWindowedMock(%s).TestMock = %v; want %v", m.Name, w.TestMock, !m.IsUndelivered())
		}
		mocks = append(mocks, w)
	}
	got := DeriveWindowMappings(windows, mocks)
	want := map[string][]string{"test-1": {"read-in-test-1"}, "test-2": {}}
	if names := mappedNames(got); !reflect.DeepEqual(names, want) {
		t.Fatalf("mapped %v; want %v", names, want)
	}
	if w := workerPlaces(got); !reflect.DeepEqual(w, []string{"read-in-gap@test-1"}) {
		t.Fatalf("worker scope %v; want only the answer read in the gap", w)
	}
}
