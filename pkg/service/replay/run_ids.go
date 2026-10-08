package replay

import (
	"context"
	"maps"
	"slices"
	"strings"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// runIDs follows, on the test-case side of a replay, the ids the app made
// this run in place of recorded ones.
//
// An app that mints its own ids (uuid.New(), a nonce) makes a new one on every
// run. The agent binds it to the recorded one where the app first sends it to
// a dependency, and from then on matches and answers dependency calls with the
// live id (integrations.ValueBindings). The test cases keploy replays still
// carry the recorded one: the next request would ask the app for an entity it
// now knows under another id, and every expected response would name an id
// the app no longer uses.
//
// The agent alone decides what is bound. This side names the ids it may bind
// (followedValues), reads the pairs it did bind, and acts on those and nothing
// else — it never works out for itself what will be followed. A test case is
// sent with its request rewritten with the pairs read so far (outgoing), and
// its expected response is compared rewritten with those read once the app
// has answered (expected) — by when the agent has bound what this test case
// made the app mint. Both on a copy: the recorded test case is never changed.
// Ids are replaced as whole words, as the agent replaces them
// (mocknoise.ReplaceWords). An id with no pair is handled exactly as it is
// without this: sent and expected as recorded, and written back to the set's
// templates as the run left it.
//
// Where a test case holds a template placeholder ({{string .id}}) in place of
// the id, keploy's templates already put this run's value there; runIDs is for
// the places an id is still written out.
//
// What is not followed, and replays as it does without this:
//   - an id the agent did not bind. It binds one only when the dependency
//     call that first carried it is made again exactly, but for the id: a
//     call with another field that changes every run (a timestamp) and is not
//     request-body noise binds nothing, and neither does a second new id for
//     a recorded id that already has one (a retry that mints again, more
//     creates than were recorded). The agent's debug log says why;
//   - an id the app first sends to a dependency only after it has answered (a
//     background job): it is bound too late for that answer, and followed
//     from the next test case on;
//   - an id that is part of a longer token ("order-<id>"): another word;
//   - the test sets followedValues leaves alone.
//
// A nil *runIDs follows nothing; every method accepts it.
type runIDs struct {
	logger   *zap.Logger
	read     func(ctx context.Context) (map[string]string, error)
	followed map[string]bool     // the recorded ids the agent may bind
	keys     map[string]string   // the set's template keys that hold them: key -> recorded id
	names    map[string][]string // test case -> the followed ids written out in it (not behind a placeholder)
	pairs    map[string]string   // recorded -> live: every pair read from the agent so far
	used     map[string]string   // the pairs reported on a test case so far (TestResult.RunIDs)
}

// idPairReader is the instrumentation that can read the agent's pairs.
type idPairReader interface {
	GetIDPairs(ctx context.Context) (map[string]string, error)
}

// followedValues are the recorded ids of a test set that the agent may bind
// and this replay follows through the test cases; nil when it follows none.
//
// They are the generated UUIDs (mocknoise.IsMintedUUID) in the set's template
// map that a test case's response produces and no test case supplies first —
// values `keploy templatize` put in the map, and of a shape an app only ever
// generates. (Whether a later request reuses one is not asked: an id that
// only answers name is followed through those answers all the same.) Being in
// the template map does not say a value changes from run to run: a content
// digest or a name-based UUID is templated the same way, and one that a
// regression altered must fail its test, since keploy compares the app's
// answers itself. So no other shape is followed here until something has seen
// it change on a healthy run.
//
// A template value that a test case SUPPLIES before any response carries it is
// the client's, not made by the app, and is left out: an app that stops using
// the id it was given must fail too. So is one no response carries at all.
// Test cases are read for this as they were recorded, each placeholder
// standing for its template value.
//
// A set is followed on both sides or left alone, and it is left alone — it
// replays exactly as it does with test.disableMockRebinding — when either
// side could not do its half:
//   - the app is not run by keploy (--base-path), or the run serves no mocks:
//     there is no recording to tie an id to;
//   - this build cannot read what the agent bound;
//   - the set is replayed in several cycles (retryPassing): a later cycle
//     would run over the first one's pairs;
//   - mocks are matched strictly (mockNoiseStrict): the agent follows nothing
//     there, since only learned or configured noise may differ;
//   - only some of the set's test cases are selected: the mocks of the others
//     are not staged, and the agent can only tell where an id was first sent
//     from the whole set;
//   - a test case is not HTTP, or is a streaming one: its answer is compared
//     as it is read, before what it made the app mint can be known.
//
// The reason is logged at Debug only: a set that is left alone behaves as it
// always has, and has nothing to tell a user who did not ask.
func (r *Replayer) followedValues(testSetID string, testCases []*models.TestCase, template map[string]interface{}) []string {
	templated := map[string]bool{}
	for _, v := range template {
		if s, ok := v.(string); ok && mocknoise.IsMintedUUID(s) {
			templated[s] = true
		}
	}
	if len(templated) == 0 {
		return nil
	}
	leftAlone := func(why string) []string {
		r.logger.Debug("the ids this test set's templates name are not followed this run: its test cases and mocks replay as recorded",
			zap.String("testSetID", testSetID), zap.String("why", why))
		return nil
	}
	_, readsPairs := r.instrumentation.(idPairReader)
	switch {
	case r.config.Test.DisableMockRebinding:
		return leftAlone("test.disableMockRebinding is set")
	case !r.instrument:
		return leftAlone("the app is not run by keploy (--base-path): no mocks are served to it")
	case !r.config.Test.Mocking:
		return leftAlone("the run serves no mocks (test.mocking is off)")
	case !readsPairs:
		return leftAlone("this build cannot read the ids the agent binds")
	case r.config.RetryPassing:
		return leftAlone("the set is replayed in several cycles (retryPassing)")
	case r.config.Test.NoiseStrict():
		return leftAlone("mocks are matched strictly (test.mockNoiseStrict): only learned or configured noise may differ from the recording")
	}
	// In the order they were recorded, whatever order they were loaded in.
	ordered := slices.DeleteFunc(slices.Clone(testCases), func(tc *models.TestCase) bool { return tc == nil })
	slices.SortStableFunc(ordered, func(a, b *models.TestCase) int {
		return a.HTTPReq.Timestamp.Compare(b.HTTPReq.Timestamp)
	})
	selected := r.config.Test.SelectedTests[testSetID]
	// firstRequest / firstResponse: the index of the first test case whose
	// request / response carries each templated value.
	firstRequest, firstResponse := map[string]int{}, map[string]int{}
	note := func(first map[string]int, i int) func(string) {
		return func(text string) {
			r.eachNamed(text, template, templated, func(w string) {
				if _, seen := first[w]; !seen {
					first[w] = i
				}
			})
		}
	}
	for i, tc := range ordered {
		switch {
		case tc.Kind != models.HTTP:
			return leftAlone("test case " + tc.Name + " is of kind " + string(tc.Kind) + "; only HTTP test cases are followed")
		case pkg.IsHTTPStreamingTestCase(tc):
			return leftAlone("test case " + tc.Name + " answers with a stream; ids are not followed in streaming test cases")
		case len(selected) > 0 && !slices.Contains(selected, tc.Name):
			return leftAlone("only some of the set's test cases are selected (test.selectedTests): test case " + tc.Name + " is not run")
		}
		mocknoise.EachRequestText(&tc.HTTPReq, note(firstRequest, i))
		mocknoise.EachResponseText(&tc.HTTPResp, note(firstResponse, i))
	}
	var out []string
	for v := range templated {
		resp, produced := firstResponse[v]
		if !produced {
			continue // no response carries it: not an id the app is seen to make
		}
		if req, requested := firstRequest[v]; requested && req <= resp {
			continue // a test case supplied it before the app produced it
		}
		out = append(out, v)
	}
	slices.Sort(out)
	return out
}

// recordedText is a text of a test case as it was recorded: every template
// placeholder in it replaced by the value the set's templates hold for it.
func (r *Replayer) recordedText(text string, template map[string]interface{}) string {
	if !strings.Contains(text, "{{") {
		return text
	}
	// A placeholder that cannot be rendered is left as written, and the rest
	// of the text is still rendered.
	out, _ := utils.RenderTemplatesInString(r.logger, text, template)
	return out
}

// eachNamed visits each of values that a text of a test case names as a whole
// word, the text read as it was recorded (recordedText).
func (r *Replayer) eachNamed(text string, template map[string]interface{}, values map[string]bool, visit func(string)) {
	text = r.recordedText(text, template)
	mocknoise.ScanWords(text, func(start, end int) {
		if w := text[start:end]; values[w] {
			visit(w)
		}
	})
}

// newRunIDs starts following, through a set's test cases, the recorded ids its
// templates name (followedValues); nil when there are none. It is made before
// the set runs, while the template map still holds the recorded values.
func (r *Replayer) newRunIDs(testSetID string, testCases []*models.TestCase, template map[string]interface{}) *runIDs {
	followed := r.followedValues(testSetID, testCases, template)
	if len(followed) == 0 {
		return nil
	}
	l := &runIDs{
		logger:   r.logger,
		read:     r.instrumentation.(idPairReader).GetIDPairs,
		followed: make(map[string]bool, len(followed)),
		keys:     map[string]string{},
		names:    map[string][]string{},
		pairs:    map[string]string{},
		used:     map[string]string{},
	}
	for _, v := range followed {
		l.followed[v] = true
	}
	for k, v := range template {
		if s, ok := v.(string); ok && l.followed[s] {
			l.keys[k] = s
		}
	}
	for _, tc := range testCases {
		if tc == nil {
			continue
		}
		// As written, not as recorded: where a placeholder stands for the
		// id, keploy's templates put this run's value there whether or not
		// the agent bound it, so the test case did not run with a stale id.
		named := map[string]bool{}
		note := func(text string) {
			mocknoise.ScanWords(text, func(start, end int) {
				if w := text[start:end]; l.followed[w] {
					named[w] = true
				}
			})
		}
		mocknoise.EachRequestText(&tc.HTTPReq, note)
		mocknoise.EachResponseText(&tc.HTTPResp, note)
		if len(named) > 0 {
			l.names[tc.Name] = slices.Sorted(maps.Keys(named))
		}
	}
	return l
}

// values are the recorded ids this replay follows, sorted.
func (l *runIDs) values() []string {
	if l == nil {
		return nil
	}
	return slices.Sorted(maps.Keys(l.followed))
}

// rebinding is what the agent is asked to bind (models.OutgoingOptions.Rebind):
// the recorded ids this replay follows; nil when it follows none.
func (l *runIDs) rebinding() *models.Rebinding {
	if l == nil {
		return nil
	}
	return &models.Rebinding{Values: l.values()}
}

// templatesToWrite is the set's template values as they may be written back
// to its config (--update-template): each id that was followed this run as
// RECORDED. The values of a run replace the recorded ones there so the next
// run starts from them, which suits a token; an id the app mints is made anew
// by the next run, and its recorded value is the one thing that ties the
// template to the mocks and test cases that name it. Written over, the id
// could not be followed again.
//
// Only an id the agent bound this run is kept so. One it did not bind — the
// whole of a set that is not followed — is written as the run made it, as it
// is without this.
func (l *runIDs) templatesToWrite(values map[string]interface{}) map[string]interface{} {
	if l == nil || len(l.pairs) == 0 {
		return values
	}
	out := maps.Clone(values)
	for k, recorded := range l.keys {
		_, bound := l.pairs[recorded]
		if _, held := out[k]; held && bound {
			out[k] = recorded
		}
	}
	return out
}

// refresh reads the pairs the agent has bound so far and adds those of the
// ids this replay follows to the ones it holds. A pair is never dropped
// within a set: the agent binds a recorded id once, so a read that lacks one
// (a failed read, an agent that was replaced) says nothing about an id the
// app has already been using. A test case compared without the newest pair
// fails loudly on that id, which is better than one compared with none.
func (l *runIDs) refresh(ctx context.Context) {
	pairs, err := l.read(ctx)
	if err != nil {
		l.logger.Debug("could not read the ids this run made in place of recorded ones", zap.Error(err))
		return
	}
	for recorded, live := range pairs {
		if l.followed[recorded] {
			l.pairs[recorded] = live
		}
	}
	// The set's templates hold the live id from now on, so a placeholder
	// that stands for a followed id renders the id the agent bound — also
	// where no response of this run taught the template (the id came back in
	// a header, or its producing response no longer holds a placeholder).
	held := map[string]interface{}{}
	for key, recorded := range l.keys {
		if live, ok := l.pairs[recorded]; ok {
			held[key] = live
		}
	}
	pkg.SetTemplateValues(held)
}

// settled are the live ids bound so far: values an answer must name where
// its test case expects them (httpMatcher.WithSettledValues).
func (l *runIDs) settled() map[string]bool {
	if l == nil || len(l.pairs) == 0 {
		return nil
	}
	out := make(map[string]bool, len(l.pairs))
	for _, live := range l.pairs {
		out[live] = true
	}
	return out
}

func (l *runIDs) live(recorded string) (string, bool) {
	v, ok := l.pairs[recorded]
	return v, ok
}

// outgoing returns tc as it should be sent: its request rewritten with the
// pairs bound by now, on a copy. tc itself when it carries none of them.
// Reading the pairs also gives the set's templates the ids bound (refresh).
func (l *runIDs) outgoing(ctx context.Context, tc *models.TestCase) *models.TestCase {
	if l == nil || tc == nil || tc.Kind != models.HTTP {
		return tc
	}
	if l.refresh(ctx); len(l.pairs) == 0 {
		return tc
	}
	req := tc.HTTPReq
	req.URL = mocknoise.ReplaceWords(req.URL, l.live)
	req.Body = mocknoise.ReplaceWords(req.Body, l.live)
	req.Header = l.rewriteValues(req.Header)
	req.URLParams = l.rewriteValues(req.URLParams)
	req.Form = l.rewriteForm(req.Form)
	if req.URL == tc.HTTPReq.URL && req.Body == tc.HTTPReq.Body &&
		maps.Equal(req.Header, tc.HTTPReq.Header) && maps.Equal(req.URLParams, tc.HTTPReq.URLParams) &&
		slices.EqualFunc(req.Form, tc.HTTPReq.Form, func(a, b models.FormData) bool { return slices.Equal(a.Values, b.Values) }) {
		return tc
	}
	out := *tc
	out.HTTPReq = req
	return &out
}

// expected returns tc as its response should be compared, now that the app
// has answered it: the expected response rewritten with the pairs bound by
// now, on a copy; tc itself when it carries none of them. Reading the pairs
// also gives the set's templates the ids bound (refresh).
//
// With it come the pairs this test case is reported with (TestResult.RunIDs,
// recorded -> live): those swapped into the expected response, and those
// whose live id the answer names, in its body or a header. The second kind is
// there for an expected response that named the id through a placeholder, or
// not at all: `keploy normalize` puts the recorded id back with them.
func (l *runIDs) expected(ctx context.Context, tc *models.TestCase, answer *models.HTTPResp) (*models.TestCase, map[string]string) {
	if l == nil || tc == nil || tc.Kind != models.HTTP {
		return tc, nil
	}
	if l.refresh(ctx); len(l.pairs) == 0 {
		return tc, nil
	}
	ids := map[string]string{}
	follow := func(w string) (string, bool) {
		live, ok := l.pairs[w]
		if ok {
			ids[w] = live
		}
		return live, ok
	}
	resp, swapped := mocknoise.ReplaceResponseWords(tc.HTTPResp, follow)
	if answer != nil {
		recorded := make(map[string]string, len(l.pairs))
		for rec, live := range l.pairs {
			recorded[live] = rec
		}
		mocknoise.EachResponseText(answer, func(text string) {
			mocknoise.ScanWords(text, func(start, end int) {
				if rec, ok := recorded[text[start:end]]; ok {
					ids[rec] = text[start:end]
				}
			})
		})
	}
	if len(ids) == 0 {
		return tc, nil
	}
	maps.Copy(l.used, ids)
	if !swapped {
		return tc, ids
	}
	out := *tc
	out.HTTPResp = resp
	return &out, ids
}

func (l *runIDs) rewriteValues(in map[string]string) map[string]string {
	if len(in) == 0 {
		return in
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = mocknoise.ReplaceWords(v, l.live)
	}
	return out
}

func (l *runIDs) rewriteForm(in []models.FormData) []models.FormData {
	if len(in) == 0 {
		return in
	}
	out := slices.Clone(in)
	for i, f := range out {
		out[i].Values = make([]string, len(f.Values))
		for j, v := range f.Values {
			out[i].Values[j] = mocknoise.ReplaceWords(v, l.live)
		}
	}
	return out
}

// logSummary says, once a test set has run, what following its ids came to.
//
// When ids were followed: how many, so a reader of the log knows why an
// expected response in the report differs from the recorded file.
//
// When a test case FAILED that has, written out in it, a followed id the
// agent bound nothing for: which ids, and in which test cases. That test case
// was sent and compared with the recorded id. Whether that is why it failed
// this does not say — only where to look. A set whose test cases pass, or
// fail without such an id written in them, has nothing to say here.
func (l *runIDs) logSummary(testSetID string, results []models.TestResult) {
	if l == nil {
		return
	}
	if len(l.used) > 0 {
		l.logger.Info("the app made new ids this run where the recording had others; each was followed through the test cases that name it",
			zap.Int("ids", len(l.used)), zap.String("testSetID", testSetID),
			zap.String("see", "run_ids on each test case in the test report: recorded id -> the id made this run"),
			zap.String("to_turn_off", "test.disableMockRebinding: true in keploy.yml"))
	}
	failedWith := map[string][]string{} // recorded id with no pair -> the failed test cases it is written in
	for _, tr := range results {
		if tr.Status != models.TestStatusFailed {
			continue
		}
		for _, id := range l.names[tr.TestCaseID] {
			if _, bound := l.pairs[id]; !bound {
				failedWith[id] = append(failedWith[id], tr.TestCaseID)
			}
		}
	}
	if len(failedWith) == 0 {
		return
	}
	first := make(map[string][]string, 5)
	for _, id := range slices.Sorted(maps.Keys(failedWith))[:min(len(failedWith), 5)] {
		first[id] = slices.Sorted(slices.Values(failedWith[id]))
	}
	l.logger.Info("failed test cases have a recorded id written out in them that this run did not follow, so they were sent and compared with the recorded id",
		zap.Int("ids", len(failedWith)), zap.Any("first", first), zap.String("testSetID", testSetID),
		zap.String("when_an_id_is_followed", "when the app makes the dependency call that first carried it again, exactly but for a new id in its place"),
		zap.String("to_see_more", "run with --debug: the agent says when it leaves a test set alone, and names the dependency calls it answered as recorded"))
}
