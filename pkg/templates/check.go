package templates

import (
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// Refusal is a placeholder a test case cannot be run with, found when the test
// set loads, and why.
type Refusal struct {
	TestCase    string
	Where       string
	Placeholder string
	Why         string
}

func (f Refusal) Error() string {
	return fmt.Sprintf("%s: %s at %s: %s", f.TestCase, f.Placeholder, f.Where, f.Why)
}

// Undefined is a key a placeholder names that the test set does not define,
// at the first place it is used. OutsideGrammar marks template text that is no
// placeholder of the grammar (FindOutsideGrammar): placeholder-shaped text
// whose key is not a key ({{.a.b}}, {{string .user-id}}), or an expression an
// earlier keploy rendered ({{ .id | print }}).
type Undefined struct {
	Key            string
	TestCase       string
	Where          string
	Placeholder    string
	OutsideGrammar bool
	// Expression marks OutsideGrammar text that is no placeholder with
	// another key but an expression (isExpression): a pipe, written with
	// spaces or without ({{ .id | print }}, {{.id|len}}), a call with an
	// argument, or a bare dot ({{.}}).
	Expression bool
	// Fix, for a key a request sends that no earlier test case captures, is
	// the next step when an earlier response uses the key at a place that
	// can capture (producerAdvice): both edits, one for a value the app
	// issues anew each run ("change {{string .id}} at test-1/response
	// body.id to {{capture string .id}}") and one for a constant or what the
	// request sent (write the recorded value in place of each placeholder).
	// A set templatized by an older keploy wrote such a use where the value
	// is issued.
	Fix string
	// Recorded is the key's value as recorded, for the edit that writes it.
	Recorded string
}

// UncapturedNextStep is the next step for a key a request sends that no
// earlier test case captures: Fix where an earlier response uses the key,
// else where to put the capture. keploy test says it when the set loads.
func (u Undefined) UncapturedNextStep() string {
	if u.Fix != "" {
		return u.Fix
	}
	return fmt.Sprintf("if an earlier response issues this value, mark that place {{capture string .%s}} (or int/float); if it is a constant or what the request sent, write its recorded value %q in place of every placeholder of %s",
		u.Key, u.Recorded, u.Key)
}

// Place is one variable place of an expected response: a capture, or a use
// of a defined key.
type Place struct {
	TestCase    string
	Where       string
	Placeholder Placeholder
	// Header is the header the place is in; "" for the body.
	Header string
	// Path is the body leaf or object key the place is in; nil for a header,
	// for a body that is not JSON, and for a gRPC message.
	Path []PathSeg
	// GRPC marks a place of a gRPC response.
	GRPC bool
	// Stream marks a place in a frame of a streamed response (Path is then
	// the path in the frame's JSON), compared by the stream comparison.
	Stream bool
	// Producer marks a use at a place a capture would replace: a whole JSON
	// value, a header value, a cookie's whole value. Bare marks such a use
	// written outside a JSON string ({"n":{{int .n}}}), which a capture
	// replaces in quotes.
	Producer, Bare bool
	// NoCaptureWhy says why a capture would be refused at this place, ""
	// where one can stand: inside an array under ignoreOrdering, in an
	// object key, in part of a cookie, beside another placeholder of its
	// JSON value, beside a capture or a use of a key its response captures
	// in a header value, in a body that is not JSON, or a part of a
	// URL-valued header whose recorded value holds a delimiter of that part
	// (a whole Location an older templatize wrote, "/jobs/j_1"). Where it is
	// set, a next step never offers a capture here. (A use of a key its own
	// response captures is told by its capture: NextStep never offers a
	// second one.)
	NoCaptureWhy string
	// Shared marks a header value that holds other placeholders too: it
	// holds one capture, so at most one of them can become one.
	Shared bool
	// PassedOver marks a Producer place in a URL-valued header outside a
	// 201's Location (a redirect's target, a Content-Location): a capture
	// may stand there, but it would accept any target, a regression
	// included, and keploy templatize never writes one there. The producer a
	// next step names for a key nothing captures passes over it for a later
	// place of its response that is neither refused nor passed over, as
	// templatize's producer rule does in a plain recording; a capture edit
	// offered there (the only place of its response, or the failing place
	// itself) says what it gives up.
	PassedOver bool
	// Recorded is a Producer place's value as recorded, for its edits.
	Recorded string
}

// KeyPath is the object keys of a body place's path, in order: what a body
// noise entry names.
func (p Place) KeyPath() []string {
	var keys []string
	for _, s := range p.Path {
		if !s.IsIndex {
			keys = append(keys, s.Key)
		}
	}
	return keys
}

// CheckResult is what Check finds in a test set before any of it runs.
type CheckResult struct {
	// Refused is, per test case, the placeholders it cannot run with.
	Refused map[string][]Refusal
	// Undefined is each key used that the set does not define, at its first
	// use, and each piece of template text outside the grammar (a key that
	// is not one, {{.a.b}}, or an expression, {{ .id | print }}): all are
	// sent and compared as text.
	Undefined []Undefined
	// Uncaptured is each key a request sends that no earlier test case
	// captures, at its first such request: every run sends config.yaml's
	// value there.
	Uncaptured []Undefined
	// Places are the variable places of the expected responses.
	Places []Place
	// Sends is, per test case, each key its request sends (a use of a
	// defined key, never a secret): where the response then holds the key,
	// it answers what it was asked for (a read-back).
	Sends map[string]map[string]bool
	// keys are the keys config.yaml's template defines: a next step that
	// names a new key gives it a name none of them has.
	keys map[string]bool
}

// CheckOptions are the settings a check depends on.
type CheckOptions struct {
	// IgnoreOrdering is test.ignoreOrdering: arrays compare in any order, so
	// a capture inside one cannot say which element it takes.
	IgnoreOrdering bool
	// Streaming reports a test case whose response is compared as a stream.
	Streaming func(*models.TestCase) bool
}

// Why a placeholder is refused. Each is said with the edit to make, written
// with the placeholder's own key (the refusal functions below).
const (
	whyCaptureInRequest    = "a capture says what the app ANSWERS, so it belongs in an expected response; a request only sends values"
	whyCaptureNoType       = "a capture needs the JSON type it expects"
	whyCaptureSecret       = "a secret comes from secret.yaml and is never taken from an answer"
	whyCaptureGRPC         = "gRPC test cases do not capture: their values can only be sent and asserted"
	whyCaptureStream       = "a streamed response does not capture (streamed test cases run after the others)"
	whyCaptureTwice        = "the same key is captured twice in one response"
	whyCaptureArray        = "this capture is inside an array and test.ignoreOrdering is on, so which element it takes cannot be decided: turn ignoreOrdering off for this set, or capture the value outside the array"
	whyCaptureNotLeaf      = "a capture must be a whole JSON value written in quotes, a part of a JSON string, or a header value"
	whyCaptureNotAlone     = "a capture must be the only placeholder in its value, so the text around it fixes where it starts and ends"
	whyCaptureTwiceInValue = "a header value holds one capture, so the text around it fixes where it starts and ends"
	whyCaptureBesideOwnUse = "a header capture may sit beside uses only of keys this response does not capture, so the text around it is known before the answer is read"
	whyCaptureNotCookie    = "in Set-Cookie a capture must be a cookie's whole value, one capture per cookie"
	whyCookieShadowed      = "a later cookie in this Set-Cookie value has the same name, Domain and Path and replaces this one (RFC 6265), so what this one sets never reaches the next request: move the placeholder to the last cookie of that name"
	whyCaptureInKey        = "a capture takes a value the app answers; an object key is matched, not taken"
	whyUnusablePlaceholder = "names a key whose value is a map or a list, and a placeholder stands for one value: give the key a string, a number or a boolean in config.yaml, or remove the placeholder"
	whyShadowedKey         = "the JSON object repeats a key on this value's path, and a reader keeps only the last occurrence of a repeated key (encoding/json, JSON.parse, json.loads), so this is not the response's value at its path and a placeholder here can be neither captured nor asserted: remove the earlier occurrence, or move the placeholder to the last one"
	whyStreamNotFramed     = "this streamed response's body is kept whole, not as frames keploy splits (a multipart body, or a stream whose Content-Type keploy does not split), so a variable in it cannot be asserted at its place"
	whyInName              = "a name (a header's, a form field's) is matched as written, never sent or asserted at a key's value, so a variable in it would be neither"
	whyInTrailer           = "a gRPC response's trailers are read only for grpc-status, so a variable there could not be asserted"
	whyOutsideCookie       = "a Set-Cookie value is read cookie by cookie (name=value, then its attributes), and this placeholder stands in none, so it could be neither captured nor asserted at its place"
	whyInAssertion         = "an assertion is read as written, never at a key's value, so a variable in it would be neither sent nor asserted"
	whyInCookieName        = "a run reads a cookie's value and its attributes' values; the cookie's name, an attribute's name and an attribute with no value (HttpOnly) are matched as written, so a variable there would be neither captured nor asserted"
	whyCookieSplit         = "a Set-Cookie value is read cookie by cookie (name=value, then its attributes), and the recorded values written in place of its placeholders (this one's, or several side by side) hold a ';', a line break or a ',' that starts a name=, so the value would read as other cookies or attributes than this text does and this placeholder could not be asserted at its place"
	// whyAttrOverridden is a format of the attribute's name.
	whyAttrOverridden = "a later %s attribute of this cookie overrides this one (a user agent keeps the last attribute of a name, RFC 6265 §5.3), and a run reads that last one, so a variable here would be neither captured nor asserted"
	whyCookieSpace    = "a cookie's value and its attributes' values are read without the spaces around them, and this placeholder's recorded value, written in its place beside the others at theirs, falls outside what a run reads of its part (a space, or an empty value, at the part's edge), so no answer could hold it there"
)

// Why a capture cannot stand at a use's place (Place.NoCaptureWhy), said in
// a next step that then offers no capture there.
const (
	noCaptureArray      = "it is inside an array and test.ignoreOrdering is on, so a capture there could not say which element it takes"
	noCaptureKey        = "it is an object key, which is matched, not taken"
	noCaptureCookiePart = "it is part of a cookie's value, and a capture must be a cookie's whole value"
	// noCaptureCookieAttribute is said for a use in an attribute's value
	// (Path=, Domain=): a use in a name or a flag is refused
	// (whyInCookieName).
	noCaptureCookieAttribute = "it is in an attribute's value, outside the cookie's value, and a capture must be a cookie's whole value"
	noCaptureNotAlone        = "its JSON value holds other placeholders, and a capture must be the only placeholder in its value"
	noCaptureNotJSON         = "the body is not JSON, and a capture must be a JSON value, part of a JSON string or a header value"
	noCaptureHeaderHeld      = "its header value holds a capture already, and a header value holds one capture"
)

// noCaptureURLDelimiter is why a capture cannot stand at a part of a
// URL-valued header whose recorded value holds a delimiter of that part.
func noCaptureURLDelimiter(recorded string) string {
	return fmt.Sprintf("the URL is read by its parts, and %q holds a delimiter of its part", recorded)
}

func noCaptureOwnUse(key string) string {
	return "its header value also asserts " + key + ", which this response captures, and a capture may sit only beside uses of keys its response does not capture"
}

// useFor is the use that asserts p's key at a place: of p's type.
func useFor(p Placeholder) string { return Format(false, useType(p), p.Key) }

// asRecorded is the text p stands for as recorded, quoted, for an edit that
// writes it out.
func (c *checker) asRecorded(p Placeholder) string {
	v, _ := c.run.recordedValue(p)
	return strconv.Quote(v)
}

// recordedList names each placeholder of ps but skip at its recorded value:
// `{{string .x}} as "2"`.
func (c *checker) recordedList(ps []Placeholder, skip Placeholder) string {
	var out []string
	for _, o := range ps {
		if o.Start == skip.Start {
			continue
		}
		out = append(out, o.Text+" as "+c.asRecorded(o))
	}
	return strings.Join(out, ", ")
}

func refuseCaptureInRequest(p Placeholder) string {
	return whyCaptureInRequest + ": write " + useFor(p) + " here"
}

func refuseCaptureNoType(p Placeholder) string {
	return whyCaptureNoType + ": write " + Format(true, TypeString, p.Key) + ", " + Format(true, TypeInt, p.Key) + " or " + Format(true, TypeFloat, p.Key)
}

func refuseCaptureSecret(p Placeholder) string {
	return whyCaptureSecret + ": write " + Format(false, TypeString, p.Key) + " to assert it"
}

func refuseCaptureGRPC(p Placeholder) string {
	return whyCaptureGRPC + ": write " + useFor(p) + " here, and capture the value in an earlier HTTP test case"
}

func refuseCaptureStream(p Placeholder) string {
	return whyCaptureStream + ": assert the value with " + useFor(p) + ", or capture it in an earlier, non-streamed test case"
}

func refuseCaptureTwice(p Placeholder) string {
	return whyCaptureTwice + ": keep the first capture of " + p.Key + " and write " + useFor(p) + " here to assert it"
}

func (c *checker) refuseCaptureNotLeaf(p Placeholder) string {
	return whyCaptureNotLeaf + `: write "` + Format(true, useType(p), p.Key) + `" (in quotes) where the value is a whole JSON value, or its recorded value ` + c.asRecorded(p) + " here"
}

func (c *checker) refuseCaptureNotAlone(p Placeholder, all []Placeholder) string {
	return whyCaptureNotAlone + ": write " + c.recordedList(all, p) + " here, or capture the value where it stands alone"
}

func (c *checker) refuseCaptureTwiceInValue(p Placeholder) string {
	return whyCaptureTwiceInValue + ": keep one capture and write each other one as its recorded value (here, " + p.Text + " as " + c.asRecorded(p) + ")"
}

func (c *checker) refuseCaptureBesideOwnUse(own []Placeholder) string {
	return whyCaptureBesideOwnUse + ": write " + c.recordedList(own, Placeholder{Start: -1}) + " here"
}

func refuseCaptureNotCookie(p Placeholder, cookie string) string {
	return whyCaptureNotCookie + ": " + cookie + "=" + Format(true, useType(p), p.Key) + "; Path=/"
}

func refuseCaptureInKey(p Placeholder) string {
	return whyCaptureInKey + ": write " + useFor(p) + " in the key, or capture the value it names"
}

func (c *checker) refuseStreamNotFramed(p Placeholder) string {
	return whyStreamNotFramed + ": write its recorded value " + c.asRecorded(p) + " here"
}

// refuseInName is why a variable in a name is refused, with the name to
// write: each placeholder at its recorded value, a capture's included.
func (c *checker) refuseInName(name string) string {
	return whyInName + ": write the name as recorded, " + strconv.Quote(renderRaw(name, c.run.asRecorded, c.run))
}

func (c *checker) refuseInTrailer(p Placeholder) string {
	return whyInTrailer + ": write its recorded value " + c.asRecorded(p) + " here"
}

// refuseOutsideCookie is why a variable of a Set-Cookie value that stands in
// no cookie is refused, with both edits.
func (c *checker) refuseOutsideCookie(p Placeholder) string {
	return whyOutsideCookie + ": write it as a cookie's whole value (name=" + Format(p.Capture, useType(p), p.Key) + "), or write its recorded value " + c.asRecorded(p) + " here"
}

func (c *checker) refuseInAssertion(p Placeholder) string {
	return whyInAssertion + ": write its recorded value " + c.asRecorded(p) + " here"
}

func (c *checker) refuseInCookieName(p Placeholder) string {
	return whyInCookieName + ": write its recorded value " + c.asRecorded(p) + " here"
}

func (c *checker) refuseCookieSplit(p Placeholder) string {
	return whyCookieSplit + ": write its recorded value " + c.asRecorded(p) + " here"
}

func (c *checker) refuseOverriddenAttr(p Placeholder, attr string) string {
	return fmt.Sprintf(whyAttrOverridden+": move the placeholder to the last %s attribute of this cookie, or write its recorded value %s here", attr, attr, c.asRecorded(p))
}

func (c *checker) refuseCookieSpace(p Placeholder) string {
	return whyCookieSpace + ": write its recorded value " + c.asRecorded(p) + " here"
}

// useType is p's type, string when it names none.
func useType(p Placeholder) string {
	if p.Type == "" {
		return TypeString
	}
	return p.Type
}

// Check reads every placeholder in cases before any is run. Only a
// placeholder whose key the test set defines can be refused: any other is
// text (rule 6), so a set with no templates is never refused anything.
func (r *Run) Check(cases []*models.TestCase, opts CheckOptions) CheckResult {
	c := &checker{run: r, opts: opts, res: CheckResult{Refused: map[string][]Refusal{}, Sends: map[string]map[string]bool{}, keys: map[string]bool{}}, seen: map[string]bool{}, captured: map[string]bool{}, uncaptured: map[string]bool{}, firstUse: map[string]producerUse{}, refusedCapture: map[string]producerUse{}}
	for k := range r.recorded {
		c.res.keys[k] = true
	}
	for k := range r.unusable {
		c.res.keys[k] = true
	}
	// In the order a replay runs them: streamed test cases after the others.
	streamed := func(tc *models.TestCase) bool {
		return tc.Kind != models.GRPC_EXPORT && opts.Streaming != nil && opts.Streaming(tc)
	}
	for _, late := range []bool{false, true} {
		for _, tc := range cases {
			if tc == nil || streamed(tc) != late {
				continue
			}
			c.tc = tc
			switch tc.Kind {
			case models.GRPC_EXPORT:
				c.grpc(tc)
			default:
				c.inResponse = false
				c.request(tc.HTTPReq)
				c.inResponse = true
				c.response(tc)
				c.inResponse = false
			}
			c.assertions(tc)
		}
	}
	return c.res
}

type checker struct {
	run  *Run
	opts CheckOptions
	tc   *models.TestCase
	res  CheckResult
	seen map[string]bool
	// captured is each key an earlier test case's response captures.
	captured   map[string]bool
	uncaptured map[string]bool
	streaming  bool
	// firstUse is, per key, the use in the first earlier response holding
	// one at a place that could capture it instead: that response's first
	// such place that is not weak (producerUse.weak), else its first.
	firstUse map[string]producerUse
	// refusedCapture is, per key, the first capture of it a response holds
	// that is refused: a later request's key is then not captured there.
	refusedCapture map[string]producerUse
	inResponse     bool
}

// producerUse is a use at a place of a response that a capture could take.
type producerUse struct {
	testCase, where string
	p               Placeholder
	// bare marks a placeholder written outside a JSON string, which a
	// capture must be written in quotes to replace.
	bare bool
	// noCaptureWhy says why a capture would be refused there (Place).
	noCaptureWhy string
	shared       bool
	// passedOver marks a part of a URL-valued header outside a 201's
	// Location (Place.PassedOver).
	passedOver bool
}

// weak reports whether a capture at u is refused (noCaptureWhy) or is one
// keploy templatize passes over (passedOver): a later place of the same
// response that is neither names the producer instead.
func (u producerUse) weak() bool { return u.noCaptureWhy != "" || u.passedOver }

// noteProducer records a use at a place a capture of its key would replace:
// per key, the first response holding one names the producer, at its first
// such place that is not weak (producerUse.weak), else at its first, as
// keploy templatize's producer rule picks the place that captures.
// noCaptureWhy says why a capture there would be refused, so only the edit
// that writes the value as recorded is given; shared, that its header value
// holds other placeholders too; passedOver, that it is a part of a
// URL-valued header outside a 201's Location.
//
// pl is the place place just recorded for p (nil for a secret, which is no
// place).
func (c *checker) noteProducer(pl *Place, where string, p Placeholder, bare bool, noCaptureWhy string, shared, passedOver bool) {
	if p.Capture || isSecret(p) || c.streaming || c.tc.Kind == models.GRPC_EXPORT {
		return
	}
	if pl != nil {
		pl.Producer, pl.Bare, pl.NoCaptureWhy, pl.Shared, pl.PassedOver = true, bare, noCaptureWhy, shared, passedOver
		pl.Recorded, _ = c.run.recordedValue(p)
	}
	u := producerUse{testCase: c.tc.Name, where: where, p: p, bare: bare, noCaptureWhy: noCaptureWhy, shared: shared, passedOver: passedOver}
	if cur, seen := c.firstUse[p.Key]; !seen || (cur.testCase == u.testCase && cur.weak() && !u.weak()) {
		c.firstUse[p.Key] = u
	}
}

// noCapture says why a capture cannot stand at pl, the use place place just
// recorded (nil for a secret, which is no place), which no capture would
// replace.
func (c *checker) noCapture(pl *Place, why string) {
	if pl != nil {
		pl.NoCaptureWhy = why
	}
}

func (c *checker) refuse(where string, p Placeholder, why string) {
	c.res.Refused[c.tc.Name] = append(c.res.Refused[c.tc.Name], Refusal{TestCase: c.tc.Name, Where: where, Placeholder: p.Text, Why: why})
	if _, seen := c.refusedCapture[p.Key]; p.Capture && c.inResponse && !seen {
		c.refusedCapture[p.Key] = producerUse{testCase: c.tc.Name, where: where, p: p}
	}
}

// look notes each placeholder of text: an undefined key, template text
// outside the grammar (a key that is not one, or an expression an earlier
// keploy rendered), a key with no usable value. It returns the placeholders
// whose key is defined and usable, the only ones a rule can refuse.
func (c *checker) look(where, text string) []Placeholder {
	ps := Find(text)
	for _, d := range FindOutsideGrammar(text) {
		c.noteOutsideGrammar(where, d)
	}
	var defined []Placeholder
	for _, p := range ps {
		if !c.run.Defined(p) {
			c.note(where, p)
			continue
		}
		if why, bad := c.run.Unusable(p); bad {
			c.refuse(where, p, why+"; "+p.Text+" "+whyUnusablePlaceholder)
			continue
		}
		defined = append(defined, p)
	}
	return defined
}

// note records an undefined key at its first use.
func (c *checker) note(where string, p Placeholder) {
	if c.seen[p.Key] {
		return
	}
	// A set with no templates whose traffic holds {{ .Name }} (a mail
	// template, a Helm chart): that is the app's text.
	if c.run.Empty() && p.Type == "" && !p.Capture {
		return
	}
	c.seen[p.Key] = true
	c.res.Undefined = append(c.res.Undefined, Undefined{Key: p.Key, TestCase: c.tc.Name, Where: where, Placeholder: p.Text})
}

// noteOutsideGrammar records template text that is no placeholder of the
// grammar, at its first use: a key that is not one, or an expression an
// earlier keploy rendered ({{ .id | print }}), which this one sends and
// compares as written. In a set with no templates, untyped text such as
// {{ .Values.name }} is the app's (a Helm chart, a mail template).
func (c *checker) noteOutsideGrammar(where string, p Held) {
	if c.seen["\x00"+p.Key] || (c.run.Empty() && p.Type == "" && !p.Capture) {
		return
	}
	c.seen["\x00"+p.Key] = true
	c.res.Undefined = append(c.res.Undefined, Undefined{Key: p.Key, TestCase: c.tc.Name, Where: where, Placeholder: p.Text, OutsideGrammar: true, Expression: isExpression(p.Key)})
}

// isExpression reports whether key, the text after the '.' of template text
// outside the grammar (FindOutsideGrammar), is more than one name: nothing (a
// bare dot), or text holding a space (an argument, a pipe written with
// spaces) or a pipe written without ({{.id|len}}). Any other is
// placeholder-shaped text whose key is not a key ({{.a.b}}, {{string
// .user-id}}).
func isExpression(key string) bool {
	return key == "" || strings.ContainsAny(key, " \t\r\n|")
}

// sendOnly checks text a request sends: no capture, and a key it sends that
// no earlier test case captures is named. Each key it sends is recorded
// (CheckResult.Sends).
func (c *checker) sendOnly(where, text string, why func(Placeholder) string) {
	for _, p := range c.look(where, text) {
		if p.Capture {
			c.refuse(where, p, why(p))
			continue
		}
		if isSecret(p) {
			continue
		}
		if c.res.Sends[c.tc.Name] == nil {
			c.res.Sends[c.tc.Name] = map[string]bool{}
		}
		c.res.Sends[c.tc.Name][p.Key] = true
		if !c.captured[p.Key] && !c.uncaptured[p.Key] {
			c.uncaptured[p.Key] = true
			u := Undefined{Key: p.Key, TestCase: c.tc.Name, Where: where, Placeholder: p.Text}
			u.Recorded, _ = c.run.recordedValue(p)
			if pu, ok := c.firstUse[p.Key]; ok {
				recorded, _ := c.run.recordedValue(pu.p)
				u.Fix = producerAdvice(pu.testCase, pu.where, pu.p, recorded, pu.bare, pu.noCaptureWhy, pu.shared, pu.passedOver)
			} else if rc, ok := c.refusedCapture[p.Key]; ok {
				// The one capture of the key is refused: its own edit is
				// that refusal's, and the key is not captured there.
				u.Fix = fmt.Sprintf("the capture %s at %s/%s is refused when the set loads (its refusal names the edit), so nothing captures %s: write its recorded value %q in place of every placeholder of %s, or capture the value where a capture can stand",
					rc.p.Text, rc.testCase, rc.where, p.Key, u.Recorded, p.Key)
			}
			c.res.Uncaptured = append(c.res.Uncaptured, u)
		}
	}
}

// names refuses each variable in a map's names (a header name, a form field
// name): a name is matched as written, so a variable there is neither sent
// at its value nor asserted. A secret in a name is the text the recording
// held, rendered as main rendered it.
func (c *checker) names(where string, names []string) {
	for _, name := range names {
		for _, p := range c.look(where+" "+name, name) {
			if !isSecret(p) {
				c.refuse(where+" "+name, p, c.refuseInName(name))
			}
		}
	}
}

func (c *checker) request(req models.HTTPReq) {
	c.sendOnly("request url", req.URL, refuseCaptureInRequest)
	for _, k := range sortedKeys(req.URLParams) {
		c.sendOnly("request url_params."+k, req.URLParams[k], refuseCaptureInRequest)
	}
	c.names("request header name", sortedKeys(req.Header))
	for _, k := range sortedKeys(req.Header) {
		c.sendOnly("request header "+k, req.Header[k], refuseCaptureInRequest)
	}
	formKeys := make([]string, 0, len(req.Form))
	for _, f := range req.Form {
		formKeys = append(formKeys, f.Key)
	}
	c.names("request form field name", formKeys)
	for _, f := range req.Form {
		for _, v := range f.Values {
			c.sendOnly("request form "+f.Key, v, refuseCaptureInRequest)
		}
	}
	c.sendOnly("request body", req.Body, refuseCaptureInRequest)
}

// place records a variable place of an expected response, which a run
// asserts against the answer. A streamed response's places are its headers'
// and its frames' (placeFrame). A secret is not a variable (Run.isUse): it is
// the literal the recording held, compared as the literal would be. It
// returns the place it recorded, nil for a secret, for the caller to note at
// once why a capture cannot stand there (noCapture) or that one could
// (noteProducer); the pointer is good until the next place is recorded.
func (c *checker) place(where, header string, path []PathSeg, p Placeholder) *Place {
	if isSecret(p) {
		return nil
	}
	c.res.Places = append(c.res.Places, Place{TestCase: c.tc.Name, Where: where, Placeholder: p, Header: header, Path: path, GRPC: c.tc.Kind == models.GRPC_EXPORT})
	return &c.res.Places[len(c.res.Places)-1]
}

// placeFrame records a variable place in a frame of a streamed response.
func (c *checker) placeFrame(where string, path []PathSeg, p Placeholder) {
	if isSecret(p) {
		return
	}
	c.res.Places = append(c.res.Places, Place{TestCase: c.tc.Name, Where: where, Placeholder: p, Path: path, Stream: true})
}

func (c *checker) response(tc *models.TestCase) {
	streaming := c.opts.Streaming != nil && c.opts.Streaming(tc)
	c.streaming = streaming
	captured := map[string]bool{}
	// capture checks one capture's key, type and secrecy; false when refused.
	capture := func(where string, p Placeholder) bool {
		switch {
		case streaming:
			c.refuse(where, p, refuseCaptureStream(p))
		case p.Type == "":
			c.refuse(where, p, refuseCaptureNoType(p))
		case isSecret(p):
			c.refuse(where, p, refuseCaptureSecret(p))
		case captured[p.Key]:
			c.refuse(where, p, refuseCaptureTwice(p))
		default:
			captured[p.Key] = true
			return true
		}
		return false
	}
	// The keys this response captures anywhere: a header capture may sit
	// beside uses only of other keys.
	captures := c.responseCaptures(tc.HTTPResp)

	body := tc.HTTPResp.Body
	switch {
	case streaming && len(tc.HTTPResp.StreamBody) == 0 && !Contains(body) && !strings.Contains(body, "{{"):
		// Nothing in the body to read.
	case streaming:
		// A streamed response is read frame by frame: its recorded frames,
		// or its flat body split as a recording splits a stream. A flat body
		// that does not split (multipart) has no frame a variable could be
		// asserted in.
		if frames := StreamFrames(tc.HTTPResp); frames != nil {
			c.frames(frames, capture)
		} else {
			c.unsplitStream(body)
		}
	case Contains(body) || strings.Contains(body, "{{"):
		// "{{" alone too: template text outside the grammar is named (look).
		c.body(body, capture)
	}
	c.names("response header name", sortedKeys(tc.HTTPResp.Header))
	for _, name := range sortedKeys(tc.HTTPResp.Header) {
		v := tc.HTTPResp.Header[name]
		if strings.EqualFold(name, "Set-Cookie") {
			c.setCookie(name, v, capture)
			continue
		}
		where := "response header " + name
		ps := c.look(where, v)
		// keploy templatize captures a part of a URL-valued header only in a
		// 201's Location; any other is passed over for a later place of the
		// response, and the advice follows it.
		passedOver := isURLHeader(name) && !(strings.EqualFold(name, "Location") && tc.HTTPResp.StatusCode == http.StatusCreated)
		n := 0
		var own []Placeholder
		for _, p := range ps {
			if p.Capture {
				n++
			} else if captures[p.Key] {
				own = append(own, p)
			}
		}
		for _, p := range ps {
			if !p.Capture {
				pl := c.place(where, name, nil, p)
				c.noteProducer(pl, where, p, false, c.headerNoCapture(name, v, p, ps, n, captures), len(ps) > 1, passedOver)
				continue
			}
			delimited := c.urlDelimiter(name, v, p)
			switch {
			case n > 1:
				c.refuse(where, p, c.refuseCaptureTwiceInValue(p))
			case len(own) > 0:
				c.refuse(where, p, c.refuseCaptureBesideOwnUse(own))
			case delimited != "":
				c.refuse(where, p, delimited)
			default:
				if capture(where, p) {
					c.place(where, name, nil, p)
				}
			}
		}
	}
	for k := range captured {
		c.captured[k] = true
	}
}

// headerNoCapture says why a capture would be refused in place of the use p
// of a header value v (ps its placeholders, n of them captures), "" where
// one can stand: the response refuses it as it would the capture written.
func (c *checker) headerNoCapture(name, v string, p Placeholder, ps []Placeholder, n int, captures map[string]bool) string {
	if c.urlDelimiter(name, v, p) != "" {
		recorded, _ := c.run.recordedValue(p)
		return noCaptureURLDelimiter(recorded)
	}
	if n > 0 {
		return noCaptureHeaderHeld
	}
	for _, q := range ps {
		if q.Start != p.Start && !q.Capture && captures[q.Key] {
			return noCaptureOwnUse(q.Key)
		}
	}
	return ""
}

// responseCaptures are the keys a response's captures name, anywhere in it.
func (c *checker) responseCaptures(resp models.HTTPResp) map[string]bool {
	out := map[string]bool{}
	note := func(s string) {
		for _, p := range Find(s) {
			if p.Capture && c.run.Defined(p) {
				out[p.Key] = true
			}
		}
	}
	note(resp.Body)
	for _, v := range resp.Header {
		note(v)
	}
	return out
}

// body checks a flat expected body: a JSON document's keys and values, or
// text.
func (c *checker) body(body string, capture func(string, Placeholder) bool) {
	d, isJSON := parseJSON(body)
	if !isJSON {
		for _, p := range c.look("response body", body) {
			if p.Capture {
				c.refuse("response body", p, c.refuseCaptureNotLeaf(p))
				continue
			}
			c.noCapture(c.place("response body", "", nil, p), noCaptureNotJSON)
		}
		return
	}
	for _, k := range d.keys {
		where := "response body key " + k.DisplayPath()
		for _, p := range c.look(where, k.Raw(body)) {
			switch {
			case p.Capture:
				c.refuse(where, p, refuseCaptureInKey(p))
			case k.Shadowed && !isSecret(p):
				c.refuse(where, p, whyShadowedKey)
			default:
				c.noCapture(c.place(where, "", k.Path, p), noCaptureKey)
			}
		}
	}
	for _, l := range d.leaves {
		if l.Kind != LeafString && l.Kind != LeafBare {
			continue
		}
		where := l.Under("response body")
		all := Find(l.Raw(body))
		ps := c.look(where, l.Raw(body))
		for _, p := range ps {
			if l.Shadowed && !isSecret(p) {
				c.refuse(where, p, whyShadowedKey)
				continue
			}
			if !p.Capture {
				pl := c.place(where, "", l.Path, p)
				switch {
				case len(all) > 1:
					c.noCapture(pl, noCaptureNotAlone)
				case l.InArray && c.opts.IgnoreOrdering:
					c.noteProducer(pl, where, p, l.Kind == LeafBare, noCaptureArray, false, false)
				default:
					c.noteProducer(pl, where, p, l.Kind == LeafBare, "", false, false)
				}
				continue
			}
			switch {
			case l.Kind == LeafBare:
				c.refuse(where, p, c.refuseCaptureNotLeaf(p))
			case len(all) > 1:
				c.refuse(where, p, c.refuseCaptureNotAlone(p, all))
			case l.InArray && c.opts.IgnoreOrdering:
				c.refuse(where, p, whyCaptureArray)
			default:
				if capture(where, p) {
					c.place(where, "", l.Path, p)
				}
			}
		}
	}
}

// frames checks a streamed response's frames: nothing captures (the capture
// check refuses it), and each use is a variable place of its frame, at its
// JSON path when the frame's field is a JSON document.
func (c *checker) frames(chunks []models.HTTPStreamChunk, capture func(string, Placeholder) bool) {
	for i, chunk := range chunks {
		for _, f := range chunk.Data {
			where := "response " + frameWhere(i, f.Key)
			if !Contains(f.Value) && !strings.Contains(f.Value, "{{") {
				continue
			}
			if d, ok := parseJSON(f.Value); ok {
				for _, l := range d.leaves {
					lw := l.Under(where)
					for _, p := range c.look(lw, l.Raw(f.Value)) {
						switch {
						case p.Capture:
							capture(lw, p)
						case l.Shadowed && !isSecret(p):
							c.refuse(lw, p, whyShadowedKey)
						default:
							c.placeFrame(lw, l.Path, p)
						}
					}
				}
				for _, k := range d.keys {
					kw := where + " key " + k.DisplayPath()
					for _, p := range c.look(kw, k.Raw(f.Value)) {
						switch {
						case p.Capture:
							capture(kw, p)
						case k.Shadowed && !isSecret(p):
							c.refuse(kw, p, whyShadowedKey)
						default:
							c.placeFrame(kw, k.Path, p)
						}
					}
				}
				continue
			}
			for _, p := range c.look(where, f.Value) {
				if p.Capture {
					capture(where, p)
					continue
				}
				c.placeFrame(where, nil, p)
			}
		}
	}
}

// frameWhere names a field of a streamed response's frame: "body frame 0"
// for a frame that is one line (NDJSON, text), "body frame 0 data" for an
// SSE field.
func frameWhere(i int, key string) string {
	where := "body frame " + strconv.Itoa(i)
	if key != "" && !strings.EqualFold(key, "raw") {
		where += " " + key
	}
	return where
}

// unsplitStream refuses each variable in a streamed body that does not split
// into frames (StreamFrames is nil): the stream comparison reads such a body
// whole, under its own rules, so no variable in it has a place a run could
// assert. A secret there is the recorded literal, compared as before.
func (c *checker) unsplitStream(body string) {
	for _, p := range c.look("response body", body) {
		switch {
		case p.Capture:
			c.refuse("response body", p, refuseCaptureStream(p))
		case !isSecret(p):
			c.refuse("response body", p, c.refuseStreamNotFramed(p))
		}
	}
}

// urlDelimiter says why a capture in a URL-valued header (Location,
// Content-Location) is refused when its recorded value holds a delimiter of
// the part it stands in, a '/' in a path segment or a '&', '=' or '#' in a
// query value: no answer can be read there as that value. "" when it is not.
func (c *checker) urlDelimiter(name, v string, p Placeholder) string {
	if !isURLHeader(name) {
		return ""
	}
	part := urlPartAt(v, p.Start)
	recorded, _ := c.run.recordedValue(p)
	d, held := heldDelimiter(recorded, part)
	if !held {
		return ""
	}
	return fmt.Sprintf("the URL is read by its parts, and the recorded value %q holds %q, which delimits a %s: no answer can hold it there; write this part as recorded, and if a response also gives the value whole (a JSON value), capture it there", recorded, d, partName(part))
}

// setCookie checks a Set-Cookie value cookie by cookie: a capture must be a
// cookie's whole value, one per cookie, and no placeholder may sit in a
// cookie that a later one of the same identity replaces.
func (c *checker) setCookie(name, v string, capture func(string, Placeholder) bool) {
	cookies := SplitSetCookie(v)
	// The identity rule a run applies, at the values a run starts from.
	shadowed := c.run.shadowedAsRecorded(cookies, v)
	// How the value reads with its placeholders at those values.
	reading := c.readCookies(v)
	for i, ck := range cookies {
		where := "response header " + name + "." + ck.Name
		cookie := v[ck.Start:ck.End]
		for _, p := range c.look(where, cookie) {
			if shadowed[i] {
				// A secret is the literal the recording held (Run.isUse).
				if p.Capture || !isSecret(p) {
					c.refuse(where, p, whyCookieShadowed)
				}
				continue
			}
			if !p.Capture {
				start, end := ck.Start+p.Start, ck.Start+p.End
				inValue, attr := cookiePartOf(ck, start, end)
				inAttr := attr >= 0
				if !isSecret(p) {
					if why := c.cookieUseWhy(reading, ck, i, inValue, attr, start, p); why != "" {
						c.refuse(where, p, why)
						continue
					}
				}
				pl := c.place(where, name, nil, p)
				switch {
				case inAttr:
					c.noCapture(pl, noCaptureCookieAttribute)
				case start != ck.ValueStart || end != ck.ValueEnd:
					c.noCapture(pl, noCaptureCookiePart)
				default:
					c.noteProducer(pl, where, p, false, "", false, false)
				}
				continue
			}
			if ck.Start+p.Start != ck.ValueStart || ck.Start+p.End != ck.ValueEnd {
				c.refuse(where, p, refuseCaptureNotCookie(p, ck.Name))
				continue
			}
			if capture(where, p) {
				c.place(where, name, nil, p)
			}
		}
	}
	// The text of the value that is in no cookie (a part with no name=, as
	// when the whole value is one placeholder): a run reads a Set-Cookie
	// value cookie by cookie, so a variable there has no place.
	where := "response header " + name
	outside := func(text string) {
		for _, p := range c.look(where, text) {
			if p.Capture || !isSecret(p) {
				c.refuse(where, p, c.refuseOutsideCookie(p))
			}
		}
	}
	pos := 0
	for _, ck := range cookies {
		outside(v[pos:ck.Start])
		pos = ck.End
	}
	outside(v[pos:])
}

// cookiePartOf says which part of cookie ck the span [start, end) of its
// Set-Cookie value stands in: the cookie's value, or the value of one of its
// attributes (Path=/u/1), by its index in ck.Attrs (-1 for none). Neither
// holds for the cookie's name, an attribute's name, or an attribute with no
// value (HttpOnly).
func cookiePartOf(ck SetCookie, start, end int) (inValue bool, attr int) {
	if start >= ck.ValueStart && end <= ck.ValueEnd {
		return true, -1
	}
	for j, a := range ck.Attrs {
		if a.HasValue && start >= a.ValueStart && end <= a.ValueEnd {
			return false, j
		}
	}
	return false, -1
}

// cookieReading is how a Set-Cookie value reads with each placeholder of a
// key the set defines at its recorded value (a secret's, a capture's
// included): the placeholders whose recorded value alone would split it into
// other cookies or attributes than its text does (split, by start), whether
// the others, written in their places together, would (together), and when
// they would not, the cookies of that rendered value and each placeholder's
// span in it.
type cookieReading struct {
	split    map[int]bool
	together bool
	cookies  []SetCookie
	spans    map[int][2]int
}

// readCookies reads the Set-Cookie value v at its recorded values
// (cookieReading).
func (c *checker) readCookies(v string) cookieReading {
	ps := Find(v)
	shape := cookieShape(v)
	recorded := func(p Placeholder) (string, bool) {
		if !c.run.Defined(p) {
			return "", false
		}
		if _, bad := c.run.Unusable(p); bad {
			return "", false
		}
		return c.run.recordedValue(p)
	}
	r := cookieReading{split: map[int]bool{}, spans: map[int][2]int{}}
	for _, p := range ps {
		if val, ok := recorded(p); ok && cookieShape(v[:p.Start]+val+v[p.End:]) != shape {
			r.split[p.Start] = true
		}
	}
	var b strings.Builder
	last := 0
	for _, p := range ps {
		val, ok := recorded(p)
		if !ok || r.split[p.Start] {
			continue
		}
		b.WriteString(v[last:p.Start])
		at := b.Len()
		b.WriteString(val)
		r.spans[p.Start] = [2]int{at, b.Len()}
		last = p.End
	}
	b.WriteString(v[last:])
	rendered := b.String()
	if cookieShape(rendered) != shape {
		r.together = true
		return r
	}
	r.cookies = SplitSetCookie(rendered)
	return r
}

// cookieUseWhy says why the use p starting at start of a Set-Cookie value, in
// ck, its cookie i (ck's value, or its attribute attr), cannot be asserted
// where it stands, "" when it can; reading is how the value reads at its
// recorded values. A run reads the answer's cookie of the same identity: its
// value and, of each attribute name, the last attribute of that name
// (SetCookie.Attr, as a user agent keeps it), each without the spaces around
// it. So a use is refused in a name or a flag, in an attribute a later one of
// its name overrides, where the recorded values written in place of the
// value's placeholders (its own, or several side by side) would split the
// value into other cookies or attributes, or where its own recorded value,
// so written, falls outside what a run reads of its part (a space, or an
// empty value, at the part's edge).
func (c *checker) cookieUseWhy(reading cookieReading, ck SetCookie, i int, inValue bool, attr, start int, p Placeholder) string {
	if !inValue && attr < 0 {
		return c.refuseInCookieName(p)
	}
	if attr >= 0 {
		a := ck.Attrs[attr]
		if last, _ := ck.Attr(a.Name); last.ValueStart != a.ValueStart {
			return c.refuseOverriddenAttr(p, a.Name)
		}
	}
	if reading.split[start] || reading.together {
		return c.refuseCookieSplit(p)
	}
	// The same shape, so the same cookie and attribute indexes.
	rck := reading.cookies[i]
	ps, pe := rck.ValueStart, rck.ValueEnd
	if attr >= 0 {
		ps, pe = rck.Attrs[attr].ValueStart, rck.Attrs[attr].ValueEnd
	}
	if span := reading.spans[start]; span[0] < ps || span[1] > pe {
		return c.refuseCookieSpace(p)
	}
	return ""
}

// cookieShape is how a Set-Cookie value splits: each cookie's name and each
// of its attributes' names, with whether the attribute has a value, and each
// stretch of text between them that is in no cookie (a line with no name=).
func cookieShape(v string) string {
	var b strings.Builder
	pos := 0
	gap := func(text string) {
		if strings.Trim(text, " \t\r\n,") != "" {
			b.WriteString("\x02")
		}
	}
	for _, ck := range SplitSetCookie(v) {
		gap(v[pos:ck.Start])
		pos = ck.End
		b.WriteString(ck.Name)
		for _, a := range ck.Attrs {
			b.WriteString("\x00")
			b.WriteString(a.Name)
			if a.HasValue {
				b.WriteString("=")
			}
		}
		b.WriteString("\x01")
	}
	gap(v[pos:])
	return b.String()
}

// assertions refuses each variable in a test case's assertions, their names
// and noise entries included: the comparison reads an assertion as written
// (httpMatcher.AssertionMatch), never at a key's value, so a variable there
// would be neither sent nor asserted. A secret there is text as well, and is
// left as it is.
func (c *checker) assertions(tc *models.TestCase) {
	EachString(tc.Assertions, func(path, text string) {
		where := "assertions"
		if path != "" {
			where += " " + path
		}
		for _, p := range c.look(where, text) {
			if p.Capture || !isSecret(p) {
				c.refuse(where, p, c.refuseInAssertion(p))
			}
		}
	})
}

// EachString calls visit with every string v holds, however nested: a map's
// keys and values (the keys in sorted order), a list's elements, a struct's
// exported fields. path names where the string is, a key or a field joined by
// '.', an element by its index; a key's own text is visited at the path of
// the value it names.
func EachString(v interface{}, visit func(path, text string)) {
	eachString("", reflect.ValueOf(v), visit)
}

func eachString(path string, v reflect.Value, visit func(path, text string)) {
	for v.IsValid() && (v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer) {
		if v.IsNil() {
			return
		}
		v = v.Elem()
	}
	if !v.IsValid() {
		return
	}
	switch v.Kind() {
	case reflect.String:
		visit(path, v.String())
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			eachString(path+"["+strconv.Itoa(i)+"]", v.Index(i), visit)
		}
	case reflect.Map:
		keys := v.MapKeys()
		names := make([]string, len(keys))
		for i, k := range keys {
			names[i] = fmt.Sprint(k.Interface())
		}
		order := make([]int, len(keys))
		for i := range order {
			order[i] = i
		}
		sort.SliceStable(order, func(a, b int) bool { return names[order[a]] < names[order[b]] })
		for _, i := range order {
			at := names[i]
			if path != "" {
				at = path + "." + names[i]
			}
			visit(at, names[i])
			eachString(at, v.MapIndex(keys[i]), visit)
		}
	case reflect.Struct:
		// A document a store decodes into structs (bson.D's {Key, Value}
		// elements): each exported field, by its name.
		for i := 0; i < v.NumField(); i++ {
			if f := v.Type().Field(i); f.IsExported() {
				at := f.Name
				if path != "" {
					at = path + "." + f.Name
				}
				eachString(at, v.Field(i), visit)
			}
		}
	}
}

// grpc checks a gRPC test case: nothing captures, and each use in its
// response is a variable place (a header, or the body). A variable in a
// header's name, or anywhere in the response's trailers, is refused: names
// are matched as written, and the gRPC comparison reads only grpc-status of
// the trailers.
func (c *checker) grpc(tc *models.TestCase) {
	c.streaming = false
	check := func(where, header string, response bool, text string) {
		for _, p := range c.look(where, text) {
			switch {
			case p.Capture:
				c.refuse(where, p, refuseCaptureGRPC(p))
			case response:
				c.place(where, header, nil, p)
			case !isSecret(p):
				if c.res.Sends[c.tc.Name] == nil {
					c.res.Sends[c.tc.Name] = map[string]bool{}
				}
				c.res.Sends[c.tc.Name][p.Key] = true
			}
		}
	}
	for _, h := range []struct {
		where    string
		m        map[string]string
		response bool
	}{
		{"request pseudo header ", tc.GrpcReq.Headers.PseudoHeaders, false},
		{"request header ", tc.GrpcReq.Headers.OrdinaryHeaders, false},
		{"response pseudo header ", tc.GrpcResp.Headers.PseudoHeaders, true},
		{"response header ", tc.GrpcResp.Headers.OrdinaryHeaders, true},
	} {
		c.names(h.where+"name", sortedKeys(h.m))
		for _, k := range sortedKeys(h.m) {
			check(h.where+k, k, h.response, h.m[k])
		}
	}
	for _, h := range []struct {
		where string
		m     map[string]string
	}{
		{"response trailer ", tc.GrpcResp.Trailers.PseudoHeaders},
		{"response trailer ", tc.GrpcResp.Trailers.OrdinaryHeaders},
	} {
		c.names(h.where+"name", sortedKeys(h.m))
		for _, k := range sortedKeys(h.m) {
			for _, p := range c.look(h.where+k, h.m[k]) {
				switch {
				case p.Capture:
					c.refuse(h.where+k, p, refuseCaptureGRPC(p))
				case !isSecret(p):
					c.refuse(h.where+k, p, c.refuseInTrailer(p))
				}
			}
		}
	}
	check("request body", "", false, tc.GrpcReq.Body.DecodedData)
	for _, m := range tc.GrpcReq.AdditionalMessages {
		check("request body", "", false, m.DecodedData)
	}
	check("response body", "", true, tc.GrpcResp.Body.DecodedData)
	for _, m := range tc.GrpcResp.AdditionalMessages {
		check("response body", "", true, m.DecodedData)
	}
}

func isSecret(p Placeholder) bool {
	_, ok := p.Secret()
	return ok
}

// RefusedNames is the names of the refused test cases, sorted.
func RefusedNames(refused map[string][]Refusal) []string {
	names := make([]string, 0, len(refused))
	for n := range refused {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// producerAdvice is the next step for a use written where its value is
// issued, which nothing captures (a set templatized by an older keploy wrote
// one there): one edit for each thing the value can be. A value the app
// issues anew each run is captured there; a constant, or what the request
// sent, is written as recorded in place of every placeholder of its key.
// Where a capture cannot stand (noCaptureWhy says why: a part of a
// URL-valued header whose recorded value holds a delimiter of that part,
// such as a whole Location "/jobs/j_1", inside an array under
// ignoreOrdering, beside a capture in a header value), only the second edit
// is given, and the value can be captured only where a response gives it on
// its own, if one does. shared says the place's header value holds other
// placeholders, of which at most one can become a capture. passedOver says
// the place is a part of a URL-valued header outside a 201's Location
// (Place.PassedOver), the producer only when no other place of its response
// could be: the capture edit then says what a capture there gives up.
func producerAdvice(testCase, where string, p Placeholder, recorded string, bare bool, noCaptureWhy string, shared, passedOver bool) string {
	literal := fmt.Sprintf("write its recorded value %q in place of every placeholder of %s", recorded, p.Key)
	if noCaptureWhy != "" {
		return fmt.Sprintf("a capture cannot stand at %s/%s (%s): %s; if a response also gives the value whole (a JSON value), capture it there",
			testCase, where, noCaptureWhy, literal)
	}
	return fmt.Sprintf("if the app issues this value anew each run: change %s at %s/%s to %s%s%s; if it is a constant or what the request sent: %s",
		p.Text, testCase, where, captureForValue(p, bare, recorded), oneCapture(shared), targetCapture(passedOver), literal)
}

// targetCapture is what a capture edit adds for a part of a URL-valued
// header outside a 201's Location (a redirect's target, a Content-Location):
// keploy templatize never writes one there, since it accepts any target.
func targetCapture(passedOver bool) string {
	if !passedOver {
		return ""
	}
	return " (a capture in that header checks only that a value stands there, so any target passes there, a regression included)"
}

// oneCapture is what a capture edit adds for a header value that holds other
// placeholders too: it holds one capture.
func oneCapture(shared bool) string {
	if !shared {
		return ""
	}
	return " (that header value holds other placeholders and one capture: change at most one of them)"
}

// placeOf is the variable place of testCase that the failure f names: the one
// at its place (a failure names it as the test case writes it), or else,
// where f names it by this run's values (a place under an object key that
// holds a placeholder, as a report does), the test case's one place holding
// its placeholder; nil when that is not one place.
func (c CheckResult) placeOf(testCase string, f Failure) *Place {
	var same *Place
	n := 0
	for i := range c.Places {
		pl := &c.Places[i]
		if pl.TestCase != testCase || pl.Placeholder.Text != f.Placeholder {
			continue
		}
		if pl.Where == "response "+f.Where {
			return pl
		}
		same = pl
		n++
	}
	if n == 1 {
		return same
	}
	return nil
}

// weakPlace reports whether a capture at a Producer place is refused there
// (NoCaptureWhy) or is one keploy templatize passes over (PassedOver).
func weakPlace(pl *Place) bool { return pl.NoCaptureWhy != "" || pl.PassedOver }

// newKey is a key named after key that config.yaml's template does not
// define: key2, key3, ...
func (c CheckResult) newKey(key string) string {
	for n := 2; ; n++ {
		if k := key + strconv.Itoa(n); !c.keys[k] {
			return k
		}
	}
}

// captureFor is the capture that replaces a use p: of p's type, in quotes
// when p is written outside a JSON string.
func captureFor(p Placeholder, bare bool) string {
	return captureForValue(p, bare, "")
}

// captureForValue is captureFor for a use whose value as recorded is
// recorded: a use that names no type ({"n":{{.n}}}, as an older templatize
// wrote a number) takes the JSON type of what it was recorded as, so the
// capture holds the number the app answers there.
func captureForValue(p Placeholder, bare bool, recorded string) string {
	typ := p.Type
	if typ == "" {
		typ = TypeString
		if bare {
			switch {
			case fitsType(TypeInt, recorded):
				typ = TypeInt
			case fitsType(TypeFloat, recorded):
				typ = TypeFloat
			}
		}
	}
	capture := Format(true, typ, p.Key)
	if bare {
		capture = `"` + capture + `"`
	}
	return capture
}

// NextStep is what to do about a variable place f that a run found the
// answer does not satisfy in testCase, said for both things it can mean: the
// app issues the value anew each run, so the place that issues it must
// capture it (named, with the edit); or the value changed on purpose, so the
// expected value is edited or the test case re-recorded. It never says "run
// keploy normalize": normalize skips a HIGH-risk failure, and with
// --allow-high-risk writes this run's value over the placeholder, which the
// next run fails on. canCapture is false for a test case that cannot capture
// (gRPC, streamed).
func (c CheckResult) NextStep(testCase string, f Failure, canCapture bool) string {
	// The failing place as the test case writes it (Failure.Where: body key
	// roles.{{string .id}}, not this run's value of the key, roles.u_LIVE2,
	// which the report shows), as the set check names it: where the edit
	// goes.
	at := c.placeOf(testCase, f)
	here := testCase + "/response " + f.Where
	if at != nil {
		here = testCase + "/" + at.Where
	}
	changed := "if the value changed on purpose, edit the expected value at " + here + " or re-record the test case"
	ps := Find(f.Placeholder)
	if len(ps) != 1 {
		return changed
	}
	p := ps[0]
	if p.Capture {
		return "the answer has no value of the capture's type there: " + changed
	}
	// The places are in the order a run reaches them, each test case's
	// together: a capture of the key in testCase's own response gave the
	// value the run asserted (captures apply before uses), else the last
	// capture of it before testCase did; and the first use at a place that
	// could capture it is where an older templatize wrote the producer.
	var captured, producer *Place
	reached := false
	for i := range c.Places {
		pl := &c.Places[i]
		if pl.TestCase == testCase {
			reached = true
		} else if reached {
			break
		}
		if pl.Placeholder.Key != p.Key {
			continue
		}
		switch {
		case pl.Placeholder.Capture:
			captured = pl
		case pl.Producer && (producer == nil || (pl.TestCase == producer.TestCase && weakPlace(producer) && !weakPlace(pl))):
			// The first response with a use where a capture could stand
			// names the producer: its first such place that is not weak,
			// as Check's firstUse picks it.
			producer = pl
		}
	}
	switch {
	case captured != nil && captured.TestCase == testCase:
		// The same answer gave the value and holds another here: one more
		// capture of the key would contradict the first.
		return fmt.Sprintf("this run's value of %s is what %s/%s captured from this same answer, and the answer here holds another: %s", p.Key, captured.TestCase, captured.Where, changed)
	case captured != nil && c.Sends[testCase][p.Key]:
		// A read-back: this test case's own request sent the value, and the
		// answer holds another. A capture here would accept whatever the
		// app answers and send it on, so it is never offered.
		return fmt.Sprintf("this run's value of %s is what %s/%s captured, and this test case's own request sent it: the app answered a different value than the one captured at %s/%s; check the read-back at %s, or the producer at %s/%s; %s",
			p.Key, captured.TestCase, captured.Where, captured.TestCase, captured.Where, here, captured.TestCase, captured.Where, changed)
	case captured != nil && canCapture && at != nil && at.Producer && at.NoCaptureWhy == "":
		// Two edits, one per thing this place can be. A capture of the same
		// key would re-point every later use of it to this answer, so a
		// read-back after it would send this place's value, not the one its
		// producer issued: right only where this place replaces the value
		// from then on. A value of its own takes a key of its own.
		fresh := at.Placeholder
		fresh.Key = c.newKey(p.Key)
		return fmt.Sprintf("this run's value of %s is what %s/%s captured, and the answer here holds another: if this place issues a value of its own each run, capture it under a key of its own: change %s at %s to %s%s%s, give %s its recorded value %q in config.yaml's template, and write %s where later requests send this place's value; if this place replaces %s from then on (a refreshed session cookie), change %s there to %s instead, which points every later use of %s at this answer; %s",
			p.Key, captured.TestCase, captured.Where, p.Text, here, captureForValue(fresh, at.Bare, at.Recorded), oneCapture(at.Shared), targetCapture(at.PassedOver), fresh.Key, at.Recorded, useFor(fresh),
			p.Key, p.Text, captureForValue(at.Placeholder, at.Bare, at.Recorded), p.Key, changed)
	case captured != nil && at != nil && at.NoCaptureWhy != "":
		return fmt.Sprintf("this run's value of %s is what %s/%s captured, and the answer here holds another (a capture cannot stand at %s: %s): %s",
			p.Key, captured.TestCase, captured.Where, here, at.NoCaptureWhy, changed)
	case captured != nil:
		return fmt.Sprintf("this run's value of %s is what %s/%s captured, and the answer here holds another: %s", p.Key, captured.TestCase, captured.Where, changed)
	case producer != nil && producer.NoCaptureWhy != "":
		return fmt.Sprintf("nothing captures %s, so every run expects its recorded value, and a capture cannot stand at %s/%s (%s): if a response also gives the value whole (a JSON value), capture it there; %s",
			p.Key, producer.TestCase, producer.Where, producer.NoCaptureWhy, changed)
	case producer != nil:
		return fmt.Sprintf("nothing captures %s, so every run expects its recorded value: if the app issues this value anew each run, make the producer a capture: change %s at %s/%s to %s%s%s; %s",
			p.Key, producer.Placeholder.Text, producer.TestCase, producer.Where, captureForValue(producer.Placeholder, producer.Bare, producer.Recorded), oneCapture(producer.Shared), targetCapture(producer.PassedOver), changed)
	}
	return fmt.Sprintf("nothing captures %s before this test case, so every run expects its recorded value here: if the app issues this value anew each run, mark the place of an earlier response that issues it %s; %s",
		p.Key, captureFor(p, false), changed)
}
