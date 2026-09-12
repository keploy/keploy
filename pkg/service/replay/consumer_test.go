package replay

import (
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/consumer/consumerfake"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The consumer judge's table.
//
// compareEffects is the whole verdict of a Kind: Consumer test — the pairing,
// the lanes, the payload diff, the count assertion, the end-reason assertion
// and every refusal. Nothing else in the tree can reach it: OSS ships no
// protocol parser, so without this file the judge is unreachable by any test
// and the slice's entire no-false-pass claim rests on inspection. It was
// measured: replacing compareEffects' body with `return consumerVerdict{Pass:
// true}` left the whole suite green.
//
// OBSERVED VIEWS COME OUT OF THE FAKE PROJECTOR, not out of a struct literal.
// consumerfake exists as a separate package precisely so this table can drive
// the real Projector seam an enterprise parser will implement; hand-built
// views here would test the assertions against themselves.

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

const fakeProto = consumerfake.Protocol

// depChecked is the ordinary state of the sync path's per-test dependency
// assertion: this test has a mapping and the presence assertion ran over it.
// Every row in this file uses it except the ones that are ABOUT the assertion
// not having run — see the unverifiable-claim rows in the table.
var depChecked = consumerDepAssertion{HasMapping: true, Ran: true}

// judge runs the whole verdict with the dependency assertion having run, which
// is what every healthy replay looks like.
func judge(tc *models.TestCase, res *models.ConsumerResult) consumerVerdict {
	return compareEffects(tc, res, depChecked)
}

// view builds a recorded (spec-side) effect view.
func view(op, target, key, body string) models.EffectView {
	return consumerfake.View(op, target, key, body)
}

// observe pushes views through the FAKE PROJECTOR and returns what came back,
// so an observed view in this table has crossed the same seam a real parser's
// projector sits behind.
func observe(t *testing.T, views ...models.EffectView) []models.EffectView {
	t.Helper()
	if len(views) == 0 {
		return nil
	}
	m := consumerfake.Mock(consumerfake.MockOptions{
		Name:  "observed",
		Role:  models.RoleEffect,
		Views: views,
	})
	out, err := (consumerfake.Projector{}).Project(m)
	if err != nil {
		t.Fatalf("fake projector refused the observed views: %v", err)
	}
	return out
}

// specWith builds a CONSUMER test case around a set of recorded effects, with
// the completion rule the recorder would have minted for them.
func specWith(effects ...models.EffectView) *models.TestCase {
	records := 0
	for _, e := range effects {
		// MIRRORS Recorder.onEffect EXACTLY. A presence stand-in is not
		// counted into ExpectEffects at record, is not counted into the
		// observed total by the gate, and is not paired by the judge. A
		// helper that counted it here would build specs no recorder can
		// produce and pin the judge against a rule the record side does not
		// implement.
		if e.IsPresenceOnly() {
			continue
		}
		records += e.RecordCount()
	}
	return &models.TestCase{
		Kind: models.CONSUMER,
		Name: "test-1",
		ConsumerSpec: &models.ConsumerSpec{
			Protocol: fakeProto,
			Trigger:  view("fetch", "orders", "o-1", `{"orderId":"o-1"}`),
			Effects:  effects,
			Completion: models.ConsumerCompletion{
				ExpectEffects: records,
				GraceMs:       models.ConsumerGraceMinMs,
				TimeoutMs:     models.ConsumerDefaultTimeoutMs,
			},
		},
	}
}

// resultFor builds the gate result a healthy run would have produced for the
// given observed views: the count agrees with what arrived, and the window
// closed on the completion rule.
func resultFor(tc *models.TestCase, observed []models.EffectView) *models.ConsumerResult {
	records := 0
	for _, o := range observed {
		if o.IsPresenceOnly() {
			continue
		}
		records += o.RecordCount()
	}
	return &models.ConsumerResult{
		TestID:          tc.Name,
		TriggerAccepted: true,
		ExpectEffects:   tc.ConsumerSpec.Completion.ExpectEffects,
		ObservedEffects: records,
		EndReason:       models.ConsumerEndReasonCountReached,
		Effects:         observed,
	}
}

// rowKeys renders every failing meta as "<rowName>|<key>" so a case can assert
// on the EXACT report vocabulary a user will paste into spec.assertions.noise.
func rowKeys(rows []models.DepResult) []string {
	var out []string
	for _, r := range rows {
		for _, m := range r.Meta {
			if m.Normal {
				continue
			}
			out = append(out, r.Name+"|"+m.Key)
		}
	}
	sort.Strings(out)
	return out
}

func catStrings(v consumerVerdict) []string {
	out := categoryStrings(v.Categories)
	sort.Strings(out)
	return out
}

func assertCategories(t *testing.T, v consumerVerdict, want ...string) {
	t.Helper()
	sort.Strings(want)
	got := catStrings(v)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("categories = %v, want %v (summary: %s)", got, want, v.Summary)
	}
}

func assertRowKeys(t *testing.T, v consumerVerdict, want ...string) {
	t.Helper()
	sort.Strings(want)
	got := rowKeys(v.Rows)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("row keys =\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// ---------------------------------------------------------------------------
// The table — the eight shapes design §9 names, plus the shapes this slice's
// own halves disagreed about.
// ---------------------------------------------------------------------------

func TestCompareEffectsTable(t *testing.T) {
	produced := view("produce", "order-events", "o-1", `{"orderId":"o-1","status":"CONFIRMED","total":41.97}`)

	t.Run("ok", func(t *testing.T) {
		tc := specWith(produced)
		res := resultFor(tc, observe(t, produced))
		v := judge(tc, res)
		if !v.Pass {
			t.Fatalf("a run that reproduced its recording must pass: %+v", v)
		}
		if len(v.Rows) != 0 {
			t.Fatalf("a passing test writes no rows, got %v", rowKeys(v.Rows))
		}
	})

	t.Run("missing", func(t *testing.T) {
		tc := specWith(produced)
		// The worker stopped producing. THE FLAGSHIP REGRESSION.
		res := resultFor(tc, nil)
		res.ObservedEffects = 0
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("a worker that stopped producing must never pass")
		}
		assertCategories(t, v, string(models.CategoryEffectMissing))
		assertRowKeys(t,
			v,
			"effects[0] fake produce order-events key=o-1|effects.0.presence",
			"effects[*] fake count|effects.count",
		)
	})

	t.Run("extra", func(t *testing.T) {
		tc := specWith(produced)
		dup := view("produce", "order-events", "o-2", `{"orderId":"o-2"}`)
		res := resultFor(tc, observe(t, produced, dup))
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("an over-producing worker must never pass")
		}
		assertCategories(t, v, string(models.CategoryEffectUnexpected))
		assertRowKeys(t,
			v,
			"effects[*] fake count|effects.count",
			"effects[*] fake produce order-events key=o-2|effects.*.unexpected",
		)
	})

	t.Run("body-diff", func(t *testing.T) {
		tc := specWith(produced)
		drifted := view("produce", "order-events", "o-1", `{"orderId":"o-1","status":"PENDING","total":41.97}`)
		res := resultFor(tc, observe(t, drifted))
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("a changed payload field must fail")
		}
		assertCategories(t, v, string(models.CategoryEffectBodyChanged))
		assertRowKeys(t, v, "effects[0] fake produce order-events key=o-1|effects.0.body.status")
		// The Expected/Actual pair is what the user reads.
		m := v.Rows[0].Meta[0]
		if m.Expected != "CONFIRMED" || m.Actual != "PENDING" {
			t.Fatalf("diff rendered expected=%q actual=%q", m.Expected, m.Actual)
		}
	})

	t.Run("target-swap", func(t *testing.T) {
		tc := specWith(produced)
		rerouted := view("produce", "order-events-v2", "o-1", `{"orderId":"o-1","status":"CONFIRMED","total":41.97}`)
		res := resultFor(tc, observe(t, rerouted))
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("a routing change must fail")
		}
		// Pass 2 recognises this as ONE event that went elsewhere rather than
		// reporting a separate missing and a separate extra.
		assertCategories(t, v, string(models.CategoryEffectTargetChanged))
		assertRowKeys(t, v, "effects[0] fake produce order-events key=o-1|effects.0.identity")
		m := v.Rows[0].Meta[0]
		if !strings.Contains(m.Expected, "order-events") || !strings.Contains(m.Actual, "order-events-v2") {
			t.Fatalf("identity row expected=%q actual=%q", m.Expected, m.Actual)
		}
	})

	t.Run("trigger-not-delivered", func(t *testing.T) {
		tc := specWith(produced)
		res := resultFor(tc, nil)
		res.ObservedEffects = 0
		res.TriggerAccepted = false
		res.EndReason = models.ConsumerEndReasonTriggerNotDelivered
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("an undelivered trigger must fail")
		}
		// It must NOT be reported as a missing effect: the worker was never
		// given the message, so nothing it did is evidence about the worker.
		assertCategories(t, v, string(models.CategoryConsumerTriggerNotDelivered))
		assertRowKeys(t, v, "effects[*] fake refused|effects.refusal")
	})

	t.Run("trigger-discarded", func(t *testing.T) {
		tc := specWith(produced)
		res := resultFor(tc, nil)
		res.ObservedEffects = 0
		res.EndReason = models.ConsumerEndReasonTriggerDiscarded
		res.RefusalDetail = "the client re-fetched the offset it was just served"
		v := judge(tc, res)
		assertCategories(t, v, string(models.CategoryConsumerTriggerDiscarded))
		if !strings.Contains(v.Rows[0].Meta[0].Actual, "re-fetched") {
			t.Fatalf("the gate's own words must reach the row, got %q", v.Rows[0].Meta[0].Actual)
		}
	})

	t.Run("opaque-observed", func(t *testing.T) {
		tc := specWith(produced)
		opaque := produced
		opaque.Decoded = models.DecodedOpaque
		res := resultFor(tc, observe(t, opaque))
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("an opaque payload compared against a decoded one must never pass")
		}
		assertCategories(t, v, string(models.CategoryConsumerOpaqueEffectBody))
		assertRowKeys(t, v, "effects[0] fake produce order-events key=o-1|effects.0.decoded")
	})

	t.Run("opaque-recorded", func(t *testing.T) {
		opaque := produced
		opaque.Decoded = models.DecodedOpaque
		tc := specWith(opaque)
		res := resultFor(tc, observe(t, produced))
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("an opaque RECORDING must never pass either")
		}
		assertCategories(t, v, string(models.CategoryConsumerOpaqueEffectBody))
	})

	t.Run("opaque-both-sides-is-still-not-a-pass", func(t *testing.T) {
		// The one shape no amount of field diffing can catch: a misparse
		// compared against the same misparse agrees for the wrong reason.
		opaque := produced
		opaque.Decoded = models.DecodedOpaque
		tc := specWith(opaque)
		res := resultFor(tc, observe(t, opaque))
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("opaque == opaque is a silent pass and must be refused")
		}
		assertCategories(t, v, string(models.CategoryConsumerOpaqueEffectBody))
		if !strings.Contains(v.Rows[0].Meta[0].Actual, "both") {
			t.Fatalf("the row must say both sides were opaque, got %q", v.Rows[0].Meta[0].Actual)
		}
	})

	t.Run("vacuous", func(t *testing.T) {
		// No effects, no expected count, and nothing else happened either.
		tc := specWith()
		res := resultFor(tc, nil)
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("a test that asserts nothing must be refused, not passed")
		}
		assertCategories(t, v, string(models.CategoryConsumerNoObservableEffect))
	})

	t.Run("an-empty-list-that-disappeared-is-a-diff", func(t *testing.T) {
		// flattenJSON records an EMPTY container under its own path rather
		// than letting it vanish. Without that, expected {"a":1,"items":[]}
		// and observed {"a":1} flatten to identical maps and a payload that
		// dropped a field entirely PASSES — the one guard in the differ whose
		// removal produces a silent green rather than a differently-named
		// red. The case that matters is this direction: the empty container
		// disappearing.
		before := view("produce", "order-events", "o-1", `{"a":1,"items":[]}`)
		after := view("produce", "order-events", "o-1", `{"a":1}`)
		tc := specWith(before)
		res := resultFor(tc, observe(t, after))
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("a payload that dropped a field entirely must fail")
		}
		assertCategories(t, v, string(models.CategoryEffectBodyChanged))
		assertRowKeys(t, v, "effects[0] fake produce order-events key=o-1|effects.0.body.items")
		if got := v.Rows[0].Meta[0].Actual; got != absentValue {
			t.Fatalf("the vanished list must render as %q, got %q", absentValue, got)
		}
	})

	t.Run("an-empty-object-that-disappeared-is-a-diff", func(t *testing.T) {
		// The symmetric half. It survives an array-only mutation of
		// flattenJSON, which is exactly why it needs its own row.
		before := view("produce", "order-events", "o-1", `{"a":1,"tags":{}}`)
		after := view("produce", "order-events", "o-1", `{"a":1}`)
		tc := specWith(before)
		res := resultFor(tc, observe(t, after))
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("a payload that dropped an empty object must fail")
		}
		assertCategories(t, v, string(models.CategoryEffectBodyChanged))
		assertRowKeys(t, v, "effects[0] fake produce order-events key=o-1|effects.0.body.tags")
		if got := v.Rows[0].Meta[0].Actual; got != absentValue {
			t.Fatalf("the vanished object must render as %q, got %q", absentValue, got)
		}
	})

	t.Run("a-list-that-became-empty-is-a-diff", func(t *testing.T) {
		// The direction the doc comment names, kept so the two are pinned
		// together and neither can be removed as "already covered".
		before := view("produce", "order-events", "o-1", `{"items":[{"sku":"SKU-9"}]}`)
		after := view("produce", "order-events", "o-1", `{"items":[]}`)
		tc := specWith(before)
		res := resultFor(tc, observe(t, after))
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("a list that became empty must fail")
		}
		assertCategories(t, v, string(models.CategoryEffectBodyChanged))
	})

	t.Run("a-dropped-header-is-a-finding", func(t *testing.T) {
		// EffectView.Headers is projected, persisted, rendered and — until
		// this row — never compared. Dropping a tenant/routing/trace header
		// is a real worker regression and it went green with nothing in the
		// report, the JUnit output or --format json to hint at it.
		before := view("produce", "order-events", "o-1", `{"a":1}`)
		before.Headers = map[string]string{"traceparent": "00-aaa-01", "tenant": "acme"}
		after := view("produce", "order-events", "o-1", `{"a":1}`)
		after.Headers = map[string]string{"tenant": "evil-corp"}
		tc := specWith(before)
		res := resultFor(tc, observe(t, after))
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("a corrupted tenant header and a dropped trace header must never pass")
		}
		assertCategories(t, v, string(models.CategoryEffectHeadersChanged))
		assertRowKeys(t, v,
			"effects[0] fake produce order-events key=o-1|effects.0.headers.tenant",
			"effects[0] fake produce order-events key=o-1|effects.0.headers.traceparent",
		)
		var dropped models.DepMetaResult
		for _, m := range v.Rows[0].Meta {
			if strings.HasSuffix(m.Key, ".traceparent") {
				dropped = m
			}
		}
		if dropped.Actual != absentValue {
			t.Fatalf("a dropped header must render as %q, not as an empty value: %+v", absentValue, dropped)
		}
	})

	t.Run("a-worker-that-dropped-every-header-is-a-finding", func(t *testing.T) {
		// THE ONE-SIDED-EMPTY CASE, and the guard it pins. diffEffectHeaders
		// opens with `len(exp.Headers) == 0 && len(obs.Headers) == 0`; an
		// automated &&->|| sweep proved that mutation SURVIVED, because every
		// other header row leaves at least one header standing on the observed
		// side. With `||` a worker that stopped emitting headers ENTIRELY —
		// strictly more severe than the partial drop above — returns no
		// findings at all.
		before := view("produce", "order-events", "o-1", `{"a":1}`)
		before.Headers = map[string]string{"tenant": "acme", "traceparent": "00-aaa-01"}
		after := view("produce", "order-events", "o-1", `{"a":1}`)
		after.Headers = nil
		tc := specWith(before)
		res := resultFor(tc, observe(t, after))
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("a worker that stopped emitting headers entirely must never pass")
		}
		assertCategories(t, v, string(models.CategoryEffectHeadersChanged))
		assertRowKeys(t, v,
			"effects[0] fake produce order-events key=o-1|effects.0.headers.tenant",
			"effects[0] fake produce order-events key=o-1|effects.0.headers.traceparent",
		)
		for _, m := range v.Rows[0].Meta {
			if m.Actual != absentValue {
				t.Fatalf("every dropped header must render as %q: %+v", absentValue, m)
			}
		}
	})

	t.Run("a-header-declared-noise-is-silenced-per-test", func(t *testing.T) {
		// A traceparent is genuinely new every run. The remedy is the same
		// one a non-deterministic payload field gets: paste the reported path
		// into THAT test's spec.assertions.noise. Per test, never per set.
		before := view("produce", "order-events", "o-1", `{"a":1}`)
		before.Headers = map[string]string{"traceparent": "00-aaa-01"}
		after := view("produce", "order-events", "o-1", `{"a":1}`)
		after.Headers = map[string]string{"traceparent": "00-bbb-01"}
		tc := specWith(before)
		tc.Noise = map[string][]string{"effects.0.headers.traceparent": {}}
		res := resultFor(tc, observe(t, after))
		v := judge(tc, res)
		if !v.Pass {
			t.Fatalf("a header declared noise must not fail the test: %v %s", catStrings(v), v.Summary)
		}
	})

	t.Run("headers-and-body-are-two-findings-not-one", func(t *testing.T) {
		// They have different remedies, so collapsing them would make the
		// report name whichever the judge happened to check first.
		before := view("produce", "order-events", "o-1", `{"status":"CONFIRMED"}`)
		before.Headers = map[string]string{"tenant": "acme"}
		after := view("produce", "order-events", "o-1", `{"status":"PENDING"}`)
		after.Headers = map[string]string{"tenant": "evil-corp"}
		tc := specWith(before)
		res := resultFor(tc, observe(t, after))
		v := judge(tc, res)
		assertCategories(t, v,
			string(models.CategoryEffectBodyChanged),
			string(models.CategoryEffectHeadersChanged))
		assertRowKeys(t, v,
			"effects[0] fake produce order-events key=o-1|effects.0.body.status",
			"effects[0] fake produce order-events key=o-1|effects.0.headers.tenant",
		)
	})

	t.Run("matching-headers-are-not-a-finding", func(t *testing.T) {
		before := view("produce", "order-events", "o-1", `{"a":1}`)
		before.Headers = map[string]string{"tenant": "acme"}
		tc := specWith(before)
		res := resultFor(tc, observe(t, before))
		v := judge(tc, res)
		if !v.Pass {
			t.Fatalf("a worker that reproduced its headers must pass: %v %s", catStrings(v), v.Summary)
		}
	})

	t.Run("two-opaque-effects-in-different-lanes-stay-two-findings", func(t *testing.T) {
		// sameEffectPayload refuses to call two OPAQUE payloads "the same
		// payload", because nothing decoded either of them. Without that
		// guard pass 2 would pair them and the verdict would read
		// EFFECT_TARGET_CHANGED — one routing regression — for what is
		// actually a message that was never produced plus one that was not
		// recorded, on payloads nobody has read.
		opaque := produced
		opaque.Decoded = models.DecodedOpaque
		elsewhere := view("produce", "audit-log", "o-1", produced.Body)
		elsewhere.Decoded = models.DecodedOpaque
		tc := specWith(opaque)
		res := resultFor(tc, observe(t, elsewhere))
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("an opaque payload can never pass")
		}
		assertCategories(t, v,
			string(models.CategoryEffectMissing),
			string(models.CategoryEffectUnexpected))
		assertRowKeys(t, v,
			"effects[0] fake produce order-events key=o-1|effects.0.presence",
			"effects[*] fake produce audit-log key=o-1|effects.*.unexpected",
		)
	})

	t.Run("consume-and-write-only-worker-passes", func(t *testing.T) {
		// THE SHAPE THE TWO HALVES OF THIS SLICE USED TO CONTRADICT EACH OTHER
		// ON. The recorder deliberately mints a unit that produced nothing but
		// made calls of another protocol family (Recorder.closeUnit spares it
		// because u.sideEffects > 0), with Effects: [] and ExpectEffects: 0.
		// The judge used to refuse exactly that shape as vacuous, so one of
		// the two most common consumer shapes recorded cleanly and then failed
		// 100% of the time. Its assertion is real and lives on the sync path's
		// deps[i] rows, which for this Kind are non-demotable.
		tc := specWith()
		tc.ConsumerSpec.SideEffects = 1
		res := resultFor(tc, nil)
		v := judge(tc, res)
		if !v.Pass {
			t.Fatalf("a consume-and-write worker that behaved must pass: categories=%v summary=%s", catStrings(v), v.Summary)
		}
		if len(v.Rows) != 0 {
			t.Fatalf("no rows expected, got %v", rowKeys(v.Rows))
		}
	})

	t.Run("consume-and-write-worker-with-no-mapping-is-refused", func(t *testing.T) {
		// THE OTHER HALF OF THE SAME SHAPE, AND THE ONE THAT WAS A FALSE
		// GREEN. spec.SideEffects is a RECORD-TIME COUNT; nothing here ever
		// turns it into an assertion. Its whole claim is carried by the sync
		// path's deps[i] presence rows, which only exist when this test has a
		// usable entry in mappings.yaml — and a revoked mapping, a mock reaped
		// by the seven-second cutoff or a partially regenerated
		// --update-test-mapping leaves it with none. The test then passed with
		// zero rows, zero categories and zero assertions executed, on one of
		// the two most common consumer shapes.
		tc := specWith()
		tc.ConsumerSpec.SideEffects = 2
		res := resultFor(tc, nil)
		v := compareEffects(tc, res, consumerDepAssertion{})
		if v.Pass {
			t.Fatal("a test whose only claim could not be checked must be FAILED by name, never passed")
		}
		assertCategories(t, v, string(models.CategoryConsumerMappingsRequired))
	})

	t.Run("consume-and-write-worker-whose-mapping-asserts-nothing-is-refused", func(t *testing.T) {
		// It HAS a mapping, but everything in it is DNS or reusable-tier, so
		// the presence assertion is computed over an empty set and can never
		// fail. Indistinguishable, as far as this test's claim goes, from
		// having no mapping at all — but the remedy is different, so the name
		// is different.
		tc := specWith()
		tc.ConsumerSpec.SideEffects = 1
		res := resultFor(tc, nil)
		v := compareEffects(tc, res, consumerDepAssertion{HasMapping: true})
		if v.Pass {
			t.Fatal("a test that would pass without asserting anything must be refused")
		}
		assertCategories(t, v, string(models.CategoryConsumerNoObservableEffect))
	})

	t.Run("a-presence-only-spec-with-no-mapping-is-refused", func(t *testing.T) {
		// THE SAME DELEGATED CLAIM, IN THE SPELLING THE RECORDER ACTUALLY
		// MINTS. Recorder.closeUnit deliberately lets a unit through whose
		// only effect is a presence stand-in ("THE TEST IS len(u.effects), NOT
		// u.effectRecords"), so this spec has len(spec.Effects) == 1 — and
		// compareEffectLists filters presence views out of BOTH lanes before
		// pairing, so it asserts exactly as much as an empty slice: nothing.
		// While the guard keyed on len(spec.Effects) it was skipped outright
		// for this spelling, including its mapping branches, and the test
		// passed with zero rows and zero categories.
		presence := models.EffectView{Protocol: "postgres", Op: "insert", Target: "orders_table", Decoded: models.DecodedPresence}
		tc := specWith(presence)
		if got := len(tc.ConsumerSpec.Effects); got != 1 {
			t.Fatalf("guard: this row is only meaningful with a non-empty Effects slice, got %d", got)
		}
		if got := tc.ConsumerSpec.Completion.ExpectEffects; got != 0 {
			t.Fatalf("guard: a presence view is not counted by the completion rule, got %d", got)
		}
		res := resultFor(tc, nil)
		v := compareEffects(tc, res, consumerDepAssertion{})
		if v.Pass {
			t.Fatal("a presence-only spec whose claim could not be checked must be FAILED by name, never passed")
		}
		assertCategories(t, v, string(models.CategoryConsumerMappingsRequired))
	})

	t.Run("a-presence-only-spec-whose-mapping-asserts-nothing-is-refused", func(t *testing.T) {
		presence := models.EffectView{Protocol: "postgres", Op: "insert", Target: "orders_table", Decoded: models.DecodedPresence}
		tc := specWith(presence)
		res := resultFor(tc, nil)
		v := compareEffects(tc, res, consumerDepAssertion{HasMapping: true})
		if v.Pass {
			t.Fatal("a presence-only spec that would pass without asserting anything must be refused")
		}
		assertCategories(t, v, string(models.CategoryConsumerNoObservableEffect))
	})

	t.Run("a-presence-only-spec-whose-mapping-carries-the-claim-passes", func(t *testing.T) {
		// The delegate ran, so the claim IS checked and this is the shape the
		// recorder mints for a database write. Refusing it would refuse
		// exactly the projector shape design §2 describes.
		presence := models.EffectView{Protocol: "postgres", Op: "insert", Target: "orders_table", Decoded: models.DecodedPresence}
		tc := specWith(presence)
		res := resultFor(tc, nil)
		v := judge(tc, res)
		if !v.Pass {
			t.Fatalf("a presence-only spec whose sync-path claim was checked must pass: %v %s", catStrings(v), v.Summary)
		}
		if len(v.Rows) != 0 {
			t.Fatalf("no rows expected, got %v", rowKeys(v.Rows))
		}
	})

	t.Run("a-presence-only-spec-with-nothing-at-all-is-refused-even-with-a-checked-mapping", func(t *testing.T) {
		// The `delegated <= 0` arm is unreachable from a presence-only spec
		// (a presence view IS a delegated claim), so this row uses the empty
		// spelling to keep that arm pinned alongside the others.
		tc := specWith()
		res := resultFor(tc, nil)
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("a test that records nothing at all asserts nothing and must be refused")
		}
		assertCategories(t, v, string(models.CategoryConsumerNoObservableEffect))
	})

	t.Run("a-test-with-real-effects-is-not-refused-for-an-uncheckable-side-effect", func(t *testing.T) {
		// The guard is deliberately narrow. This test still asserts its
		// effect, so refusing it because the side-effect count happened to be
		// uncheckable would be a false RED on a partially regenerated
		// mapping.
		tc := specWith(produced)
		tc.ConsumerSpec.SideEffects = 1
		res := resultFor(tc, observe(t, produced))
		v := compareEffects(tc, res, consumerDepAssertion{})
		if !v.Pass {
			t.Fatalf("a test that asserts a real effect must still be judged on it: %v %s", catStrings(v), v.Summary)
		}
	})

	t.Run("dead-app-consume-to-db", func(t *testing.T) {
		// DESIGN §5 FALSE-PASS ROW 2, on the one shape that can reach it.
		// A consume-and-write-to-a-database worker records ExpectEffects: 0,
		// so "observed >= expected" is satisfied before the application has
		// done anything at all: a worker that crashed at boot, joined the
		// wrong group or never subscribed closes its window on the count with
		// nothing having happened, every per-effect assertion is vacuously
		// satisfied, and the verdict is a pass with zero evidence the app ever
		// ran. The gate refuses to close such a window (Gate.Complete), and
		// this is the judge's own independent check on the same fact.
		tc := specWith()
		tc.ConsumerSpec.SideEffects = 1
		res := resultFor(tc, nil)
		res.TriggerAccepted = false
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("a window that closed on the count with no evidence the application ever received the message must never pass")
		}
		assertCategories(t, v, string(models.CategoryConsumerTriggerNotDelivered))
		assertRowKeys(t, v, "effects[*] fake window|effects.end_reason")
	})

	t.Run("count-reached-with-effects-needs-no-accept", func(t *testing.T) {
		// The other half of the same rule: a worker that produced has
		// PROVED it received the message, so a parser that does not
		// implement the positive delivery check does not redden every test
		// that produces something.
		tc := specWith(produced)
		res := resultFor(tc, observe(t, produced))
		res.TriggerAccepted = false
		v := judge(tc, res)
		if !v.Pass {
			t.Fatalf("observed effects are themselves evidence the app ran: %v %s", catStrings(v), v.Summary)
		}
	})

	t.Run("run-cancelled", func(t *testing.T) {
		// Ctrl-C during a delivery window. It must NOT be reported as an
		// agent that lacks consumer support: that sends whoever reads the
		// report looking for a missing capability instead of noticing that
		// someone stopped the run.
		tc := specWith(produced)
		res := resultFor(tc, nil)
		res.ObservedEffects = 0
		res.EndReason = models.ConsumerEndReasonInternalError
		res.Refusal = models.CategoryConsumerRunCancelled
		res.RefusalDetail = "the run was cancelled while waiting for this test's effects: context canceled"
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("a cancelled window must never pass")
		}
		assertCategories(t, v, string(models.CategoryConsumerRunCancelled))
		if !strings.Contains(v.Rows[0].Meta[0].Actual, "cancelled") {
			t.Fatalf("the gate's own words must reach the row, got %q", v.Rows[0].Meta[0].Actual)
		}
	})

	t.Run("internal-error-without-a-refusal-is-still-cancelled", func(t *testing.T) {
		// The same end reason reaching the judge with no Refusal set — an
		// agent implementation that reports the reason and not the category.
		// It must still land on CONSUMER_RUN_CANCELLED rather than falling
		// through to the "no end reason" default.
		tc := specWith(produced)
		res := resultFor(tc, observe(t, produced))
		res.EndReason = models.ConsumerEndReasonInternalError
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("a window that ended on an internal error must never pass")
		}
		assertCategories(t, v, string(models.CategoryConsumerRunCancelled))
		assertRowKeys(t, v, "effects[*] fake window|effects.end_reason")
	})

	t.Run("unrelated-missing-and-extra-stay-two-findings", func(t *testing.T) {
		// PASS 2 MUST NOT MERGE UNRELATED EVENTS. It exists to give a better
		// name to two failures that are really one message routed elsewhere;
		// making it unconditional would collapse a genuine missing effect and
		// a genuine over-production into a single EFFECT_TARGET_CHANGED row
		// and drop both categories — losing information in the direction that
		// makes a regression look smaller than it is.
		tc := specWith(produced)
		unrelated := view("produce", "audit-log", "o-9", `{"orderId":"o-9","kind":"audit"}`)
		res := resultFor(tc, observe(t, unrelated))
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("a missing effect and an unrelated extra must fail")
		}
		assertCategories(t, v,
			string(models.CategoryEffectMissing),
			string(models.CategoryEffectUnexpected))
		assertRowKeys(t, v,
			"effects[0] fake produce order-events key=o-1|effects.0.presence",
			"effects[*] fake produce audit-log key=o-9|effects.*.unexpected",
		)
	})

	t.Run("target-swap-through-json-normalisation", func(t *testing.T) {
		// Pass 2's payload comparison is not bytes-only: two encodings of the
		// same document (key order, whitespace) are the same payload, and
		// without that normalisation this reports a separate missing and a
		// separate extra for one rerouted message.
		tc := specWith(view("produce", "order-events", "o-1", `{"orderId":"o-1","status":"CONFIRMED"}`))
		reordered := view("produce", "order-events-v2", "o-1", "{\n  \"status\": \"CONFIRMED\",\n  \"orderId\": \"o-1\"\n}")
		res := resultFor(tc, observe(t, reordered))
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("a rerouted effect must fail")
		}
		assertCategories(t, v, string(models.CategoryEffectTargetChanged))
		assertRowKeys(t, v, "effects[0] fake produce order-events key=o-1|effects.0.identity")
	})

	t.Run("non-json-body-changed", func(t *testing.T) {
		// The whole-body fallback: protobuf, avro, plain text. Returning no
		// meta here makes every changed non-JSON payload compare equal and
		// the test PASS, which is the same silent green the JSON path exists
		// to remove.
		before := view("produce", "order-events", "o-1", "AAAA")
		before.BodyType = ""
		after := view("produce", "order-events", "o-1", "BBBB")
		after.BodyType = ""
		tc := specWith(before)
		res := resultFor(tc, observe(t, after))
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("a changed non-JSON payload must fail")
		}
		assertCategories(t, v, string(models.CategoryEffectBodyChanged))
		assertRowKeys(t, v, "effects[0] fake produce order-events key=o-1|effects.0.body")
		m := v.Rows[0].Meta[0]
		if m.Expected != "AAAA" || m.Actual != "BBBB" {
			t.Fatalf("whole-body diff rendered expected=%q actual=%q", m.Expected, m.Actual)
		}
	})

	t.Run("body-declared-json-that-no-longer-parses", func(t *testing.T) {
		// The recording says JSON and the observed payload is not. That is
		// itself the finding, so it falls through to the whole-body
		// comparison rather than being silently skipped.
		after := view("produce", "order-events", "o-1", "not json at all")
		tc := specWith(view("produce", "order-events", "o-1", `{"orderId":"o-1"}`))
		res := resultFor(tc, observe(t, after))
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("a payload the recording declared JSON that no longer parses must fail")
		}
		assertCategories(t, v, string(models.CategoryEffectBodyChanged))
		assertRowKeys(t, v, "effects[0] fake produce order-events key=o-1|effects.0.body")
	})

	t.Run("whole-body-noise-silences-the-fallback", func(t *testing.T) {
		// Per-test noise reaches the whole-body path too, and the path it is
		// declared under is the one the report printed.
		before := view("produce", "order-events", "o-1", "AAAA")
		before.BodyType = ""
		after := view("produce", "order-events", "o-1", "BBBB")
		after.BodyType = ""
		tc := specWith(before)
		tc.Noise = map[string][]string{"effects.0.body": {}}
		res := resultFor(tc, observe(t, after))
		v := judge(tc, res)
		if !v.Pass {
			t.Fatalf("a body declared noise must not fail the test: %v %s", catStrings(v), v.Summary)
		}
	})

	t.Run("a-recorded-presence-view-is-never-missing", func(t *testing.T) {
		// THE OTHER HALF OF THE PRESENCE RULE. A projector that returns a
		// presence stand-in for a role=effect mock — the database-write shape
		// design §2 describes — puts that view in spec.Effects. Nothing on
		// the replay path calls ObserveEffect for a database write, so if the
		// judge built its expected lane from ALL of spec.Effects that view
		// could never be paired: every such test would report EFFECT_MISSING
		// on a healthy worker, and the positional pairing of every real
		// effect behind it in the lane would shift by one, manufacturing a
		// spurious identity change as well.
		presence := models.EffectView{Protocol: "postgres", Op: "write", Target: "orders", Decoded: models.DecodedPresence}
		tc := specWith(presence, produced)
		if got := tc.ConsumerSpec.Completion.ExpectEffects; got != 1 {
			t.Fatalf("ExpectEffects = %d, want 1: a presence view is not counted by the completion rule", got)
		}
		res := resultFor(tc, observe(t, produced))
		v := judge(tc, res)
		if !v.Pass {
			t.Fatalf("a worker that did exactly what it recorded must pass: %v %s rows=%v", catStrings(v), v.Summary, rowKeys(v.Rows))
		}
	})

	t.Run("presence-views-are-neither-diffed-nor-counted", func(t *testing.T) {
		// A mapped database write reaches the gate as a presence view. It has
		// no projector, so it is not an effects[i] row — and it must not move
		// the completion count either, or a healthy test reports 2 observed
		// against 1 expected and goes red.
		presence := models.EffectView{Protocol: "postgres", Op: "write", Target: "orders", Decoded: models.DecodedPresence}
		tc := specWith(produced, presence)
		if got := tc.ConsumerSpec.Completion.ExpectEffects; got != 1 {
			t.Fatalf("ExpectEffects = %d, want 1: a presence view is not counted by the completion rule", got)
		}
		res := resultFor(tc, observe(t, produced, presence))
		if res.ObservedEffects != 1 {
			t.Fatalf("a presence view must not be counted, observed=%d", res.ObservedEffects)
		}
		v := judge(tc, res)
		if !v.Pass {
			t.Fatalf("a produce plus a presence view is a healthy run: %v %s rows=%v", catStrings(v), v.Summary, rowKeys(v.Rows))
		}
	})

	t.Run("a-presence-effect-produced-twice-is-an-over-production", func(t *testing.T) {
		// THE ROW THAT USED TO BE SILENT. "The worker now writes the row
		// twice" produces no effects[i] row at all — the write has no
		// projector, so both rows arrive as presence views —
		// compareEffectLists filters them out of both lanes, and
		// ObservedEffects/ExpectEffects are blind to them on BOTH sides, so
		// the count assertion cannot see it either. The sync path cannot see
		// it: mockSetMismatch only fires on expected-not-consumed, never on an
		// extra. That left it a pass with no row, no category and no named
		// refusal, while the count assertion's own comment claimed it was
		// covered.
		presence := models.EffectView{Protocol: "postgres", Op: "write", Target: "orders", Decoded: models.DecodedPresence}
		tc := specWith(presence)
		res := resultFor(tc, observe(t, presence, presence))
		if res.ObservedEffects != 0 || res.ExpectEffects != 0 {
			t.Fatalf("guard: the completion arithmetic must stay blind to presence views, observed=%d expect=%d",
				res.ObservedEffects, res.ExpectEffects)
		}
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("a worker that writes the row twice where the recording writes it once must not pass silently")
		}
		assertCategories(t, v, string(models.CategoryEffectUnexpected))
		assertRowKeys(t, v, "effects[*] fake presence_count|effects.presence_count")
	})

	t.Run("an-extra-presence-effect-alongside-a-real-one-is-still-an-over-production", func(t *testing.T) {
		// The mixed shape: a worker that both produces and writes. The
		// presence pair is counted over the effects the judge does NOT assert,
		// so it has to be derived from the assertable count rather than from
		// the raw slice length — otherwise the recorded produce inflates the
		// expected presence total and the extra write hides inside it.
		presence := models.EffectView{Protocol: "postgres", Op: "write", Target: "orders", Decoded: models.DecodedPresence}
		tc := specWith(produced, presence)
		res := resultFor(tc, observe(t, produced, presence, presence))
		if res.ObservedEffects != 1 || res.ExpectEffects != 1 {
			t.Fatalf("guard: the completion arithmetic counts only the produce, observed=%d expect=%d",
				res.ObservedEffects, res.ExpectEffects)
		}
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("a second write the recording does not have must not hide behind a matching produce")
		}
		assertCategories(t, v, string(models.CategoryEffectUnexpected))
		assertRowKeys(t, v, "effects[*] fake presence_count|effects.presence_count")
		if got := v.Rows[0].Meta[0].Expected; got != "1" {
			t.Fatalf("expected presence count = %q, want \"1\": the produce is asserted elsewhere and must not be counted here", got)
		}
	})

	t.Run("fewer-presence-effects-is-the-sync-paths-claim-not-the-judges", func(t *testing.T) {
		// THE DELIBERATE ASYMMETRY, written down so it is a decision rather
		// than an oversight. v1 has no projector for a database write, so a
		// healthy replay legitimately observes ZERO presence views against a
		// recording that holds several; asserting equality here would redden
		// every consume-and-write test on a worker doing exactly what it
		// recorded. The missing half is carried by the sync path — an
		// unconsumed role=effect mock is non-demotable — which is the same
		// delegation the vacuity guard refuses to accept unchecked.
		presence := models.EffectView{Protocol: "postgres", Op: "write", Target: "orders", Decoded: models.DecodedPresence}
		tc := specWith(produced, presence, presence)
		res := resultFor(tc, observe(t, produced))
		v := judge(tc, res)
		if !v.Pass {
			t.Fatalf("v1 cannot observe a presence write, so its absence must not be the judge's finding: %v %s rows=%v",
				catStrings(v), v.Summary, rowKeys(v.Rows))
		}
	})
}

// ---------------------------------------------------------------------------
// §0.3 — the judge and the mock matcher must be provably disjoint.
// ---------------------------------------------------------------------------

// TestTheJudgeSeesThroughTheMatchersNoiseNames is the BEHAVIOURAL half of the
// disjointness pin.
//
// The mock matcher strips keys by BARE NAME at every nesting depth before it
// scores a candidate — timestamp, host, sequence, epoch, createTime — because
// those are the fields that legitimately drift between a recording and a
// replay of the same OUTGOING CALL. Inside a message payload they are ordinary
// business fields. A judge that reused that filter could not see a diff in any
// of them, which is most event envelopes ever written.
//
// Every key in this payload is one of those names, at two depths. All of them
// must diff.
func TestTheJudgeSeesThroughTheMatchersNoiseNames(t *testing.T) {
	before := `{"timestamp":"2026-08-22T10:00:00Z","host":"worker-a","sequence":1,"epoch":7,"createTime":"t0","event":{"timestamp":"inner-a","host":"inner-host-a"}}`
	after := `{"timestamp":"2026-08-22T11:00:00Z","host":"worker-b","sequence":2,"epoch":8,"createTime":"t1","event":{"timestamp":"inner-b","host":"inner-host-b"}}`

	tc := specWith(view("produce", "events", "k", before))
	res := resultFor(tc, observe(t, view("produce", "events", "k", after)))
	v := judge(tc, res)

	if v.Pass {
		t.Fatal("every field in this payload changed; a judge that passes it is the matcher wearing a different name")
	}
	assertCategories(t, v, string(models.CategoryEffectBodyChanged))
	assertRowKeys(t, v,
		"effects[0] fake produce events key=k|effects.0.body.createTime",
		"effects[0] fake produce events key=k|effects.0.body.epoch",
		"effects[0] fake produce events key=k|effects.0.body.event.host",
		"effects[0] fake produce events key=k|effects.0.body.event.timestamp",
		"effects[0] fake produce events key=k|effects.0.body.host",
		"effects[0] fake produce events key=k|effects.0.body.sequence",
		"effects[0] fake produce events key=k|effects.0.body.timestamp",
	)
}

// TestTheJudgeImportsNothingFromTheMatcher is the STRUCTURAL half. The
// behavioural test above holds today by construction, but nothing stops the
// next person deleting flattenBody in favour of the matcher's
// filterNoisyFields "just for the JSON walk" — and the file's own comment
// would still claim a test guards it. This makes that a build failure.
func TestTheJudgeImportsNothingFromTheMatcher(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "consumer.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse consumer.go: %v", err)
	}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if strings.Contains(path, "/pkg/matcher") {
			t.Errorf("pkg/service/replay/consumer.go imports %s. The judge must share no code with the mock matcher: "+
				"the matcher accepts a candidate at 0.65 with zero body agreement and name-filters timestamp/host/sequence/epoch "+
				"at every depth, so a verdict built on it cannot see a real payload diff (design §0.3).", path)
		}
	}
}

// ---------------------------------------------------------------------------
// Refusals and structural checks.
// ---------------------------------------------------------------------------

func TestCompareEffectsRefusals(t *testing.T) {
	produced := view("produce", "order-events", "o-1", `{"a":1}`)

	t.Run("no-spec", func(t *testing.T) {
		tc := &models.TestCase{Kind: models.CONSUMER, Name: "test-1"}
		v := judge(tc, &models.ConsumerResult{TestID: "test-1"})
		assertCategories(t, v, string(models.CategoryConsumerUnsupportedSpec))
	})

	t.Run("nil-result", func(t *testing.T) {
		v := judge(specWith(produced), nil)
		assertCategories(t, v, string(models.CategoryConsumerUnsupportedAgent))
	})

	t.Run("another-tests-window", func(t *testing.T) {
		tc := specWith(produced)
		res := resultFor(tc, observe(t, produced))
		res.TestID = "test-9"
		v := judge(tc, res)
		assertCategories(t, v, string(models.CategoryConsumerUnsupportedAgent))
		if !strings.Contains(v.Summary, "test-9") {
			t.Fatalf("the summary must name the window that came back, got %q", v.Summary)
		}
	})

	t.Run("agent-ignored-the-completion-rule", func(t *testing.T) {
		// The count assertion compares two numbers the AGENT supplied. Without
		// this cross-check a spec declaring three expected effects paired with
		// an agent reporting 0/0 clears every guard and ends count_reached — a
		// PASS for a test that asserts three effects.
		tc := specWith(produced)
		tc.ConsumerSpec.Completion.ExpectEffects = 3
		res := resultFor(tc, nil)
		res.ExpectEffects = 0
		res.ObservedEffects = 0
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("a window opened with a different completion rule cannot be graded against this spec")
		}
		assertCategories(t, v, string(models.CategoryConsumerUnsupportedAgent))
	})

	t.Run("agents-count-disagrees-with-its-own-views", func(t *testing.T) {
		// THE THIRD AGENT-SUPPLIED NUMBER. The count assertion grades
		// ObservedEffects; compareEffectLists pairs Effects. Nothing else
		// relates them, so an agent that reports "2 observed" and ships no
		// views was graded 2-against-2, paired nothing, and returned PASS
		// with zero per-effect comparisons — probed against the real judge
		// and confirmed green before this check existed.
		tc := specWith(produced, produced)
		res := resultFor(tc, nil)
		res.ObservedEffects = tc.ConsumerSpec.Completion.ExpectEffects
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("a window whose count and contents disagree cannot be graded: nothing in it was compared")
		}
		assertCategories(t, v, string(models.CategoryConsumerUnsupportedAgent))
		if !strings.Contains(v.Summary, "shipped views covering 0") {
			t.Fatalf("the refusal must name both numbers, got %q", v.Summary)
		}
	})

	t.Run("agents-count-disagrees-with-its-own-views-presence-is-not-counted", func(t *testing.T) {
		// The exclusion that keeps the check from reddening every healthy
		// consume-and-write window: a presence stand-in is SHIPPED as a view
		// and deliberately not counted into ObservedEffects, exactly as
		// Gate.pendingLocked has it.
		presence := models.EffectView{Protocol: fakeProto, Op: "insert", Target: "orders", Decoded: models.DecodedPresence}
		tc := specWith(presence)
		res := resultFor(tc, []models.EffectView{presence})
		if v := judge(tc, res); !v.Pass {
			t.Fatalf("a presence-only window must still be judgeable: %v %s", catStrings(v), v.Summary)
		}
	})

	t.Run("spec-declares-a-count-its-own-effects-do-not-support", func(t *testing.T) {
		// The file-side twin, on the vacuity guard's own premise: a spec is a
		// file and files get edited. Recorder.mint writes ExpectEffects as
		// the record count of exactly these views — an overflow refuses the
		// unit rather than dropping one — so a divergence is an edit.
		tc := specWith(produced)
		tc.ConsumerSpec.Completion.ExpectEffects = 3
		res := resultFor(tc, observe(t, produced))
		res.ExpectEffects = 3
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("a spec whose completion rule contradicts its own effects grades the worker against a number neither half states")
		}
		assertCategories(t, v, string(models.CategoryConsumerUnsupportedSpec))
	})

	t.Run("gate-refusal-wins-outright", func(t *testing.T) {
		tc := specWith(produced)
		res := resultFor(tc, nil)
		res.ObservedEffects = 0
		res.Refusal = models.CategoryConsumerUnsupportedWireVersion
		res.RefusalDetail = "Produce v9 is flexible and this build does not model it"
		v := judge(tc, res)
		// NOT "1 effect missing": the refusal was raised closest to the cause,
		// and blaming the worker for a decoder gap sends the reader to the
		// wrong place.
		assertCategories(t, v, string(models.CategoryConsumerUnsupportedWireVersion))
		assertRowKeys(t, v, "effects[*] fake refused|effects.refusal")
	})

	t.Run("unimplemented-assert-mode-is-refused-not-defaulted", func(t *testing.T) {
		e := produced
		e.Assert = models.EffectAssert("headers-only")
		tc := specWith(e)
		res := resultFor(tc, observe(t, e))
		v := judge(tc, res)
		assertCategories(t, v, string(models.CategoryConsumerUnsupportedSpec))
		assertRowKeys(t, v, "effects[0] fake produce order-events key=o-1|effects.0.assert")
	})
}

// EVERY EFFECT MATCHING IS NECESSARY AND NOT SUFFICIENT. A window that closed
// on its backstop was not fully observed — more effects may have been in
// flight — so passing on it would be passing on whatever happened to arrive
// before we stopped looking.
func TestTheWindowMustHaveClosedForTheRightReason(t *testing.T) {
	produced := view("produce", "order-events", "o-1", `{"a":1}`)

	cases := []struct {
		name   string
		reason models.ConsumerEndReason
		want   models.FailureCategory
	}{
		{"timeout", models.ConsumerEndReasonTimeout, models.CategoryConsumerCompletionTimeout},
		{"internal-error", models.ConsumerEndReasonInternalError, models.CategoryConsumerRunCancelled},
		{"no-end-reason-at-all", "", models.CategoryConsumerUnsupportedAgent},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			tc := specWith(produced)
			res := resultFor(tc, observe(t, produced))
			res.EndReason = tt.reason
			v := judge(tc, res)
			if v.Pass {
				t.Fatalf("every effect matched but the window ended %q; that is not a pass", tt.reason)
			}
			assertCategories(t, v, string(tt.want))
			assertRowKeys(t, v, "effects[*] fake window|effects.end_reason")
		})
	}
}

// A reported diff path pastes straight into that test's spec.assertions.noise
// and silences EXACTLY that path — not the sibling field, and not the test.
func TestAReportedPathPastedIntoNoiseSilencesExactlyThatPath(t *testing.T) {
	before := view("produce", "events", "k", `{"status":"CONFIRMED","processedAt":"t0"}`)
	after := view("produce", "events", "k", `{"status":"CONFIRMED","processedAt":"t1"}`)

	tc := specWith(before)
	res := resultFor(tc, observe(t, after))

	v := judge(tc, res)
	if v.Pass {
		t.Fatal("a drifting field must be reported before it can be silenced")
	}
	reported := v.Rows[0].Meta[0].Key
	if reported != "effects.0.body.processedAt" {
		t.Fatalf("reported key %q", reported)
	}

	// Paste it back in, verbatim.
	tc.Noise = map[string][]string{reported: {}}
	if v2 := judge(tc, res); !v2.Pass {
		t.Fatalf("the reported path must silence itself when pasted back: %v", rowKeys(v2.Rows))
	}

	// And it must silence NOTHING else: the sibling field still fails.
	drifted := view("produce", "events", "k", `{"status":"PENDING","processedAt":"t1"}`)
	res2 := resultFor(tc, observe(t, drifted))
	v3 := judge(tc, res2)
	if v3.Pass {
		t.Fatal("noising processedAt must not silence status")
	}
	assertRowKeys(t, v3, "effects[0] fake produce events key=k|effects.0.body.status")

	// A PASTED PATH CARRIES WHITESPACE. The remedy this judge hands the user
	// is "paste effects.<i>.body.<field> into spec.assertions.noise", and a
	// path pasted out of a terminal or hand-indented into YAML arrives with a
	// space on it. Silently not matching would leave the user re-noising a
	// field they already noised, with nothing to tell them why.
	tc.Noise = map[string][]string{"  " + reported + "\t": {}}
	if v4 := judge(tc, res); !v4.Pass {
		t.Fatalf("a noise path with surrounding whitespace must still match: %v", rowKeys(v4.Rows))
	}
}

// A field that exists on one side only renders as a distinct token, so "the
// field is gone" and "the field is now empty" do not read identically.
func TestAnAbsentFieldIsNotAnEmptyField(t *testing.T) {
	tc := specWith(view("produce", "events", "k", `{"a":"x","b":"y"}`))
	res := resultFor(tc, observe(t, view("produce", "events", "k", `{"a":"x","b":""}`)))
	v := judge(tc, res)
	if got := v.Rows[0].Meta[0].Actual; got != "" {
		t.Fatalf("an emptied field renders as empty, got %q", got)
	}

	res2 := resultFor(tc, observe(t, view("produce", "events", "k", `{"a":"x"}`)))
	v2 := judge(tc, res2)
	if got := v2.Rows[0].Meta[0].Actual; got != absentValue {
		t.Fatalf("a removed field renders as %q, got %q", absentValue, got)
	}
}

// Array ORDER is part of the assertion: [a, b] and [b, a] are different
// messages, and a set-wise comparison would hide a reordering regression in a
// list of line items.
func TestArrayOrderIsPartOfTheAssertion(t *testing.T) {
	tc := specWith(view("produce", "events", "k", `{"items":[{"sku":"A"},{"sku":"B"}]}`))
	res := resultFor(tc, observe(t, view("produce", "events", "k", `{"items":[{"sku":"B"},{"sku":"A"}]}`)))
	v := judge(tc, res)
	if v.Pass {
		t.Fatal("a reordered list is a different message")
	}
	assertRowKeys(t, v,
		"effects[0] fake produce events key=k|effects.0.body.items.0.sku",
		"effects[0] fake produce events key=k|effects.0.body.items.1.sku",
	)
}

// ---------------------------------------------------------------------------
// Lanes: ordered WITHIN a lane, unordered ACROSS lanes.
// ---------------------------------------------------------------------------

func TestLaneOrdering(t *testing.T) {
	laneView := func(target, lane, body string) models.EffectView {
		v := view("produce", target, "", body)
		v.Coords = map[string]string{"partition": lane}
		return v
	}
	a := laneView("orders", "0", `{"n":1}`)
	b := laneView("orders", "1", `{"n":2}`)

	t.Run("across-lanes-is-unordered-when-the-recording-names-a-lane", func(t *testing.T) {
		tc := specWith(a, b)
		tc.ConsumerSpec.OrderBy = "partition"
		// Two goroutines producing to different partitions interleave
		// nondeterministically; that is not a regression.
		res := resultFor(tc, observe(t, b, a))
		v := judge(tc, res)
		if !v.Pass {
			t.Fatalf("effects in different lanes are not ordered against each other: %v %s", rowKeys(v.Rows), v.Summary)
		}
	})

	t.Run("within-a-lane-order-is-asserted", func(t *testing.T) {
		first := laneView("orders", "0", `{"n":1}`)
		second := laneView("orders", "0", `{"n":2}`)
		tc := specWith(first, second)
		tc.ConsumerSpec.OrderBy = "partition"
		res := resultFor(tc, observe(t, second, first))
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("a swap inside one lane is the observable semantics of a log changing; it must fail")
		}
		assertCategories(t, v, string(models.CategoryEffectBodyChanged))
	})

	t.Run("no-orderBy-takes-the-stricter-reading", func(t *testing.T) {
		// A recording that does not claim its effects are independently
		// ordered has not earned the weaker assertion.
		tc := specWith(a, b)
		res := resultFor(tc, observe(t, b, a))
		v := judge(tc, res)
		if v.Pass {
			t.Fatal("with no lane coordinate every effect to a target is ordered against every other")
		}
	})
}

// ---------------------------------------------------------------------------
// backdateFor — the one place an EXISTING kind's behaviour changes.
// ---------------------------------------------------------------------------

func TestBackdateFor(t *testing.T) {
	at := func(s string) time.Time {
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		return ts
	}
	httpTC := func(ts time.Time, created int64) *models.TestCase {
		tc := &models.TestCase{Kind: models.HTTP, Created: created}
		tc.HTTPReq.Timestamp = ts
		return tc
	}
	consumerTC := func(req time.Time) *models.TestCase {
		return &models.TestCase{
			Kind:         models.CONSUMER,
			ConsumerSpec: &models.ConsumerSpec{ReqTimestampMock: req},
		}
	}
	grpcTC := func(req time.Time) *models.TestCase {
		tc := &models.TestCase{Kind: models.GRPC_EXPORT}
		tc.GrpcReq.Timestamp = req
		return tc
	}

	first := at("2026-08-22T10:00:00Z")
	second := at("2026-08-22T10:05:00Z")

	cases := []struct {
		name string
		in   []*models.TestCase
		want time.Time
	}{
		{
			// UNCHANGED, and this is the row that matters for backward
			// compatibility: the overwhelmingly common HTTP shape returns
			// exactly what `testCases[0].HTTPReq.Timestamp` returned.
			name: "http set returns the first case's request timestamp",
			in:   []*models.TestCase{httpTC(first, 0), httpTC(second, 0)},
			want: first,
		},
		{
			// CHANGED, deliberately. The old code returned the zero time for
			// this set (an imported test, a curl-generated one, an older
			// recording); ca.go then substituted time.Now(). Created is
			// strictly closer to the recording.
			name: "http set whose first case has no timestamp falls back to Created",
			in:   []*models.TestCase{httpTC(time.Time{}, first.Unix()), httpTC(second, 0)},
			want: time.Unix(first.Unix(), 0),
		},
		{
			// CHANGED, deliberately: a later case's real timestamp beats
			// nothing at all.
			name: "http set with no usable anchor on the first case scans forward",
			in:   []*models.TestCase{{Kind: models.HTTP}, httpTC(second, 0)},
			want: second,
		},
		{
			name: "consumer set anchors on the trigger",
			in:   []*models.TestCase{consumerTC(first)},
			want: first,
		},
		{
			// CHANGED, deliberately, and this is the ONE existing Kind whose
			// behaviour moves. `testCases[0].HTTPReq.Timestamp` is the zero
			// time for a gRPC test case, so a gRPC-only set used to hand
			// ca.go the zero time and ca.go substituted wall-clock now; it
			// now anchors on the recording. NotBefore = anchor-1y and
			// NotAfter = now+1y are set independently in CertForClient, so an
			// earlier anchor only WIDENS the validity window and can never
			// expire a certificate.
			name: "grpc-only set anchors on the recorded request",
			in:   []*models.TestCase{grpcTC(first)},
			want: first,
		},
		{
			name: "a mixed HTTP and gRPC set still anchors on the first usable timestamp",
			in:   []*models.TestCase{grpcTC(time.Time{}), httpTC(second, 0)},
			want: second,
		},
		{
			// Still possible, and still handed to ca.go, which has always
			// substituted time.Now() for it. Manufacturing a plausible anchor
			// here would hide the problem one layer further down.
			name: "a set with no usable timestamp anywhere returns the zero time",
			in:   []*models.TestCase{{Kind: models.HTTP}, {Kind: models.CONSUMER}},
			want: time.Time{},
		},
		{
			name: "an empty set returns the zero time",
			in:   nil,
			want: time.Time{},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := backdateFor(tt.in); !got.Equal(tt.want) {
				t.Fatalf("backdateFor = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// The two set-level refusals.
// ---------------------------------------------------------------------------

// newConsumerDepAssertion is the wiring the vacuity guard depends on, and the
// one piece of it RunTestSet used to compute inline where nothing could reach
// it: a reviewer rewrote `Ran: depAssertionValid && len(filteredExpectedNames)
// > 0` as `... || true` with every identifier preserved and the package stayed
// green.
//
// THE PREDICATE IS NOT "THE MAPPING IS NON-EMPTY". Every mapped consumer test
// carries its own role=trigger mock, so a mapping degraded to trigger-only —
// the write mock revoked, reaped by the stale cutoff, or landed in the
// previous window — would report Ran=true while the only thing the sync path
// asserted is that keploy's own trigger mock was consumed. A consume-and-write
// test would then pass with zero assertions about its writes, which is the
// exact hole the guard exists to close, one step removed.
//
// NOR IS IT "A MAPPED MOCK OF ANOTHER PROTOCOL FAMILY", which was the first
// fix and was defeated by the very traffic the claim is made of: spec.
// SideEffects is Recorder.onOther's count of every cross-family mock inside
// the unit's window, and mappings.yaml is built from that same window, so a
// worker that consumed the message and dropped it still maps its process's
// ambient /health database ping next to its trigger. The delegate then
// vouched for the claim with the number it was supposed to check. The
// predicate is POSITIVE ATTRIBUTION — role=effect — and nothing weaker.
func TestNewConsumerDepAssertion(t *testing.T) {
	trigger := mockDisplayInfo{kind: models.KAFKA, role: models.RoleTrigger}
	effect := mockDisplayInfo{kind: models.KAFKA, role: models.RoleEffect}
	coord := mockDisplayInfo{kind: models.KAFKA}
	write := mockDisplayInfo{kind: models.Postgres}

	tests := []struct {
		name              string
		hasMapping        bool
		depAssertionValid bool
		expected          []string
		lookup            map[string]mockDisplayInfo
		want              consumerDepAssertion
	}{
		{
			name:       "no mapping at all",
			hasMapping: false, depAssertionValid: false,
			want: consumerDepAssertion{HasMapping: false, Ran: false},
		},
		{
			// THE ROW THE FIX EXISTS FOR.
			name:       "a trigger-only mapping asserts nothing about the worker",
			hasMapping: true, depAssertionValid: true,
			expected: []string{"mock-trigger"},
			lookup:   map[string]mockDisplayInfo{"mock-trigger": trigger},
			want:     consumerDepAssertion{HasMapping: true, Ran: false},
		},
		{
			name:       "a trigger plus same-family coordination traffic still asserts nothing about the worker",
			hasMapping: true, depAssertionValid: true,
			expected: []string{"mock-trigger", "mock-coord"},
			lookup:   map[string]mockDisplayInfo{"mock-trigger": trigger, "mock-coord": coord},
			want:     consumerDepAssertion{HasMapping: true, Ran: false},
		},
		{
			// No trigger is evidence about the worker, whatever its family.
			// Without the role skip a second trigger-tagged mock of a
			// different Kind would vouch for the worker's writes.
			name:       "a second trigger of another family is still a trigger",
			hasMapping: true, depAssertionValid: true,
			expected: []string{"mock-trigger", "mock-trigger-other"},
			lookup: map[string]mockDisplayInfo{
				"mock-trigger":       trigger,
				"mock-trigger-other": {kind: models.Postgres, role: models.RoleTrigger},
			},
			want: consumerDepAssertion{HasMapping: true, Ran: false},
		},
		{
			name:       "a mapped effect mock carries the claim",
			hasMapping: true, depAssertionValid: true,
			expected: []string{"mock-trigger", "mock-effect"},
			lookup:   map[string]mockDisplayInfo{"mock-trigger": trigger, "mock-effect": effect},
			want:     consumerDepAssertion{HasMapping: true, Ran: true},
		},
		{
			// THE ROW THE SECOND FIX EXISTS FOR, and the probe from the
			// review verbatim: a worker that took the message and silently
			// dropped it, whose recording still minted sideEffects: 1 from
			// the process's ambient /health database ping — which the same
			// window authority then mapped next to the trigger. An untagged
			// cross-family mock is indistinguishable from that, so it carries
			// nothing. Restoring the `info.kind != triggerKind` arm turns
			// this row green and reopens design §5 false-pass row 3.
			name:       "an untagged mock of another family is ambient traffic, not evidence",
			hasMapping: true, depAssertionValid: true,
			expected: []string{"mock-trigger", "mock-write"},
			lookup:   map[string]mockDisplayInfo{"mock-trigger": trigger, "mock-write": write},
			want:     consumerDepAssertion{HasMapping: true, Ran: false},
		},
		{
			// The consume-and-write shape ONCE A PARSER TAGS IT. This is what
			// slice 6 must do for such a test to be graded rather than
			// refused; OSS ships no parser that does, which is why the row
			// above is the OSS reality and this one is the contract.
			name:       "a mapped write TAGGED role=effect carries the claim",
			hasMapping: true, depAssertionValid: true,
			expected: []string{"mock-trigger", "mock-write"},
			lookup: map[string]mockDisplayInfo{
				"mock-trigger": trigger,
				"mock-write":   {kind: models.Postgres, role: models.RoleEffect},
			},
			want: consumerDepAssertion{HasMapping: true, Ran: true},
		},
		{
			// The mapping degraded to just the write — trigger revoked, or
			// landed in the previous window. Still refused, and for the
			// reason the message now names: nothing here is attributed.
			name:       "an untagged write with no trigger in the mapping still carries nothing",
			hasMapping: true, depAssertionValid: true,
			expected: []string{"mock-write"},
			lookup:   map[string]mockDisplayInfo{"mock-write": write},
			want:     consumerDepAssertion{HasMapping: true, Ran: false},
		},
		{
			name:       "the assertion did not run at all",
			hasMapping: true, depAssertionValid: false,
			expected: []string{"mock-trigger", "mock-effect"},
			lookup:   map[string]mockDisplayInfo{"mock-trigger": trigger, "mock-effect": effect},
			want:     consumerDepAssertion{HasMapping: true, Ran: false},
		},
		{
			// A mapping that survived tier filtering with nothing in it.
			name:       "an empty filtered mapping carries nothing",
			hasMapping: true, depAssertionValid: true,
			expected: nil,
			lookup:   map[string]mockDisplayInfo{"mock-trigger": trigger},
			want:     consumerDepAssertion{HasMapping: true, Ran: false},
		},
		{
			// The registry did not load. Nothing can be shown to carry a
			// claim, so the judge refuses by name rather than passing on a
			// delegate it cannot confirm ran.
			name:       "an unloadable mock registry carries nothing",
			hasMapping: true, depAssertionValid: true,
			expected: []string{"mock-trigger", "mock-write"},
			lookup:   map[string]mockDisplayInfo{},
			want:     consumerDepAssertion{HasMapping: true, Ran: false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newConsumerDepAssertion(tt.hasMapping, tt.depAssertionValid, tt.expected, tt.lookup)
			if got != tt.want {
				t.Fatalf("newConsumerDepAssertion = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// The end-to-end pin: a trigger-only mapping must make the judge REFUSE a
// consume-and-write test rather than pass it. This is the composition the call
// site performs, driven through the same two functions.
func TestATriggerOnlyMappingCannotVouchForTheWorker(t *testing.T) {
	lookup := map[string]mockDisplayInfo{"mock-trigger": {kind: models.KAFKA, role: models.RoleTrigger}}
	dep := newConsumerDepAssertion(true, true, []string{"mock-trigger"}, lookup)

	tc := specWith()
	tc.ConsumerSpec.SideEffects = 3
	v := compareEffects(tc, resultFor(tc, nil), dep)
	if v.Pass {
		t.Fatal("the test's own trigger is not evidence that the worker made its writes again")
	}
	assertCategories(t, v, string(models.CategoryConsumerNoObservableEffect))

	// An UNTAGGED cross-family mock does not rescue it either: that is the
	// ambient-traffic shape (a /health handler's database ping inside the same
	// window, mapped by the same window authority), and letting it vouch would
	// be the sideEffects count vouching for itself.
	lookup["mock-write"] = mockDisplayInfo{kind: models.Postgres}
	dep = newConsumerDepAssertion(true, true, []string{"mock-trigger", "mock-write"}, lookup)
	v = compareEffects(tc, resultFor(tc, nil), dep)
	if v.Pass {
		t.Fatal("an untagged cross-family mock is not evidence that THIS worker made THIS write again")
	}
	assertCategories(t, v, string(models.CategoryConsumerNoObservableEffect))
	if !strings.Contains(v.Summary, string(models.MetaKeyRole)+"="+models.RoleEffect) {
		t.Fatalf("the refusal must name what is missing, so the reader does not go regenerate a mapping that is already complete: %s", v.Summary)
	}

	// And with the write TAGGED as the worker's own production the same test
	// is judged, not refused — the guard must stay narrow.
	lookup["mock-write"] = mockDisplayInfo{kind: models.Postgres, role: models.RoleEffect}
	dep = newConsumerDepAssertion(true, true, []string{"mock-trigger", "mock-write"}, lookup)
	if v := compareEffects(tc, resultFor(tc, nil), dep); !v.Pass {
		t.Fatalf("a mapping that can carry the claim must be accepted: %v %s", catStrings(v), v.Summary)
	}
}

// THE TWO EFFECT PREDICATES MUST FAIL IN OPPOSITE DIRECTIONS, because they
// answer opposite questions:
//
//	hasUnconsumedEffectMock gates a RED (promote a PASSED test to FAILED). Its
//	wrong answer is a missed regression, so it fails CLOSED: anything not
//	positively identified as same-family coordination traffic vetoes.
//
//	mappingCanCarryAnEffectClaim gates a GREEN (let a test with no assertable
//	effect pass on a delegated claim). Its wrong answer is a SILENT PASS, so
//	it fails closed the other way: only positive attribution counts.
//
// They were briefly one predicate with one vocabulary and it could not be
// right for both — the shared version let ambient cross-family traffic vouch
// for a worker that produced nothing. This pins the asymmetry so a future
// "cleanup" that re-unifies them fails here rather than in production.
func TestTheTwoEffectPredicatesFailInOppositeDirections(t *testing.T) {
	trigger := mockDisplayInfo{kind: models.KAFKA, role: models.RoleTrigger}
	untaggedWrite := mockDisplayInfo{kind: models.Postgres}
	taggedWrite := mockDisplayInfo{kind: models.Postgres, role: models.RoleEffect}
	coord := mockDisplayInfo{kind: models.KAFKA}

	tests := []struct {
		name        string
		expected    []string
		lookup      map[string]mockDisplayInfo
		wantCarries bool // may this test PASS on the delegated claim?
		wantVetoes  bool // must an unconsumed one FAIL a passing test?
		why         string
	}{
		{
			name:        "untagged cross-family traffic: no green, but still a red if it goes missing",
			expected:    []string{"mock-trigger", "mock-write"},
			lookup:      map[string]mockDisplayInfo{"mock-trigger": trigger, "mock-write": untaggedWrite},
			wantCarries: false, wantVetoes: true,
			why: "it cannot be told from an unrelated handler's call, so it may not vouch for the worker; " +
				"but if it WAS the write and it stopped happening, the run must go red",
		},
		{
			name:        "a tagged effect: both",
			expected:    []string{"mock-trigger", "mock-write"},
			lookup:      map[string]mockDisplayInfo{"mock-trigger": trigger, "mock-write": taggedWrite},
			wantCarries: true, wantVetoes: true,
			why: "the recording attributed it to the worker, so it carries the claim AND its absence is a regression",
		},
		{
			name:        "same-family coordination: neither",
			expected:    []string{"mock-trigger", "mock-coord"},
			lookup:      map[string]mockDisplayInfo{"mock-trigger": trigger, "mock-coord": coord},
			wantCarries: false, wantVetoes: false,
			why: "a fetch position or a heartbeat is the one shape a client may legitimately skip; " +
				"it is neither evidence nor a regression",
		},
		{
			name:        "an unloadable registry: no green, and a red",
			expected:    []string{"mock-trigger", "mock-write"},
			lookup:      map[string]mockDisplayInfo{},
			wantCarries: false, wantVetoes: true,
			why: "a side lookup that did not load must degrade to red, never to green",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mappingCanCarryAnEffectClaim(tt.expected, tt.lookup); got != tt.wantCarries {
				t.Errorf("mappingCanCarryAnEffectClaim = %v, want %v (the GREEN gate).\nWHY: %s", got, tt.wantCarries, tt.why)
			}
			// Nothing consumed: every expected mock went missing.
			if got := hasUnconsumedEffectMock(tt.expected, nil, tt.lookup); got != tt.wantVetoes {
				t.Errorf("hasUnconsumedEffectMock = %v, want %v (the RED gate).\nWHY: %s", got, tt.wantVetoes, tt.why)
			}
		})
	}
}

// THE REFUSAL'S PREDICATE IS "THE MAPPING IS ARMED", NOT "A MAPPINGS.YAML WAS
// LOADED". Those are different states: SendMockFilterParamsToAgent returns
// early when r.instrument is false, so with a complete mappings.yaml on disk
// and no application command NOTHING is armed and the agent serves every test
// from the whole recorded pool — precisely the timestamp-window state this
// refusal exists to prevent, reached through the door the refusal was not
// watching.
func TestRefuseUnmappedConsumerSet(t *testing.T) {
	consumerSet := []*models.TestCase{{Kind: models.CONSUMER, ConsumerSpec: &models.ConsumerSpec{}}}
	httpSet := []*models.TestCase{{Kind: models.HTTP}}

	tests := []struct {
		name                              string
		instrument                        bool
		useMappingBased, isMappingEnabled bool
		set                               []*models.TestCase
		wantRefused                       bool
		wantDetail                        string
		why                               string
	}{
		{
			name:       "no mappings at all",
			instrument: true, useMappingBased: false, isMappingEnabled: true,
			set: consumerSet, wantRefused: true,
			wantDetail: "has no usable per-test mock mappings",
			why:        "timestamp-window filtering cannot arm exactly one trigger",
		},
		{
			// THE ROW THE FIX EXISTS FOR.
			name:       "mappings on disk but nothing to arm them at the agent",
			instrument: false, useMappingBased: true, isMappingEnabled: true,
			set: consumerSet, wantRefused: true,
			wantDetail: "cannot arm its per-test mock mappings",
			why: "useMappingBased only means a mappings.yaml loaded; without an application command " +
				"SendMockFilterParamsToAgent returns early and the whole pool stays resident",
		},
		{
			name:       "mappings disabled by configuration",
			instrument: true, useMappingBased: true, isMappingEnabled: false,
			set: consumerSet, wantRefused: true,
			wantDetail: "disabled by configuration",
			why:        "test.disableMapping puts the run back on timestamp-window filtering",
		},
		{
			name:       "an armed consumer set runs",
			instrument: true, useMappingBased: true, isMappingEnabled: true,
			set: consumerSet, wantRefused: false,
		},
		{
			name:       "an unmapped HTTP set is untouched by this rule",
			instrument: false, useMappingBased: false, isMappingEnabled: false,
			set: httpSet, wantRefused: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Replayer{logger: zap.NewNop(), instrument: tt.instrument}
			err := r.refuseUnmappedConsumerSet("set-0", tt.set, tt.useMappingBased, tt.isMappingEnabled)
			if !tt.wantRefused {
				if err != nil {
					t.Fatalf("must run: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("the set must be refused.\nWHY THIS MATTERS: %s", tt.why)
			}
			if !strings.Contains(err.Error(), string(models.CategoryConsumerMappingsRequired)) {
				t.Fatalf("the category must be named in the error so the log, the exit code and an agent agree: %v", err)
			}
			if !strings.Contains(err.Error(), tt.wantDetail) {
				t.Fatalf("the error must name WHICH conjunct failed, or the reader regenerates a mapping that is already complete.\ngot:  %v\nwant it to contain: %q", err, tt.wantDetail)
			}
		})
	}
}

func TestRefuseRepeatPassOverConsumerSet(t *testing.T) {
	consumerSet := []*models.TestCase{{Kind: models.CONSUMER, ConsumerSpec: &models.ConsumerSpec{}}}
	httpSet := []*models.TestCase{{Kind: models.HTTP}}

	newReplayer := func(retryPassing bool, maxFlaky uint32) *Replayer {
		cfg := &config.Config{}
		cfg.RetryPassing = retryPassing
		cfg.Test.MaxFlakyChecks = maxFlaky
		return &Replayer{logger: zap.NewNop(), config: cfg}
	}

	// BOTH FLAGS ARE NAMED IN ONE PLACE so they cannot drift: --must-pass is
	// --retryPassing spelled differently (it sets MaxFlakyChecks), and
	// refusing one while honouring the other is how this rule would rot.
	for _, tt := range []struct {
		name         string
		retryPassing bool
		maxFlaky     uint32
		wantFlag     string
	}{
		{"retryPassing", true, 0, "--retryPassing"},
		{"mustPass", false, 3, "--must-pass"},
		{"both", true, 3, "--retryPassing and --must-pass"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := newReplayer(tt.retryPassing, tt.maxFlaky)
			err := r.refuseRepeatPassOverConsumerSet("set-0", consumerSet)
			if err == nil {
				t.Fatal("a repeat pass over a consumer set is not a repeat: the fetch position and the producer sequence do not rewind")
			}
			if !strings.Contains(err.Error(), string(models.CategoryConsumerRepeatPassUnsupported)) {
				t.Fatalf("category missing from %v", err)
			}
			for _, want := range strings.Split(tt.wantFlag, " and ") {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error must name %s: %v", want, err)
				}
			}
		})
	}

	t.Run("no-flag-no-refusal", func(t *testing.T) {
		r := newReplayer(false, 1)
		if err := r.refuseRepeatPassOverConsumerSet("set-0", consumerSet); err != nil {
			t.Fatalf("a plain run of a consumer set is fine: %v", err)
		}
	})
	t.Run("http-set-is-untouched", func(t *testing.T) {
		r := newReplayer(true, 3)
		if err := r.refuseRepeatPassOverConsumerSet("set-0", httpSet); err != nil {
			t.Fatalf("--retryPassing over an HTTP set must keep working: %v", err)
		}
	})
}

// CompareEffects is the *Replayer wrapper the replay loop calls. It must
// produce a result whose FailureInfo carries every category, and it must mark
// StatusCode Normal so the report's status-diff renderer does not print a
// meaningless "expected 0, got 0" under every failed consumer test.
func TestCompareEffectsWrapper(t *testing.T) {
	r := &Replayer{logger: zap.NewNop()}
	produced := view("produce", "order-events", "o-1", `{"a":1}`)

	tc := specWith(produced)
	pass, res := r.CompareEffects(tc, resultFor(tc, observe(t, produced)), "set-0", true, depChecked)
	if !pass {
		t.Fatal("a healthy run must pass through the wrapper too")
	}
	if !res.StatusCode.Normal {
		t.Fatal("a consumer test has no status code; leaving it non-Normal prints a fabricated status diff")
	}
	if len(res.FailureInfo.Category) != 0 {
		t.Fatalf("a passing test carries no categories, got %v", res.FailureInfo.Category)
	}

	drifted := view("produce", "order-events", "o-1", `{"a":2}`)
	pass, res = r.CompareEffects(tc, resultFor(tc, observe(t, drifted)), "set-0", true, depChecked)
	if pass {
		t.Fatal("a body diff must not pass")
	}
	if res.FailureInfo.Risk != models.High {
		t.Fatalf("risk = %v", res.FailureInfo.Risk)
	}
	if len(res.FailureInfo.Category) != 1 || res.FailureInfo.Category[0] != models.CategoryEffectBodyChanged {
		t.Fatalf("categories = %v", res.FailureInfo.Category)
	}
	if len(res.DepResult) == 0 {
		t.Fatal("the verdict's rows must reach models.Result.DepResult, which is what the renderer, JUnit and --format json read")
	}
	if !models.IsEffectRow(res.DepResult[0]) {
		t.Fatalf("row %q must carry the effects[ prefix that tells it from a sync-path deps[ row", res.DepResult[0].Name)
	}
}
