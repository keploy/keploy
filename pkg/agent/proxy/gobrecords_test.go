package proxy

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
)

// TestAKeptDecoderRefusesWhatItDoesNotReadWhole: a record a kept decoder
// reads only part of fails the read, and the decoder is not kept.
func TestAKeptDecoderRefusesWhatItDoesNotReadWhole(t *testing.T) {
	d := newStoreWith(t, residentBudgetBytes, perTestMockAt("a", 1))
	defer d.Close()
	e := d.byName["a"]
	rec, err := d.bytesAt(e.off, e.length)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeRecord[models.Mock](d, e.off, rec); err != nil {
		t.Fatalf("precondition: %v", err)
	}
	built := d.DecodersBuilt()
	padded := append(append([]byte(nil), rec...), rec...)
	if _, err := decodeRecord[models.Mock](d, e.off, padded); !errors.Is(err, errGobRecord) {
		t.Fatalf("a record followed by more bytes read with %v, want errGobRecord", err)
	}
	if _, err := decodeRecord[models.Mock](d, e.off, rec); err != nil {
		t.Fatal(err)
	}
	if got := d.DecodersBuilt(); got != built+1 {
		t.Fatalf("after a refused read, %d decoders built, want the refused one dropped and one more (%d)", got, built+1)
	}
}

// TestAStoreKeepsAtMostMaxKeptDecodersIdle: a burst of readers builds a
// decoder each, and the store keeps no more than maxKeptDecoders of them.
func TestAStoreKeepsAtMostMaxKeptDecodersIdle(t *testing.T) {
	d := newStoreWith(t, residentBudgetBytes)
	defer d.Close()
	var ks []*keptDecoder
	for i := 0; i < maxKeptDecoders+8; i++ {
		ks = append(ks, d.records.get())
	}
	for _, k := range ks {
		d.records.put(k)
	}
	if got := len(d.records.free); got != maxKeptDecoders {
		t.Fatalf("%d idle decoders kept, want %d", got, maxKeptDecoders)
	}
}

// TestAStreamThatLostALessonGoesOnInStreamsOfTheirOwn: a lesson the store
// fails to keep leaves the encoder having sent definitions no lesson holds,
// so every record after it is a gob stream of its own, which a new decoder
// reads alone.
func TestAStreamThatLostALessonGoesOnInStreamsOfTheirOwn(t *testing.T) {
	g := newGobRecords()
	failing := func([]byte) (int64, int, error) { return 0, 0, errors.New("disk full") }
	if _, err := g.encodeLocked(perTestMockAt("first", 1), false, 0, failing); err == nil {
		t.Fatal("a lesson the store failed to keep was not an error")
	}
	var kept [][]byte
	keeping := func(b []byte) (int64, int, error) { kept = append(kept, b); return int64(len(kept)), len(b), nil }
	b, err := g.encodeLocked(perTestMockAt("second", 2), false, 0, keeping)
	if err != nil {
		t.Fatal(err)
	}
	if g.freshFrom != 0 {
		t.Fatalf("records from %d are streams of their own, want from 0", g.freshFrom)
	}
	var m models.Mock
	if err := gob.NewDecoder(bytes.NewReader(b)).Decode(&m); err != nil || m.Name != "second" {
		t.Fatalf("the record after the lost lesson does not read alone: %v (%q)", err, m.Name)
	}
}

// mysqlExecMock is a per-test MySQL COM_STMT_EXECUTE mock whose one
// parameter holds v: an interface value (Parameter.Value) inside an
// interface value (PacketBundle.Message).
func mysqlExecMock(name string, sec int64, v any) *models.Mock {
	m := perTestMockAt(name, sec)
	m.Kind = models.MySQL
	m.Spec.MySQLRequests = []mysql.Request{{PacketBundle: mysql.PacketBundle{
		Header:  &mysql.PacketInfo{Type: "COM_STMT_EXECUTE"},
		Message: &mysql.StmtExecutePacket{StatementID: 1, ParameterCount: 1, Parameters: []mysql.Parameter{{Type: 245, Value: v}}},
	}}}
	return m
}

// nestedMocks are three mocks whose second is the first to hold a
// map[string]interface{} in an interface value inside an interface value.
// gob writes that map's definition inside the outer value's message, so the
// second mock's record, one gob message, still carries a definition, and
// the third mock's record reads only with it.
func nestedMocks() []*models.Mock {
	return []*models.Mock{
		mysqlExecMock("a-int", 1, int64(7)),
		mysqlExecMock("b-json", 2, map[string]interface{}{"k": "v"}),
		mysqlExecMock("c-json", 3, map[string]interface{}{"k": "w"}),
	}
}

// TestADefinitionInsideAnInterfaceValueIsALesson: a mock whose only new type
// sits in an interface value inside an interface value teaches that type in
// a lesson, so a reader that skips it (a window that starts after it, a
// re-run of one failing test) reads a later mock of that type, as main did,
// where every record was a stream of its own.
func TestADefinitionInsideAnInterfaceValueIsALesson(t *testing.T) {
	ms := nestedMocks()
	for _, budget := range []int64{0, residentBudgetBytes} {
		d := newStoreWith(t, budget, ms...)
		if got, want := servedRead(t, d, "c-json"), freshRead(t, ms[2]); !reflect.DeepEqual(got, want) {
			t.Fatalf("budget %d: c-json served\n%+v\nwant\n%+v", budget, got.Spec, want.Spec)
		}
		_ = d.Close()
	}
}

// TestNestedInterfaceMocksReadInEveryOrder: a new store, read in each of
// the six orders, serves every mock as a new decoder reads it alone.
func TestNestedInterfaceMocksReadInEveryOrder(t *testing.T) {
	ms := nestedMocks()
	orders := [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	for _, budget := range []int64{0, residentBudgetBytes} {
		for _, o := range orders {
			d := newStoreWith(t, budget, ms...)
			sameAsFresh(t, d, []*models.Mock{ms[o[0]], ms[o[1]], ms[o[2]]}, fmt.Sprintf("budget %d, order %v", budget, o))
			_ = d.Close()
		}
	}
}

// TestASecondDecoderReadsANestedInterfaceMock: while one reader holds the
// kept decoder that read the second mock, another reader builds a decoder
// of its own and reads the third.
func TestASecondDecoderReadsANestedInterfaceMock(t *testing.T) {
	ms := nestedMocks()
	for _, budget := range []int64{0, residentBudgetBytes} {
		d := newStoreWith(t, budget, ms...)
		servedRead(t, d, "b-json")
		held := d.records.get()
		if d.DecodersBuilt() != 1 {
			t.Fatalf("precondition: %d decoders built before the second reader, want 1", d.DecodersBuilt())
		}
		if got, want := servedRead(t, d, "c-json"), freshRead(t, ms[2]); !reflect.DeepEqual(got, want) {
			t.Fatalf("budget %d: c-json served by a second decoder\n%+v\nwant\n%+v", budget, got.Spec, want.Spec)
		}
		if d.DecodersBuilt() != 2 {
			t.Fatalf("precondition: %d decoders built, want the held one and the second reader's", d.DecodersBuilt())
		}
		d.records.put(held)
		_ = d.Close()
	}
}
