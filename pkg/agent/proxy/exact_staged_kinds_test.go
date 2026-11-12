package proxy

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// watchWalks counts, until the row ends, the walks of tree's values
// (rangeValues).
func watchWalks(t *testing.T, tree *TreeDb) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	w := func(db *TreeDb) {
		if db == tree {
			n.Add(1)
		}
	}
	rangeValuesWatch.Store(&w)
	t.Cleanup(func() { rangeValuesWatch.Store(nil) })
	return &n
}

// An exact view's install, at each test's open and close, moves the
// revision of every kind the staged trees hold, the startup tier's
// included, so a parser that keeps an index of one kind reads again; it
// reads those kinds as the staging kept them, never walking the startup
// tier: a walk there cost each test the size of the whole staged set (an
// initial staging files the session pool in the startup tier too). A kind
// only the startup tier holds (a three-tier staging's connection mock),
// and one a session update brings into it (mirrored into the startup tier),
// move as before.
func TestAnInstallMovesEveryStagedKindWithoutWalkingTheStartupTier(t *testing.T) {
	const n = 3000
	pool := make([]*models.Mock, 0, n)
	for i := range n {
		pool = append(pool, exactMongo(fmt.Sprintf("pool-%d", i), time.Duration(i)*time.Millisecond, models.LifetimeSession))
	}
	conn := exactMock("conn-only", time.Hour, models.LifetimeConnection)
	conn.Kind = models.Postgres
	owned := exactMongo("owned", 2*time.Hour, models.LifetimePerTest)
	mm := NewMockManager(nil, nil, nil)
	t.Cleanup(mm.Close)
	mm.SetMocksWithWindowThreeTier([]*models.Mock{owned}, pool, []*models.Mock{conn}, models.BaseTime, time.Now())
	if s := mm.startupTree(); s == nil || s.size() < n+1 {
		t.Fatalf("precondition: the startup tier does not hold the pool and the connection mock")
	}
	// A session update that brings a kind into the startup tier (the
	// initial staging filed the pool in both: mirrored there).
	mysql := *pool[0]
	mysql.Kind = models.MySQL
	if !mm.UpdateUnFilteredMock(pool[0], &mysql) {
		t.Fatalf("precondition: the session update did not land")
	}
	found := false
	mm.startupTree().rangeValues(func(v interface{}) bool {
		found = found || v.(*models.Mock) == &mysql
		return !found
	})
	if !found {
		t.Fatalf("precondition: the update was not mirrored into the startup tier")
	}
	walks := watchWalks(t, mm.startupTree())
	mm.ServeExactPool("k", nil)
	for i := range 5 {
		before := map[models.Kind]uint64{}
		for _, k := range []models.Kind{models.Mongo, models.Postgres, models.MySQL} {
			before[k] = mm.RevisionByKind(k)
		}
		mm.ServeExactTest(fmt.Sprint("T", i), []*models.Mock{owned})
		mm.ServeExactPool("k", nil)
		for k, was := range before {
			if got := mm.RevisionByKind(k); got < was+2 {
				t.Errorf("test %d: its open and close moved %s's revision %d -> %d, want two moves", i, k, was, got)
			}
		}
	}
	if got := walks.Load(); got != 0 {
		t.Errorf("exact serving's start and five tests' opens and closes walked the startup tier (%d mocks) %d times, want 0", n+1, got)
	}
}
