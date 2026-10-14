// Package realdeps decides which of a run's connections reach a dependency
// the user declared real (keploy.yml mock.real, design v14 §4), and relays
// those to the real thing: never recorded, never mocked.
//
// A list holds three kinds of entry:
//
//	mock:
//	  real:
//	    - localhost:5432        # an address entry: localhost is 127.0.0.1 and ::1
//	    - 10.0.4.7:6379         # an address entry: an IP literal
//	    - auth.example.com:8443    # a name entry
//	    - container:redis       # every container of this image repository
//
// An address entry matches a connection to that address and port. A name
// entry matches a connection to that port by the name it was opened for: the
// one name keploy's DNS gave the run for the address, else the name the
// client's first bytes carry (a TLS ClientHello's SNI, an HTTP/1 request's
// Host, a CONNECT target), read before anything else happens to the
// connection and with no clock deciding anything. A connection to an address
// a name entry resolves to whose name keploy cannot read is
// capture_incomplete, never guessed. Every other connection is a dependency,
// recorded and mocked as before.
//
// Everything here runs only under the proven switch: without it the agent
// never receives a list.
package realdeps

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/models"
)

// containerPrefix starts a container entry.
const containerPrefix = "container:"

// kind is what an entry matches by.
type kind uint8

const (
	kindAddress kind = iota + 1
	kindName
	kindContainer
)

// Entry is one parsed mock.real entry.
type Entry struct {
	// Label is the entry as keploy writes the destination: in REAL rows, in
	// a recording's REAL list, and in rule 4's "never reached". It is the
	// entry normalised: a name lowercased, an IP literal in its canonical
	// form.
	Label string
	kind  kind
	// addrs are an address entry's addresses: the literal, or 127.0.0.1 and
	// ::1 for localhost.
	addrs []netip.Addr
	// host is a name entry's name, lowercased, without a trailing dot.
	host string
	port uint16
	// repo is a container entry's image repository.
	repo string
}

// List is a parsed mock.real. The zero List, and a nil one, list nothing.
type List struct {
	entries []Entry
	// byAddr maps an address entry's address and port to its label.
	byAddr map[netip.AddrPort]string
	// byName maps a name entry's name, then port, to its label.
	byName map[string]map[uint16]string
	// byPort maps a port to the names listed on it, sorted.
	byPort map[uint16][]string
	// byRepo maps a container entry's repository to its label.
	byRepo map[string]string
}

// ErrBadEntry is what Parse returns for an entry it cannot read.
var ErrBadEntry = errors.New("not a mock.real entry")

// Parse reads mock.real. An entry it cannot read is an error that names it
// and says what an entry looks like; the same destination listed twice is
// one entry.
func Parse(raw []string) (*List, error) {
	l := &List{
		byAddr: map[netip.AddrPort]string{},
		byName: map[string]map[uint16]string{},
		byPort: map[uint16][]string{},
		byRepo: map[string]string{},
	}
	seen := map[string]bool{}
	for _, s := range raw {
		e, err := parseEntry(s)
		if err != nil {
			return nil, err
		}
		if seen[e.Label] {
			continue
		}
		seen[e.Label] = true
		l.entries = append(l.entries, e)
		switch e.kind {
		case kindAddress:
			for _, a := range e.addrs {
				l.byAddr[netip.AddrPortFrom(a, e.port)] = e.Label
			}
		case kindName:
			if l.byName[e.host] == nil {
				l.byName[e.host] = map[uint16]string{}
			}
			l.byName[e.host][e.port] = e.Label
			l.byPort[e.port] = append(l.byPort[e.port], e.host)
		case kindContainer:
			l.byRepo[e.repo] = e.Label
		}
	}
	for _, hosts := range l.byPort {
		sort.Strings(hosts)
	}
	return l, nil
}

// entryHelp is what every refusal of an entry says an entry looks like.
const entryHelp = `an entry is an address and port (localhost:5432, 10.0.4.7:6379, [::1]:5432), a name and port (auth.example.com:8443), or container:<image repository> (container:redis)`

func badEntry(s, why string) error {
	return fmt.Errorf("%w: %q %s; %s", ErrBadEntry, s, why, entryHelp)
}

func parseEntry(raw string) (Entry, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Entry{}, badEntry(raw, "is empty")
	}
	if repo, ok := strings.CutPrefix(s, containerPrefix); ok {
		return parseContainer(raw, repo)
	}
	host, portText, err := net.SplitHostPort(s)
	if err != nil {
		return Entry{}, badEntry(raw, "has no port")
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 || strconv.FormatUint(port, 10) != portText {
		return Entry{}, badEntry(raw, "has no port from 1 to 65535")
	}
	p := uint16(port)
	if strings.EqualFold(host, "localhost") {
		// Defined, not looked up: what /etc/hosts says of localhost does
		// not change what the entry matches.
		return Entry{Label: net.JoinHostPort("localhost", portText), kind: kindAddress,
			addrs: []netip.Addr{netip.MustParseAddr("127.0.0.1"), netip.IPv6Loopback()}, port: p}, nil
	}
	if a, err := netip.ParseAddr(host); err == nil {
		if a.Zone() != "" {
			return Entry{}, badEntry(raw, "names an address with a zone")
		}
		a = a.Unmap()
		return Entry{Label: net.JoinHostPort(a.String(), portText), kind: kindAddress, addrs: []netip.Addr{a}, port: p}, nil
	}
	name, ok := canonicalName(host)
	if !ok {
		return Entry{}, badEntry(raw, "names neither an IP address nor a DNS name")
	}
	return Entry{Label: net.JoinHostPort(name, portText), kind: kindName, host: name, port: p}, nil
}

func parseContainer(raw, repo string) (Entry, error) {
	if repo == "" {
		return Entry{}, badEntry(raw, "names no image repository")
	}
	if strings.ContainsAny(repo, " \t#@") || strings.Contains(repo, "::") {
		return Entry{}, badEntry(raw, "is not an image repository")
	}
	// A tag or a digest would keep one version of an image live and mock
	// another, which an entry cannot say: it names the repository, and
	// every container of it, at any port.
	if i := strings.LastIndexByte(repo, ':'); i > strings.LastIndexByte(repo, '/') {
		return Entry{}, badEntry(raw, "names a tag or a port; a container entry names the image repository only, and covers every tag and port of it")
	}
	// Docker's short form, as container keys name an image's repository:
	// docker.io/library/redis is redis.
	repo = containerkey.Repository(strings.ToLower(repo))
	return Entry{Label: containerPrefix + repo, kind: kindContainer, repo: repo}, nil
}

// canonicalName is host as a name entry keeps it: lowercased, without a
// trailing dot. ok is false for a string that is not a DNS name: one made of
// labels of letters, digits, '-' and '_', each 1 to 63 characters, 253 at
// most in all. A wildcard is not a name.
func canonicalName(host string) (string, bool) {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "" || len(h) > 253 {
		return "", false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return "", false
			}
		}
	}
	return h, true
}

// CanonicalName is a name as keploy compares it to a name entry: lowercased,
// without a trailing dot, or "" when it is not a DNS name (an IP literal, an
// empty SNI).
func CanonicalName(host string) string {
	h, ok := canonicalName(host)
	if !ok {
		return ""
	}
	if _, err := netip.ParseAddr(h); err == nil {
		return ""
	}
	return h
}

// Empty reports whether l lists nothing.
func (l *List) Empty() bool { return l == nil || len(l.entries) == 0 }

// Labels are l's entries as keploy writes them, in the list's order.
func (l *List) Labels() []string {
	if l == nil {
		return nil
	}
	out := make([]string, len(l.entries))
	for i, e := range l.entries {
		out[i] = e.Label
	}
	return out
}

// Names are the names l's name entries list, sorted, each once.
func (l *List) Names() []string {
	if l == nil {
		return nil
	}
	out := make([]string, 0, len(l.byName))
	for h := range l.byName {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// HasContainers reports whether l has a container entry.
func (l *List) HasContainers() bool { return l != nil && len(l.byRepo) > 0 }

// IsName reports whether name (a DNS question's, with or without its
// trailing dot) is a name entry's name, at any port: its DNS queries are
// answered live, and never recorded or mocked.
func (l *List) IsName(name string) bool {
	if l == nil {
		return false
	}
	h, ok := canonicalName(name)
	return ok && l.byName[h] != nil
}

// address is the label of the address entry dst matches.
func (l *List) address(dst netip.AddrPort) (string, bool) {
	if l == nil {
		return "", false
	}
	label, ok := l.byAddr[netip.AddrPortFrom(dst.Addr().Unmap(), dst.Port())]
	return label, ok
}

// named is the label of the name entry for host at port.
func (l *List) named(host string, port uint16) (string, bool) {
	if l == nil {
		return "", false
	}
	h, ok := canonicalName(host)
	if !ok {
		return "", false
	}
	label, ok := l.byName[h][port]
	return label, ok
}

// covers is the label of the address or name entry that host at port is:
// host an IP literal (an address entry), localhost (an address entry, as
// parseEntry reads one), or a name (a name entry).
func (l *List) covers(host string, port uint16) (string, bool) {
	if l == nil || port == 0 {
		return "", false
	}
	h := strings.Trim(host, "[]")
	if a, err := netip.ParseAddr(h); err == nil {
		return l.address(netip.AddrPortFrom(a, port))
	}
	if strings.EqualFold(strings.TrimSuffix(h, "."), "localhost") {
		// Which loopback localhost reached is not known: it is listed only
		// when both are, as a localhost entry lists them.
		v4, ok4 := l.address(netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), port))
		_, ok6 := l.address(netip.AddrPortFrom(netip.IPv6Loopback(), port))
		if !ok4 || !ok6 {
			return "", false
		}
		return v4, true
	}
	return l.named(h, port)
}

// Recorded is the entry a recorded mock's destination is: the one the
// accept decided the connection the mock was recorded on by, read from what
// the recording kept of that connection:
//
//   - its container key (containerkey.MetadataKey): a container entry of
//     the key's image repository;
//   - metadata.destAddr, the address and port the app connected to (the
//     agent stamps it on each mock of keploy mock record): an address entry
//     of that address, and the port a name entry is read at;
//   - the name an HTTP mock's request carries (models.Mock.
//     RecordedDestination: its Host, else its URL's authority): a name entry
//     of that name, at destAddr's port, else at the port the name is written
//     with.
//
// A recording that keeps no port for its call decides nothing (ok false),
// and its test still owns the mock: a port is never taken from a scheme. A
// Host with no port is written alike for http's 80 and https's 443, and the
// recorder writes an origin-form URL ("/token"), with no scheme at all.
func (l *List) Recorded(m *models.Mock) (string, bool) {
	if l.Empty() || m == nil {
		return "", false
	}
	if key, ok := containerkey.Recorded(m); ok {
		if k, err := containerkey.Parse(key); err == nil {
			if label, ok := l.container(k.Repository()); ok {
				return label, true
			}
		}
	}
	host, port, connected := splitPort(m.Spec.Metadata["destAddr"])
	if connected {
		if label, ok := l.covers(host, port); ok {
			return label, true
		}
	}
	dest, ok := m.RecordedDestination()
	if !ok {
		return "", false
	}
	name, written, ok := splitPort(dest)
	if !ok {
		name, written = strings.Trim(dest, "[]"), 0
	}
	if connected {
		// The connection decides the address and the port; the request
		// adds only a name. An IP literal Host names nothing (Check.Allows).
		if CanonicalName(name) == "" {
			return "", false
		}
		return l.named(name, port)
	}
	return l.covers(name, written)
}

// splitPort splits "host:port" into its host and a port from 1 to 65535.
func splitPort(hostport string) (string, uint16, bool) {
	h, p, err := net.SplitHostPort(strings.TrimSpace(hostport))
	if err != nil {
		return "", 0, false
	}
	n, err := strconv.ParseUint(p, 10, 16)
	if err != nil || n == 0 {
		return "", 0, false
	}
	return h, uint16(n), true
}

// hostsOn are the names listed at port.
func (l *List) hostsOn(port uint16) []string {
	if l == nil {
		return nil
	}
	return l.byPort[port]
}

// container is the label of the container entry for an image repository.
func (l *List) container(repo string) (string, bool) {
	if l == nil || repo == "" {
		return "", false
	}
	label, ok := l.byRepo[repo]
	return label, ok
}
