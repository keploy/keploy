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
// once, for a fresh list (Store.Refresh) over the stream of asks the CLI holds
// open (Store.Serve), and waits for that list or the CLI's going away: a
// container started after the last list is found, and a name no container
// holds is answered by keploy itself. A miss joins only an ask the CLI has
// not taken yet: the CLI reads the networks after it takes an ask, so a list
// for an ask it took before the miss can predate the miss's container.
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
	asked    uint64 // asks made
	// taken is the highest ask handed to a CLI (Serve). The CLI reads the
	// networks after it takes an ask, so a list for a taken ask may predate
	// a container that joined since: a miss joins no taken ask.
	taken uint64
	// settled is the highest ask the CLI is done with: a List answered it,
	// or the CLI said it could not list the names for it (Serve's after).
	settled uint64
	// streams counts the CLI's held streams of asks (Serve) open now: a CLI
	// answers asks while one is. A Refresh waits only then, and returns as
	// soon as none is, so it waits on the CLI's list or its going away,
	// never on a clock.
	streams int
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
	if l.Answers > s.settled {
		s.settled = l.Answers
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

// Refresh asks the CLI for a fresh List, joining an ask the CLI has not
// taken yet (concurrent misses, such as glibc's A and AAAA, share one), and
// waits until the CLI is done with that ask (a List answers it, or the CLI
// could not list the names), no CLI holds a stream of asks, or ctx ends.
// With no CLI holding one it returns at once: nothing would answer.
func (s *Store) Refresh(ctx context.Context) {
	s.mu.Lock()
	if s.streams == 0 {
		s.mu.Unlock()
		return
	}
	want := s.asked
	if want <= s.settled || want <= s.taken {
		s.asked++
		want = s.asked
	}
	s.waiting++
	s.signal()
	defer func() {
		s.waiting--
		s.mu.Unlock()
	}()
	for s.settled < want && s.streams > 0 {
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

// Serve is the CLI's held stream of asks: it hands send each of the agent's
// asks after after, in order, until ctx ends (the CLI went away) or send
// fails. A CLI holding one is present from Serve's start to its end, the
// time it lists the names for an ask included, so a Refresh made meanwhile
// waits for a List: that of a fresh ask, handed on once the CLI is done with
// the one it holds. The CLI opens a stream with after set to the last ask
// it is done with: one it could not list the names for ends its stream, and
// the next stream it opens settles that ask, so a Refresh waiting for it
// returns.
func (s *Store) Serve(ctx context.Context, after uint64, send func(Ask) error) {
	s.mu.Lock()
	if after > s.asked {
		// The CLI counted the asks of an agent process this one replaced.
		after = s.asked
	}
	if after > s.settled {
		s.settled = after
	}
	s.streams++
	s.signal()
	defer func() {
		s.streams--
		s.signal()
		s.mu.Unlock()
	}()
	for {
		for s.asked <= after {
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
		ask := Ask{Asked: s.asked}
		if ask.Asked > s.taken {
			s.taken = ask.Asked
		}
		s.mu.Unlock()
		err := send(ask)
		s.mu.Lock()
		if err != nil || ctx.Err() != nil {
			return
		}
		after = ask.Asked
	}
}

// Present reports whether a CLI holds a stream of asks for a fresh List,
// and the channel it returns is closed at the Store's next change: together
// they let a caller wait for one to be.
func (s *Store) Present() (bool, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streams > 0, s.changed
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
