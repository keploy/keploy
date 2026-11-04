package http

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

func tiedMock(name string, id int, at time.Time) *models.Mock {
	m := &models.Mock{Name: name, Kind: models.HTTP}
	m.Spec.ReqTimestampMock = at
	m.TestModeInfo.ID = id
	return m
}

func joinNames(ms []*models.Mock) string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return strings.Join(out, ",")
}

// TestRecordedBeforeBreaksATieByTheRecordedNumber: of two recordings of one
// request time, the one mockdb numbered first is first, whatever IDs the
// tiers stamped (mock-2 with ID 5 before mock-10 with ID 1); names with no
// number keep the ID, then the name.
func TestRecordedBeforeBreaksATieByTheRecordedNumber(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)
	for _, tc := range []struct {
		a, b *models.Mock
	}{
		{tiedMock("mock-2", 5, at), tiedMock("mock-10", 1, at)},
		{tiedMock("mock-9", 0, at), tiedMock("mock-11", 0, at)},
		{tiedMock("mock-1", 9, at), tiedMock("custom", 0, at)},
		{tiedMock("custom-b", 1, at), tiedMock("custom-a", 2, at)},
		{tiedMock("custom-a", 3, at), tiedMock("custom-b", 3, at)},
		{tiedMock("mock-10", 0, at), tiedMock("mock-2", 0, at.Add(time.Nanosecond))},
	} {
		if !recordedBefore(tc.a, tc.b) || recordedBefore(tc.b, tc.a) {
			t.Errorf("want %s (ID %d) recorded before %s (ID %d)", tc.a.Name, tc.a.TestModeInfo.ID, tc.b.Name, tc.b.TestModeInfo.ID)
		}
	}
}

// TestInRecordedOrderBreaksATieByTheRecordedNumber: the learn pass reads the
// recordings of one request time in the order mockdb numbered them, mock-2
// before mock-10, not as their names sort as text; names with no number
// after them, by name.
func TestInRecordedOrderBreaksATieByTheRecordedNumber(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)
	in := []*models.Mock{tiedMock("mock-10", 0, at), tiedMock("custom-b", 0, at), tiedMock("mock-2", 0, at), tiedMock("custom-a", 0, at), tiedMock("mock-1", 0, at.Add(-time.Second))}
	if got, want := joinNames(inRecordedOrder(in)), "mock-1,mock-2,mock-10,custom-a,custom-b"; got != want {
		t.Fatalf("in recorded order %s, want %s", got, want)
	}
}
