package learn

import "go.keploy.io/server/v3/pkg/models"

// WarmSet reads, in the background, what the rules of every call read of
// each payload of the running set (warm), for each recording src visits,
// once for the set: the agent calls it as it stages a set of record's full
// round, src visiting every recording of the set (those staged and those
// the agent keeps on disk), so the set's payloads are read before its tests'
// calls read them, off their answer path. Before, the set's first call that
// drifted read every payload recorded before it while its client waited
// (api-server's round: tens of seconds on one core, past the client's 30 s).
//
// It waits on no clock. A call reads what it needs of the set as it did:
// each recording neither it nor the staging has taken it reads itself, and
// it waits only for those of its own recordings another is reading (the
// staging reads one at a time), never for the staging to end. The staging
// takes no recording a call took. It stops at the set's NewSet (the set
// keeps nothing more) or where the set's budget is spent (setBudget: the
// set reads the rest once per test, as before), whichever comes first, and
// the channel it returns gives then what the set it staged read and kept
// (SetUse), and is closed. A nil src reads nothing.
func (t *Tracker) WarmSet(src func(visit func(*models.Mock) bool) error) <-chan SetUse {
	done := make(chan SetUse, 1)
	if t == nil || src == nil {
		close(done)
		return done
	}
	t.mu.Lock()
	// A cache of its own, on the set's (newDrifts): what it reads past the
	// set's budget it keeps for none of the set's tests, and lets go of as
	// it ends.
	dc := t.newDrifts()
	t.mu.Unlock()
	go func() {
		defer close(done)
		_ = dc.warmSet(src)
		done <- dc.set.use()
	}()
	return done
}

// warmSet is WarmSet's read, on dc's set (setOf): each recording src
// visits is read (warmOne) as the payload it recorded (a lazy Raw of it
// unserved: LazyRawOf, its answer the agent parked on disk read back first,
// withAnswer). What is derived of a payload depends on its text alone, the
// recording's, however a test was served it (setDerived), so the calls'
// reads of a recording, served or not, find the staging's.
func (dc *DriftCache) warmSet(src func(visit func(*models.Mock) bool) error) error {
	if dc == nil || dc.set == nil {
		return nil
	}
	s := dc.set
	return src(func(m *models.Mock) bool {
		if m == nil {
			return true
		}
		x := dc.LazyRawOf(m, false, withAnswer(m))
		ch, mine := s.take(x)
		if ch == nil {
			return false
		}
		if mine {
			dc.warmOne(s, x, ch, true)
		}
		return true
	})
}

// withAnswer loads m with its recorded answer, one the agent parked on disk
// read back (models.Mock.HydrateResponse) into a copy, so m stays parked.
func withAnswer(m *models.Mock) func() (*models.Mock, error) {
	return func() (*models.Mock, error) {
		if !m.HasSpilledResponse() {
			return m, nil
		}
		c := m.ShallowCopy()
		if err := c.HydrateResponse(); err != nil {
			return nil, err
		}
		return c, nil
	}
}

// SetUse is what the running set's learn step has read and kept of its
// payloads (setDerived), for the agent's log: how many its staging read
// (WarmSet) and its calls read for the set, the bytes each tier keeps, the
// set's budget (setBudget), and whether the tiers that let none go (held,
// forms) were full, past which the set's payloads are read once per test.
type SetUse struct {
	Staged, Called             int
	Held, Words, Forms, Budget int64
	HeldFull, FormsFull        bool
}

func (s *setDerived) use() SetUse {
	if s == nil {
		return SetUse{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return SetUse{Staged: s.staged, Called: s.called, Held: s.held.used, Words: s.words.used, Forms: s.forms.used,
		Budget: s.budget.limit, HeldFull: s.held.full, FormsFull: s.forms.full}
}
