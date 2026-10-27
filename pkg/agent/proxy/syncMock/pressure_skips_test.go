package manager

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"go.uber.org/zap"
)

// The count of requests not recorded under pressure keeps each connection's
// apart up to its bound, and past it counts a new connection's requests in the
// total and in Other, so a load that opens a connection per request cannot
// grow it for as long as a pause lasts; a session reports what it grew by.
func TestNotRecordedUnderPressureCountsByConnectionWithinItsBound(t *testing.T) {
	t.Parallel()
	m := New(zap.NewNop())
	before := m.NotRecordedUnderPressure()
	for i := 0; i < maxPressureSkipConns; i++ {
		m.NoteNotRecordedUnderPressure(fmt.Sprintf("c%d", i))
	}
	m.NoteNotRecordedUnderPressure("c0")
	m.NoteNotRecordedUnderPressure("past the bound")
	m.NoteNotRecordedUnderPressure("past the bound")

	got := m.NotRecordedUnderPressure().Since(before)
	if got.Total != maxPressureSkipConns+3 {
		t.Fatalf("total = %d, want %d", got.Total, maxPressureSkipConns+3)
	}
	if len(got.ByConn) != maxPressureSkipConns || got.ByConn["c0"] != 2 {
		t.Fatalf("kept %d connections apart, c0 has %d: want %d, and c0's two", len(got.ByConn), got.ByConn["c0"], maxPressureSkipConns)
	}
	if got.Other != 2 || got.Connections() != maxPressureSkipConns+1 {
		t.Fatalf("other = %d, connections = %d: want the two past the bound, on one more connection", got.Other, got.Connections())
	}

	mid := m.NotRecordedUnderPressure()
	m.NoteNotRecordedUnderPressure("c1")
	if d := m.NotRecordedUnderPressure().Since(mid); d.Total != 1 || !reflect.DeepEqual(d.ByConn, map[string]uint64{"c1": 1}) || d.Other != 0 {
		t.Fatalf("since the mid read: %+v, want c1's one", d)
	}
}

// Most names the connections with the most requests first, ties by name, and
// says "unnamed" for a connection its caller could not name.
func TestPressureSkipsMostNamesTheBusiestFirst(t *testing.T) {
	t.Parallel()
	p := PressureSkips{Total: 7, ByConn: map[string]uint64{"b": 2, "a": 2, "": 3}}
	if got, want := p.Most(2), []string{"unnamed (3)", "a (2)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Most(2) = %q, want %q", got, want)
	}
	if got := (PressureSkips{}).Most(8); len(got) != 0 {
		t.Fatalf("Most of nothing = %q", got)
	}
}

// A nil manager counts nothing and reads nothing, and an empty connection name
// leaves the context as it is.
func TestNotRecordedUnderPressureNilSafe(t *testing.T) {
	t.Parallel()
	var m *SyncMockManager
	m.NoteNotRecordedUnderPressure("c")
	if got := m.NotRecordedUnderPressure(); got.Total != 0 || got.ByConn != nil {
		t.Fatalf("nil manager read %+v", got)
	}
	ctx := context.Background()
	if WithIngressConn(ctx, "") != ctx || IngressConn(ctx) != "" {
		t.Fatal("an empty name changed the context")
	}
	if got := IngressConn(WithIngressConn(ctx, "c -> a")); got != "c -> a" {
		t.Fatalf("IngressConn = %q", got)
	}
}
