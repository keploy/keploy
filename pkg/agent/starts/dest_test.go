package starts

import (
	"strconv"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A mock is stamped with the address its connection reached, whether its
// parser put the connection's ID on the mock or in its metadata; a
// recorder's own destAddr stays; the metadata map a parser handed over is
// replaced by a copy, never edited, since a parser may hand several mocks
// one map.
func TestStampDestNamesTheAddressItsConnectionReached(t *testing.T) {
	r := New(newFake(), 0)
	r.Dest("7", "127.0.0.1:4000")

	byID := &models.Mock{Kind: models.HTTP, ConnectionID: "7"}
	r.StampDest(byID)
	if got := byID.Spec.Metadata["destAddr"]; got != "127.0.0.1:4000" {
		t.Fatalf("a mock carrying its connection's ID: destAddr = %q, want 127.0.0.1:4000", got)
	}

	shared := map[string]string{"connID": "7", "type": "config"}
	byMeta := &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{Metadata: shared}}
	r.StampDest(byMeta)
	if got := byMeta.Spec.Metadata["destAddr"]; got != "127.0.0.1:4000" {
		t.Fatalf("a mock naming its connection in its metadata: destAddr = %q, want 127.0.0.1:4000", got)
	}
	if _, edited := shared["destAddr"]; edited {
		t.Fatal("the parser's metadata map was edited in place; another mock handed over with it would change too")
	}
	if byMeta.Spec.Metadata["type"] != "config" {
		t.Fatalf("the copy lost the mock's own metadata: %v", byMeta.Spec.Metadata)
	}

	own := &models.Mock{Kind: models.MySQL, ConnectionID: "7", Spec: models.MockSpec{Metadata: map[string]string{"destAddr": "db:3306"}}}
	r.StampDest(own)
	if got := own.Spec.Metadata["destAddr"]; got != "db:3306" {
		t.Fatalf("a recorder's own destAddr must stay, got %q", got)
	}

	other := &models.Mock{Kind: models.HTTP, ConnectionID: "8"}
	r.StampDest(other)
	if other.Spec.Metadata != nil {
		t.Fatalf("a mock of a connection never noted was stamped: %v", other.Spec.Metadata)
	}
}

// A closed connection's address is kept for a late mock, among the last
// keptClosedDests closed: the table is bounded by the connections open, not
// by how many a recording made. An open connection is never dropped,
// however many close after it opened.
func TestAClosedConnectionsAddressIsKeptOnlyAmongTheLastClosed(t *testing.T) {
	r := New(newFake(), 0)
	r.Dest("open", "127.0.0.1:3306")
	n := keptClosedDests + 10
	for i := 0; i < n; i++ {
		id := strconv.Itoa(i)
		r.Dest(id, "127.0.0.1:4000")
		r.ForgetDest(id)
	}
	stamp := func(id string) string {
		m := &models.Mock{ConnectionID: id}
		r.StampDest(m)
		return m.Spec.Metadata["destAddr"]
	}
	if got := stamp(strconv.Itoa(n - 1)); got != "127.0.0.1:4000" {
		t.Fatalf("the last connection closed: destAddr = %q, want it kept for a late mock", got)
	}
	if got := stamp(strconv.Itoa(n - keptClosedDests)); got != "127.0.0.1:4000" {
		t.Fatalf("the oldest connection still among the kept: destAddr = %q, want 127.0.0.1:4000", got)
	}
	if got := stamp("0"); got != "" {
		t.Fatalf("a connection closed before the last %d: destAddr = %q, want it forgotten", keptClosedDests, got)
	}
	if got := stamp("open"); got != "127.0.0.1:3306" {
		t.Fatalf("an open connection: destAddr = %q, want 127.0.0.1:3306", got)
	}
	r.dest.mu.Lock()
	size := len(r.dest.addrs)
	r.dest.mu.Unlock()
	if want := keptClosedDests + 1; size != want {
		t.Fatalf("the table holds %d addresses after %d connections closed; want %d (the kept closed and the open one)", size, n, want)
	}

	// A connection ID noted again is the later connection's: open, so the
	// closes after it do not drop it.
	r.Dest(strconv.Itoa(n-1), "127.0.0.1:8080")
	for i := 0; i < keptClosedDests; i++ {
		id := "later-" + strconv.Itoa(i)
		r.Dest(id, "127.0.0.1:4000")
		r.ForgetDest(id)
	}
	if got := stamp(strconv.Itoa(n - 1)); got != "127.0.0.1:8080" {
		t.Fatalf("a connection ID noted again and still open: destAddr = %q, want 127.0.0.1:8080", got)
	}
}

// A new session starts the table empty, and a close of a connection the
// session never noted keeps nothing.
func TestResetForgetsEveryAddress(t *testing.T) {
	r := New(newFake(), 0)
	r.Dest("7", "127.0.0.1:4000")
	r.Reset()
	r.ForgetDest("7")
	m := &models.Mock{ConnectionID: "7"}
	r.Stamp(m)
	if got := m.Spec.Metadata["destAddr"]; got != "" {
		t.Fatalf("after a reset, destAddr = %q; want none", got)
	}
	r.dest.mu.Lock()
	closed := len(r.dest.closed)
	r.dest.mu.Unlock()
	if closed != 0 {
		t.Fatalf("a close of a connection the session never noted was kept: %d closed", closed)
	}
}
