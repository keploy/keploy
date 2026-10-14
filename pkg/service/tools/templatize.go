package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/invopop/yaml"
	"go.keploy.io/server/v3/pkg"
	matcherUtils "go.keploy.io/server/v3/pkg/matcher"
	httpMatcher "go.keploy.io/server/v3/pkg/matcher/http"
	"go.keploy.io/server/v3/pkg/models"
	setfile "go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/configdb/testset"
	"go.keploy.io/server/v3/pkg/templates"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// PartType names the part of a test case a value sits in.
type PartType int

// The parts of a test case.
const (
	RequestHeader PartType = iota
	RequestURL
	RequestBody
	ResponseHeader
	ResponseBody
)

func (pt PartType) String() string {
	return [...]string{"Request Header", "Request URL", "Request Body", "Response Header", "Response Body"}[pt]
}

// ValueLocation is where a templatized value sits, for the chain printout and
// the ASSERT_CHAINS_WITH comparison.
type ValueLocation struct {
	TestCaseIndex int
	Part          PartType
	Path          string
	OriginalType  string
}

// TemplateChain is one value templatize made a template key of: the response
// that produces it and the later requests that send it.
type TemplateChain struct {
	TemplateKey string
	Value       string
	Producer    *ValueLocation
	Consumers   []*ValueLocation
}

// --- Canonical Structs for Assertion ---

type CanonicalChain struct {
	VariableName string              `yaml:"variable_name"`
	Value        string              `yaml:"value"`
	Producer     CanonicalProducer   `yaml:"producer"`
	Consumers    []CanonicalConsumer `yaml:"consumers"`
}

type CanonicalProducer struct {
	RequestID string `yaml:"request_id"`
	Part      string `yaml:"part"`
	Path      string `yaml:"path"`
}

type CanonicalConsumer struct {
	RequestID string `yaml:"request_id"`
	Part      string `yaml:"part"`
	Path      string `yaml:"path"`
}

// field names the text of a test case a location's span is in.
type field struct {
	kind       fieldKind
	name       string // header name
	form, item int    // multipart form field and value index
}

type fieldKind int

const (
	fieldURL fieldKind = iota
	fieldReqHeader
	fieldReqBody
	fieldForm
	fieldRespHeader
	fieldRespBody
)

// location is one place a value sits, with the span templatize writes over.
type location struct {
	tc    int
	part  PartType
	path  string // as printed: "body.items[0].id", "Cookie.sid", "path.1"
	value string
	// jsonType is "string", "int" or "float" for a JSON leaf; "" elsewhere,
	// where a value is always text.
	jsonType string
	// nameHint is what a key made here is named after.
	nameHint string
	field    field
	// start and end are the span in the field's text; for a JSON leaf the
	// whole leaf, a string's quotes included.
	start, end int
	inArray    bool
	// keys are a JSON leaf's object keys along its path, without its array
	// indices: what a body noise entry names.
	keys []string
	// unholdable marks a part of a URL-valued response header whose value
	// holds a delimiter of that part (a%2Fb in a path segment, a%26b in a
	// query value): the replay cannot tell it from two parts, so no
	// placeholder is written there. It still counts as a producer candidate
	// of its header (soleURLProducers), so the header's other parts are not
	// taken for the value the app issues.
	unholdable bool
	// status is, for a part of a URL-valued response header, its response's
	// status code: only the Location of a 201 names a resource the request
	// created (createdURL).
	status int
	// prevSeg is, for a URL path segment, the text of the segment before it,
	// which a key made here is named after unless it is a value itself.
	prevSeg string
}

func (l *location) inResponse() bool { return l.part == ResponseHeader || l.part == ResponseBody }

// valueType is the type a place's value has: its JSON type, or "string"
// for text in a URL, a header or a form.
func (l *location) valueType() string {
	if l.jsonType == "" {
		return templates.TypeString
	}
	return l.jsonType
}

// typesCompatible reports whether a value produced at p may be written at c:
// a JSON number only where a JSON number goes, a string only where text does.
// A number never flows into a URL, a header or a form, where it would stand
// for text the app reads as a string.
func typesCompatible(p, c *location) bool {
	pt, ct := p.valueType(), c.valueType()
	if isNum(pt) && isNum(ct) {
		return true
	}
	return pt == ct
}

func isNum(t string) bool { return t == templates.TypeInt || t == templates.TypeFloat }

// clientHeaders are request headers the client sets itself, from the request,
// its connection or its own settings, never from what an earlier answer said.
// Their value is never a template, whatever it equals.
var clientHeaders = map[string]bool{
	"content-length":    true,
	"host":              true,
	"connection":        true,
	"transfer-encoding": true,
	"accept-encoding":   true,
	"content-encoding":  true,
	"date":              true,
	"content-type":      true,
	"accept":            true,
	"accept-language":   true,
	"user-agent":        true,
	"origin":            true,
	"referer":           true,
}

// edit is one placeholder written over a span.
type edit struct {
	start, end int
	text       string
}

// ProcessTestCasesV2 is keploy templatize for one test set. It finds each value
// a response produces and a later request sends, and writes it as a template
// key: {{capture ...}} at the response that produces it (the app's answer
// there is the key's value from then on), a use placeholder at each later
// request that sends it, and a use at a later response only where that test
// case's own request sent the key (a read-back answers what it was asked
// for) and keploy test can tie it to its place (untiedInArray). config.yaml's
// template gets each key's recorded value.
//
// Same recording, same files: places are visited in a fixed order (test case,
// then request before response, then a fixed order within each), so which
// field produces a value never depends on map order. Only structural places
// are read: whole JSON values, URL path segments and query values, cookie
// values, a Bearer token, whole request header and form values, and the
// Set-Cookie cookies and the Location of a response (which captures only on a
// 201, soleURLProducers). A JSON number is written only
// where a JSON number goes. A test case with nothing to template is not
// rewritten.
//
// It writes only into a plain recording: a set that holds any template text
// already, anywhere in a test case file or in config.yaml's template (heldIn:
// a placeholder an older keploy templatize wrote, a person wrote, or this one
// wrote on an earlier run), is left exactly as it is, and one line names the
// set, its first placeholder and what to do. A secret keploy sanitize wrote
// ({{string .secret.K}}) is the literal it redacts, no template text, and is
// left as it is in a set templatize writes into.
//
// It writes all of a set or none of it: the test cases it changes and
// config.yaml are replaced together (TestSetFiles.ReplaceFiles), so a run that
// fails, at a config.yaml that does not parse or at a file that cannot be
// written, leaves the set as it was, and the next run starts from the same
// plain recording. tcs are changed only once the files are in place.
func (t *Tools) ProcessTestCasesV2(ctx context.Context, tcs []*models.TestCase, testSetID string) error {
	setFiles, confFile, err := t.setFiles()
	if err != nil {
		utils.LogError(t.logger, err, "keploy templatize cannot write this test store, so it changed nothing",
			zap.String("testSet", testSetID),
			zap.String("next_step", "run keploy templatize on the test sets as files (the keploy directory a recording writes)"))
		return err
	}
	// The read that reports a config.yaml it cannot parse (Read hands back an
	// empty one): templatize replaces that file, and reads its keys.
	conf, err := t.testSetConf.ReadForUpdate(ctx, testSetID)
	if err != nil {
		utils.LogError(t.logger, err, "failed to read the test set's config.yaml; keploy templatize changed nothing in this set",
			zap.String("testSet", testSetID),
			zap.String("next_step", "fix config.yaml so that it reads (the error says what is wrong with it), then run keploy templatize again"))
		return err
	}
	existing := map[string]interface{}{}
	if conf != nil {
		for k, v := range conf.Template {
			existing[k] = v
		}
	}

	held, err := heldIn(ctx, setFiles, testSetID, existing)
	if err != nil {
		utils.LogError(t.logger, err, "failed to read the test set's files; keploy templatize changed nothing in this set",
			zap.String("testSet", testSetID),
			zap.String("next_step", "do what the error says, then run keploy templatize again"))
		return err
	}
	if held != nil {
		t.logger.Warn(fmt.Sprintf("%s holds template placeholders already (%s at %s): keploy templatize writes only into a plain recording, so it changes nothing in this set",
			testSetID, held.Text, held.Where),
			zap.String("next_step", heldNextStep))
		return nil
	}
	locs := indexLocations(tcs)
	plan := planChains(t.logger, testSetID, tcs, locs, existing, t.config.Test.IgnoreOrdering, t.untiedInArray(testSetID, tcs))
	t.sayCrowded(testSetID, tcs, plan.crowded)
	if len(plan.chains) == 0 {
		// A set with a value left for the user to edit, each said in its own
		// line, is not one with nothing to templatize.
		if len(plan.crowded) == 0 && plan.left == 0 {
			t.logger.Info("found no value a response produces and a later request sends; nothing to templatize", zap.String("testSet", testSetID))
		}
		return nil
	}

	// Every file the run changes, written out before any is put in place: the
	// test cases with their placeholders, on copies, and config.yaml.
	edited := applyEdits(tcs, plan.edits)
	changed := make([]int, 0, len(edited))
	for i := range edited {
		changed = append(changed, i)
	}
	sort.Ints(changed)
	var files []setfile.File
	for _, i := range changed {
		f, err := setFiles.TestCaseFile(ctx, edited[i], testSetID)
		if err != nil {
			utils.LogError(t.logger, err, "failed to write a test case; keploy templatize changed nothing in this set",
				zap.String("testSet", testSetID), zap.String("testCase", tcs[i].Name))
			return err
		}
		files = append(files, f)
	}

	for k, v := range plan.newKeys {
		existing[k] = v
	}
	// READ-MODIFY-WRITE, not a fresh struct. Write replaces the whole
	// config.yaml, and a literal that names the fields it keeps silently
	// drops every field it forgets (AppCommand, PreScript, PostScript).
	// Mutating what was read cannot drop one. The store it writes through
	// hands the file to the group replaced below.
	staged := stagedConfig{TestSetConfig: t.testSetConf, file: confFile, add: func(f setfile.File) { files = append(files, f) }}
	if err := testset.WriteTemplatedConfig(ctx, staged, testSetID, existing); err != nil {
		utils.LogError(t.logger, err, "failed to write test set; keploy templatize changed nothing in this set", zap.String("testSet", testSetID))
		return err
	}
	if err := setFiles.ReplaceFiles(ctx, files); err != nil {
		utils.LogError(t.logger, err, "failed to put the templatized files in place",
			zap.String("testSet", testSetID),
			zap.String("next_step", "the error says whether every file is as it was (it is, unless it names a file that could not be put back); fix what it names (a full disk, a file another program holds open or that is not writable), then run keploy templatize again"))
		return err
	}
	for i, tc := range edited {
		*tcs[i] = *tc
	}

	secrets, err := t.testSetConf.ReadSecret(ctx, testSetID)
	if err != nil {
		t.logger.Debug("Failed to read the test set's secrets", zap.Error(err))
	}
	if len(secrets) > 0 {
		if err := utils.AddToGitIgnore(t.logger, t.config.Path, "/*/secret.yaml"); err != nil {
			t.logger.Debug("Failed to add secret files to .gitignore", zap.Error(err))
		}
	}

	t.logAPIChains(plan.chains, tcs)
	// What a capture gives up, said once per capture written: the producer's
	// field is no longer compared, only checked.
	for _, c := range plan.captures {
		t.logger.Info(fmt.Sprintf("%s/%s %s now captures %s: its value is no longer compared, only that it is present, of type %s, and not empty",
			testSetID, tcs[c.tc].Name, c.path, c.key, c.typ))
	}
	t.sayRepeats(testSetID, tcs, plan.captures)

	if fuzzerYamlPath := os.Getenv("ASSERT_CHAINS_WITH"); fuzzerYamlPath != "" {
		fmt.Println("Asserting chains with fuzzer YAML:", fuzzerYamlPath)
		t.AssertChains(plan.chains, tcs, fuzzerYamlPath)
	}
	return nil
}

// untiedInArray says why a use templatize would write at l, a place of a
// later response's JSON body, could not be tied to its element when the set
// is replayed ("" when it could be): inside an array under test.ignoreOrdering, with
// --schema-match set, a noise entry over the place, or assertions that judge
// the test case without reading the whole body
// (httpMatcher.AssertionsUntieBody, the predicate keploy test refuses by),
// any element holding the value would satisfy it, and keploy test refuses
// such a use when the set loads. The value is then left as recorded, as it
// was before templatize ran. Without these the comparison ties the use to
// its element, and it is written.
func (t *Tools) untiedInArray(testSetID string, tcs []*models.TestCase) func(*location) string {
	if !t.config.Test.IgnoreOrdering {
		return func(*location) string { return "" }
	}
	body := map[string][]string{}
	for k, v := range t.config.Test.GlobalNoise.Global["body"] {
		body[k] = v
	}
	for k, v := range t.config.Test.GlobalNoise.Testsets[testSetID]["body"] {
		body[k] = v
	}
	global := map[string]map[string][]string{"body": body}
	return func(l *location) string {
		if l.part != ResponseBody || !l.inArray {
			return ""
		}
		if t.config.Test.SchemaMatch {
			return "test.schemaMatch is on, which compares no value"
		}
		if entry := matcherUtils.BodyNoiseCovering(l.keys, tcs[l.tc].Noise, global, t.logger); entry != "" {
			return "the noise entry " + entry + " covers it"
		}
		if names := httpMatcher.AssertionsUntieBody(tcs[l.tc]); names != "" {
			return "the test case's assertions (" + names + ") judge its answer without reading the whole body"
		}
		return ""
	}
}

// heldNextStep is what to do about a set that holds placeholders already,
// which templatize leaves as it is: keploy test replays it as written, and an
// older keploy's set (V2 templates, never migrated) sends and asserts each
// key's recorded value. The edit that gives the plain recording back writes
// each placeholder as the text it was recorded as, which for a number
// templatize wrote in quotes ("{{capture int .id}}" over 1) is the number
// without them.
const heldNextStep = `if this keploy templatize wrote them, nothing to do. Otherwise (a set an older keploy templatized, or one a person edited), re-record the set and run keploy templatize on the new recording; or write back, in place of each placeholder, what the recording held there, and run keploy templatize again: config.yaml's template has each key's recorded value, a placeholder of type int or float that stands in quotes as a whole JSON value ("{{int .n}}", "{{capture float .n}}") goes back as the bare number, without the quotes, a value inside a JSON string goes back with JSON's escapes (a " as \", a \ as \\), and an expression goes back as the text it rendered. If the text is the app's own (a mail template, a chart the API serves), there is nothing to write back: templatize cannot tell it from a placeholder, and leaves this set as it is`

// sayCrowded says the values of each URL-valued header templatize captures
// nothing in though a later request sends one (a crowded 201 Location, or any
// other Location or Content-Location), one line per header naming each
// value, with the edit for a value the app issues anew.
func (t *Tools) sayCrowded(testSetID string, tcs []*models.TestCase, crowded []crowdedHeader) {
	for _, c := range crowded {
		names := make([]string, 0, len(c.candidates))
		for _, l := range c.candidates {
			names = append(names, fmt.Sprintf("%s %q", l.path, l.value))
		}
		if c.created {
			t.logger.Info(fmt.Sprintf("%s/%s response header %s holds more than one value its response gives first (%s), and a later request sends one of them: templatize captures none of them, since a header holds one capture and one recording cannot tell which value the app issues anew",
				testSetID, tcs[c.tc].Name, c.header, strings.Join(names, ", ")),
				zap.String("next_step", crowdedNextStep(c)))
			continue
		}
		t.logger.Info(fmt.Sprintf("%s/%s response header %s of a %d response holds %s, which a later request sends: templatize captures nothing there and it stays compared as recorded, since only the Location of a 201 (Created) response names a resource the request created (RFC 9110), and a capture would accept any value there, another target included",
			testSetID, tcs[c.tc].Name, c.header, c.status, strings.Join(names, ", ")),
			zap.String("next_step", crowdedNextStep(c)))
	}
}

// crowdedNextStep is the edit for a header templatize captures nothing in,
// said with the key templatize would give each value (the one config.yaml
// gives it already, or a new one): capture the one value the app issues
// anew, give a new key the value it replaces, and send the key where later
// requests send the value. A candidate that holds a delimiter of its part
// cannot be captured there, which is said instead of its edit. A candidate
// its response captures elsewhere (c.own) is offered a use of that capture
// instead, for a value the app issues anew (templatize writes none: it
// would accept whatever that capture takes, a constant's regression
// included), and then no capture is offered beside it.
func crowdedNextStep(c crowdedHeader) string {
	var uses []string
	for i, l := range c.candidates {
		if i < len(c.own) && c.own[i] != "" {
			uses = append(uses, fmt.Sprintf("%s in place of %q", templates.Format(false, templates.TypeString, c.own[i]), l.value))
		}
	}
	if len(uses) > 0 {
		// The value its response captures elsewhere can be asserted here by a
		// use of that capture; a capture of any value beside such a use is
		// refused, so none is offered. The use is the user's choice: it
		// accepts whatever the response's capture takes, so it is right only
		// for a value the app issues anew, and a constant stays as recorded.
		target := ""
		if c.redirect() {
			target = " (a redirect's target)"
		}
		return "if the value is one its response issues anew each run, write " + strings.Join(uses, " and ") + ": the header then asserts it names the value its response captures, and every other value of it stays as recorded (a header value that holds a use of a key its response captures holds no capture); if it is the same every run" + target + ", nothing to do"
	}
	var edits, whole, fresh []string
	for i, l := range c.candidates {
		if l.unholdable {
			whole = append(whole, strconv.Quote(l.value))
			continue
		}
		key := c.keys[i]
		edits = append(edits, fmt.Sprintf("%s in place of %q (and %s where later requests send it)",
			templates.Format(true, templates.TypeString, key), l.value, templates.Format(false, templates.TypeString, key)))
		if !c.reused[i] {
			fresh = append(fresh, key)
		}
	}
	next := "if these values are the same every run, nothing to do"
	switch {
	case c.redirect():
		next = "if the value is the same every run (a redirect's target), nothing to do"
	case !c.created:
		next = "if the value is the same every run, nothing to do"
	}
	if len(edits) > 0 {
		capture := "capture the one value the app issues anew each run (an id, a code): write " + strings.Join(edits, ", or ")
		if !c.created {
			capture = "if the app issues one anew each run (a code, a job id), capture it, which then checks only that a value stands there: write " + strings.Join(edits, ", or ")
		}
		if len(fresh) > 0 {
			capture += ", and if its key is new (" + strings.Join(fresh, ", ") + "), give it the value it replaced in config.yaml's template"
		}
		next = capture + "; " + next
	}
	if len(whole) > 0 {
		next += "; " + strings.Join(whole, ", ") + " holds a delimiter of its part and cannot be captured here; if a response also gives it whole (a JSON value), capture it there"
	}
	return next
}

// indexLocations lists every structural place of every HTTP test case, in
// the fixed order templatize reads them. A place holding a placeholder holds
// a secret (templatize writes only into a set that holds no other): it is
// the literal keploy sanitize redacted, and is left out.
func indexLocations(tcs []*models.TestCase) []*location {
	var all []*location
	for i, tc := range tcs {
		if tc == nil || tc.Kind != models.HTTP {
			continue
		}
		all = append(all, requestLocations(i, tc.HTTPReq)...)
		// A streamed response is compared after every other test case and
		// never captures, so it produces nothing.
		if !pkg.IsHTTPStreamingTestCase(tc) {
			all = append(all, responseLocations(i, tc.HTTPResp)...)
		}
	}
	out := all[:0]
	for _, l := range all {
		if !templates.Contains(l.value) {
			out = append(out, l)
		}
	}
	return out
}

// requestLocations: the URL's path segments and query values, the headers by
// name (a Cookie per cookie, a Bearer token without its scheme), the form
// values, the body's JSON values. A header the client computes is not read.
func requestLocations(i int, req models.HTTPReq) []*location {
	var out []*location
	out = append(out, urlLocations(i, RequestURL, field{kind: fieldURL}, req.URL, "")...)
	for _, name := range sortedNames(req.Header) {
		if clientHeaders[strings.ToLower(name)] {
			continue
		}
		v := req.Header[name]
		f := field{kind: fieldReqHeader, name: name}
		switch {
		case strings.EqualFold(name, "Cookie"):
			for _, c := range cookieSpans(v, ";") {
				out = append(out, &location{tc: i, part: RequestHeader, path: "Cookie." + c.name, value: c.value, nameHint: c.name, field: f, start: c.start, end: c.end})
			}
		case strings.EqualFold(name, "Authorization") && strings.HasPrefix(v, "Bearer "):
			out = append(out, &location{tc: i, part: RequestHeader, path: name, value: v[len("Bearer "):], nameHint: "token", field: f, start: len("Bearer "), end: len(v)})
		default:
			out = append(out, &location{tc: i, part: RequestHeader, path: name, value: v, nameHint: name, field: f, start: 0, end: len(v)})
		}
	}
	for fi, fd := range req.Form {
		for vi, v := range fd.Values {
			out = append(out, &location{tc: i, part: RequestBody, path: "form." + fd.Key, value: v, nameHint: fd.Key, field: field{kind: fieldForm, form: fi, item: vi}, start: 0, end: len(v)})
		}
	}
	if isFormBody(req.Header) {
		for _, p := range formSpans(req.Body) {
			out = append(out, &location{tc: i, part: RequestBody, path: "form." + p.name, value: p.value, nameHint: p.name, field: field{kind: fieldReqBody}, start: p.start, end: p.end})
		}
	} else {
		out = append(out, jsonLocations(i, RequestBody, field{kind: fieldReqBody}, req.Body)...)
	}
	return out
}

// responseLocations: the body's JSON values, then each Set-Cookie cookie's
// value and the path and query of a Location. Other response headers are
// not read: what a server sets there (Content-Type, Server, Cache-Control) is
// a constant far more often than a value it issues, and a capture there would
// stop asserting it. Of the cookies of one identity (name, Domain, Path) only
// the last is read: it replaces the others (a clear-then-set login), and an
// empty cookie value ("" included) sets nothing.
func responseLocations(i int, resp models.HTTPResp) []*location {
	out := jsonLocations(i, ResponseBody, field{kind: fieldRespBody}, resp.Body)
	for _, name := range sortedNames(resp.Header) {
		v := resp.Header[name]
		f := field{kind: fieldRespHeader, name: name}
		switch {
		case strings.EqualFold(name, "Set-Cookie"):
			cookies := templates.SplitSetCookie(v)
			shadowed := templates.Shadowed(cookies, v, nil)
			for ci, c := range cookies {
				if shadowed[ci] || c.Value(v) == "" {
					continue
				}
				out = append(out, &location{tc: i, part: ResponseHeader, path: "Set-Cookie." + c.Name, value: v[c.ValueStart:c.ValueEnd], nameHint: c.Name, field: field{kind: fieldRespHeader, name: name}, start: c.ValueStart, end: c.ValueEnd})
			}
		case strings.EqualFold(name, "Location") || strings.EqualFold(name, "Content-Location"):
			// A URL-valued header is read by its parts: a value that holds
			// a delimiter of its part (a%2Fb in a path segment) could not
			// be told from two parts, so nothing is written there.
			for _, l := range urlLocations(i, ResponseHeader, f, v, name+".") {
				l.unholdable = templates.HoldsURLDelimiter(l.value, strings.HasPrefix(l.path, name+".query."))
				l.status = resp.StatusCode
				out = append(out, l)
			}
		}
	}
	return out
}

// jsonLocations are a JSON body's string and number values. A string that
// holds a placeholder (a secret's) keeps it, and indexLocations leaves it
// out. A value a later occurrence of a key on its path replaces (Leaf.Shadowed)
// is not the body's value there, and is not read.
func jsonLocations(i int, part PartType, f field, body string) []*location {
	leaves, ok := templates.JSONLeaves(body)
	if !ok {
		return nil
	}
	var out []*location
	for _, l := range leaves {
		if l.Shadowed {
			continue
		}
		raw := l.Raw(body)
		loc := &location{tc: i, part: part, path: l.Under("body"), field: f, start: l.Start, end: l.End, inArray: l.InArray, keys: objectKeys(l.Path), nameHint: leafName(l)}
		switch l.Kind {
		case templates.LeafString:
			loc.value, loc.jsonType = l.Value(body), templates.TypeString
		case templates.LeafNumber:
			loc.value, loc.jsonType = raw, templates.TypeInt
			if strings.ContainsAny(raw, ".eE") {
				loc.jsonType = templates.TypeFloat
			}
		default:
			// null, a boolean, or a placeholder written outside quotes (a
			// secret's, the only placeholder a set templatize writes into
			// holds): no value a chain carries.
			continue
		}
		out = append(out, loc)
	}
	return out
}

// objectKeys are the object keys of a JSON path, its array indices left out.
func objectKeys(path []templates.PathSeg) []string {
	var keys []string
	for _, s := range path {
		if !s.IsIndex {
			keys = append(keys, s.Key)
		}
	}
	return keys
}

// leafName is what a key produced at a JSON leaf is named after: its own key,
// or for an array element its nearest key plus the index ("itemsIx0", which
// planChains normalises to the key itemsix0).
func leafName(l templates.Leaf) string {
	if len(l.Path) == 0 {
		return "body"
	}
	last := l.Path[len(l.Path)-1]
	if !last.IsIndex {
		return last.Key
	}
	parent := "arr"
	for i := len(l.Path) - 2; i >= 0; i-- {
		if !l.Path[i].IsIndex {
			parent = l.Path[i].Key
			break
		}
	}
	return normalizeBaseKey(parent) + "Ix" + strconv.Itoa(last.Index)
}

// urlLocations are a URL's path segments and query values with their spans.
func urlLocations(i int, part PartType, f field, u, prefix string) []*location {
	var out []*location
	pathStart := 0
	authority := -1
	if k := strings.Index(u, "://"); k >= 0 {
		authority = k + 3
	} else if strings.HasPrefix(u, "//") {
		// A network-path reference (RFC 3986 §4.2): its host is no segment.
		authority = 2
	}
	if authority >= 0 {
		slash := strings.IndexByte(u[authority:], '/')
		if slash < 0 {
			return nil
		}
		pathStart = authority + slash
	}
	end := len(u)
	if k := strings.IndexByte(u, '#'); k >= 0 {
		end = k
	}
	queryAt := end
	if k := strings.IndexByte(u[:end], '?'); k >= 0 {
		queryAt = k
	}
	if pathStart > queryAt {
		return nil
	}
	seg, prev := 0, ""
	start := pathStart
	for pos := pathStart; pos <= queryAt; pos++ {
		if pos < queryAt && u[pos] != '/' {
			continue
		}
		if pos > start {
			raw := u[start:pos]
			value := raw
			if v, err := url.PathUnescape(raw); err == nil {
				value = v
			}
			hint := "location"
			if prev != "" {
				hint = prev
			}
			out = append(out, &location{tc: i, part: part, path: fmt.Sprintf("%spath.%d", prefix, seg), value: value, nameHint: hint, field: f, start: start, end: pos, prevSeg: prev})
			prev = value
			seg++
		}
		start = pos + 1
	}
	if queryAt < end {
		qs := queryAt + 1
		for _, p := range pairSpans(u[qs:end], "&") {
			value := p.value
			if v, err := url.QueryUnescape(value); err == nil {
				value = v
			}
			out = append(out, &location{tc: i, part: part, path: prefix + "query." + p.name, value: value, nameHint: p.name, field: f, start: qs + p.start, end: qs + p.end})
		}
	}
	return out
}

type span struct {
	name, value string
	start, end  int
}

// pairSpans splits name=value pairs on sep, with each value's span.
func pairSpans(s, sep string) []span {
	var out []span
	pos := 0
	for _, part := range strings.Split(s, sep) {
		if eq := strings.IndexByte(part, '='); eq > 0 {
			out = append(out, span{name: part[:eq], value: part[eq+1:], start: pos + eq + 1, end: pos + len(part)})
		}
		pos += len(part) + len(sep)
	}
	return out
}

// cookieSpans splits a Cookie header ("a=1; b=2") with each value's span.
func cookieSpans(s, sep string) []span {
	var out []span
	for _, p := range pairSpans(s, sep) {
		name := strings.TrimSpace(p.name)
		lead := len(p.value) - len(strings.TrimLeft(p.value, " "))
		value := strings.TrimSpace(p.value)
		if name == "" || value == "" {
			continue
		}
		out = append(out, span{name: name, value: value, start: p.start + lead, end: p.start + lead + len(value)})
	}
	return out
}

// formSpans splits a form-urlencoded body, with each value decoded.
func formSpans(body string) []span {
	var out []span
	for _, p := range pairSpans(body, "&") {
		value := p.value
		if v, err := url.QueryUnescape(value); err == nil {
			value = v
		}
		name, err := url.QueryUnescape(p.name)
		if err != nil {
			name = p.name
		}
		out = append(out, span{name: name, value: value, start: p.start, end: p.end})
	}
	return out
}

// trivial values are never a template key: they identify nothing.
func trivial(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "true", "false", "null", "nil":
		return true
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil && f == 0 {
		return true
	}
	return false
}

// capturePlan is one capture templatize writes, for the line it prints.
type capturePlan struct {
	tc        int
	path, key string
	typ       string
	value     string
}

// repeatPlan is a place of a producer's own response that holds the value it
// captures and stays written out, for the line templatize prints: with the
// use that would assert it there (use, written in place of the value), or
// why none can stand there (noUse).
type repeatPlan struct {
	tc        int
	path, key string
	value     string
	use       string
	noUse     string
}

// chainPlan is what one templatize run writes, and what it says it did not.
type chainPlan struct {
	chains   []*TemplateChain
	edits    map[int]map[field][]edit
	newKeys  map[string]string
	captures []capturePlan
	// crowded are the URL-valued response headers that templatize captures
	// nothing in though a later request sends a value they give: a 201's
	// Location that holds more than one value its response gives first, and
	// any other Location or Content-Location (createdURL).
	crowded []crowdedHeader
	// left counts the values a later request sends that templatize leaves
	// uncaptured where they are issued, each said in its own line (inside an
	// array under ignoreOrdering, holding a delimiter of its URL part): a set
	// with one is not one with nothing to templatize.
	left int
}

// redirect reports whether the header is a 3xx's Location: the target to
// redirect to (RFC 9110 §10.2.2).
func (c crowdedHeader) redirect() bool {
	return strings.EqualFold(c.header, "Location") && c.status/100 == 3
}

// crowdedHeader is a Location or Content-Location templatize captures
// nothing in, though a later request sends a value it gives: crowded, a
// 201's Location holding more than one candidate; or not a created
// resource's Location at all (created false), any other Location (a 3xx
// redirect target) or a Content-Location.
type crowdedHeader struct {
	tc         int
	header     string
	status     int
	created    bool
	candidates []*location
	// keys are the keys the edit names, one per candidate, as templatize
	// would name them: the key config.yaml already gives the value (reused),
	// or a new one.
	keys   []string
	reused []bool
	// own is, per candidate, the key its response captures the candidate's
	// value as elsewhere (the body's id), or "": that value is named here by
	// a use, and no capture stands beside it.
	own []string
}

// urlProducers is what soleURLProducers finds: per place, whether a capture
// may stand there (allowed: the one candidate of a 201's Location, the only
// place of a producer's own response where a use of its capture asserts at
// least what a capture there would); and the headers said for capturing
// nothing (crowded).
type urlProducers struct {
	allowed map[*location]bool
	crowded []crowdedHeader
}

// headerAt names one header of one test case's response.
type headerAt struct {
	tc   int
	name string
}

// soleURLProducers says which producer of each Location or Content-Location
// may capture. Only the Location of a 201 (Created) response names a
// resource the request created (RFC 9110 §10.2.2, §15.3.2), so only there is
// a part a value the protocol says the response issues. A 3xx's Location is
// the target to redirect to, a Content-Location (§8.7) the representation's
// resource, and on any other status (a 202's monitor, a 200) RFC 9110 gives a
// Location no meaning: a constant there is as likely as an issued id, and a
// capture would accept any value, a regression to another target included,
// where keploy test without templates fails it. Such a header captures nothing
// and is compared as recorded; when a later request sends a value it gives
// first, it is said (crowded, created false) with the edit for a value the
// app does issue anew each run. A 201's Location holds one capture, so when
// it holds more than one producer candidate (a constant segment and an
// issued id alike, /users/usr_REC01; /jobs/…) one recording cannot tell which
// value the app issues anew: none is captured there, and the header is named
// in crowded when a later request sends one of them. A candidate is every
// non-trivial part of the header whose value is first placed in the same
// test case's response, the header itself or the body before it
// (urlCandidate), whether or not a later request sends it and whether or not
// it is captured elsewhere: a created id the body holds as a JSON number,
// inside an array under ignoreOrdering, or that no later request sends as a
// string, is still the value the app issues there, and counting it keeps the
// constant beside it from being taken for it (/jobs/7654321 never captures
// "jobs"). A parent id, first placed in an earlier test case, and a value the
// request itself sent are no candidate, so a nested create has one.
func soleURLProducers(locs []*location, byValue map[string][]*location) urlProducers {
	var keys []headerAt
	candidates := map[headerAt][]*location{}
	counted := map[headerAt]map[string]bool{}
	status := map[headerAt]int{}
	for _, l := range locs {
		if !isLocation(l) || !urlCandidate(l, byValue[l.value]) {
			continue
		}
		k := headerAt{l.tc, l.field.name}
		if _, seen := candidates[k]; !seen {
			keys = append(keys, k)
			counted[k] = map[string]bool{}
			status[k] = l.status
		}
		// A value the header holds twice is one candidate.
		if counted[k][l.value] {
			continue
		}
		counted[k][l.value] = true
		candidates[k] = append(candidates[k], l)
	}
	out := urlProducers{allowed: map[*location]bool{}}
	var crowded []crowdedHeader
	for _, k := range keys {
		cs := candidates[k]
		var chosen, sent []*location
		for _, c := range cs {
			if sentLater(c, byValue[c.value]) {
				sent = append(sent, c)
			}
		}
		created := createdURL(cs[0])
		switch {
		case !created:
			// Said only for a value whose first place is the header
			// itself: one the body gives first is captured there.
			var own []*location
			for _, c := range sent {
				if byValue[c.value][0] == c {
					own = append(own, c)
				}
			}
			if len(own) > 0 {
				crowded = append(crowded, crowdedHeader{tc: k.tc, header: k.name, status: status[k], candidates: own})
			}
		case len(cs) == 1:
			chosen = cs
		case len(sent) > 0:
			crowded = append(crowded, crowdedHeader{tc: k.tc, header: k.name, status: status[k], created: true, candidates: cs})
		}
		for _, c := range chosen {
			out.allowed[c] = true
		}
	}
	out.crowded = crowded
	return out
}

// urlCandidate reports whether l, a part of a URL-valued response header, is
// a producer candidate of its header: its value's first place (ls[0], the
// value's places in templatize's order; none for a trivial value) is in the
// same test case's response.
func urlCandidate(l *location, ls []*location) bool {
	return len(ls) > 0 && ls[0].tc == l.tc && ls[0].inResponse()
}

// sentLater reports whether a later test case's request sends l's value (ls,
// its places) at a place of a compatible type: a value l could hand on.
func sentLater(l *location, ls []*location) bool {
	for _, o := range ls {
		if !o.inResponse() && o.tc > l.tc && typesCompatible(l, o) {
			return true
		}
	}
	return false
}

// createdURL reports whether l is a part of the Location of a 201 (Created)
// response: by RFC 9110 the one URL-valued header that names a resource the
// request created, and so the one where a part can be a value the response
// issues.
func createdURL(l *location) bool {
	return strings.EqualFold(l.field.name, "Location") && l.status == http.StatusCreated
}

// isLocation reports whether a place is in a response's Location or
// Content-Location header: the URI of the resource the response is about
// (RFC 9110 §10.2.2, §8.7), so a value the same response produces there is
// the created resource's identifier.
func isLocation(l *location) bool {
	return l.field.kind == fieldRespHeader && (strings.EqualFold(l.field.name, "Location") || strings.EqualFold(l.field.name, "Content-Location"))
}

// planChains finds the chains among locs and the edits that write them. A
// value is a chain when the first place it appears is in a response, and a
// later test case's request sends it at a place of a compatible type. That
// first place is captured (in a Location or Content-Location only as
// soleURLProducers allows); each such request place is a use, and so is a
// later response place of a compatible type in a test case whose own request
// sent the key. In the producer's own response, a part of its Location is a
// use too only where a capture itself may stand (the one candidate of a
// 201's Location): there the use asserts at least what a capture would.
// Nothing else is touched: a response that only happens to hold an equal
// value keeps it written out, and so does every other place of the
// producer's own response (a redirect's target, a Content-Location, a
// crowded 201 Location), as main compares it; each that still holds the
// value is said (repeatsOf). A response header holds at most one capture,
// beside uses only of keys its response does not capture: a nested create
// answers Location: /orgs/{{string .org}}/users/{{capture string .users}}
// (a 201's Location captures only its one candidate, and a use reaches it
// only for an earlier test case's key, whose value is planned first).
// Keys reuse the name config.yaml already gives the same value (a key of the
// grammar only), and are otherwise named after the producing place, numbered
// in producer order on a clash. untied says why a later-response place inside
// an array keploy test could not tie to its element (untiedInArray): no use
// is written there. The producer is the first place of the value's first
// response where a capture may stand (producerOf).
func planChains(logger *zap.Logger, testSetID string, tcs []*models.TestCase, locs []*location, existing map[string]interface{}, ignoreOrdering bool, untied func(*location) string) chainPlan {
	// at names a place as the lines below say it: test set, test case, path.
	at := func(l *location) string {
		return fmt.Sprintf("%s/%s %s", testSetID, tcs[l.tc].Name, l.path)
	}
	byValue := map[string][]*location{}
	var order []string
	for _, l := range locs {
		if trivial(l.value) {
			continue
		}
		if _, seen := byValue[l.value]; !seen {
			order = append(order, l.value)
		}
		byValue[l.value] = append(byValue[l.value], l)
	}

	keyOf := map[string]string{} // existing value → its key, the first key by name
	taken := map[string]bool{}
	names := make([]string, 0, len(existing))
	for k := range existing {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		taken[k] = true
		// Only a key of the grammar can be named by a placeholder: a
		// config.yaml key such as user-id is never reused, whatever value it
		// holds.
		if !templates.IsKey(k) {
			continue
		}
		if text, err := templates.ValueText(existing[k]); err == nil {
			if _, dup := keyOf[text]; !dup {
				keyOf[text] = k
			}
		}
	}

	plan := chainPlan{edits: map[int]map[field][]edit{}, newKeys: map[string]string{}}
	write := func(l *location, text string) {
		if plan.edits[l.tc] == nil {
			plan.edits[l.tc] = map[field][]edit{}
		}
		plan.edits[l.tc][l.field] = append(plan.edits[l.tc][l.field], edit{start: l.start, end: l.end, text: text})
	}

	// A URL segment is named after the segment before it, unless that one is
	// a value itself: a placeholder (a secret's), or a value an earlier test
	// case's response gives (/orgs/<the org's id>/<a new id> names the new id
	// after the header, never after the org's id).
	for _, l := range locs {
		if l.part != ResponseHeader || l.prevSeg == "" {
			continue
		}
		if ps := byValue[l.prevSeg]; templates.Contains(l.prevSeg) || (len(ps) > 0 && ps[0].inResponse() && ps[0].tc < l.tc) {
			l.nameHint = "location"
		}
	}
	urls := soleURLProducers(locs, byValue)
	urlAllowed, crowded := urls.allowed, urls.crowded

	for _, value := range order {
		ls := byValue[value]
		if !ls[0].inResponse() {
			continue // a value a request sends first is the client's
		}
		producer := producerOf(ls, urlAllowed, ignoreOrdering)
		others := make([]*location, 0, len(ls)-1)
		for _, l := range ls {
			if l != producer {
				others = append(others, l)
			}
		}
		var consumers []*ValueLocation
		sent := map[int]bool{}
		for _, l := range others {
			if !l.inResponse() && l.tc > producer.tc && typesCompatible(producer, l) {
				consumers = append(consumers, valueLocation(l))
				sent[l.tc] = true
			}
		}
		if len(consumers) == 0 {
			continue
		}
		if producer.inArray && ignoreOrdering {
			plan.left++
			logger.Info(fmt.Sprintf("%s gives a value a later request sends, inside an array: test.ignoreOrdering is on, so which element gives it cannot be decided, and templatize captures nothing there", at(producer)),
				zap.String("value", value))
			continue
		}
		if isLocation(producer) && !urlAllowed[producer] {
			// A crowded 201 Location or a header that names no created
			// resource: each is said once, in crowded.
			continue
		}
		if producer.unholdable {
			plan.left++
			logger.Info(fmt.Sprintf("%s gives a value a later request sends, holding a delimiter of its URL part, where it reads as two parts: the replay could not tell it from them, and templatize captures nothing there", at(producer)),
				zap.String("value", value))
			continue
		}

		key, reused := keyOf[value]
		if !reused {
			base := normalizeBaseKey(producer.nameHint)
			key = base
			for n := 1; taken[key]; n++ {
				key = base + strconv.Itoa(n)
			}
			taken[key] = true
			plan.newKeys[key] = value
		}

		write(producer, placeholderFor(producer, true, key))
		plan.captures = append(plan.captures, capturePlan{tc: producer.tc, path: producer.path, key: key, typ: producer.valueType(), value: value})
		for _, l := range others {
			if !typesCompatible(producer, l) || l.unholdable {
				continue
			}
			if l.tc == producer.tc {
				// The producer's own response, captured first (rule 4): a
				// use stands only where a capture itself could, the one
				// candidate of a 201's Location (urlAllowed), so it asserts
				// at least what that capture would. Every other place, a
				// redirect's or any non-201 Location, every Content-Location,
				// a crowded 201 Location, stays written out as main compares
				// it (repeatsOf names it): a use there would accept any value
				// the producer answers, a regression of a constant included.
				if !urlAllowed[l] {
					continue
				}
			} else if l.inResponse() && !sent[l.tc] {
				continue // equal by value only: what this test case was not asked for
			} else if why := untiedWhy(untied, l); l.inResponse() && why != "" {
				logger.Info(fmt.Sprintf("%s holds a read-back's value inside an array, and it stays as recorded: test.ignoreOrdering is on and %s, so nothing would tie a use there to its element and keploy test would refuse it", at(l), why),
					zap.String("value", value))
				continue
			}
			write(l, placeholderFor(l, false, key))
		}
		plan.chains = append(plan.chains, &TemplateChain{TemplateKey: key, Value: value, Producer: valueLocation(producer), Consumers: consumers})
	}
	// The key each response captures each value as.
	ownKey := map[int]map[string]string{}
	for _, c := range plan.captures {
		if ownKey[c.tc] == nil {
			ownKey[c.tc] = map[string]string{}
		}
		if _, seen := ownKey[c.tc][c.value]; !seen {
			ownKey[c.tc][c.value] = c.key
		}
	}
	// Every crowded header is said, with its edits: templatize writes
	// neither a capture nor a use of its own response's capture in it (a
	// use stands in the producer's own response only where a capture
	// could), so each of its values stays as recorded, and the line names
	// them all.
	for _, c := range crowded {
		// A candidate its response captures elsewhere is named there by a
		// use: a second capture of it would be refused, and a capture of
		// another candidate beside that use too.
		for _, l := range c.candidates {
			c.own = append(c.own, ownKey[c.tc][l.value])
		}
		// The key each edit names: the one config.yaml gives the value
		// already, or a new name no key holds.
		local := map[string]bool{}
		for _, l := range c.candidates {
			if k, ok := keyOf[l.value]; ok {
				c.keys, c.reused = append(c.keys, k), append(c.reused, true)
				continue
			}
			base := normalizeBaseKey(l.nameHint)
			key := base
			for n := 1; taken[key] || local[key]; n++ {
				key = base + strconv.Itoa(n)
			}
			local[key] = true
			c.keys, c.reused = append(c.keys, key), append(c.reused, false)
		}
		plan.crowded = append(plan.crowded, c)
	}
	return plan
}

// producerOf is the place of a value that captures it: the first place, in
// templatize's order, of the response that gives the value first (ls[0]'s)
// where a capture may stand. A part of a URL-valued header that may not
// capture (soleURLProducers: any but a 201's Location with one candidate),
// one that holds a delimiter of its part, and a place inside an array under
// ignoreOrdering are passed over for a later place of the same response,
// which captures; the header part passed over stays written out (planChains
// writes a use in its own response only where a capture could stand), so a
// 302 that sets a session cookie and carries its id in the Location captures
// the cookie and keeps asserting the redirect as main does, whatever the
// letter case of either header's name. When no place of that response may
// capture, it is ls[0], and the plan says why it leaves the value.
func producerOf(ls []*location, urlAllowed map[*location]bool, ignoreOrdering bool) *location {
	first := ls[0]
	for _, l := range ls {
		if l.tc != first.tc || !l.inResponse() {
			break
		}
		if !(isLocation(l) && !urlAllowed[l]) && !l.unholdable && !(l.inArray && ignoreOrdering) {
			return l
		}
	}
	return first
}

// untiedWhy is untied's answer for l, "" when untied is nil.
func untiedWhy(untied func(*location) string, l *location) string {
	if untied == nil {
		return ""
	}
	return untied(l)
}

// repeatsOf names, for each capture written, every place of the producer's
// own response (as written now) that still holds the captured value written
// out: a JSON value or object key that is it or holds it inside a longer
// text (a url, _links.self.href), a header (X-Resource-Id, a Location
// beside its use, read decoded too), a cookie, a body that is not JSON. A
// number is found only as a whole number, not as digits of a longer one.
// Nothing here is written: each is said, as a diagnostic.
func repeatsOf(tcs []*models.TestCase, captures []capturePlan, untied func(*location) string) []repeatPlan {
	var out []repeatPlan
	for _, c := range captures {
		resp := tcs[c.tc].HTTPResp
		numeric := templates.IsNumber(c.value)
		text := templates.Format(false, templates.TypeString, c.key)
		seen := map[string]bool{c.path: true}
		add := func(path, use, noUse string) {
			if !seen[path] {
				seen[path] = true
				out = append(out, repeatPlan{tc: c.tc, path: path, key: c.key, value: c.value, use: use, noUse: noUse})
			}
		}
		// tiedWhy says why a use inside an array could not be tied to its
		// element, which keploy test refuses (untiedInArray), "" when it can.
		tiedWhy := func(inArray bool, path []templates.PathSeg) string {
			if !inArray {
				return ""
			}
			if why := untiedWhy(untied, &location{tc: c.tc, part: ResponseBody, inArray: true, keys: objectKeys(path)}); why != "" {
				return "it is inside an array and test.ignoreOrdering is on and " + why + ", so nothing would tie a use there to its element and keploy test would refuse it"
			}
			return ""
		}
		if leaves, keys, ok := templates.JSONPlaces(resp.Body); ok {
			for _, l := range leaves {
				raw := l.Raw(resp.Body)
				if l.Shadowed || templates.Contains(raw) || !occurs(l.Value(resp.Body), c.value, numeric) {
					continue
				}
				switch why := tiedWhy(l.InArray, l.Path); {
				case why != "":
					add(l.Under("body"), "", why)
				case l.Kind == templates.LeafString:
					add(l.Under("body"), text, "")
				case raw == c.value:
					// A JSON number is asserted by a number use, quoted as
					// templatize writes one.
					typ := templates.TypeInt
					if strings.ContainsAny(raw, ".eE") {
						typ = templates.TypeFloat
					}
					add(l.Under("body"), `"`+templates.Format(false, typ, c.key)+`"`, "")
				default:
					add(l.Under("body"), "", "the value is part of a JSON number there, which a use cannot stand inside")
				}
			}
			for _, k := range keys {
				raw := k.Raw(resp.Body)
				if !k.Shadowed && !templates.Contains(raw) && occurs(raw[1:len(raw)-1], c.value, numeric) {
					if why := tiedWhy(k.InArray, k.Path); why != "" {
						add("body key "+k.DisplayPath(), "", why)
					} else {
						add("body key "+k.DisplayPath(), text, "")
					}
				}
			}
		} else if occurs(withoutPlaceholders(resp.Body), c.value, numeric) {
			add("body", text, "")
		}
		for _, name := range sortedNames(resp.Header) {
			v := resp.Header[name]
			if strings.EqualFold(name, "Set-Cookie") {
				for _, ck := range templates.SplitSetCookie(v) {
					if occurs(withoutPlaceholders(v[ck.Start:ck.End]), c.value, numeric) {
						add("Set-Cookie."+ck.Name, text, "")
					}
				}
				continue
			}
			written := withoutPlaceholders(v)
			decoded, err := url.PathUnescape(written)
			if err != nil {
				decoded = written
			}
			if !occurs(written, c.value, numeric) && !occurs(decoded, c.value, numeric) {
				continue
			}
			noUse := ""
			for _, p := range templates.Find(v) {
				if p.Capture {
					// keploy test refuses a header capture beside a use of a
					// key its response captures.
					noUse = "its header value holds a capture, beside which a use of a key its response captures is refused"
				}
			}
			if noUse != "" {
				add(name, "", noUse)
			} else {
				add(name, text, "")
			}
		}
	}
	return out
}

// sayRepeats says each place of a capture's own response that still holds
// the captured value written out (repeatsOf), with the edit that would assert
// it there: the test case fails there when the app makes the value anew on
// every run.
func (t *Tools) sayRepeats(testSetID string, tcs []*models.TestCase, captures []capturePlan) {
	repeats := repeatsOf(tcs, captures, t.untiedInArray(testSetID, tcs))
	for _, rp := range repeats {
		next := fmt.Sprintf("if the app issues the value anew each run, write %s in place of %q there: that place then asserts it holds what this same answer gives at the capture of %s, whatever that is (a constant's regression in both places included); if it is the same every run, nothing to do",
			rp.use, rp.value, rp.key)
		if rp.use == "" {
			next = "no use can stand there (" + rp.noUse + "): it stays as recorded"
		}
		t.logger.Info(fmt.Sprintf("%s/%s %s holds the value %s captures and stays as recorded; this test case fails there if the app issues a fresh value each run",
			testSetID, tcs[rp.tc].Name, rp.path, rp.key),
			zap.String("next_step", next))
	}
}

// withoutPlaceholders is s with each placeholder cut out: what stays written.
func withoutPlaceholders(s string) string {
	ps := templates.Find(s)
	if len(ps) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, p := range ps {
		b.WriteString(s[last:p.Start])
		b.WriteByte(0) // a placeholder joins nothing on either side
		last = p.End
	}
	b.WriteString(s[last:])
	return b.String()
}

// occurs reports whether value is in text: anywhere for a string, and for a
// number only where it stands whole, with no letter or digit on either side
// of it (the 2 of "quantity":"2", not the 2 inside an ObjectId 662508d2a3 or
// an ETag W/"1ca2-…", nor inside a longer number).
func occurs(text, value string, numeric bool) bool {
	if value == "" {
		return false
	}
	for from := 0; ; {
		i := strings.Index(text[from:], value)
		if i < 0 {
			return false
		}
		at := from + i
		if !numeric || (!alnumBefore(text, at) && !alnumAfter(text, at+len(value))) {
			return true
		}
		from = at + 1
	}
}

// alnumBefore reports whether the character that ends just before byte i of
// s is a letter or a digit, of any script.
func alnumBefore(s string, i int) bool {
	if i <= 0 {
		return false
	}
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// alnumAfter reports whether the character that starts at byte i of s is a
// letter or a digit, of any script.
func alnumAfter(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// placeholderFor writes a key at a place: in quotes for a JSON value, so the
// body stays valid JSON; as it is in a URL, header or form.
func placeholderFor(l *location, capture bool, key string) string {
	if l.jsonType != "" {
		return `"` + templates.Format(capture, l.jsonType, key) + `"`
	}
	return templates.Format(capture, templates.TypeString, key)
}

func valueLocation(l *location) *ValueLocation {
	return &ValueLocation{TestCaseIndex: l.tc, Part: l.part, Path: l.path, OriginalType: l.valueType()}
}

// applyEdits writes the planned placeholders into copies of the test cases
// they change, by index: tcs themselves stay as they are until the files are
// in place.
func applyEdits(tcs []*models.TestCase, edits map[int]map[field][]edit) map[int]*models.TestCase {
	changed := map[int]*models.TestCase{}
	for i, byField := range edits {
		tc := editable(tcs[i])
		for f, es := range byField {
			sort.Slice(es, func(a, b int) bool { return es[a].start > es[b].start })
			switch f.kind {
			case fieldURL:
				before := tc.HTTPReq.URL
				tc.HTTPReq.URL = spliceEdits(before, es)
				syncURLParams(&tc.HTTPReq, before)
			case fieldReqHeader:
				tc.HTTPReq.Header[f.name] = spliceEdits(tc.HTTPReq.Header[f.name], es)
			case fieldReqBody:
				tc.HTTPReq.Body = spliceEdits(tc.HTTPReq.Body, es)
			case fieldForm:
				fd := &tc.HTTPReq.Form[f.form]
				fd.Values[f.item] = spliceEdits(fd.Values[f.item], es)
			case fieldRespHeader:
				tc.HTTPResp.Header[f.name] = spliceEdits(tc.HTTPResp.Header[f.name], es)
			case fieldRespBody:
				tc.HTTPResp.Body = spliceEdits(tc.HTTPResp.Body, es)
			}
		}
		changed[i] = tc
	}
	return changed
}

// spliceEdits applies edits sorted from the last span back.
func spliceEdits(s string, es []edit) string {
	for _, e := range es {
		s = s[:e.start] + e.text + s[e.end:]
	}
	return s
}

// syncURLParams puts the URL's new query placeholders into URLParams, where a
// param still holds the value its query place held.
func syncURLParams(req *models.HTTPReq, before string) {
	if len(req.URLParams) == 0 {
		return
	}
	oldQ, newQ := queryOf(before), queryOf(req.URL)
	for _, np := range pairSpans(newQ, "&") {
		if !templates.Contains(np.value) {
			continue
		}
		for _, op := range pairSpans(oldQ, "&") {
			if op.name != np.name {
				continue
			}
			old, err := url.QueryUnescape(op.value)
			if err != nil {
				old = op.value
			}
			if v, ok := req.URLParams[np.name]; ok && v == old {
				req.URLParams[np.name] = np.value
			}
		}
	}
}

func queryOf(u string) string {
	if k := strings.IndexByte(u, '#'); k >= 0 {
		u = u[:k]
	}
	if k := strings.IndexByte(u, '?'); k >= 0 {
		return u[k+1:]
	}
	return ""
}

func isFormBody(h map[string]string) bool {
	for k, v := range h {
		if strings.EqualFold(k, "Content-Type") {
			return strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "application/x-www-form-urlencoded")
		}
	}
	return false
}

// sortedNames is a header map's names in the order templates reads them,
// whatever their letter case (templates.HeaderNames): which place of a
// response comes first never depends on how a header's name is written.
func sortedNames(m map[string]string) []string { return templates.HeaderNames(m) }

func (t *Tools) logAPIChains(chains []*TemplateChain, testCases []*models.TestCase) {
	if len(chains) == 0 {
		return
	}
	fmt.Println("\n✨ API Chain Analysis ✨")
	fmt.Println("========================")
	for i, chain := range chains {
		if i > 0 {
			fmt.Println("--------------------")
		}
		truncatedValue := chain.Value
		if len(truncatedValue) > 50 {
			truncatedValue = truncatedValue[:47] + "..."
		}
		fmt.Printf("🔗 Chain for {{.%s}} (value: \"%s\")\n", chain.TemplateKey, truncatedValue)
		fmt.Printf("  [PRODUCER] %s\n", formatLocation(chain.Producer, testCases))
		for j, consumer := range chain.Consumers {
			branch := "├─>"
			if j == len(chain.Consumers)-1 {
				branch = "    └─>"
			}
			fmt.Printf("    %s [CONSUMER] %s\n", branch, formatLocation(consumer, testCases))
		}
	}
	fmt.Println("========================")
}

func formatLocation(loc *ValueLocation, testCases []*models.TestCase) string {
	if loc == nil || loc.TestCaseIndex >= len(testCases) {
		return "unknown location"
	}
	testCaseName := testCases[loc.TestCaseIndex].Name
	switch loc.Part {
	case RequestHeader:
		return fmt.Sprintf("%s (%s '%s')", testCaseName, loc.Part, loc.Path)
	case ResponseBody, RequestBody:
		return fmt.Sprintf("%s (%s at '%s')", testCaseName, loc.Part, loc.Path)
	case RequestURL:
		return fmt.Sprintf("%s (%s)", testCaseName, loc.Part)
	default:
		return fmt.Sprintf("%s (%s)", testCaseName, loc.Part)
	}
}

// Utility function to safely marshal JSON and log errors
var jsonMarshal987 = json.Marshal

func (t *Tools) AssertChains(keployChains []*TemplateChain, testCases []*models.TestCase, fuzzerYamlPath string) {
	fmt.Println("\n🔎 Chain Assertion against Fuzzer Output")
	fmt.Println("==========================================")

	// 1. Load fuzzer's baseline chains from the YAML file.
	yamlFile, err := os.ReadFile(fuzzerYamlPath)
	if err != nil {
		fmt.Printf("🔴 ERROR: Could not read fuzzer's chain file at %s: %v\n", fuzzerYamlPath, err)
		return
	}

	// Use a generic map to bypass struct tag parsing issues.
	var genericFuzzerData map[string]interface{}
	if err := yaml.Unmarshal(yamlFile, &genericFuzzerData); err != nil {
		fmt.Printf("🔴 ERROR: Could not parse fuzzer's YAML file into a generic map: %v\n", err)
		return
	}

	// Manually build the canonical chains from the generic map. This is the FIX.
	fuzzerChains, err := buildCanonicalChainsFromMap(genericFuzzerData)
	if err != nil {
		fmt.Printf("🔴 ERROR: Could not process the parsed fuzzer data: %v\n", err)
		return
	}
	fmt.Printf("✅ Loaded %d chains from fuzzer baseline file.\n", len(fuzzerChains))

	// 2. Convert and Normalize Keploy's detected chains.
	canonicalKeployChains := t.convertToCanonical(keployChains, testCases)
	normalizeCanonicalChains(canonicalKeployChains)
	fmt.Printf("✅ Converted and Normalized %d detected Keploy chains for comparison.\n", len(canonicalKeployChains))

	// 3. Perform the comparison.
	passed, report := t.compareChainSets(fuzzerChains, canonicalKeployChains)

	// 4. Print the result.
	fmt.Println("\n--- Comparison Report ---")
	fmt.Print(report)
	if passed {
		fmt.Println("\n✅ PASSED: Keploy's detected chains match the fuzzer's baseline.")
	} else {
		fmt.Println("\n❌ FAILED: Keploy's detected chains DO NOT match the fuzzer's baseline.")
	}
	fmt.Println("==========================================")
}

// buildCanonicalChainsFromMap manually constructs the chain structs from a generic map,
// avoiding any reliance on struct tags which were failing.
func buildCanonicalChainsFromMap(data map[string]interface{}) ([]CanonicalChain, error) {
	var chains []CanonicalChain

	chainsData, ok := data["chains"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("could not find 'chains' array in fuzzer YAML")
	}

	for _, chainInterface := range chainsData {
		chainMap, ok := chainInterface.(map[string]interface{})
		if !ok {
			continue // Skip malformed entries
		}

		var canonicalChain CanonicalChain
		if val, ok := chainMap["variable_name"].(string); ok {
			canonicalChain.VariableName = val
		}
		if val, ok := chainMap["value"].(string); ok {
			canonicalChain.Value = val
		}

		// Manually parse the producer
		if prodInterface, ok := chainMap["producer"].(map[string]interface{}); ok {
			if val, ok := prodInterface["request_id"].(string); ok {
				canonicalChain.Producer.RequestID = val
			}
			if val, ok := prodInterface["part"].(string); ok {
				canonicalChain.Producer.Part = val
			}
			if val, ok := prodInterface["path"].(string); ok {
				canonicalChain.Producer.Path = indexAsStep(val)
			}
		}

		// Manually parse consumers
		if consumersInterface, ok := chainMap["consumers"].([]interface{}); ok {
			for _, consInterface := range consumersInterface {
				consMap, ok := consInterface.(map[string]interface{})
				if !ok {
					continue
				}
				var consumer CanonicalConsumer
				if val, ok := consMap["request_id"].(string); ok {
					consumer.RequestID = val
				}
				if val, ok := consMap["part"].(string); ok {
					consumer.Part = val
				}
				if val, ok := consMap["path"].(string); ok {
					consumer.Path = indexAsStep(val)
				}
				canonicalChain.Consumers = append(canonicalChain.Consumers, consumer)
			}
		}
		chains = append(chains, canonicalChain)
	}

	return chains, nil
}

// convertToCanonical transforms Keploy's internal chain representation to the common format.
func (t *Tools) convertToCanonical(chains []*TemplateChain, tcs []*models.TestCase) []CanonicalChain {
	var canonicalChains []CanonicalChain
	for _, chain := range chains {
		producerReqID := fmt.Sprintf("test-%d", chain.Producer.TestCaseIndex+1)
		producer := CanonicalProducer{
			RequestID: producerReqID,
			Part:      chain.Producer.Part.String(),
			Path:      canonicalPath(chain.Producer.Path),
		}
		var consumers []CanonicalConsumer
		for _, c := range chain.Consumers {
			consumerReqID := fmt.Sprintf("test-%d", c.TestCaseIndex+1)
			consumers = append(consumers, CanonicalConsumer{
				RequestID: consumerReqID,
				Part:      c.Part.String(),
				Path:      canonicalPath(c.Path),
			})
		}
		canonicalChains = append(canonicalChains, CanonicalChain{
			VariableName: "{{" + chain.TemplateKey + "}}",
			Value:        chain.Value,
			Producer:     producer,
			Consumers:    consumers,
		})
	}
	return canonicalChains
}

// canonicalPath writes a keploy chain place's path in the form the
// ASSERT_CHAINS_WITH comparison reads both sides in: a body path without its
// "body." prefix, and each array index as a path step (indexAsStep):
// body.items[0].id is items.0.id, and body[1].id (an element of a top-level
// array) is 1.id.
func canonicalPath(path string) string {
	switch {
	case strings.HasPrefix(path, "body."):
		return indexAsStep(strings.TrimPrefix(path, "body."))
	case strings.HasPrefix(path, "body["):
		return indexAsStep(strings.TrimPrefix(path, "body"))
	}
	return path
}

// indexAsStep writes each array index of a path as a path step: items[0].id
// and [1].ids[2] become items.0.id and 1.ids.2. go-fuzzers' baseline writes a
// body path as items[0].id (rerecord/client extractNestedValues), so the
// baseline's paths go through it as keploy's do.
func indexAsStep(path string) string {
	if !strings.Contains(path, "[") {
		return path
	}
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		switch c := path[i]; c {
		case '[':
			if b.Len() > 0 && !strings.HasSuffix(b.String(), ".") {
				b.WriteByte('.')
			}
		case ']':
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// normalizeCanonicalChains standardizes the representation of chains in-place.
func normalizeCanonicalChains(chains []CanonicalChain) {
	for i := range chains {
		// Normalize producer
		chains[i].Producer.Part = strings.ReplaceAll(chains[i].Producer.Part, " ", "")

		// Normalize consumers
		for j := range chains[i].Consumers {
			chains[i].Consumers[j].Part = strings.ReplaceAll(chains[i].Consumers[j].Part, " ", "")
			if chains[i].Consumers[j].Part == "RequestURL" {
				// Standardize all specific URL paths (e.g., path.1) to a generic one.
				chains[i].Consumers[j].Path = "URL_PATH"
			}
		}
	}
}

func (t *Tools) compareChainSets(fuzzerChains, keployChains []CanonicalChain) (bool, string) {
	var report strings.Builder
	passed := true

	fuzzerChains = filterInsignificantChains(fuzzerChains)
	keployChains = filterInsignificantChains(keployChains)

	fuzzerMap := make(map[string]CanonicalChain)
	for _, c := range fuzzerChains {
		fuzzerMap[c.Value] = c
	}
	keployMap := make(map[string]CanonicalChain)
	for _, c := range keployChains {
		keployMap[c.Value] = c
	}

	normalizeConsumer := func(c CanonicalConsumer) string {
		part := strings.ReplaceAll(c.Part, " ", "")
		path := c.Path
		if part == "RequestURL" {
			path = "URL_PATH"
		}
		return fmt.Sprintf("%s|%s|%s", c.RequestID, part, path)
	}

	// Check 1: Does Keploy find every chain the fuzzer found?
	for value, fChain := range fuzzerMap {
		kChain, exists := keployMap[value]
		if !exists {
			report.WriteString(fmt.Sprintf("❌ MISSING CHAIN: Fuzzer found chain for value '%s', but Keploy did not.\n", value))
			passed = false
			continue
		}

		// --- NEW, MORE ROBUST PRODUCER COMPARISON ---
		producersMatch := false
		// Normalize part names for comparison
		fProducerPart := strings.ReplaceAll(fChain.Producer.Part, " ", "")
		kProducerPart := strings.ReplaceAll(kChain.Producer.Part, " ", "")

		if fChain.Producer.RequestID == kChain.Producer.RequestID && fProducerPart == kProducerPart {
			// Paths are identical, this is a clear match.
			if fChain.Producer.Path == kChain.Producer.Path {
				producersMatch = true
			} else {
				// Handle cases like "id" vs "data.id". If one path is a suffix of the other,
				// consider it a match because they refer to the same value semantically.
				if strings.HasSuffix(fChain.Producer.Path, "."+kChain.Producer.Path) || strings.HasSuffix(kChain.Producer.Path, "."+fChain.Producer.Path) {
					producersMatch = true
				}
			}
		}

		if !producersMatch {
			report.WriteString(fmt.Sprintf("❌ PRODUCER MISMATCH for value '%s':\n", value))
			report.WriteString(fmt.Sprintf("  - Expected (Fuzzer): %+v\n", fChain.Producer))
			report.WriteString(fmt.Sprintf("  - Actual (Keploy):   %+v\n", kChain.Producer))
			passed = false
		}
		// --- END OF NEW PRODUCER COMPARISON ---

		fConsumerSet := make(map[string]bool)
		for _, c := range fChain.Consumers {
			fConsumerSet[normalizeConsumer(c)] = true
		}
		kConsumerSet := make(map[string]bool)
		for _, c := range kChain.Consumers {
			kConsumerSet[normalizeConsumer(c)] = true
		}

		if !reflect.DeepEqual(fConsumerSet, kConsumerSet) {
			report.WriteString(fmt.Sprintf("❌ CONSUMER MISMATCH for value '%s':\n", value))
			for cKey := range fConsumerSet {
				if !kConsumerSet[cKey] {
					report.WriteString(fmt.Sprintf("  - Keploy MISSED consumer: %s\n", cKey))
				}
			}
			for cKey := range kConsumerSet {
				if !fConsumerSet[cKey] {
					report.WriteString(fmt.Sprintf("  - Keploy found EXTRA consumer: %s\n", cKey))
				}
			}
			passed = false
		}
	}

	// Check 2: Did Keploy find any extra chains the fuzzer didn't?
	for value, kChain := range keployMap {
		if _, exists := fuzzerMap[value]; !exists {
			report.WriteString(fmt.Sprintf("❌ EXTRA CHAIN: Keploy found chain for value '%s' (var: %s), but fuzzer did not.\n", value, kChain.VariableName))
			passed = false
		}
	}

	if passed {
		report.WriteString("All checks passed.\n")
	}
	return passed, report.String()
}

// filterInsignificantChains removes chains based on short, numeric values that are likely coincidental.
func filterInsignificantChains(chains []CanonicalChain) []CanonicalChain {
	var significantChains []CanonicalChain
	for _, chain := range chains {
		// For purely numeric values, require a longer length to be considered
		// significant. Short numbers like "3600", "200", "1" are common
		// constants that cause false-positive chain matches.
		if _, err := strconv.ParseFloat(chain.Value, 64); err == nil {
			if len(chain.Value) >= 6 {
				significantChains = append(significantChains, chain)
			}
			continue
		}
		// Non-numeric values: keep if length >= 4, or any non-trivial string.
		if len(chain.Value) >= 4 {
			significantChains = append(significantChains, chain)
			continue
		}
		// Short non-numeric values are still significant (e.g., "true", "v1")
		significantChains = append(significantChains, chain)
	}
	return significantChains
}

func toCamelCase(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "v"
	}
	// Tokenize on non-alphanumeric
	var tokens []string
	var cur []rune
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			cur = append(cur, r)
		} else if len(cur) > 0 {
			tokens = append(tokens, string(cur))
			cur = cur[:0]
		}
	}
	if len(cur) > 0 {
		tokens = append(tokens, string(cur))
	}
	if len(tokens) == 0 {
		return "v"
	}

	// Build camelCase
	var b strings.Builder
	for i, t := range tokens {
		t = strings.ToLower(t)
		if i == 0 {
			b.WriteString(t)
		} else {
			r, size := utf8.DecodeRuneInString(t)
			if size == 0 || r == utf8.RuneError {
				// invalid/empty, just append as-is
				b.WriteString(t)
			} else {
				b.WriteRune(unicode.ToUpper(r))
				b.WriteString(t[size:])
			}
		}
	}
	out := b.String()
	// Must not start with digit for Go templates: {{ .<ident> }}
	if out != "" {
		r, size := utf8.DecodeRuneInString(out)
		if size == 0 || r == utf8.RuneError || unicode.IsDigit(r) {
			out = "v" + out
		}
	}

	return out
}

// Use this everywhere you finalize a base key.
func normalizeBaseKey(s string) string {
	return toCamelCase(s)
}
