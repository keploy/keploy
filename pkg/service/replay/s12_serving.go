package replay

import "sync"

// WHO MAY TURN ON THE LOOSER BACKGROUND SERVING (the compatibility
// audit's B3 and C2).
//
// background serving is looser than today's for a set's background: the set's end
// no longer requires its worker scope answered, the pool answers inside
// tests, and a background write may be answered by its kind. Each is safe only
// because record's judging (keploy enterprise's auto-replay: the bake, the
// owner's fate, the confirming round, the unchecked lines) judges what it
// answers. So it runs for a test set only when all three hold:
//
//   - a registered func (RegisterS12Serving) returns true: keploy enterprise
//     registers it, so keploy alone, or a keploy relinked without the
//     judging, serves every set by today's rules and ships nothing looser;
//   - the proven switch is on;
//   - the set's mappings.yaml carries the mark of a proven-routing record
//     (models.RecordedWithS12Routing in models.Mapping.RecordedWith): a set
//     recorded before, or by a build without the judging, keeps today's rules
//     whatever replays it, so an upgrade relaxes no old set.

// RegisterS12Serving declares that this build judges what the looser
// background serving answers, and when: enabled is asked as each test set is staged
// (and by keploy record, before it marks and routes a set), and background
// serving runs only while it returns true, under the proven switch, for a set
// marked models.RecordedWithS12Routing. keploy enterprise registers it, from
// an init function or before its first record or replay. One registration: a
// second panics, as does a nil enabled. The returned function removes it
// (tests); it removes only the registration it returned.
func RegisterS12Serving(enabled func() bool) (unregister func()) {
	if enabled == nil {
		panic("replay.RegisterS12Serving: a nil func")
	}
	s12Serving.mu.Lock()
	defer s12Serving.mu.Unlock()
	if s12Serving.reg != nil {
		panic("replay.RegisterS12Serving: a func is registered already")
	}
	reg := &s12Registration{enabled: enabled}
	s12Serving.reg = reg
	return func() {
		s12Serving.mu.Lock()
		defer s12Serving.mu.Unlock()
		if s12Serving.reg == reg {
			s12Serving.reg = nil
		}
	}
}

// S12Serving reports whether the registered func (RegisterS12Serving) enables
// the looser background serving now: false when none is registered. It says nothing of
// the switch or of a set's mark, which the caller also needs.
func S12Serving() bool {
	s12Serving.mu.RLock()
	reg := s12Serving.reg
	s12Serving.mu.RUnlock()
	return reg != nil && reg.enabled()
}

// s12Registration is one RegisterS12Serving call's.
type s12Registration struct {
	enabled func() bool
}

var s12Serving struct {
	mu  sync.RWMutex
	reg *s12Registration
}
