package learn

import (
	"encoding/base64"
	"mime"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.keploy.io/server/v3/pkg/models"
)

// THE TEST CASE'S OWN REQUEST.
//
// A keploy test case's request is what the application was given before any
// call of the test, and an earlier place of the test as any call's is
// (Earlier.Given): a value the application took from it (an id it carries
// on, a part of a template value, the next of the slots it lists picked by
// the clock, which moves between record and record's full round as a read
// of the clock does) is no noise. The classifier reads the request, each
// part as recorded and as keploy test sent it, as it reads an exchange F
// served: a value that drifted with a part keploy test sent with a template
// value came from there (sources); a word of what changed that the request
// holds is a place it may have come from (unaccounted); and the echo reads
// its instants (instants.go). Nothing binds a variable at the request, so a
// value found there is refused (the HTTP adapter), never learned as noise;
// nor is anything read back from it (ReadBack): it writes no record. And a
// value whose recorded moment the request, as recorded, holds is refused
// where record's full round saw a value the request, as sent, holds by no
// rule that names the recorded one (requestHolds): a pick from it whose pick
// in F the request does not list (a slot after the last, none).
//
// The replay sends the request with the filter that opens the test case
// (models.MockFilterParams.TestRequest), and the agent gives it to the
// tracker (Tracker.Give). Where it could not be read (a keploy older than its
// agent sent none, or keploy reads no request of the test case's kind), the
// place says so (Earlier.Unread), and every value that changed is refused
// rather than learned as noise or an echo (ClassifyIn): whatever the request
// held, keploy cannot tell what of it the value took.

// givenPlace names a test case's request in a reason: "test case <name>".
func givenPlace(test string) string {
	if test == "" {
		return "the test case's request"
	}
	return "test case " + test
}

// unsentRequest is why a test case's request could not be read when the
// filter that opened it carried none.
const unsentRequest = "keploy test sent the agent no copy of the test case's request, as a keploy older than its agent does"

// unopenedRequest is why it could not be read when the test's window closed
// before the filter that opens it brought it (Tracker.Await): a call that
// came before the filter waits for it (Tracker.AwaitRequest), and reads this
// only where no such filter came.
const unopenedRequest = "the filter that opens the test case had not brought its request to the agent"

// strayRequest is why it could not be read when it came while no test was
// open (Tracker.Give): the test's begin never reached the agent.
const strayRequest = "the test case's request reached the agent while no test was open, so keploy could not tie it to the calls the test made"

// GivenRequest is req, a keploy test case's own request as the filter that
// opened it carries it (models.TestRequest), as an earlier place of the test
// (Earlier.Given), read as the HTTP adapter reads an exchange F served: each
// part a leaf, recorded and as sent; its URL as its path's segments and its
// query's values (url.path.<i>, url.query.<key>), never its scheme, host or
// port, where the application is reached; a JSON body's leaves too, ahead of
// the body whole, so a reason names the leaf, and its object keys
// (KeysLeaf); the text a body kept as the base64 of its bytes holds
// (binaryText), beside the base64; the names of its query
// parameters, headers and form fields (request{names}); and none of what
// addresses or frames it (a Host, an :authority or a :scheme header, a
// Content-Length, a Content-Type, a header keploy adds). A nil req (a keploy
// older than its agent sent none), one keploy test could not read
// (TestRequest.Unread), and one whose body its Content-Type does not declare
// text (textMedia: a protobuf, an image; or bytes that are no text,
// binaryUnread) are a place that could not be read (Earlier.Unread), with no
// leaves: whether a declared body is text is read from what the request
// declares, never from the look of its bytes. A body that declares no media
// type (no Content-Type) is read as text where its bytes are text
// (plainText), its leaves marked so (Seen.Undeclared): a word of what
// changed glued in it to the bytes around it is refused, as keploy cannot
// tell printable framing bytes from words; one whose bytes are not text is
// a place that could not be read (undeclaredUnread).
func GivenRequest(req *models.TestRequest) Earlier {
	if req == nil {
		return Earlier{Mock: givenPlace(""), Given: true, Unread: unsentRequest}
	}
	e := Earlier{Mock: givenPlace(req.Test), Given: true, Unread: req.Unread}
	if req.Unread != "" {
		return e
	}
	recMedia, sentMedia := requestMedia(req.Parts)
	recNames, sentNames := map[string]bool{}, map[string]bool{}
	name := func(n, recorded, sent string) {
		// A part one side alone holds is "" on the other.
		if recorded != "" || sent == "" {
			recNames[n] = true
		}
		if sent != "" || recorded == "" {
			sentNames[n] = true
		}
	}
	for _, part := range req.Parts {
		if !givenPart(part.Name) {
			continue
		}
		if part.Name == "body" || part.Name == "binary" {
			// A body its Content-Type declares no text in (a protobuf
			// sent as application/x-protobuf, recorded as its bytes' text
			// or their base64) holds the application's words beside the
			// bytes that frame them (a field's tag and length), which may
			// be printable and glued to a word: keploy cannot tell one
			// from the other, so it cannot read the request. Each side is
			// read by what it declares: the recorded one first.
			why := bodyUnread(part.Name, part.Recorded, recMedia)
			if why == "" {
				why = bodyUnread(part.Name, part.Sent, sentMedia)
			}
			if why != "" {
				return Earlier{Mock: e.Mock, Given: true, Unread: why}
			}
		}
		from := len(e.Seen)
		switch {
		case part.Name == "url":
			if leaves, rn, sn, ok := urlSeen(part.Recorded, part.Sent); ok {
				e.Seen = append(e.Seen, leaves...)
				for _, n := range rn {
					recNames[n] = true
				}
				for _, n := range sn {
					sentNames[n] = true
				}
				continue
			}
		case part.Name == "body":
			e.Seen = append(e.Seen, jsonSeen(part.Recorded, part.Sent)...)
		case strings.HasPrefix(part.Name, "header."):
			// A header holding a JWT: its claims, ahead of the header whole,
			// each a leaf of its own (JWT.Leaves), so a value a claim holds
			// whole is found whole there and a reason names the claim.
			e.Seen = append(e.Seen, jwtSeen(part.Name, part.Recorded, part.Sent)...)
		case part.Name == "binary":
			// A body of a text media type keploy keeps as the base64 of
			// its bytes (models.HTTPReq.Binary): read as the text its bytes
			// are, as a part of its own beside the base64 as it stands.
			// Bytes that are no text whatever the request declares are
			// one keploy cannot read.
			r, rok := binaryText(part.Recorded)
			l, lok := binaryText(part.Sent)
			if !rok || !lok {
				return Earlier{Mock: e.Mock, Given: true, Unread: binaryUnread}
			}
			if r != "" || l != "" {
				e.Seen = append(e.Seen, Seen{Side: Request, Leaf: Leaf{Path: "binary{text}", Noise: "binary{text}", Type: String, Value: r}, Live: l})
			}
		}
		e.Seen = append(e.Seen, Seen{Side: Request, Leaf: Leaf{Path: part.Name, Noise: part.Name, Type: String, Value: part.Recorded}, Live: part.Sent})
		if undeclaredBody(part, recMedia, sentMedia) {
			// Read as text by its bytes: a word glued to the bytes around
			// it is refused too (Seen.Undeclared). Not the base64 a binary
			// part keeps, whose letters frame no words.
			for k := from; k < len(e.Seen); k++ {
				if e.Seen[k].Leaf.Path != "binary" {
					e.Seen[k].Undeclared = true
				}
			}
		}
		if n, ok := paramName(part.Name); ok {
			name(n, part.Recorded, part.Sent)
		}
	}
	if len(recNames)+len(sentNames) > 0 {
		e.Seen = append(e.Seen, Seen{Side: Request, Leaf: Leaf{Path: "request{names}", Type: Keys, Value: sortedSet(recNames), Unnamed: true}, Live: sortedSet(sentNames)})
	}
	return e
}

// binaryText is the text the bytes b64 (standard base64, padded or not)
// hold, and ok, where every byte is a part of a printable character or of
// white space (valid UTF-8); "" and ok for none. Not ok where b64 is no
// base64, or a byte is neither. It decodes a body its request declares text
// (textMedia), and refuses one that is not: it never tells text from bytes
// that frame words by bytes of their own (a protobuf's tags and lengths may
// all be printable, "BA" before "s17"), which only the declared media type
// does.
func binaryText(b64 string) (string, bool) {
	s, ok := decodeBase64(b64)
	if !ok {
		return "", false
	}
	b := []byte(s)
	for i := 0; i < len(b); {
		r, n := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && n <= 1 || !unicode.IsPrint(r) && !unicode.IsSpace(r) {
			return "", false
		}
		i += n
	}
	return string(b), true
}

// decodeBase64 is the bytes b64 (standard base64, padded or not) holds, and
// ok; not ok where b64 is no base64.
func decodeBase64(b64 string) (string, bool) {
	b, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		if b, err = base64.RawStdEncoding.DecodeString(b64); err != nil {
			return "", false
		}
	}
	return string(b), true
}

// requestMedia is the Content-Type a test case's request declares, recorded
// and as sent (its header part), "" where it declares none.
func requestMedia(parts []models.TestRequestPart) (recorded, sent string) {
	for _, p := range parts {
		if h, ok := strings.CutPrefix(p.Name, "header."); ok && strings.EqualFold(h, "content-type") {
			return p.Recorded, p.Sent
		}
	}
	return "", ""
}

// textMedia reports whether a body declared as ct (a Content-Type) is text by
// its media type: text/*, and of application/* JSON, XML (and their +json
// and +xml types), a URL-encoded form, GraphQL, JavaScript, NDJSON and YAML.
// Any other (application/x-protobuf, application/grpc, application/octet-
// stream, multipart/*, image/*), one that does not parse, and none ("")
// are not.
func textMedia(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	typ, sub, _ := strings.Cut(mt, "/")
	switch {
	case typ == "text":
		return true
	case typ != "application":
		return false
	case strings.HasSuffix(sub, "+json"), strings.HasSuffix(sub, "+xml"):
		return true
	}
	switch sub {
	case "json", "xml", "x-www-form-urlencoded", "graphql", "javascript", "x-ndjson", "yaml", "x-yaml":
		return true
	}
	return false
}

// bodyUnread is why one side of a test case's request body (part: "body",
// its text, or "binary", the base64 of its bytes) could not be read, "" where
// it can, or where that side holds none. Declared as media, it is read where
// media is a text media type (textMedia); declaring none (no Content-Type),
// where its bytes are text (plainText), a binary part's once decoded.
func bodyUnread(part, value, media string) string {
	switch {
	case value == "":
		return ""
	case media != "":
		if textMedia(media) {
			return ""
		}
		return mediaUnread(media)
	}
	text := value
	if part == "binary" {
		b, ok := decodeBase64(value)
		if !ok {
			return binaryUnread
		}
		text = b
	}
	if plainText(text) {
		return ""
	}
	return undeclaredUnread
}

// undeclaredBody reports whether part is a body ("body", or "binary" once
// decoded) that one of its sides, holding one, reads as text by its bytes
// (plainText): that side declares no media type (no Content-Type).
func undeclaredBody(part models.TestRequestPart, recMedia, sentMedia string) bool {
	if part.Name != "body" && part.Name != "binary" {
		return false
	}
	return part.Recorded != "" && recMedia == "" || part.Sent != "" && sentMedia == ""
}

// plainText reports whether s, a body that declares no media type, is text
// by its bytes: valid UTF-8 that holds no NUL and no other control
// character but a tab, a line feed and a carriage return. A body that
// declares a media type is read by it (textMedia), never so.
func plainText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r != '\t' && r != '\n' && r != '\r' && unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// mediaUnread is why a test case's request could not be read when its body
// is declared as ct, no text media type (textMedia).
func mediaUnread(ct string) string {
	return "its body is declared " + strconv.Quote(ct) + ", no text media type, and keploy cannot tell the words the application was given from the bytes that frame them"
}

// undeclaredUnread is why a test case's request could not be read when its
// body declares no media type (no Content-Type) and its bytes are no text
// (plainText).
const undeclaredUnread = "its body declares no media type (no Content-Type) and its bytes are no text (no UTF-8, or a NUL or another control byte), and keploy cannot tell the words the application was given from the bytes that frame them"

// binaryUnread is why a test case's request could not be read when its body
// is bytes that are no text (binaryText).
const binaryUnread = "its body is bytes that are no text, kept as their base64, and keploy cannot tell the words the application was given from the bytes that frame them"

// givenPart reports whether a part of a test case's request is one the
// application is given: none that addresses or frames the request (a Host,
// an :authority or a :scheme header, a Content-Length, a Content-Type) or
// that keploy adds to it (a header named keploy...), as the HTTP adapter
// reads no such header of an exchange.
func givenPart(part string) bool {
	h, ok := strings.CutPrefix(part, "header.")
	if !ok {
		return true
	}
	switch h = strings.ToLower(h); h {
	case "host", ":authority", ":scheme", "content-length", "content-type":
		return false
	}
	return !strings.HasPrefix(h, "keploy")
}

// urlSeen are the leaves of a test case's URL, recorded and as sent: its
// path's segments (url.path.<i>) and its query's values (url.query.<key>, a
// repeated key's values sorted and joined), with the names of its query
// parameters on each side; ok is false for a URL that does not parse, read
// whole then.
func urlSeen(recorded, sent string) (leaves []Seen, recNames, sentNames []string, ok bool) {
	ru, err := url.Parse(recorded)
	if err != nil {
		return nil, nil, nil, false
	}
	lu, lerr := url.Parse(sent)
	segs := func(u *url.URL) []string {
		var out []string
		for _, seg := range strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/") {
			if v, err := url.PathUnescape(seg); err == nil {
				seg = v
			}
			out = append(out, seg)
		}
		return out
	}
	query := func(u *url.URL) map[string]string {
		out := map[string]string{}
		for k, vs := range u.Query() {
			vs = append([]string(nil), vs...)
			sort.Strings(vs)
			out[k] = strings.Join(vs, ", ")
		}
		return out
	}
	rs, rq := segs(ru), query(ru)
	var ls []string
	lq := map[string]string{}
	if lerr == nil {
		ls, lq = segs(lu), query(lu)
	}
	for i, seg := range rs {
		live := ""
		if i < len(ls) {
			live = ls[i]
		}
		p := "url.path." + strconv.Itoa(i)
		leaves = append(leaves, Seen{Side: Request, Leaf: Leaf{Path: p, Noise: p, Type: String, Value: seg}, Live: live})
	}
	keys := make([]string, 0, len(rq))
	for k := range rq {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		p := "url.query." + k
		leaves = append(leaves, Seen{Side: Request, Leaf: Leaf{Path: p, Noise: p, Type: String, Value: rq[k], Unnamed: strings.ContainsAny(k, ".[]")}, Live: lq[k]})
		recNames = append(recNames, k)
	}
	for k := range lq {
		sentNames = append(sentNames, k)
	}
	return leaves, recNames, sentNames, true
}

// sortedSet is set's members, sorted, one per line.
func sortedSet(set map[string]bool) string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// paramName is the name of the header or form field a test case's request
// part names ("header.<name>", "form.<key>", a repeated key's later parts
// "form.<key>#2"), and ok, false for any other part.
func paramName(part string) (string, bool) {
	if n, ok := strings.CutPrefix(part, "header."); ok {
		return n, true
	}
	if n, ok := strings.CutPrefix(part, "form."); ok {
		if i := strings.LastIndexByte(n, '#'); i >= 0 {
			n = n[:i]
		}
		return n, true
	}
	return "", false
}

// jwtSeen are the claims of the JWT a header part (place) holds as recorded
// (ReadJWT), each as recorded and as sent ("<place>{jwt}.<claim>"), by path:
// "" as sent where the sent value holds no token or not that claim. None
// where the recorded value holds no token.
func jwtSeen(place, recorded, sent string) []Seen {
	rj, ok := ReadJWT(recorded)
	if !ok {
		return nil
	}
	rec, live := map[string]Leaf{}, map[string]Leaf{}
	rj.Leaves(place, rec)
	if lj, ok := ReadJWT(sent); ok {
		lj.Leaves(place, live)
	}
	paths := make([]string, 0, len(rec))
	for p := range rec {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := make([]Seen, 0, len(paths))
	for _, p := range paths {
		out = append(out, Seen{Side: Request, Leaf: rec[p], Live: live[p].Value})
	}
	return out
}

// jsonSeen are the leaves of a JSON body, recorded and as sent, by path, and
// its object keys (KeysLeaf) after them; none for a body that is not JSON.
func jsonSeen(recorded, sent string) []Seen {
	rv, ok := DecodeJSON([]byte(recorded))
	if !ok {
		return nil
	}
	rec := map[string]Leaf{}
	JSONLeaves("body", rv, rec)
	live := map[string]Leaf{}
	lv, liveOK := DecodeJSON([]byte(sent))
	if liveOK {
		JSONLeaves("body", lv, live)
	}
	paths := make([]string, 0, len(rec))
	for p := range rec {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	out := make([]Seen, 0, len(paths)+1)
	for _, p := range paths {
		out = append(out, Seen{Side: Request, Leaf: rec[p], Live: live[p].Value})
	}
	if k, ok := KeysLeaf("body", rv); ok {
		var lk Leaf
		if liveOK {
			lk, _ = KeysLeaf("body", lv)
		}
		out = append(out, Seen{Side: Request, Leaf: k, Live: lk.Value})
	}
	return out
}

// requestUnread is why every value that changed is refused, never learned
// as noise or an echo, where a place earlier than the call could not be read
// (an Unread place among served): the running test case's own request
// (Given, unreadRequestWhy), or the places of the run's past its bound
// (Tracker.keepRun, whose reason it is). "" where each was read, and where
// none is among served (a call outside a test, a test keploy mock opened, a
// set whose run kept every place).
func requestUnread(served []Earlier) string {
	for _, e := range served {
		switch {
		case e.Unread == "":
		case e.Incoming:
			// A client test's incoming requests keploy could not read
			// (IncomingGiven) give no place, and refuse nothing by
			// themselves: a client test's own rules stand
			// (SetClientTests), as where keploy reads none.
		case e.Given:
			return unreadRequestWhy(e.Unread)
		default:
			return e.Unread
		}
	}
	return ""
}

// unreadRequestWhy is why a drifted value is refused where the test case's
// request could not be read (why), and how to correct it: where keploy test
// sent none (a keploy older than its agent), run the two of one version, so
// that keploy test sends it; else, a field the application makes anew every
// run may be declared noise.
func unreadRequestWhy(why string) string {
	fix := "If the field is a value the application makes anew every run (a time it reads from its own clock, a random id), declare it noise in keploy.yml, and keploy stops comparing it"
	if why == unsentRequest {
		fix = "Run keploy record with the keploy agent of its own version, which hands the agent each test case's request"
	}
	return "keploy could not read the test case's own request (" + why + "), what the application was given before any call of the test: the value may have come from there (a part of it, or a pick from it by the application's clock), and keploy cannot tell. " + fix
}

// requestHolds names where the test case's own request (Earlier.Given), as
// recorded, holds one of lost, the moments a drifted value was recorded with
// and lost, by a rule (cuts) by which the request as keploy test sent it
// names none of live, the moments F saw in it: "test case <name> at
// <part>", with sent, whether keploy test sent that part as recorded; "" where
// none does. The value may be a pick from the request by the application's
// clock whose pick in F the request does not list (a slot after the last,
// none), which no echo finds; or one from a part a template value changed,
// which the learn step cannot follow. An instant that names a lost moment by
// one rule and a moment F saw by another (one truncated, one rounded) does
// not explain F's: the application cuts by one rule.
//
// A client test's incoming requests (Earlier.Incoming) are no such request:
// keploy test sends a test case's request again as recorded, or with
// template values, so the application picks in F from what the recording
// gave it while its clock runs ahead; but the harness sends its requests in
// F itself, with its own clock, and keploy reads them as the agent saw them.
// What F's request holds is then the evidence: a value F's request names by
// the rule is a copy the variable rules follow (appearedIn), and one it does
// not name did not come from it in F, which refutes the recorded coincidence
// (the application's clock read in the second the harness signed its token
// at). Either way the incoming requests refuse nothing here, and a client
// test's own rules judge the value (clientTestUnread).
func (dc *DriftCache) requestHolds(lost, live []moment, served []Earlier) (where string, asRecorded bool) {
	if len(lost) == 0 {
		return "", false
	}
	for _, e := range served {
		if !e.Given || e.Incoming {
			continue
		}
		// The rules by which the request, as sent, names a moment F saw.
		var sent heldMoments
		for _, s := range e.Seen {
			dc.heldIn(&sent, s.Live, dc.rawExtra(s.Live))
		}
		explained := map[cutRule]bool{}
		for _, h := range sent.list {
			for _, m := range live {
				cuts(h, m, func(c cutRule) bool {
					explained[c] = true
					return false
				})
			}
		}
		for _, s := range e.Seen {
			var held heldMoments
			dc.heldIn(&held, s.Leaf.Value, dc.rawExtra(s.Leaf.Value))
			for _, h := range held.list {
				for _, m := range lost {
					if cuts(h, m, func(c cutRule) bool { return !explained[c] }) {
						return e.Mock + " at " + s.Leaf.Path, s.Live == s.Leaf.Value
					}
				}
			}
		}
	}
	return "", false
}

// requestHoldsWhy is why a drifted value is refused whose recorded moment
// stands at where, in the test case's own request as recorded
// (requestHolds), which keploy test sent as recorded, or with template
// values in it (asRecorded false).
func requestHoldsWhy(where string, asRecorded bool) string {
	sent := "which keploy test sent as recorded"
	if !asRecorded {
		sent = "which keploy test sent with template values in it"
	}
	return "the moment it was recorded with stands in " + where + ", " + sent + ", while record's full round saw another value, which the request as sent holds by no rule that gives the recorded one: it may be a pick from the test case's request, by the application's clock, and keploy cannot tell it is a read of the clock. If the field is a time the application reads from its own clock, declare it noise in keploy.yml, and keploy stops comparing it"
}

// KEPLOY MOCK'S CLIENT TESTS.
//
// keploy mock is the mocking framework of client tests (a Playwright test, a
// CLI test, any harness): the harness gives the application its inputs, and
// no test case's request reaches keploy, so the learn step has no request
// place (Earlier.Given) there, and that is no failure to read one. What the
// harness gave the application may still change between the recording and
// record's full round (a slot it picks a minute on, a date it writes anew),
// and a value the application took from it moves then as a read of the
// clock may, by about F's lead, or with no source keploy sees. So in a
// client test (DriftCache.ClientTests) the lead shows no read of the
// application's clock (clockIn, instantClock), and a value with an instant
// that changed, in any declared layout, is learned as noise or an echo only
// where each such instant lies, in both runs, within its call's window on the
// application's clock, as the agent's clock hooks report that clock
// (Window.holdsRead): the recorded instant after the recording's previous
// call, or the test's recorded begin, and no later than the call, and F's
// after the previous call F served, or the test's begin, and no later than
// the call's arrival; a wall clock of no zone read in UTC or the
// application's own zone only. That is a read of the application's clock
// for that call; anything else is refused, and the reason says to have the
// test supply the value the same in every run where it supplies it
// (clientTestWhy). A value with no instant is read as in any test: no word
// of what changed may stand at an earlier place. A value that only reads as
// an instant of a declared layout (an id of thirteen digits reads as epoch
// milliseconds) is refused so too where it changed: a false red, which
// fails closed, never noise. A call with no window (no clock hooks; a call
// neither an earlier recorded call nor the test's recorded begin precedes)
// learns no changed instant as noise, and the reason says keploy saw no
// read of the application's clock for it. Nor does the lead give a variable
// a client test binds clock evidence (DriftCache.ClockReads).

// holdsRead reports whether rec and live, an instant a value was recorded
// with and the one F saw in its place, are each a read of its run's clock
// within w (nil: neither is), at its own resolution: as it names its
// instant; or, for a wall clock of no zone on both sides, as the wall clock
// of zone in both runs (wallIn), the zone the application writes one in
// beside UTC (Tracker.SetAppZone; nil: UTC alone). No other zone: a wall
// clock any zone reads as the clock's reading is as likely a value the test
// harness gave the application (the clock's reading plus two hours) as a
// read of it, and keploy cannot tell them apart, so it is refused whether
// its text names a zone or not. A time of day names no day and lies in no
// span.
func (w *Window) holdsRead(rec, live moment, zone *time.Location) bool {
	if w == nil || rec.key.day || live.key.day {
		return false
	}
	in := func(r, l time.Time) bool { return w.Rec.holds(r, rec.res) && w.F.holds(l, live.res) }
	if in(time.Unix(0, rec.key.at).UTC(), time.Unix(0, live.key.at).UTC()) {
		return true
	}
	if rec.zoned || live.zoned || zone == nil {
		return false
	}
	return in(wallIn(rec.key.at, zone), wallIn(live.key.at, zone))
}

// wallIn is the instant a wall clock of no zone names in zone, at its
// reading in UTC (a moment's key): its date and time there, as zone's rules
// give them at that date (summer time too). A wall clock zone's clocks
// skipped (Santiago's midnight on the day summer time starts) names the
// instant as far past the skip as it is past the last wall clock before
// it, as Java's ZonedDateTime and JavaScript's Date read it: a date of no
// zone is that day's first instant, never the day before's 23:00, which
// Go's time.Date gives in a zone behind UTC.
func wallIn(at int64, zone *time.Location) time.Time {
	w := time.Unix(0, at).UTC()
	t := time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), w.Nanosecond(), zone)
	if models.WallNanos(t, zone) == at {
		return t
	}
	// Skipped: read at the offset in force before the skip, the lesser of
	// the two it lies between (the clocks move ahead over a skip). Go's t
	// lies before the skip (a zone behind UTC) or past it, and a day
	// earlier the earlier offset is in force: no zone moves its clocks
	// twice within a day.
	_, o := t.Zone()
	if _, x := t.Add(-26 * time.Hour).Zone(); x < o {
		o = x
	}
	return w.Add(-time.Duration(o) * time.Second).In(zone)
}

// clientTestUnread says why a drifted value v of a client test, read in its
// call's window w, is no noise and no echo: an instant of it that changed
// (v.instants) is no read of the application's clock within w (holdsRead,
// in the cache's zone); "" where every one is, or none changed.
func (dc *DriftCache) clientTestUnread(v *valueDrift, w *Window) string {
	for _, p := range v.instants {
		if w.holdsRead(p.rec, p.live, dc.zone) {
			continue
		}
		part := ""
		for _, sp := range instantSpans(v.rec) {
			if sp.keyed && sp.key == p.rec.key {
				part = v.rec[sp.start:sp.end]
				break
			}
		}
		return clientTestWhy(partOf(v.rec, part), w != nil)
	}
	return ""
}

// clientTestWhy is why a drifted value of a client test is refused (read
// after "<field> changed, and "): no read of the application's clock keploy
// saw matched its instant, or, where its call had no window (windowed
// false), keploy saw no clock of the application's for the call at all
// (part names where the instant stands in the value; "" for a leaf its
// protocol types as one instant, the value whole); why that refuses it; and
// how to correct it.
func clientTestWhy(part string, windowed bool) string {
	which := ""
	if part != "" {
		which = " (an instant, " + part + ")"
	}
	what := "no clock read by the app matched it" + which
	seen := ""
	if !windowed {
		what = "keploy saw no read of the app's clock for its call" + which
		seen = ", and saw none for this call: keploy's clock hooks did not report the application's clock, or no earlier call and no begin of the test bounds the read"
	}
	return what + ": keploy mock's test harness gives the application its inputs, which keploy does not see, so keploy learns a changed instant as noise only where it lies, in the recording and in record's full round, within its call's window on the application's clock as keploy's clock hooks report it" + seen + ". If your test supplies this value, have it supply the same value in every run, and keploy keeps comparing it; if the application reads it from its own clock, declare it noise in keploy.yml, and keploy stops comparing it"
}
