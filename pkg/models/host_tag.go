package models

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// A host name is traffic, as a path is: the application chose it (a
// tenant's subdomain, a customer's own domain, a name that spells who it
// calls), and keploy's log reaches log stores. So a log line names a host by
// its labels' shape, each {} ("api.example.com." is "{}.{}.{}."), beside its
// tag: LogKey of its lower-case fully qualified name under the session the
// line is logged in (LogKeySession), so the session's lines about one host
// match up, and no one without the key can tell which host a tag names. The
// console and the report name it whole. An IP address is no name: it is
// written whole, as is a port, but for the text a client may put there (an
// IPv6 zone, a port that is no number), written {} (HostShape).

// HostShape is host (a DNS name, a URL's or a Host header's host, with a
// port or a name's trailing dot) as a line writes it with no tag: each label
// {}, its port and its trailing dot kept ("api.example.com:8443" is
// "{}.{}.{}:8443"). An IP address, with its port or not, is written whole,
// but for an IPv6 address's zone, which is {} ("[fe80::1%{}]:443"). A port
// that is no number is {} too. A client's CONNECT target or Host header can
// carry any text in either place (Go checks neither, and netip takes any
// text after % for a zone). "" names "".
func HostShape(host string) string {
	name, port := splitHostPort(host)
	if name == "" || isIPHost(name) {
		out := host
		if i := strings.IndexByte(name, '%'); i >= 0 {
			// name is in host once: it holds a ':' (only an IPv6 address
			// has a zone), which a port never does.
			out = strings.Replace(host, name, name[:i+1]+pathHole, 1)
		}
		if port == "" || portNumber(port) {
			return out
		}
		return strings.TrimSuffix(out, port) + pathHole
	}
	labels := strings.Split(strings.TrimSuffix(name, "."), ".")
	for i := range labels {
		labels[i] = pathHole
	}
	shape := strings.Join(labels, ".")
	if strings.HasSuffix(name, ".") {
		shape += "."
	}
	switch {
	case port == "":
	case portNumber(port):
		shape += ":" + port
	default:
		shape += ":" + pathHole
	}
	return shape
}

// portNumber reports whether port is a port's number: one to five digits.
func portNumber(port string) bool {
	if port == "" || len(port) > 5 {
		return false
	}
	for i := 0; i < len(port); i++ {
		if port[i] < '0' || port[i] > '9' {
			return false
		}
	}
	return true
}

// HostTag is the tag of host's name in the session ctx carries (LogKeys):
// LogKey of its lower-case fully qualified name, its port left out, so
// "API.example.com", "api.example.com." and "api.example.com:8443" have one
// tag. "" for an IP address, or "".
func HostTag(ctx context.Context, host string) string {
	return LogKeys(ctx).HostTag(host)
}

// HostTag is HostTag under s's key (the process's for a nil s).
func (s *LogKeySession) HostTag(host string) string {
	name, _ := splitHostPort(host)
	if name == "" || isIPHost(name) {
		return ""
	}
	name = strings.ToLower(name)
	if !strings.HasSuffix(name, ".") {
		name += "."
	}
	return s.LogKey(name)
}

// HostLogName is host as a line logged under ctx names it: its shape
// (HostShape) and its tag (HostTag), "{}.{}.{}. #3fa2c1d0e9b8". An IP address
// whole, with no tag, but an IPv6 zone and a port that is no number. "" names
// "".
func HostLogName(ctx context.Context, host string) string {
	return LogKeys(ctx).HostLogName(host)
}

// HostLogName is HostLogName under s's key (the process's for a nil s).
func (s *LogKeySession) HostLogName(host string) string {
	if tag := s.HostTag(host); tag != "" {
		return HostShape(host) + " " + tag
	}
	return HostShape(host)
}

// splitHostPort is host's name and port: "api:8443" is "api" and "8443",
// "[::1]:80" is "::1" and "80", a host with no port (or an IPv6 address with
// none) is itself and "".
func splitHostPort(host string) (name, port string) {
	if h, p, err := net.SplitHostPort(host); err == nil {
		return h, p
	}
	return strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"), ""
}

// isIPHost reports whether name is an IP address (v4 or v6, a zone allowed).
func isIPHost(name string) bool {
	_, err := netip.ParseAddr(name)
	return err == nil
}

// HostTagLabel is how a console or a report line writes a host tag beside the
// host it names whole: "[host tag #3fa2c1d0e9b8]", as PathTagLabel writes a
// path tag. A reader of the agent's log finds the host a line names by its
// shape and tag ({}.{}.{}. #3fa2c1d0e9b8) there.
func HostTagLabel(tag string) string {
	return "[host tag " + tag + "]"
}

// WithHostTag is host, as a console or a report names it whole, followed by
// its host tag (HostTagLabel): "api.example.com:443 [host tag #3fa2c1d0e9b8]".
// host alone when tag is "" (an IP address, or a call an agent that tags no
// host reported).
func WithHostTag(host, tag string) string {
	if tag == "" {
		return host
	}
	return host + " " + HostTagLabel(tag)
}

// TaggedHostLogName is host as a log line of keploy's CLI names a host the
// agent tagged: its shape (HostShape) beside tag, the tag the agent's own
// line named it by (MockMismatchReport.HostTag), "{}.{}.{}:443 #3fa2c1d0e9b8".
// Its shape alone for a tag "".
func TaggedHostLogName(host, tag string) string {
	if tag == "" {
		return HostShape(host)
	}
	return HostShape(host) + " " + tag
}

// LossHost is a host name the text of a capture loss names whole (its What:
// CaptureLoss.Hosts; a capture_incomplete's Why: CaptureIncomplete.Hosts),
// where the agent wrote it there: Name, as the text spells it at byte At (a
// name, with its port or its root's dot where the text has them), so the
// place is At and Name's length. Tag is its host tag (HostTag) in the
// session the agent counted the loss in, the tag the agent's own lines name
// it by, so keploy's lines and its console and report name it by one tag.
// Tag is "" for a loss charged as evidence (Untagged: a test's harness and
// the receipts keep it past its session), and for an IP address with a zone
// or a port that is no number (LossText.Host), whose lines name the host by
// its shape alone. Only that place is read as the host: the same word elsewhere
// in the text is keploy's own ("of test worker 2" beside a compose service
// named worker).
type LossHost struct {
	Name string `json:"name" yaml:"name"`
	Tag  string `json:"tag,omitempty" yaml:"tag,omitempty"`
	At   int    `json:"at" yaml:"at"`
}

// LossHosts are the host names a loss's text names whole (LossHost).
type LossHosts []LossHost

// LossText writes the text of a capture loss (a CaptureLoss's What, a
// CaptureIncomplete's Why) and lists each host it names whole where it
// writes it (LossHost), with its tag in the session it is written under:
// keploy's lines, its console and its report read no other place of the
// text as a host. Words are keploy's own (Say, Sayf); a host is the
// application's (Host).
type LossText struct {
	keys  *LogKeySession
	text  strings.Builder
	hosts LossHosts
}

// NewLossText is a LossText whose hosts are tagged in ctx's session
// (LogKeys).
func NewLossText(ctx context.Context) *LossText { return LogKeys(ctx).LossText() }

// LossText is NewLossText under s's key (the process's for a nil s).
func (s *LogKeySession) LossText() *LossText { return &LossText{keys: s} }

// Say writes words, keploy's own.
func (t *LossText) Say(words string) *LossText {
	t.text.WriteString(words)
	return t
}

// Sayf writes keploy's own words, formatted: never a host's name, which
// Host writes.
func (t *LossText) Sayf(format string, a ...any) *LossText {
	fmt.Fprintf(&t.text, format, a...)
	return t
}

// Host writes host whole, a destination the text names (a name with its
// port or its root's dot as the text has them), and lists it where it
// stands with its tag in t's session. An IP address, which is no name, is
// listed with no tag where a line writes it otherwise than whole (an IPv6
// zone, or a port that is no number: a Host header can carry any text in
// either), so a log line writes it by its shape (10.0.0.1:{},
// [fe80::1%{}]:443); one a line writes whole, and "", are written and not
// listed.
func (t *LossText) Host(host string) *LossText { return t.HostTagged(host, t.keys.HostTag(host)) }

// HostTagged is Host with tag, the tag a line of the agent named host by
// already (a miss's report's HostTag): "" lists it with no tag.
func (t *LossText) HostTagged(host, tag string) *LossText {
	switch {
	case t.keys.HostTag(host) != "":
		t.hosts = append(t.hosts, LossHost{Name: host, Tag: tag, At: t.text.Len()})
	case HostShape(host) != host:
		t.hosts = append(t.hosts, LossHost{Name: host, At: t.text.Len()})
	}
	t.text.WriteString(host)
	return t
}

// String is the text written.
func (t *LossText) String() string { return t.text.String() }

// Hosts are the hosts the text names, each where it stands. nil for none.
func (t *LossText) Hosts() LossHosts { return append(LossHosts(nil), t.hosts...) }

// LogText is text, which names hs whole where each stands (LossHost), as a
// log line carries it: each of hs by its shape beside its tag
// (TaggedHostLogName, "{}.{}.{}:443 #3fa2c1d0e9b8"; its shape alone for one
// with no tag), at its place and nowhere else. A list whose places text
// does not hold is no list keploy can trust: text is then logged by its
// size alone (misplacedLogText).
func (hs LossHosts) LogText(text string) string {
	out, ok := hs.write(text, false, logHost)
	if !ok {
		return misplacedLogText(text)
	}
	return out
}

// ReportText is text as the report and the console name each of hs: whole,
// at its place, followed by its host tag (WithHostTag, "api.example.com:443
// [host tag #3fa2c1d0e9b8]"), so a reader finds the host a log line names by
// its shape and tag; a host with no tag whole alone. text as it is when its
// places are not where hs says.
func (hs LossHosts) ReportText(text string) string {
	out, ok := hs.write(text, false, reportHost)
	if !ok {
		return text
	}
	return out
}

// logHost and reportHost write a listed host as a log line and as the
// report name it.
func logHost(h LossHost) string    { return TaggedHostLogName(h.Name, h.Tag) }
func reportHost(h LossHost) string { return WithHostTag(h.Name, h.Tag) }

// write is text with each of hs written host(h) at its place, and keploy's
// words around them as they are or, with shape, as captureWhatShape writes
// them (only the part of a word that is no host shaped). ok is false, and
// out "", when a place is not in text, does not hold its name, holds white
// space, or overlaps another.
func (hs LossHosts) write(text string, shape bool, host func(LossHost) string) (out string, ok bool) {
	places := slices.Clone(hs)
	sort.SliceStable(places, func(i, j int) bool { return places[i].At < places[j].At })
	end := 0
	for _, h := range places {
		if h.Name == "" || h.At < end || h.At+len(h.Name) > len(text) || text[h.At:h.At+len(h.Name)] != h.Name ||
			strings.IndexFunc(h.Name, unicode.IsSpace) >= 0 {
			return "", false
		}
		end = h.At + len(h.Name)
	}
	if !shape {
		var b strings.Builder
		from := 0
		for _, h := range places {
			b.WriteString(text[from:h.At])
			b.WriteString(host(h))
			from = h.At + len(h.Name)
		}
		b.WriteString(text[from:])
		return b.String(), true
	}
	return shapeWords(text, places, host), true
}

// Shifted is hs with each place n bytes later: the hosts of a text that
// another text n bytes long now precedes.
func (hs LossHosts) Shifted(n int) LossHosts {
	if len(hs) == 0 {
		return nil
	}
	out := make(LossHosts, len(hs))
	for i, h := range hs {
		h.At += n
		out[i] = h
	}
	return out
}

// Untagged is hs with no tag: for evidence that outlives the session its
// tags name hosts in (a test's harness, the receipts). Each name and its
// place are kept. nil for none.
func (hs LossHosts) Untagged() LossHosts {
	if len(hs) == 0 {
		return nil
	}
	out := make(LossHosts, len(hs))
	for i, h := range hs {
		out[i] = LossHost{Name: h.Name, At: h.At}
	}
	return out
}

// With is hs and each of o not in it (the same name, tag and place), in
// order.
func (hs LossHosts) With(o LossHosts) LossHosts {
	out := append(LossHosts(nil), hs...)
	for _, h := range o {
		if !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// UnlistedLogText is text, a loss's What or a capture_incomplete's Why from
// an agent that lists none of the hosts it names (one that predates
// CaptureLoss.Hosts), as a log line carries it: by its size alone, never
// whole, as nothing tells its hosts apart from keploy's own words. The
// report and the console name it whole.
func UnlistedLogText(text string) string {
	return "withheld, " + strconv.Itoa(len(text)) + " bytes: " + UnlistedLogNote
}

// UnlistedLogNote is what UnlistedLogText says of a text it withholds.
const UnlistedLogNote = "the agent lists none of the host names in it"

// misplacedLogText is text whose hosts were listed at places it does not
// hold (LossHosts.write), as a log line carries it: by its size alone, as
// nothing then tells its hosts from keploy's words.
func misplacedLogText(text string) string {
	return "withheld, " + strconv.Itoa(len(text)) + " bytes: " + MisplacedLogNote
}

// MisplacedLogNote is what misplacedLogText says of a text it withholds.
const MisplacedLogNote = "the host names listed in it are not where the list says"

// DestinationNamed is how a console or a report line names a call's
// destination after the call: "  →  " and the destination whole beside its
// host tag (WithHostTag). A call with no destination whose host tag names
// the host its summary names whole (a DNS question) gets the tag alone,
// " [host tag #3fa2c1d0e9b8]". "" for neither.
func DestinationNamed(destination, hostTag string) string {
	switch {
	case destination != "":
		return "  →  " + WithHostTag(destination, hostTag)
	case hostTag != "":
		return " " + HostTagLabel(hostTag)
	}
	return ""
}

// HostsInText is text, a log line's or an error's, with each of hosts it
// names written as a line logged under ctx names it (HostLogName): a host
// with its port ("api.example.com:443") as one, then the name alone, in any
// case, wherever it stands as a whole name (not as a part of a longer one).
// An IP address is kept whole, but for an IPv6 zone, which is any text a
// client chose (its TLS server name, a CONNECT target): written {} wherever
// the text writes it, in whatever form the text writes the address ("dial
// tcp [fe80::1%{}]:443" for a server name "FE80:0::1%<text>"). text itself
// when it names none of hosts. It is for text keploy did not write (an error
// of a dial or a TLS handshake), whose places it cannot know: a capture
// loss's text, which keploy writes, lists each host where it writes it
// (LossText), so a host named as one of its words ("worker") is read there
// alone.
func HostsInText(ctx context.Context, text string, hosts ...string) string {
	return LogKeys(ctx).HostsInText(text, hosts...)
}

// HostsInText is HostsInText under s's key (the process's for a nil s).
func (s *LogKeySession) HostsInText(text string, hosts ...string) string {
	return hostsWritten(text, hosts, func(_ int, at string) string { return s.HostLogName(at) })
}

// hostsWritten is text with each place it names one of hosts as a whole name
// written write(i, at): i is the host's index in hosts, at the text's own
// spelling of it there. A host is found with its port as one
// ("api.example.com:443"), then with its root's dot where hosts lists it
// with one ("api.example.com."), then by its name alone, in any ASCII case,
// the longest first, each place once; a place a name character (a letter,
// a digit, '-' or '_') or a dot precedes, or a name character, or a dot and
// then one, follows, is part of a longer name, and is kept. "" is never
// looked for, nor an IP address, which a line writes whole, but for its IPv6
// zone: '%' and the zone are looked for by themselves, wherever they stand
// (a name character before them or not), and written "%{}", with no call of
// write. An error writes the address in its own form (Go's dial in its
// canonical one: "fe80::1%<zone>" for "FE80:0::1%<zone>", "1.2.3.4%<zone>"
// for "::ffff:1.2.3.4%<zone>"), but the zone, the client's text, as it is.
// text itself when it names none of hosts.
func hostsWritten(text string, hosts []string, write func(i int, at string) string) string {
	type find struct {
		as, lower string
		host      int
		zone      bool // an IPv6 zone with its '%': written "%{}"
	}
	var finds []find
	seen := map[string]bool{}
	add := func(s string, i int, zone bool) {
		l := strings.ToLower(s)
		if l == "" || l == "." || seen[l] {
			return
		}
		seen[l] = true
		finds = append(finds, find{s, l, i, zone})
	}
	for i, h := range hosts {
		name, port := splitHostPort(h)
		if name == "" {
			continue
		}
		if isIPHost(strings.TrimSuffix(name, ".")) {
			// The zone is all of name after its first '%', a trailing dot
			// too, as a dial reads it (netip).
			if at := strings.IndexByte(name, '%'); at >= 0 {
				add(name[at:], i, true)
			}
			continue
		}
		if port != "" {
			add(name+":"+port, i, false)
		}
		if strings.HasSuffix(name, ".") {
			add(name, i, false)
		}
		add(strings.TrimSuffix(name, "."), i, false)
	}
	if len(finds) == 0 {
		return text
	}
	sort.SliceStable(finds, func(i, j int) bool { return len(finds[i].lower) > len(finds[j].lower) })
	lower := strings.ToLower(text)
	fold := len(lower) == len(text)
	if !fold {
		lower = text // not ASCII: the case kept
	}
	var b strings.Builder
	from := 0
	for at := 0; at < len(text); {
		// Inside a longer name only a zone is looked for: it follows its
		// address.
		inName := at > 0 && (isNameByte(text[at-1]) || text[at-1] == '.')
		if inName && text[at] != '%' {
			at++
			continue
		}
		matched := false
		for _, f := range finds {
			if inName && !f.zone {
				continue
			}
			lf := f.lower
			if !fold {
				lf = f.as // not ASCII: matched as listed
			}
			end := at + len(lf)
			if end > len(text) || lower[at:end] != lf {
				continue
			}
			if end < len(text) && (isNameByte(text[end]) || text[end] == '.' && end+1 < len(text) && isNameByte(text[end+1])) {
				continue
			}
			b.WriteString(text[from:at])
			if f.zone {
				b.WriteString("%" + pathHole)
			} else {
				b.WriteString(write(f.host, text[at:end]))
			}
			from, at, matched = end, end, true
			break
		}
		if !matched {
			at++
		}
	}
	if from == 0 {
		return text
	}
	b.WriteString(text[from:])
	return b.String()
}

// isNameByte reports whether c may stand in a DNS label: a letter, a digit,
// '-' or '_'.
func isNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
}

// HostsInError is err with each of hosts its text names written as
// HostsInText writes them, for a log line or a caller that logs it: a dial's
// error names the host it looked up ("lookup api.example.com: no such host"),
// and a TLS handshake's the name it checked, beside the certificate's own
// names, which are written so too ("x509: certificate is valid for
// *.example.com, not api.example.com"). An IP address is kept whole, but for
// an IPv6 zone, written {} ("dial tcp [fe80::1%{}]:443": a client's server
// name may be a zoned address, and keploy record dials it). errors.Is and
// errors.As still see err. err itself when its text names none; nil for nil.
func HostsInError(ctx context.Context, err error, hosts ...string) error {
	if err == nil {
		return nil
	}
	var hn x509.HostnameError
	if errors.As(err, &hn) {
		hosts = append(hosts, hn.Host)
		if hn.Certificate != nil {
			hosts = append(hosts, hn.Certificate.DNSNames...)
			hosts = append(hosts, hn.Certificate.Subject.CommonName)
		}
	}
	text := HostsInText(ctx, err.Error(), hosts...)
	if text == err.Error() {
		return err
	}
	return hostsShapedErr{err: err, text: text}
}

// hostsShapedErr is an error whose text HostsInError shaped.
type hostsShapedErr struct {
	err  error
	text string
}

func (e hostsShapedErr) Error() string { return e.text }
func (e hostsShapedErr) Unwrap() error { return e.err }
