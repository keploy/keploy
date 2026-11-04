package mock

import (
	"sync"

	"go.keploy.io/server/v3/pkg/models"
)

// WHO NAMES A TEST'S REPEATED READS.
//
// Exact serving answers a test's repeated read once more than recorded, and
// does not charge one of its recordings left unmade, only when the replay
// names it (models.ScopeTableReq.RepeatReads) and its recordings prove it one
// read with one answer (httpint.RepeatReadOf). Which reads a replay names is
// the namer's to decide (RegisterRepeatReadNamer): keploy enterprise
// registers one, which also says each read in the test's keploy.lock entry
// and leaves out the endpoints a keploy.yml lists as never repeated. keploy
// alone registers none: it names no read, and serves every test its exact
// count.

// RepeatReadNamer names the repeated reads of each test of mock set set that
// a replay serves under the proven switch. owned is, by test ID, the test's
// own recordings the agent holds, in the order its mapping lists them. It
// returns each test's reads by test ID; a read naming a recording owned does
// not hold for its test is not sent.
type RepeatReadNamer func(set string, owned map[string][]*models.Mock) map[string][]models.RepeatRead

var repeatReadNamer struct {
	mu  sync.RWMutex
	reg *RepeatReadNamer
}

// RegisterRepeatReadNamer makes n name each replayed test's repeated reads. One
// registration: a second panics, as does a nil n. The returned function
// removes it (tests); it removes only the registration it returned.
func RegisterRepeatReadNamer(n RepeatReadNamer) (unregister func()) {
	if n == nil {
		panic("mock.RegisterRepeatReadNamer: a nil namer")
	}
	repeatReadNamer.mu.Lock()
	defer repeatReadNamer.mu.Unlock()
	if repeatReadNamer.reg != nil {
		panic("mock.RegisterRepeatReadNamer: a namer is registered already")
	}
	reg := &n
	repeatReadNamer.reg = reg
	return func() {
		repeatReadNamer.mu.Lock()
		defer repeatReadNamer.mu.Unlock()
		if repeatReadNamer.reg == reg {
			repeatReadNamer.reg = nil
		}
	}
}

// namedRepeatReads is the repeated reads the registered namer names for set's
// tests (table: each test's own recordings the agent holds, by name), each
// keeping only reads of at least two of its test's own recordings; nil when
// no namer is registered or it names none.
func namedRepeatReads(set string, table map[string][]string, loaded []*models.Mock) map[string][]models.RepeatRead {
	repeatReadNamer.mu.RLock()
	reg := repeatReadNamer.reg
	repeatReadNamer.mu.RUnlock()
	if reg == nil {
		return nil
	}
	byName := make(map[string]*models.Mock, len(loaded))
	for _, mk := range loaded {
		if mk != nil && mk.Name != "" {
			byName[mk.Name] = mk
		}
	}
	owned := make(map[string][]*models.Mock, len(table))
	for test, names := range table {
		for _, n := range names {
			if mk, ok := byName[n]; ok {
				owned[test] = append(owned[test], mk)
			}
		}
	}
	var out map[string][]models.RepeatRead
	for test, reads := range (*reg)(set, owned) {
		own := make(map[string]bool, len(owned[test]))
		for _, mk := range owned[test] {
			own[mk.Name] = true
		}
		for _, r := range reads {
			if r.Name == "" || len(r.Mocks) < 2 || !allOwn(r.Mocks, own) {
				continue
			}
			if out == nil {
				out = map[string][]models.RepeatRead{}
			}
			out[test] = append(out[test], models.RepeatRead{Name: r.Name, Mocks: append([]string(nil), r.Mocks...)})
		}
	}
	return out
}

// allOwn reports whether every name is one own holds.
func allOwn(names []string, own map[string]bool) bool {
	for _, n := range names {
		if !own[n] {
			return false
		}
	}
	return true
}
