// Package netnames holds the names a docker-mode replay's own networks answer
// themselves: the names docker's embedded resolver (127.0.0.11) holds for the
// containers on the networks keploy's agent container is on (each container's
// name, its compose service name and network aliases, its short ID), and each
// of them qualified by the network's name.
//
// A replay's DNS mock miss is forwarded to docker's resolver only for one of
// these names. That resolver answers the names it holds and recurses every
// other one, single-label names included, to the host's or public resolvers,
// so forwarding any other name would send it off the machine.
//
// keploy's agent container has no Docker socket, so the keploy CLI, which
// drives Docker for the run, lists the names (List) and gives them to the
// agent (Store.Apply). A miss for a name that is not on the list asks the CLI,
// once, for a fresh list (Store.Refresh): a container started after the last
// list is found, and a name no container holds is answered by keploy itself.
package netnames

import (
	"context"
	"strings"
	"sync"
)

// List is the keploy CLI's list of the names the replay's networks answer.
type List struct {
	// Answers is the refresh ask (Ask.Asked) this list was made for, 0 for
	// a list given at a session's start.
	Answers uint64 `json:"answers"`
	// Networks are the names of the networks the names were read from.
	Networks []string `json:"networks"`
	// Names are the names docker's resolver answers on those networks.
	Names []string `json:"names"`
}

// Ask is the agent's ask for a fresh List, the Asked'th since it started.
type Ask struct {
	Asked uint64 `json:"asked"`
}

// Store is an agent's last List and its refresh asks.
type Store struct {
	mu       sync.Mutex
	names    map[string]struct{}
	networks map[string]struct{}
	given    bool   // a List was applied
	answered uint64 // the highest ask a List answered
	asked    uint64 // asks made
	// serving counts the CLI's held asks (AwaitAsk) open now. present is
	// whether a CLI answers asks: set when one holds an ask, kept while it
	// lists the names for one it took, cleared when its last held ask ends
	// with the CLI gone. A Refresh waits only while one is present.
	serving int
	present bool
	waiting int           // Refresh calls waiting for a list now
	changed chan struct{} // closed, and replaced, on every change
}

// New is an empty Store.
func New() *Store {
	return &Store{changed: make(chan struct{})}
}

var std = New()

// Default is the agent process's Store, the one its routes fill.
func Default() *Store { return std }

// Canon is the form a name is held and looked up in: lower case, without the
// root's trailing dot.
func Canon(name string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
}

// Apply takes l in place of the last List.
func (s *Store) Apply(l List) {
	names := make(map[string]struct{}, len(l.Names))
	for _, n := range l.Names {
		if c := Canon(n); c != "" {
			names[c] = struct{}{}
		}
	}
	networks := make(map[string]struct{}, len(l.Networks))
	for _, n := range l.Networks {
		if c := Canon(n); c != "" {
			networks[c] = struct{}{}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.names, s.networks, s.given = names, networks, true
	if l.Answers > s.answered {
		s.answered = l.Answers
	}
	s.signal()
}

// Owns reports whether name is on the last List.
func (s *Store) Owns(name string) bool {
	c := Canon(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.names[c]
	return ok
}

// MayOwn reports whether a fresh List could hold name: a single-label name
// (a container or alias started after the last List), or one qualified by
// one of the listed networks. No other name is one docker's resolver holds,
// so a miss for it asks for no List.
func (s *Store) MayOwn(name string) bool {
	c := Canon(name)
	if c == "" {
		return false
	}
	i := strings.IndexByte(c, '.')
	if i < 0 {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.networks[c[i+1:]]
	return ok
}

// Refresh asks the CLI for a fresh List, joining an ask not answered yet, and
// waits until a List answers it, no CLI holds an ask that could, or ctx ends.
// With no CLI holding an ask it returns at once: nothing would answer.
func (s *Store) Refresh(ctx context.Context) {
	s.mu.Lock()
	if !s.present {
		s.mu.Unlock()
		return
	}
	want := s.asked
	if want <= s.answered {
		s.asked++
		want = s.asked
		s.signal()
	}
	s.waiting++
	s.signal()
	defer func() {
		s.waiting--
		s.mu.Unlock()
	}()
	for s.answered < want && s.present {
		ch := s.changed
		s.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			s.mu.Lock()
			return
		}
		s.mu.Lock()
	}
}

// AwaitAsk is the CLI's held ask: it returns the agent's next ask after
// after, once there is one, and false when ctx ends first (the CLI went away).
func (s *Store) AwaitAsk(ctx context.Context, after uint64) (Ask, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.serving++
	s.present = true
	s.signal()
	if after > s.asked {
		// The CLI counted the asks of an agent process this one replaced.
		after = s.asked
	}
	for s.asked <= after {
		ch := s.changed
		s.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			s.mu.Lock()
			s.serving--
			if s.serving == 0 {
				s.present = false
			}
			s.signal()
			return Ask{}, false
		}
		s.mu.Lock()
	}
	// The CLI lists the names for this ask and holds the next one: it stays
	// present meanwhile, so a Refresh made now waits for its list.
	s.serving--
	return Ask{Asked: s.asked}, true
}

// Present reports whether a CLI answers asks for a fresh List, and the
// channel it returns is closed at the Store's next change: together they let a caller wait for
// one to be.
func (s *Store) Present() (bool, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.present, s.changed
}

// Given reports whether a List was applied.
func (s *Store) Given() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.given
}

// signal wakes every waiter; s.mu is held.
func (s *Store) signal() {
	close(s.changed)
	s.changed = make(chan struct{})
}
