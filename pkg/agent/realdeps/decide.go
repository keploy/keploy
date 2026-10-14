package realdeps

import (
	"net/netip"
	"sort"
	"strings"
	"sync"
)

// Book is what the run's name lookups said: for each address, the names
// keploy's DNS answered with it, and for each name entry, the addresses it
// resolves to (keploy's DNS answers, and the system resolver's when the run's
// list was given). A connection's name is read from it, never guessed.
//
// One book serves a run: the agent's single DNS listener answers every
// process of it (release1-cut §5.1 item 7), so "the one name keploy's DNS
// gave the run for the address" is what this book holds. The zero Book is
// ready to use.
type Book struct {
	mu sync.Mutex
	// namesOf is, per address, every name keploy's DNS answered with it.
	namesOf map[netip.Addr]map[string]struct{}
	// addrsOf is, per name, every address it resolved to: from keploy's DNS
	// answers and from the system resolver.
	addrsOf map[string]map[netip.Addr]struct{}
}

// Answered notes that keploy's DNS answered a question for name with addrs.
// The question's name is what is noted, never a CNAME target: the app asked
// for that name.
func (b *Book) Answered(name string, addrs []netip.Addr) {
	h, ok := canonicalName(name)
	if !ok || len(addrs) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.namesOf == nil {
		b.namesOf = map[netip.Addr]map[string]struct{}{}
	}
	for _, a := range addrs {
		a = a.Unmap()
		if b.namesOf[a] == nil {
			b.namesOf[a] = map[string]struct{}{}
		}
		b.namesOf[a][h] = struct{}{}
	}
	b.addLocked(h, addrs)
}

// Resolved notes that the system resolver resolved name to addrs. It makes
// the addresses candidates of the name, but names none of them: a lookup the
// app made through the hosts file or its own resolver is not one keploy saw.
func (b *Book) Resolved(name string, addrs []netip.Addr) {
	h, ok := canonicalName(name)
	if !ok || len(addrs) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.addLocked(h, addrs)
}

func (b *Book) addLocked(h string, addrs []netip.Addr) {
	if b.addrsOf == nil {
		b.addrsOf = map[string]map[netip.Addr]struct{}{}
	}
	if b.addrsOf[h] == nil {
		b.addrsOf[h] = map[netip.Addr]struct{}{}
	}
	for _, a := range addrs {
		b.addrsOf[h][a.Unmap()] = struct{}{}
	}
}

// namesFor are the names keploy's DNS answered with a, sorted.
func (b *Book) namesFor(a netip.Addr) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	set := b.namesOf[a.Unmap()]
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// resolvesTo reports whether name resolved to a.
func (b *Book) resolvesTo(name string, a netip.Addr) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.addrsOf[name][a.Unmap()]
	return ok
}

// Verdict is what keploy does with a connection.
type Verdict uint8

const (
	// Dependency: recorded and mocked, as without a list.
	Dependency Verdict = iota
	// Real: dialed and relayed, every byte, to the real destination.
	Real
	// NeedName: a name entry's address that keploy's DNS did not name
	// exactly once: the name is read from the client's first bytes.
	NeedName
)

// Decision is what keploy decided for a connection.
type Decision struct {
	Verdict Verdict
	// Label is a REAL connection's destination, as REAL rows write it.
	Label string
	// Check, for a connection made REAL by its name, is what its client's
	// bytes are checked against as they pass: each name they carry must be
	// listed. nil for one made REAL by its address or container.
	Check *Check
	// Candidates, for NeedName, are the name entries whose names resolve to
	// the address: what keploy tells the user when it cannot read the name.
	Candidates []string
}

// Check is what the bytes of a connection made REAL by its name are checked
// against: a name they carry (an SNI, a request's Host, a CONNECT target)
// must be listed, so one connection carries one REAL name (design §4).
type Check struct {
	list *List
	port uint16
}

// Allows reports whether host, carried at the connection's port (port 0) or
// at port (a CONNECT target), is listed. A host that is not a DNS name (an
// IP literal Host) names nothing, and is not listed.
func (c *Check) Allows(host string, port uint16) bool {
	if port == 0 {
		port = c.port
	}
	_, ok := c.list.named(host, port)
	return ok
}

// Decider decides each connection of a run from its list and the run's book.
type Decider struct {
	List *List
	Book *Book
}

// AtAccept decides a connection to dst, accepted now, before any byte of it
// is read. repo is the image repository of the container dst reaches, ""
// when dst is not a container.
//
// An address entry, or a container entry, makes it REAL at once. A name
// entry whose name resolves to dst's address, at dst's port, makes it a
// candidate: when keploy's DNS answered exactly one name with the address,
// that name decides now; otherwise the name is read from the client's first
// bytes (NeedName). Every other connection is a dependency.
func (d Decider) AtAccept(dst netip.AddrPort, repo string) Decision {
	if label, ok := d.List.address(dst); ok {
		return Decision{Verdict: Real, Label: label}
	}
	if label, ok := d.List.container(repo); ok {
		return Decision{Verdict: Real, Label: label}
	}
	var candidates []string
	for _, h := range d.List.hostsOn(dst.Port()) {
		if d.Book != nil && d.Book.resolvesTo(h, dst.Addr()) {
			candidates = append(candidates, h)
		}
	}
	if len(candidates) == 0 {
		return Decision{Verdict: Dependency}
	}
	if d.Book != nil {
		if names := d.Book.namesFor(dst.Addr()); len(names) == 1 {
			label, ok := d.List.named(names[0], dst.Port())
			if !ok {
				return Decision{Verdict: Dependency}
			}
			return Decision{Verdict: Real, Label: label, Check: &Check{list: d.List, port: dst.Port()}}
		}
	}
	return Decision{Verdict: NeedName, Candidates: candidates}
}

// ByName decides a NeedName connection to port by the name its client's
// first bytes carried.
func (d Decider) ByName(n Name, port uint16) Decision {
	p := port
	if n.Port != 0 {
		p = n.Port
	}
	label, ok := d.List.named(n.Host, p)
	if !ok {
		return Decision{Verdict: Dependency}
	}
	return Decision{Verdict: Real, Label: label, Check: &Check{list: d.List, port: port}}
}

// UnreadableWhy is the capture_incomplete text for a connection to dst whose
// name keploy could not read, while candidates resolve to its address.
func UnreadableWhy(dst string, candidates []string, what string) string {
	return "keploy could not read which name a connection to " + dst + " was opened for (" + what + "), and mock.real's " +
		strings.Join(candidates, ", ") + " resolve(s) to that address: list the address in mock.real, or give each name its own address"
}
