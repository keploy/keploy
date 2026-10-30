package replay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	matcherUtils "go.keploy.io/server/v3/pkg/matcher"
	httpMatcher "go.keploy.io/server/v3/pkg/matcher/http"
	"go.keploy.io/server/v3/pkg/models"
	setfile "go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/templates"
	"go.keploy.io/server/v3/pkg/variables"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// toSend is the copy of tc a replay hands to SimulateRequest: its request with
// each placeholder at this run's value, on maps and slices of its own. tc
// itself is never rendered, so a re-send (retryResetOnce) and a RetryPassing
// cycle start again from the test case as written. The expected response is
// resolved from this copy once the answer is in (resolveExpected): a hook that
// changes the case it is handed in place, such as enterprise's
// DecryptingTestHooks decrypting it, changes this copy, and what is compared.
func toSend(run *templates.Run, tc *models.TestCase) models.TestCase {
	sent := *tc
	switch tc.Kind {
	case models.GRPC_EXPORT:
		sent.GrpcReq = run.GRPCRequest(tc.GrpcReq)
	default:
		sent.HTTPReq = run.Request(tc.HTTPReq)
		// Each value in a rendered URL is escaped for its place; the URL
		// goes out as it stands (pkg.prepareHTTPRequest).
		sent.HTTPReq.URLRendered = sent.HTTPReq.URL != tc.HTTPReq.URL
	}
	if tc.HTTPResp.Header != nil {
		sent.HTTPResp.Header = make(map[string]string, len(tc.HTTPResp.Header))
		for k, v := range tc.HTTPResp.Header {
			sent.HTTPResp.Header[k] = v
		}
	}
	return sent
}

// resolvedCase is a test case's expected response made ready for the app's
// answer, and what its variable places made of that answer.
type resolvedCase struct {
	expected *models.TestCase
	opts     []httpMatcher.MatchOption
	captures []templates.Capture
	// failures are variable places the answer does not satisfy; each fails
	// the test case whatever the comparison says.
	failures []templates.Failure
}

// resolveExpected is the test case to compare the app's answer with: sent's
// expected response after its captures took the answer's values (which then
// hold for the rest of the run) and its other placeholders took the run's
// values. The options tell the matcher what the body was when it was recorded,
// for the headers that are a digest of it.
func (r *Replayer) resolveExpected(run *templates.Run, check *templates.CheckResult, sent *models.TestCase, answer *models.HTTPResp, testSetID string) resolvedCase {
	res := run.Expected(sent.HTTPResp, *answer, templates.ExpectOptions{IgnoreOrdering: r.responseIgnoreOrdering()})
	r.logTemplateFailures(sent.Name, testSetID, res.Failures, check, true)
	r.logCaptures(sent.Name, res.Captures)
	exp := *sent
	exp.HTTPResp = res.Resp
	var opts []httpMatcher.MatchOption
	if res.BodyTemplated {
		opts = append(opts, httpMatcher.WithRecordedBody(res.RecordedBody))
	}
	if len(res.Failures) > 0 {
		// The test case has failed on its own: a comparison that matches
		// (schema-match's included) prints no pass line for it.
		opts = append(opts, httpMatcher.WithoutPassLine())
	}
	return resolvedCase{expected: &exp, opts: opts, captures: res.Captures, failures: res.Failures}
}

// grpcTemplateFailures are the use places of sent's expected gRPC response
// that the answer does not satisfy, each asserted on its own whatever the
// comparison and its noise say.
func (r *Replayer) grpcTemplateFailures(run *templates.Run, check *templates.CheckResult, sent *models.TestCase, answer *models.GrpcResp, testSetID string) []templates.Failure {
	fs := run.GRPCUseFailures(sent.GrpcResp, *answer)
	r.logTemplateFailures(sent.Name, testSetID, fs, check, false)
	return fs
}

// streamTemplateFailures are the use places of sent's streamed expected
// response that the answer does not satisfy: its headers, and each frame
// against the frame received at its position (received, the bytes the
// answer delivered, which the run splits as a recording splits them). Each is
// asserted on its own whatever the stream comparison and its noise say.
// complete says received is all the answer delivered (see
// templates.Run.StreamUseFailures).
func (r *Replayer) streamTemplateFailures(run *templates.Run, check *templates.CheckResult, sent *models.TestCase, received string, complete bool, answer *models.HTTPResp, testSetID string) []templates.Failure {
	fs := run.StreamUseFailures(sent.HTTPResp, received, complete, *answer, templates.ExpectOptions{IgnoreOrdering: r.config.Test.IgnoreOrdering})
	r.logTemplateFailures(sent.Name, testSetID, fs, check, false)
	return fs
}

// logCaptures says, for each value a test case's answer gave a capture, its
// key, where it was and how long it is: never the value, which is what the
// app issued (a session token, an ID). The report's captures keep it.
func (r *Replayer) logCaptures(testCase string, caps []templates.Capture) {
	for _, c := range caps {
		r.logger.Debug("captured a template value from the app's answer",
			zap.String("testcase", testCase), zap.String("key", c.Key), zap.Int("value_bytes", len(c.Value)), zap.String("where", templates.PlaceShape(c.Where)))
	}
}

// logTemplateFailures says, for each variable place a test case failed on,
// where, why, and the next step for both things it can mean
// (templates.CheckResult.NextStep). A capture that misses leaves its key at
// its previous value for the test cases after it.
func (r *Replayer) logTemplateFailures(testCase, testSetID string, fs []templates.Failure, check *templates.CheckResult, canCapture bool) {
	for _, f := range fs {
		next := "if the value changed on purpose, edit the expected value or re-record the test case"
		if check != nil {
			next = check.NextStep(testCase, f, canCapture)
		}
		r.logger.Warn("a template variable's place does not hold what it must, so the test case fails",
			zap.String("testcase", testCase), zap.String("testset", testSetID),
			zap.String("where", templates.PlaceShape(f.Where)), zap.String("placeholder", f.Placeholder),
			zap.String("why", whyInReport(f.Why)),
			zap.String("next_step", next))
	}
}

// whyInReport is a template failure's or refusal's why as a log line names
// it: by its length. The why quotes what the answer or the recording held
// there (a header's value, a cookie's, a body's span, a recorded value), and
// the report's template_failures keep it whole.
func whyInReport(why string) string {
	return fmt.Sprintf("%d bytes, in the report's template_failures", len(why))
}

// markTemplateFailures says on a result that failed on template variable
// places why: the TEMPLATE_VARIABLE category, and HIGH risk. A variable place
// is never noise: an auto-replay learns noise from a failure graded below
// HIGH (enterprise's isAutoReplayHighRisk, k8s-proxy's IsAutoReplayHighRisk),
// and noise written over a variable place would only stop the next run
// asserting what the test case says. The report and JUnit name each place
// from the test result's template_failures.
func markTemplateFailures(res *models.Result) {
	if res == nil {
		return
	}
	res.FailureInfo.Category = appendCategoryUnique(res.FailureInfo.Category, models.TemplateVariable)
	res.FailureInfo.Risk = models.High
}

// vetoTemplateFailure is the reason a failed test case whose mock set
// diverged stays FAILED when it failed on a template variable's place.
const vetoTemplateFailure = "the test case failed on a template variable's place"

// keepTemplateFailure is the verdict of a test case that failed on a template
// variable's place (failed): FAILED, and its set fails, whatever its mock set
// did. A diverged mock set demotes any other failure to OBSOLETE unless
// --strict-failure is set or --assert-dependencies finds a dependency of the
// test case missing (resolveTestOutcome), and an OBSOLETE test case is not
// judged (FailureInfo.MappingStale); a template failure is never hidden that
// way. The log line then names why
// it stays FAILED, as it names a veto flag.
func keepTemplateFailure(o testOutcome, failed bool) testOutcome {
	if !failed || o.Status != models.TestStatusObsolete {
		return o
	}
	o.Status, o.FailsTestSet = models.TestStatusFailed, true
	o.Log, o.VetoFlags = mismatchLogVetoedFailure, vetoTemplateFailure
	return o
}

// reportCaptures is what the report says a test case captured.
func reportCaptures(caps []templates.Capture) []models.TemplateCapture {
	if len(caps) == 0 {
		return nil
	}
	out := make([]models.TemplateCapture, 0, len(caps))
	for _, c := range caps {
		out = append(out, models.TemplateCapture{Key: c.Key, Value: c.Value, Where: c.Where})
	}
	return out
}

// reportRefusals is what the report says about a test case refused when the
// set loaded: each placeholder it cannot run with, where, and why.
func reportRefusals(fs []templates.Refusal) []models.TemplateFailure {
	out := make([]models.TemplateFailure, 0, len(fs))
	for _, f := range fs {
		out = append(out, models.TemplateFailure{Where: f.Where, Placeholder: f.Placeholder, Why: "refused when the test set loaded: " + f.Why, Refused: true})
	}
	return out
}

// reportFailures is what the report says about the variable places a test
// case failed on.
func reportFailures(fs []templates.Failure) []models.TemplateFailure {
	if len(fs) == 0 {
		return nil
	}
	out := make([]models.TemplateFailure, 0, len(fs))
	for _, f := range fs {
		out = append(out, models.TemplateFailure{Where: f.Where, Placeholder: f.Placeholder, Why: f.Why})
	}
	return out
}

// noAnswerView is sent as it stands with no answer to capture from: its use
// placeholders at this run's values, its captures as written. A failed result
// is built from it, so the report shows the request that was sent.
func noAnswerView(run *templates.Run, sent *models.TestCase) *models.TestCase {
	out := *sent
	switch sent.Kind {
	case models.GRPC_EXPORT:
		out.GrpcResp = run.GRPCExpected(sent.GrpcResp)
	default:
		out.HTTPResp = run.ExpectedNoAnswer(sent.HTTPResp)
	}
	return &out
}

// streamView is sent's streamed expected response with this run's values, as
// the stream comparison reads it (templates.Run.ExpectedStream): the case a
// streamed result is built from.
func streamView(run *templates.Run, sent *models.TestCase) *models.TestCase {
	out := *sent
	out.HTTPResp = run.ExpectedStream(sent.HTTPResp)
	return &out
}

// passValues keeps a RetryPassing run's template values right at every test
// position. A later cycle re-runs only the test cases that passed; a test case
// it skips still gave its keys their values in the full pass, so at its place
// in the set it gives them again what it last captured. At each position a key
// then holds the value a whole pass gives it there.
type passValues struct {
	run   *templates.Run
	order map[*models.TestCase]int
	all   []*models.TestCase
	last  map[*models.TestCase][]templates.Capture
	next  int
}

func newPassValues(run *templates.Run, all []*models.TestCase) *passValues {
	order := make(map[*models.TestCase]int, len(all))
	for i, tc := range all {
		order[tc] = i
	}
	return &passValues{run: run, order: order, all: all, last: map[*models.TestCase][]templates.Capture{}}
}

// start begins a pass: every key at its recorded value (rule 5).
func (p *passValues) start() {
	p.run.Reset()
	p.next = 0
}

// reach is called before tc runs in this pass: each test case before it that
// the pass skips gives its last captures, in order. What tc itself captured
// before is dropped, since it runs again now.
func (p *passValues) reach(tc *models.TestCase) {
	pos, ok := p.order[tc]
	if !ok {
		return
	}
	for ; p.next < pos; p.next++ {
		p.run.Apply(p.last[p.all[p.next]])
	}
	p.next = pos + 1
	delete(p.last, tc)
}

// captured records what tc captured in this pass.
func (p *passValues) captured(tc *models.TestCase, caps []templates.Capture) {
	if len(caps) > 0 {
		p.last[tc] = caps
	}
}

// finish gives the captures of the test cases after the last one this pass
// ran, so what follows the pass (the streamed test cases) sees the values a
// whole pass ends with.
func (p *passValues) finish() {
	for ; p.next < len(p.all); p.next++ {
		p.run.Apply(p.last[p.all[p.next]])
	}
}

// refuseUntiedArrayUses refuses, when the set loads, each use inside an array
// that cannot be tied to its element. Under test.ignoreOrdering a use in an
// array is satisfied by any element of the answer's array that holds the
// key's value at its place; the comparison's own multiset match of the
// elements ties it to its element. Three things leave no such comparison: a
// noise entry over its field or over the element, --schema-match (which
// compares no value), and assertions that judge the test case in place of
// the comparison without reading the whole body
// (httpMatcher.AssertionsUntieBody, the predicate Match judges by). Then any
// element holding the value passes it, the moved one included
// ([{"id":A,"status":"paid"}] answered with A on the pending order). A
// capture there is refused already (Check). The refusal names what unties
// it and the edits; the test case fails unsent. A use in a streamed frame is
// not refused: the stream comparison reads a frame's arrays in order, and
// the use is asserted at its own index (templates.Run.StreamUseFailures), so
// nothing unties it there.
func (r *Replayer) refuseUntiedArrayUses(testSetID string, check *templates.CheckResult, cases []*models.TestCase) {
	if !r.responseIgnoreOrdering() || len(check.Places) == 0 {
		return
	}
	byName := make(map[string]*models.TestCase, len(cases))
	for _, tc := range cases {
		if tc != nil {
			byName[tc.Name] = tc
		}
	}
	global := r.httpNoiseConfig(testSetID)
	for _, p := range check.Places {
		tc := byName[p.TestCase]
		if tc == nil || p.Placeholder.Capture || p.Stream || !inArray(p.Path) {
			continue
		}
		untied := "nothing ties it to its element: any element holding " + p.Placeholder.Key + "'s value would satisfy it, whatever the element's other fields say"
		literal := "write " + p.Placeholder.Key + "'s recorded value in place of the placeholder (as the test case was recorded)"
		var why string
		switch entry := r.noiseCovering(p, tc, global); {
		case r.schemaMatch():
			why = "this use is inside an array and test.ignoreOrdering is on, and --schema-match compares no value, so " + untied + "; run this set without --schema-match, turn ignoreOrdering off for it, or " + literal
		case entry != "":
			why = "this use is inside an array and test.ignoreOrdering is on, and the noise entry " + entry + " covers its place, so " + untied + "; remove that noise entry (the variable is asserted anyway), turn ignoreOrdering off for this set, or " + literal
		default:
			names := httpMatcher.AssertionsUntieBody(tc)
			if names == "" {
				continue
			}
			why = "this use is inside an array and test.ignoreOrdering is on, and the test case's assertions (" + names + ") judge its answer in place of the comparison and read no whole body, so " + untied + "; add json_equal to its assertions (it compares the body as written), remove its assertions, turn ignoreOrdering off for this set, or " + literal
		}
		check.Refused[p.TestCase] = append(check.Refused[p.TestCase], templates.Refusal{TestCase: p.TestCase, Where: p.Where, Placeholder: p.Placeholder.Text, Why: why})
	}
}

// inArray reports whether a place's path steps into an array.
func inArray(path []templates.PathSeg) bool {
	for _, s := range path {
		if s.IsIndex {
			return true
		}
	}
	return false
}

// logTemplateCheck says, once when a test set loads, which placeholders its
// test cases cannot run with, which keys they name that it does not define,
// which template text is outside the grammar (a key that is not one, an
// expression), which keys a request sends that no earlier test case
// captures, and which variable places a noise entry covers.
func (r *Replayer) logTemplateCheck(testSetID string, check templates.CheckResult, cases []*models.TestCase) {
	for _, name := range templates.RefusedNames(check.Refused) {
		for _, f := range check.Refused[name] {
			utils.LogError(r.logger, nil, "a test case's placeholder is refused; the test case fails without being sent",
				zap.String("testset", testSetID), zap.String("testcase", f.TestCase), zap.String("placeholder", f.Placeholder),
				zap.String("where", templates.PlaceShape(f.Where)), zap.String("why", whyInReport(f.Why)),
				zap.String("next_step", "edit the test case as the report's template_failures say, then run keploy test again"))
		}
	}
	for _, u := range check.Undefined {
		if u.Expression {
			r.logger.Warn("template text is an expression (a pipe, a function call, a bare dot), which keploy test does not render: it is sent and compared as the literal text",
				zap.String("testset", testSetID), zap.String("testcase", u.TestCase), zap.String("placeholder", u.Placeholder),
				zap.String("where", templates.PlaceShape(u.Where)),
				zap.String("next_step", "an older keploy rendered some of these ({{ .id | print }}): write the value the expression stood for in its place, or a placeholder keploy reads ({{string .k}}, {{int .k}}, {{float .k}}), or leave the text as it is"))
			continue
		}
		if u.OutsideGrammar {
			r.logger.Warn("a placeholder names a key that is not a template key (one name of letters, digits and _, starting with a letter or _): it is sent and compared as the literal text",
				zap.String("testset", testSetID), zap.String("testcase", u.TestCase), zap.String("placeholder", u.Placeholder),
				zap.String("where", templates.PlaceShape(u.Where)),
				zap.String("next_step", "a template key is one name ({{string .userId}}): rename the key in config.yaml's template and in the test cases, or leave the text as it is"))
			continue
		}
		r.logger.Warn("a placeholder names a key the test set's config.yaml does not define: it is sent and compared as the literal text",
			zap.String("testset", testSetID), zap.String("testcase", u.TestCase), zap.String("placeholder", u.Placeholder),
			zap.String("where", templates.PlaceShape(u.Where)),
			zap.String("next_step", "add the key to config.yaml's template (its value as recorded), or fix the key's name in the test case"))
	}
	for _, u := range check.Uncaptured {
		r.logger.Warn("a request sends a template key that no earlier test case captures: every run sends config.yaml's recorded value there",
			zap.String("testset", testSetID), zap.String("testcase", u.TestCase), zap.String("placeholder", u.Placeholder),
			zap.String("where", templates.PlaceShape(u.Where)),
			zap.String("next_step", u.UncapturedNextStep()))
	}
	r.warnNoiseOverVariables(testSetID, check.Places, cases, check.Refused)
}

// The next steps of the noise-over-variable warning. A capture keeps its
// place: it gives the later requests the value the app answers there, and the
// noise changes nothing it asserts.
const (
	nextStepNoiseOverUse     = "nothing to do if this is intended; to stop asserting the place, replace the placeholder with the recorded value and keep the noise"
	nextStepNoiseOverCapture = "nothing to do: a capture is only checked to be present, of its type and not empty, which the noise entry does not change; keep the capture, which gives the later requests the value the app answers here"
)

// warnNoiseOverVariables names, once per set, each noise entry that covers a
// variable place. The variable wins: its place is asserted anyway (a capture
// present, of its type and not empty; a use equal to its key's value), and
// the noise still applies to everything else it covers. A test case refused
// when the set loaded is never sent, and is not named here.
func (r *Replayer) warnNoiseOverVariables(testSetID string, places []templates.Place, cases []*models.TestCase, refused map[string][]templates.Refusal) {
	if len(places) == 0 {
		return
	}
	byName := make(map[string]*models.TestCase, len(cases))
	for _, tc := range cases {
		if tc != nil {
			byName[tc.Name] = tc
		}
	}
	global := r.httpNoiseConfig(testSetID)
	warned := map[string]bool{}
	for _, p := range places {
		tc := byName[p.TestCase]
		if tc == nil || len(refused[p.TestCase]) > 0 {
			continue
		}
		// The entry covers the place, so its keys are the place's: it is
		// named by its shape, as the place is (a key may be a user's ID).
		entry := r.noiseCovering(p, tc, global)
		if entry == "" || warned[p.TestCase+"\x00"+p.Where+"\x00"+entry] {
			continue
		}
		warned[p.TestCase+"\x00"+p.Where+"\x00"+entry] = true
		next := nextStepNoiseOverUse
		if p.Placeholder.Capture {
			next = nextStepNoiseOverCapture
		}
		r.logger.Warn("a noise entry covers a template variable's place: the variable is asserted there anyway, and the noise still applies to the rest",
			zap.String("testset", testSetID), zap.String("testcase", p.TestCase), zap.String("noise", models.LearnedFieldShape(entry)),
			zap.String("variable", p.Placeholder.Text), zap.String("where", templates.PlaceShape(p.Where)),
			zap.String("next_step", next))
	}
}

// noiseCovering names the noise entry that covers a variable place, as the
// matcher would apply it, or "".
func (r *Replayer) noiseCovering(p templates.Place, tc *models.TestCase, global map[string]map[string][]string) string {
	tcBody, tcHeader, _ := matcherUtils.SplitNoise(tc.Noise, r.logger)
	if p.GRPC {
		return grpcNoiseCovering(p, tcBody, tcHeader, global)
	}
	if p.Stream {
		return streamNoiseCovering(p, tc.Noise, global)
	}
	if p.Header != "" {
		if _, all := tcHeader["header"]; all {
			return "header (this test case)"
		}
		if k := bestKey(p.Header, tcHeader); k != "" {
			return "header." + k + " (this test case)"
		}
		if _, all := global["header"]["header"]; all {
			return "header (globalNoise)"
		}
		if k := bestKey(p.Header, global["header"]); k != "" {
			return "header." + k + " (globalNoise)"
		}
		if !r.config.Test.DisableAutoHeaderNoise && models.IsVolatileResponseHeader(p.Header) {
			return p.Header + " (auto header noise)"
		}
		return ""
	}
	return matcherUtils.BodyNoiseCovering(p.KeyPath(), tc.Noise, global, r.logger)
}

// grpcNoiseCovering names the noise entry that covers a gRPC variable place,
// as the gRPC matcher applies it: a header entry by the header's name, and a
// body entry "decoded_data" over a message's text.
func grpcNoiseCovering(p templates.Place, tcBody, tcHeader map[string][]string, global map[string]map[string][]string) string {
	if p.Header != "" {
		for _, src := range []struct {
			noise map[string][]string
			from  string
		}{{tcHeader, "this test case"}, {global["header"], "globalNoise"}} {
			for k := range src.noise {
				if strings.EqualFold(k, p.Header) {
					return "header." + k + " (" + src.from + ")"
				}
			}
		}
		return ""
	}
	if _, ok := tcBody["decoded_data"]; ok {
		return "body.decoded_data (this test case)"
	}
	if _, ok := global["body"]["decoded_data"]; ok {
		return "body.decoded_data (globalNoise)"
	}
	return ""
}

// streamNoiseCovering names the noise entry that covers a place in a streamed
// frame, as the stream comparison applies noise: an entry without a dot (a
// "body." prefix dropped) removes that key at any depth of a frame's JSON.
func streamNoiseCovering(p templates.Place, tcNoise map[string][]string, global map[string]map[string][]string) string {
	keys := map[string]bool{}
	for _, k := range p.KeyPath() {
		keys[strings.ToLower(k)] = true
	}
	for _, src := range []struct {
		noise map[string][]string
		from  string
	}{{tcNoise, "this test case"}, {global["body"], "globalNoise"}} {
		names := make([]string, 0, len(src.noise))
		for k := range src.noise {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			name := strings.ToLower(strings.TrimSpace(k))
			name = strings.TrimPrefix(name, "body.")
			if name != "" && !strings.Contains(name, ".") && keys[name] {
				return "body." + strings.TrimPrefix(strings.TrimSpace(k), "body.") + " (" + src.from + ")"
			}
		}
	}
	return ""
}

// bestKey is the most specific noise key whose name the header holds, as
// matcherUtils.SubstringKeyMatch picks it.
func bestKey(header string, noise map[string][]string) string {
	if len(noise) == 0 {
		return ""
	}
	if _, ok := matcherUtils.SubstringKeyMatch(header, noise); !ok {
		return ""
	}
	best := ""
	for k := range noise {
		if strings.Contains(strings.ToLower(header), strings.ToLower(k)) && (len(k) > len(best) || (len(k) == len(best) && k < best)) {
			best = k
		}
	}
	return best
}

// nextStepRefused is the next step of a test case refused when its set
// loaded.
const nextStepRefused = "edit the test case's placeholder as the reason says, then run keploy test again"

// refusalError is why a refused test case fails, as its log line says it:
// each refusal's placeholder, its place by its shape and its why by its
// length (whyInReport); the report's template_failures keep them whole.
func refusalError(fs []templates.Refusal) error {
	msgs := make([]string, 0, len(fs))
	for _, f := range fs {
		msgs = append(msgs, f.Placeholder+" at "+templates.PlaceShape(f.Where)+": "+whyInReport(f.Why))
	}
	return errors.New("refused when the test set loaded: " + strings.Join(msgs, "; "))
}

// failRefusedTest reports a test case refused when the set loaded as failed,
// unsent: its refusals are its template failures (where, placeholder, why),
// graded TEMPLATE_VARIABLE and HIGH like any template failure. Nothing was
// sent and nothing compared, so the result holds no comparison: no status,
// header or body row, and no category but the refusal's.
func (r *Replayer) failRefusedTest(ctx context.Context, testRunID, testSetID string, tc *models.TestCase, refusals []templates.Refusal) error {
	utils.LogError(r.logger, refusalError(refusals), "failed the test without sending it",
		zap.String("testcase", tc.Name),
		zap.String("next_step", nextStepRefused))
	res := r.refusedResult(testSetID, tc, reportRefusals(refusals), models.TemplateVariable)
	if err := r.reportDB.InsertTestCaseResult(ctx, testRunID, testSetID, res); err != nil {
		utils.LogError(r.logger, err, "failed to insert test case result for a test that was not sent")
		return err
	}
	return nil
}

// refusedResult is the result of a test case refused unsent, with failures,
// each a refusal, as its template failures, graded HIGH and filed under
// category: TEMPLATE_VARIABLE for a placeholder the test case cannot run with
// (failRefusedTest), TEST_SET_REFUSED for a set keploy refused whole
// (refuseInterruptedSet).
func (r *Replayer) refusedResult(testSetID string, tc *models.TestCase, failures []models.TemplateFailure, category models.FailureCategory) *models.TestResult {
	now := time.Now().UTC()
	res := &models.TestResult{
		Kind:             tc.Kind,
		Name:             testSetID,
		Status:           models.TestStatusFailed,
		Started:          now.Unix(),
		Completed:        now.Unix(),
		TestCaseID:       tc.Name,
		TestCasePath:     filepath.Join(r.config.Path, testSetID),
		MockPath:         filepath.Join(r.config.Path, testSetID, "mocks.yaml"),
		Noise:            tc.Noise,
		TimeTaken:        "0s",
		TemplateFailures: failures,
		Result: models.Result{
			StatusCode:    models.IntResult{Normal: true},
			HeadersResult: []models.HeaderResult{},
			BodyResult:    []models.BodyResult{},
		},
	}
	switch tc.Kind {
	case models.GRPC_EXPORT:
		res.GrpcReq = tc.GrpcReq
	default:
		res.Req = tc.HTTPReq
	}
	res.Result.FailureInfo.Category = appendCategoryUnique(res.Result.FailureInfo.Category, category)
	res.Result.FailureInfo.Risk = models.High
	res.FailureInfo.Risk, res.FailureInfo.Category = res.Result.FailureInfo.Risk, res.Result.FailureInfo.Category
	return res
}

// interruptedSets is a test store that can say a test set holds what an
// interrupted keploy templatize left beside its files
// (testdb.TestYaml.Interrupted). It is asserted, never added to TestDB:
// other programs implement TestDB.
type interruptedSets interface {
	Interrupted(ctx context.Context, testSetID string) error
}

// refuseInterruptedSet fails a test set a killed keploy templatize left
// half-written, and runs none of its test cases: some of them may hold
// placeholders of keys its config.yaml does not define yet, and others the
// recording, so a run would replay a set nobody wrote. The error names each
// file left and the steps that put the set in one state, the steps keploy
// templatize prints. It is logged once, and each test case of the set gets a
// FAILED result refused unsent with it as the reason (refusedResult, filed
// under TEST_SET_REFUSED: no template variable failed), so
// every report format, the run's totals and --must-pass read the set as
// failed and refused: none of its test cases is removed. It reports whether
// it refused the set. Any other error of the store's (a set directory it
// cannot read) is returned as itself, as an unreadable set's is; err is also
// a failure to save the set's results.
func (r *Replayer) refuseInterruptedSet(ctx context.Context, testRunID, testSetID string, testCases []*models.TestCase, started time.Time) (bool, error) {
	store, ok := r.testDB.(interruptedSets)
	if !ok {
		return false, nil
	}
	why := store.Interrupted(ctx, testSetID)
	if why == nil {
		return false, nil
	}
	if !errors.Is(why, setfile.ErrReplaceInterrupted) {
		return false, fmt.Errorf("failed to read the test set's files: %w", why)
	}
	utils.LogError(r.logger, why, "keploy test refused this test set, and ran none of its test cases: an interrupted keploy templatize left it half-written",
		zap.String("testSet", testSetID),
		zap.String("next_step", "do what the error says (it names each file and the step for it), then run keploy test again"))
	refusal := []models.TemplateFailure{{
		Where:   "test set " + testSetID,
		Why:     "refused when the test set loaded: " + why.Error(),
		Refused: true,
	}}
	results := make([]models.TestResult, 0, len(testCases))
	for _, tc := range testCases {
		res := r.refusedResult(testSetID, tc, refusal, models.TestSetRefused)
		if err := r.reportDB.InsertTestCaseResult(ctx, testRunID, testSetID, res); err != nil {
			utils.LogError(r.logger, err, "failed to insert test case result for a test that was not sent")
			return true, err
		}
		results = append(results, *res)
	}
	timeTaken := time.Since(started)
	report := &models.TestReport{
		Version:       models.GetVersion(),
		TestSet:       testSetID,
		Status:        string(models.TestSetStatusFailed),
		FailureReason: why.Error(),
		Failure:       len(testCases),
		Total:         len(testCases),
		HighRisk:      len(testCases),
		Tests:         results,
		TimeTaken:     timeTaken.String(),
		CmdUsed:       r.config.Test.CmdUsed,
		Proven:        provenswitch.On(),
	}
	r.noteSetEnd(report)
	if err := r.reportDB.InsertReport(ctx, testRunID, testSetID, report); err != nil {
		utils.LogError(r.logger, err, "failed to insert report")
		return true, err
	}
	r.completeTestReportMu.Lock()
	r.completeTestReport[testSetID] = TestReportVerdict{
		total:     report.Total,
		failed:    report.Failure,
		status:    false,
		duration:  timeTaken,
		timeTaken: timeTaken.String(),
	}
	r.totalTests += report.Total
	r.totalTestFailed += report.Failure
	r.totalTestTimeTaken += timeTaken
	r.completeTestReportMu.Unlock()
	return true, nil
}

// mustPassDeletions splits the failed test cases of a run in two for
// --must-pass: those it removes, and those refused when their set loaded
// (every template failure a refusal), which were never sent and are kept.
func mustPassDeletions(results []models.TestResult) (deletable, refused []string) {
	for _, res := range results {
		if res.Status != models.TestStatusFailed {
			continue
		}
		if refusedUnsent(res) {
			refused = append(refused, res.TestCaseID)
			continue
		}
		deletable = append(deletable, res.TestCaseID)
	}
	return deletable, refused
}

// refusedUnsent reports whether res is a test case refused when its set
// loaded: it has template failures, and each is a refusal.
func refusedUnsent(res models.TestResult) bool {
	if len(res.TemplateFailures) == 0 {
		return false
	}
	for _, f := range res.TemplateFailures {
		if !f.Refused {
			return false
		}
	}
	return true
}

// requestVariables are the template variables tc's request sends, for the agent
// to bind as tc begins (proven switch): a dependency call that the learn step
// declared to use one is then asserted to send this run's value. None for a
// set without templates, a test case that is not HTTP, or with the switch off.
func requestVariables(run *templates.Run, tc *models.TestCase) []models.VariableBinding {
	if !provenswitch.On() || run == nil || run.Empty() || tc == nil || tc.Kind != models.HTTP {
		return nil
	}
	var out []models.VariableBinding
	for _, v := range run.RequestVariables(tc.HTTPReq) {
		out = append(out, models.VariableBinding{Name: v.Key, Value: v.Value, Recorded: v.Recorded, Mock: "test case " + tc.Name, Place: "request", Given: true})
	}
	return out
}

// testOpening is what the mock filter that opens a test case carries for it
// (proven switch): the values its request gives (requestVariables), and, in
// record's full round, its request itself (testRequestOf), which the agent's
// learn step reads.
type testOpening struct {
	vars    []models.VariableBinding
	request *models.TestRequest
}

// openingOf is tc's opening.
func (r *Replayer) openingOf(run *templates.Run, tc *models.TestCase) testOpening {
	o := testOpening{vars: requestVariables(run, tc)}
	if r.learning() {
		o.request = testRequestOf(run, tc, r.config.Path)
	}
	return o
}

// maxTestRequest is the most of a test case's request, in bytes of its
// parts, recorded and as sent, that the filter carries to the learn step.
const maxTestRequest = 16 << 20

// testRequestOf is tc's own request, as recorded (with the values its
// template placeholders were recorded with: templates.Run.RecordedRequest)
// and as this run sends it (toSend: with its template values), part by part,
// in a stable order, for
// record's full round's learn step (models.TestRequest): an HTTP test case's
// URL, headers, body (read back from its asset where it is kept apart from
// the test case, against keployPath as the replay reads it) and every part
// of its form, each as its own part in the order recorded ("form.<key>", a
// repeated key's later parts "form.<key>#2", ...), a value kept apart and a
// file's contents read back too; a gRPC test case's headers and every
// message it sends ("body", then "body#2", ...). A request keploy cannot
// read here (of another kind, an asset it cannot read, one larger than
// maxTestRequest) says why, with no parts, and the learn step refuses what
// only it could account for.
func testRequestOf(run *templates.Run, tc *models.TestCase, keployPath string) *models.TestRequest {
	if tc == nil {
		return nil
	}
	out := &models.TestRequest{Test: tc.Name}
	sent := toSend(run, tc)
	var parts []models.TestRequestPart
	size := 0
	add := func(name, recorded, live string) {
		parts = append(parts, models.TestRequestPart{Name: name, Recorded: recorded, Sent: live})
		size += len(recorded) + len(live)
	}
	headers := func(prefix string, rec, live map[string]string) {
		for _, k := range headerNames(rec, live) {
			add(prefix+k, rec[k], live[k])
		}
	}
	read := func(path string) (string, error) {
		if keployPath != "" && !filepath.IsAbs(path) {
			path = filepath.Join(keployPath, path)
		}
		b, err := os.ReadFile(path)
		return string(b), err
	}
	// numbered names the n-th part (from 0) of a name: the name, then
	// "<name>#2", ...
	numbered := func(name string, n int) string {
		if n == 0 {
			return name
		}
		return name + "#" + strconv.Itoa(n+1)
	}
	switch tc.Kind {
	case models.HTTP:
		// As recorded: the text the recording held where each placeholder
		// now is, not the placeholder (templates.Run.RecordedRequest).
		rec, live := run.RecordedRequest(tc.HTTPReq), sent.HTTPReq
		add("url", rec.URL, live.URL)
		headers("header.", rec.Header, live.Header)
		recBody, liveBody := rec.Body, live.Body
		if rec.Body == "" && rec.BodyRef.Path != "" {
			b, err := read(rec.BodyRef.Path)
			if err != nil {
				out.Unread = "its body is kept apart from the test case (" + rec.BodyRef.Path + "), and keploy could not read it: " + err.Error()
				return out
			}
			recBody = b
			if live.Body == "" {
				liveBody = b
			}
		}
		add("body", recBody, liveBody)
		if rec.Binary != "" || live.Binary != "" {
			add("binary", rec.Binary, live.Binary)
		}
		seen := map[string]int{}
		for i, f := range rec.Form {
			var lf models.FormData
			if i < len(live.Form) {
				lf = live.Form[i]
			}
			name := numbered("form."+f.Key, seen[f.Key])
			seen[f.Key]++
			r, l, why := formPart(f, lf, read)
			if why != "" {
				out.Unread = "its form part " + name + " " + why
				return out
			}
			add(name, r, l)
		}
	case models.GRPC_EXPORT:
		rec, live := run.RecordedGRPCRequest(tc.GrpcReq), sent.GrpcReq
		headers("header.", rec.Headers.PseudoHeaders, live.Headers.PseudoHeaders)
		headers("header.", rec.Headers.OrdinaryHeaders, live.Headers.OrdinaryHeaders)
		add("body", rec.Body.DecodedData, live.Body.DecodedData)
		for i, m := range rec.AdditionalMessages {
			var lm models.GrpcLengthPrefixedMessage
			if i < len(live.AdditionalMessages) {
				lm = live.AdditionalMessages[i]
			}
			add(numbered("body", i+1), m.DecodedData, lm.DecodedData)
		}
	default:
		out.Unread = "keploy reads no request of a test case of kind " + string(tc.Kind)
		return out
	}
	if size > maxTestRequest {
		out.Unread = "it is " + strconv.Itoa(size) + " bytes, recorded and as sent, more than the " + strconv.Itoa(maxTestRequest) + " keploy carries to the learn step"
		return out
	}
	out.Parts = parts
	return out
}

// formPart is one part of a test case's form, as recorded (f) and as this
// run sends it (live): its values, one per line, a value kept apart from the
// test case and a file's contents read back (read), as the replay reads
// them to send them. why says why it could not be read, "" when it was.
func formPart(f, live models.FormData, read func(string) (string, error)) (recorded, sent, why string) {
	text := func(fd models.FormData) (string, string) {
		var vals []string
		for j, v := range fd.Values {
			// A value kept apart from the test case (the replay reads it
			// back before it sends the form).
			if v == "" && len(fd.FileNames) == 0 && j < len(fd.Paths) && fd.Paths[j] != "" {
				b, err := read(fd.Paths[j])
				if err != nil {
					return "", "its value kept apart from the test case (" + fd.Paths[j] + ") could not be read: " + err.Error()
				}
				v = b
			}
			vals = append(vals, v)
		}
		// A file's contents: inline, or from its path.
		if len(fd.FileNames) > 0 {
			for j := range fd.FileNames {
				switch {
				case j < len(fd.FileContents) && fd.FileContents[j] != nil:
					vals = append(vals, string(fd.FileContents[j]))
				case j < len(fd.Paths) && fd.Paths[j] != "":
					b, err := read(fd.Paths[j])
					if err != nil {
						return "", "its file (" + fd.Paths[j] + ") could not be read: " + err.Error()
					}
					vals = append(vals, b)
				}
			}
		}
		return strings.Join(vals, "\n"), ""
	}
	recorded, why = text(f)
	if why != "" {
		return "", "", why
	}
	if live.Key == "" && len(live.Values) == 0 && len(live.FileNames) == 0 {
		return recorded, recorded, ""
	}
	sent, why = text(live)
	if why != "" {
		return "", "", why
	}
	return recorded, sent, ""
}

// variableReader reads back the test variables the agent bound in a test.
type variableReader interface {
	TestVariables(ctx context.Context) ([]models.VariableBinding, error)
}

// testVariables reads back the test variables the agent bound while tc ran
// (proven switch, with an agent), and checks each value tc's response captured
// against them: a capture of a key a dependency call bound must have taken
// the value the app sent that dependency (the app answered the id it stored,
// not another), a failure of the test case logged with its next step.
//
// What keploy itself cannot vouch for is no failure of the app: when the read
// fails (or this keploy cannot read) and tc captured something, or when a
// dependency's variable named after a template key was recorded with another
// value than config.yaml's (the learn step and the templates disagree),
// unjudged says why and the test set is not judged. A read that fails for a
// test case that captured nothing only loses its report rows, said once per
// set (warned).
func (r *Replayer) testVariables(ctx context.Context, run *templates.Run, tc *models.TestCase, caps []templates.Capture, warned *bool) (bound []models.VariableBinding, fails []models.TemplateFailure, unjudged string) {
	if !provenswitch.On() || !r.instrument || tc == nil || tc.Kind != models.HTTP {
		return nil, nil, ""
	}
	err := errors.New("this keploy cannot read the variables its agent bound")
	if reader, ok := r.instrumentation.(variableReader); ok {
		bound, err = reader.TestVariables(ctx)
	}
	if err != nil {
		if len(caps) == 0 {
			if !*warned {
				*warned = true
				r.logger.Warn("keploy could not read the test variables its agent bound; the report lists none for the test cases of this set that capture nothing",
					zap.String("testcase", tc.Name), zap.Error(err))
			}
			return nil, nil, ""
		}
		return nil, nil, "keploy could not read the variables its agent bound, so it cannot check what the test case captured: " + err.Error()
	}
	byName := make(map[string]models.VariableBinding, len(bound))
	for _, b := range bound {
		byName[b.Name] = b
	}
	for _, c := range caps {
		// Only a variable named after the key is the key's: a variable under
		// another name recorded with an equal value may be another value (two
		// counters both at 1 when recorded), so values cannot tell a misnamed
		// variable from it, and the capture is checked by its template alone.
		b, ok := byName[c.Key]
		if !ok || b.Given {
			continue
		}
		// Compared as the placeholder types the capture: numbers by their
		// value, a string as written ("1e2" is not "100").
		number := c.Type == templates.TypeInt || c.Type == templates.TypeFloat
		recorded, _ := run.Recorded(c.Key)
		if !variables.SameValue(b.Recorded, recorded, number) {
			unjudged = fmt.Sprintf("the variables block of %s names a variable after template key %s but records it as %q, and config.yaml records %q: the learn step and the templates disagree (report it to keploy, or remove that block to replay the call as recorded)",
				b.Mock, c.Key, b.Recorded, recorded)
			continue
		}
		if variables.SameValue(b.Value, c.Value, number) {
			continue
		}
		why := fmt.Sprintf("the app answered %q here, but sent %q as %s to its dependency (%s %s) in this test",
			c.Value, b.Value, c.Key, b.Mock, b.Place)
		r.logger.Warn("a value this test case captured is not the one the application sent its dependency",
			zap.String("testcase", tc.Name), zap.String("where", templates.PlaceShape(c.Where)), zap.String("why", whyInReport(why)),
			zap.String("next_step", "the application answers another value than the one it stored: fix the application, or re-record the test if that is intended"))
		fails = append(fails, models.TemplateFailure{Where: c.Where, Placeholder: templates.Format(true, "", c.Key), Why: why})
	}
	return bound, fails, unjudged
}

// headerNames are the names of the headers either side of a test case's
// request holds (rec as recorded, live as keploy test sends it), sorted, each
// once: a header one side alone holds is a part of the request too, "" on the
// other side.
func headerNames(rec, live map[string]string) []string {
	keys := make([]string, 0, len(rec)+len(live))
	for k := range rec {
		keys = append(keys, k)
	}
	for k := range live {
		if _, ok := rec[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}
