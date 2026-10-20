// Package identity rewrites a replay pod's own identity to the recorded pod's.
//
// A replay runs the app in a copy pod under a new name, uid, IP, node and
// hostname. An app that puts its own identity into an outgoing call (a self
// lookup on the Kubernetes API, a client id, a registration, a node-local agent
// reached at the host IP) sends a call that was never recorded, and the mock
// is missed. The matcher cannot know which byte of a request is "the pod's
// name", but it can know both names: the recorded one travels with the test
// set (models.PodIdentity, OutgoingOptions.RecordedPods), and the agent reads
// its own from the downward API (Live). A Rewriter replaces every whole-token
// occurrence of a live value with the recorded one, so the live request is
// compared as the recorded pod would have sent it; its Reverse puts the live
// value back into a served response, for an app that compares the answer with
// itself.
//
// WHOLE TOKENS, per kind of value. A name is replaced only where it stands as
// a DNS label (not inside api-rpl-1a2b3c-extra, but before a '.', as in
// pod.svc.ns), an IP only where it is not part of a longer dotted number
// (10.0.1.5 is left alone inside 10.0.1.50 and 110.0.1.5), a UID only where
// no hex or '-' continues it. The forms an identity takes elsewhere are
// covered too: an IP dashed as in a pod's DNS A record (10-244-1-17), a UID
// with underscores as in a cgroup path, hex compared without case.
//
// WHAT IS NEVER ALIASED. A live value equal to the recorded one (same node,
// a hostname the workload pins), an empty one, and the addresses that mean
// "here" in any pod (0.0.0.0, 127.0.0.1, ::1, localhost): a bind address is
// not an identity, and rewriting it would turn every loopback call into a
// call to the recorded pod's address.
package identity

import (
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
)

// The downward-API env the keploy sidecar is given for its own identity
// (k8s-proxy's PodIdentityEnv). Outside Kubernetes the hostname stands in for
// the name, as it does inside: the kubelet names the pod's UTS namespace after
// the pod.
const (
	PodNameEnv = "KEPLOY_POD_NAME"
	PodUIDEnv  = "KEPLOY_POD_UID"
	PodIPEnv   = "KEPLOY_POD_IP"
	NodeEnv    = "KEPLOY_NODE_NAME"
	HostIPEnv  = "KEPLOY_HOST_IP"
)

// Live is the identity of the pod this process runs in, from the downward-API
// env, with os.Hostname for the name and hostname when the env has none.
func Live() models.PodIdentity {
	p := models.PodIdentity{
		Name:   os.Getenv(PodNameEnv),
		UID:    os.Getenv(PodUIDEnv),
		IP:     os.Getenv(PodIPEnv),
		Node:   os.Getenv(NodeEnv),
		HostIP: os.Getenv(HostIPEnv),
	}
	if host, err := os.Hostname(); err == nil {
		p.Hostname = host
		if p.Name == "" {
			p.Name = host
		}
	}
	return p
}

// A kind decides which bytes may sit next to a value for it to still be that
// value, in the form the rule looks for.
type kind int

const (
	kindName       kind = iota // pod name, hostname, node: a DNS label or name
	kindIPv4                   // dotted IPv4
	kindIPv4Dashed             // IPv4 as a pod's DNS A record spells it: 10-244-1-17
	kindIPv6                   // IPv6, any text form
	kindUID                    // a UUID
	kindUIDCgroup              // a UUID as a cgroup path spells it: hyphens as underscores
)

// boundary reports whether the value found at hay[start:end] stands whole:
// what precedes and follows it is not more of the same kind of token. A
// percent-escape (%3D, %2F) next to it counts as a boundary: the value is
// then inside a URL-encoded field, which is where a query carries it.
func (k kind) boundary(hay string, start, end int) bool {
	return k.startsAt(hay, start) && k.endsAt(hay, end)
}

func (k kind) startsAt(hay string, start int) bool {
	if start == 0 || escapeEndsAt(hay, start) {
		return true
	}
	c := hay[start-1]
	switch k {
	case kindIPv4:
		// 110.244.2.4 and 1.10.244.2.4 continue a number; "ip=" does not.
		return !(c >= '0' && c <= '9') && !(c == '.' && start >= 2 && hay[start-2] >= '0' && hay[start-2] <= '9')
	case kindIPv4Dashed:
		return !(c >= '0' && c <= '9') && c != '-'
	case kindIPv6:
		return !isHex(c) && c != ':' && c != '.'
	case kindUID, kindUIDCgroup:
		// A cgroup names the pod's slice pod<uid>; that prefix is a boundary.
		if start >= 3 && strings.EqualFold(hay[start-3:start], "pod") {
			return true
		}
		return !isHex(c) && c != '-' && c != '_'
	default:
		return !isNameByte(c)
	}
}

func (k kind) endsAt(hay string, end int) bool {
	if end == len(hay) || hay[end] == '%' {
		return true
	}
	c := hay[end]
	switch k {
	case kindIPv4:
		// "10.244.2.4.5" continues a number; "10.244.2.4." ends a sentence.
		return !(c >= '0' && c <= '9') && !(c == '.' && end+1 < len(hay) && hay[end+1] >= '0' && hay[end+1] <= '9')
	case kindIPv4Dashed:
		return !(c >= '0' && c <= '9') && c != '-'
	case kindIPv6:
		return !isHex(c) && c != ':' && c != '.'
	case kindUID, kindUIDCgroup:
		return !isHex(c) && c != '-' && c != '_'
	default:
		return !isNameByte(c)
	}
}

// escapeEndsAt reports a percent-escape ending right before i.
func escapeEndsAt(hay string, i int) bool {
	return i >= 3 && hay[i-3] == '%' && isHex(hay[i-2]) && isHex(hay[i-1])
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// isNameByte reports a byte that continues a DNS name: a pod name has no
// '_', so one ends it (client-go's holderIdentity is "<hostname>_<uuid>").
func isNameByte(c byte) bool {
	return c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

type rule struct {
	kind kind
	from string // the value to find, lower-cased when fold is set
	to   string
	fold bool // compare without case (hex)
}

// Rewriter replaces every whole-token occurrence of its from-values.
type Rewriter struct {
	rules   []rule
	reverse *Rewriter
}

// New is the rewriter from the identity this process runs as (live) to the
// identity the test set was recorded by. nil when there is nothing to alias:
// no recorded identity, or none of its values differs from the live one.
//
// With several recorded identities (a set that holds more than one
// recorder's captures), each kind aliases to the first recorded value of it:
// the live pod can be one pod at a time. A set recorded by several pods keeps
// the calls that carried another pod's identity missing, as before.
func New(recorded []models.PodIdentity, live models.PodIdentity) *Rewriter {
	rec, ok := first(recorded)
	if !ok {
		return nil
	}
	var fwd, back []rule
	add := func(k kind, from, to string) {
		for _, r := range pairs(k, from, to) {
			fwd = append(fwd, r)
		}
		for _, r := range pairs(k, to, from) {
			back = append(back, r)
		}
	}
	add(kindName, live.Name, rec.Name)
	add(kindName, live.Hostname, rec.Hostname)
	add(kindName, live.Node, rec.Node)
	add(kindUID, live.UID, rec.UID)
	add(kindIPv4, live.IP, rec.IP)
	add(kindIPv4, live.HostIP, rec.HostIP)
	fwd, back = dedupe(fwd), dedupe(back)
	if len(fwd) == 0 {
		return nil
	}
	r := &Rewriter{rules: fwd}
	r.reverse = &Rewriter{rules: back, reverse: r}
	return r
}

// first is the recorded identity each kind aliases to: the first recorded
// pod's, with a hostname defaulting to its name.
func first(recorded []models.PodIdentity) (models.PodIdentity, bool) {
	for _, p := range recorded {
		if p.IsZero() {
			continue
		}
		p.Name = strings.TrimSpace(p.Name)
		if strings.TrimSpace(p.Hostname) == "" {
			p.Hostname = p.Name
		}
		return p, true
	}
	return models.PodIdentity{}, false
}

// pairs is the rules that take from to to, in every form the kind is written
// in; none when the pair is not worth aliasing.
func pairs(k kind, from, to string) []rule {
	from, to = strings.TrimSpace(from), strings.TrimSpace(to)
	if from == "" || to == "" || strings.EqualFold(from, to) || isLocal(from) || isLocal(to) {
		return nil
	}
	switch k {
	case kindIPv4:
		fromIP, toIP := net.ParseIP(from), net.ParseIP(to)
		if fromIP == nil || toIP == nil {
			return nil
		}
		if fromIP.To4() != nil && toIP.To4() != nil {
			return []rule{
				{kind: kindIPv4, from: from, to: to},
				// A pod's DNS A record: 10-244-1-17.ns.pod.cluster.local.
				{kind: kindIPv4Dashed, from: strings.ReplaceAll(from, ".", "-"), to: strings.ReplaceAll(to, ".", "-")},
			}
		}
		// IPv6, in the form each side was given (an address written two ways
		// is two values; the kubelet writes one form, and so does the app
		// that reads its env).
		return []rule{{kind: kindIPv6, from: strings.ToLower(from), to: to, fold: true}}
	case kindUID:
		if len(from) < 8 || len(to) < 8 {
			return nil
		}
		return []rule{
			{kind: kindUID, from: strings.ToLower(from), to: to, fold: true},
			// The cgroup path spells a pod UID with underscores.
			{kind: kindUIDCgroup, from: strings.ToLower(strings.ReplaceAll(from, "-", "_")), to: strings.ReplaceAll(to, "-", "_"), fold: true},
		}
	default:
		if len(from) < 3 {
			// Too short to be a pod's name, and a hazard as a token.
			return nil
		}
		return []rule{{kind: kindName, from: from, to: to}}
	}
}

// isLocal reports an address that means "here" in any pod.
func isLocal(v string) bool {
	switch strings.ToLower(v) {
	case "0.0.0.0", "127.0.0.1", "::", "::1", "localhost", "[::1]", "[::]":
		return true
	}
	return false
}

// dedupe drops repeated rules (name and hostname are usually the same pair)
// and orders the longest from-value first, so a value that contains another
// is replaced whole before its part is looked for.
func dedupe(in []rule) []rule {
	seen := map[string]bool{}
	var out []rule
	for _, r := range in {
		key := r.from + "\x00" + r.to
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i].from) > len(out[j].from) })
	return out
}

// Reverse is the rewriter from the recorded identity back to the live one.
func (r *Rewriter) Reverse() *Rewriter {
	if r == nil {
		return nil
	}
	return r.reverse
}

// Len is how many values the rewriter replaces.
func (r *Rewriter) Len() int {
	if r == nil {
		return 0
	}
	return len(r.rules)
}

// String rewrites s. A nil rewriter returns it as it is.
func (r *Rewriter) String(s string) string {
	if r == nil || s == "" {
		return s
	}
	for _, rule := range r.rules {
		s = rule.apply(s)
	}
	return s
}

// Bytes rewrites b. A nil rewriter, or one that changes nothing, returns b
// itself, so a caller can tell a rewrite happened by identity of the slice.
func (r *Rewriter) Bytes(b []byte) []byte {
	if r == nil || len(b) == 0 {
		return b
	}
	s := string(b)
	out := r.String(s)
	if out == s {
		return b
	}
	return []byte(out)
}

// apply replaces every whole-token occurrence of the rule's from-value in s.
func (rule rule) apply(s string) string {
	hay := s
	if rule.fold {
		hay = strings.ToLower(s)
	}
	var b strings.Builder
	last := 0
	for i := 0; i+len(rule.from) <= len(hay); {
		j := strings.Index(hay[i:], rule.from)
		if j < 0 {
			break
		}
		start, end := i+j, i+j+len(rule.from)
		if rule.kind.boundary(hay, start, end) {
			b.WriteString(s[last:start])
			b.WriteString(rule.to)
			last = end
		}
		i = start + 1
	}
	if last == 0 {
		return s
	}
	b.WriteString(s[last:])
	return b.String()
}

var (
	liveOnce sync.Once
	liveID   models.PodIdentity
	// forCache memoizes the rewriter per recorded identity set: For runs once
	// per proxied request, and a rewriter is a handful of string rules that
	// never change for a replay session. Keyed on the recorded identities
	// alone; the live identity is read once per process.
	forCache sync.Map // string -> *Rewriter (nil for "nothing to alias")
)

// For is the rewriter a replay session's options ask for: from this process's
// own identity to the first recorded pod's (opts.Identity), or nil when the
// options carry no recorded identity, turn the aliasing off, or nothing
// differs.
func For(opts models.OutgoingOptions) *Rewriter {
	id := opts.Identity
	if id == nil || id.Disabled || len(id.RecordedPods) == 0 {
		return nil
	}
	key := fmt.Sprintf("%+v", id.RecordedPods)
	if cached, ok := forCache.Load(key); ok {
		r, _ := cached.(*Rewriter)
		return r
	}
	liveOnce.Do(func() { liveID = Live() })
	r := New(id.RecordedPods, liveID)
	forCache.Store(key, r)
	return r
}
