package proxy

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.uber.org/zap"
)

// A store keeps one value a slot for a parser (integrations.Keep), made on
// the first call alone; a scoped view of the store reads the store's, and a
// closed store still keeps it.
func TestAStoreKeepsOneValueASlotForAParser(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	type slot struct{}
	made := 0
	mk := func() any { made++; return new(int) }
	v, ok := integrations.Keep(mm, slot{}, mk)
	if !ok || made != 1 {
		t.Fatalf("the store keeps nothing (ok %v) or made the value %d times; want it kept, made once", ok, made)
	}
	view := &scopedMockDb{MockMemDb: mm, live: true}
	for _, db := range []integrations.MockMemDb{mm, view} {
		if again, ok := integrations.Keep(db, slot{}, mk); !ok || again != v {
			t.Errorf("a read through %T keeps %v (ok %v); want the store's %v", db, again, ok, v)
		}
	}
	if other, _ := integrations.Keep(mm, struct{ other bool }{}, mk); other == v {
		t.Errorf("another slot reads the first slot's value")
	}
	mm.Close()
	if again, ok := integrations.Keep(mm, slot{}, mk); !ok || again != v {
		t.Errorf("the closed store keeps %v (ok %v); want %v", again, ok, v)
	}
	if made != 2 {
		t.Errorf("the values were made %d times; want 2, one a slot", made)
	}
}
