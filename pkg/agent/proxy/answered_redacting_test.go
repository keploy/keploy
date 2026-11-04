package proxy

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// redactingRecording is a per-test recording of kind keyed by url (byURL),
// which redacted something when redacts (it holds a pattern), with a request
// body of body.
func redactingRecording(name string, kind models.Kind, url, body string, redacts bool) *models.Mock {
	m := keyedRecording(name, kind, 1, url, time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC))
	m.Spec.HTTPReq.Body = body
	if redacts {
		m.Noise = []string{`^REDACTED_abc$`}
	}
	return m
}

func redactingNames(ms []*models.Mock) string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return strings.Join(out, ",")
}

// The answered log keeps apart, of the test set, every per-test recording
// that answered and holds a pattern, once a name and content (another
// set's of the same name too), its kind's own: whatever
// its request keys (a kind that declares none) and its size (one the keyed
// log is too small for), never one that redacted nothing, nor one an
// earlier set's staging pooled. With them its response is not kept.
func TestTheAnsweredLogKeepsEveryRecordingThatRedactedApart(t *testing.T) {
	byURL(t)
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	a := redactingRecording("mock-a", models.HTTP, "/a", `{"assertion":"REDACTED_abc"}`, true)
	a.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200, Body: "the answer"}
	plain := redactingRecording("mock-p", models.HTTP, "/p", `{}`, false)
	again := redactingRecording("mock-a", models.HTTP, "/a2", `{}`, true)
	// A kind no request key is declared for.
	keyless := redactingRecording("mock-k", models.Mongo, "/k", `{}`, true)
	big := redactingRecording("mock-b", models.HTTP, "/b", strings.Repeat("x", answeredMaxEntryBytes+1), true)
	earlier := redactingRecording("mock-e", models.HTTP, "/e", `{}`, true)
	earlier.TestModeInfo.StagedSet = mm.setGen.Load() + 7
	for _, m := range []*models.Mock{a, plain, again, keyless, big, earlier} {
		mm.noteAnswered(*m)
	}
	// The keyed log keeps, of these, the recordings with keys under its
	// entry bound: mock-a twice and mock-p.
	if mm.answered.live != 3 {
		t.Fatalf("the keyed log holds %d; want 3", mm.answered.live)
	}
	got, complete := mm.AnsweredRedacting(models.HTTP)
	if names := redactingNames(got); names != "mock-a,mock-a,mock-b" || !complete {
		t.Fatalf("HTTP kept %q (complete %v); want mock-a,mock-a,mock-b, complete", names, complete)
	}
	if got[0].Spec.HTTPReq.URL != "/a" || got[0].Spec.HTTPResp != nil || len(got[0].Noise) != 1 || got[1].Spec.HTTPReq.URL != "/a2" {
		t.Fatalf("kept %+v, %+v; want each noted's request and patterns, and no response", got[0].Spec, got[1].Spec)
	}
	if got, complete := mm.AnsweredRedacting(models.Mongo); redactingNames(got) != "mock-k" || !complete {
		t.Fatalf("Mongo kept %q (complete %v); want mock-k, complete", redactingNames(got), complete)
	}
}

// What the log keeps apart is bounded: past redactingMaxBytes a recording is
// not kept, and its kind is told incomplete, another kind not. A new test
// set (fence) forgets them all, incomplete or not; a reset with no fence (it
// runs mid-set too) keeps them, and whether they are complete, until the
// next fence.
func TestTheAnsweredRedactingRecordingsAreBounded(t *testing.T) {
	byURL(t)
	fence := func(mm *MockManager) {
		mm.swapMu.Lock()
		defer mm.swapMu.Unlock()
		mm.dropPreviousSetConsumes()
	}
	for _, forget := range []struct {
		name  string
		do    func(mm *MockManager)
		keeps bool
	}{
		{"fence", fence, false},
		{"reset", func(mm *MockManager) { mm.answered.reset() }, true},
		{"session-reset", func(mm *MockManager) { mm.ResetForReplaySession() }, true},
	} {
		t.Run(forget.name, func(t *testing.T) {
			mm := NewMockManager(nil, nil, zap.NewNop())
			defer mm.Close()
			mm.noteAnswered(*redactingRecording("mock-m", models.Mongo, "/m", `{}`, true))
			body := strings.Repeat("x", 1<<20)
			n := 0
			for ; n < 32; n++ {
				mm.noteAnswered(*redactingRecording("mock-"+string(rune('A'+n)), models.HTTP, "/x", body, true))
				if _, complete := mm.AnsweredRedacting(models.HTTP); !complete {
					break
				}
			}
			got, complete := mm.AnsweredRedacting(models.HTTP)
			if complete || n < 8 || n >= 32 || len(got) != n {
				t.Fatalf("after %d of 1 MiB: %d kept, complete %v; want the first over %d bytes not kept, incomplete", n+1, len(got), complete, redactingMaxBytes)
			}
			if got, complete := mm.AnsweredRedacting(models.Mongo); len(got) != 1 || !complete {
				t.Fatalf("another kind: %d kept, complete %v; want its one, complete", len(got), complete)
			}
			forget.do(mm)
			if forget.keeps {
				if got, complete := mm.AnsweredRedacting(models.HTTP); complete || len(got) != n {
					t.Fatalf("after a %s: %d kept, complete %v; want the %d kept, incomplete, until the next fence", forget.name, len(got), complete, n)
				}
				// The bound holds across the reset: what is kept still
				// counts, so a reset after a reset cannot grow it past.
				mm.noteAnswered(*redactingRecording("mock-after", models.HTTP, "/x", strings.Repeat("y", 1<<20), true))
				if got, complete := mm.AnsweredRedacting(models.HTTP); complete || len(got) != n || mm.answered.redactingBytes > redactingMaxBytes {
					t.Fatalf("a note after a %s: %d kept (%d bytes), complete %v; want the %d kept, under %d bytes, incomplete", forget.name, len(got), mm.answered.redactingBytes, complete, n, redactingMaxBytes)
				}
				if got, complete := mm.AnsweredRedacting(models.Mongo); len(got) != 1 || !complete {
					t.Fatalf("another kind after a %s: %d kept, complete %v; want its one, complete", forget.name, len(got), complete)
				}
				fence(mm)
			}
			for _, kind := range []models.Kind{models.HTTP, models.Mongo} {
				if got, complete := mm.AnsweredRedacting(kind); len(got) != 0 || !complete {
					t.Fatalf("%s after %s: %d kept, complete %v; want none, complete", kind, forget.name, len(got), complete)
				}
			}
		})
	}
}

// A connection's view of the store (scopedMockDb) reads the manager's, and a
// store held to a call's staging (heldScope) too, until the replay staged
// another set under the call: then none, incomplete, so the call's miss is
// redacted by every place.
func TestTheAnsweredRedactingRecordingsAreReadThroughEveryView(t *testing.T) {
	byURL(t)
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.noteAnswered(*redactingRecording("mock-a", models.HTTP, "/a", `{}`, true))
	var scoped integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	if got, complete := integrations.AnsweredRedacting(scoped, models.HTTP); redactingNames(got) != "mock-a" || !complete {
		t.Fatalf("a connection's view read %q (complete %v); want mock-a, complete", redactingNames(got), complete)
	}
	held, restaged := mm.HoldToRound(integrations.NewCallRound(mm.stagingEpoch.Load(), nil))
	if got, complete := integrations.AnsweredRedacting(held, models.HTTP); redactingNames(got) != "mock-a" || !complete || restaged() {
		t.Fatalf("a held store read %q (complete %v, restaged %v); want mock-a, complete", redactingNames(got), complete, restaged())
	}
	mm.bumpStagingEpoch()
	if got, complete := integrations.AnsweredRedacting(held, models.HTTP); len(got) != 0 || complete || !restaged() {
		t.Fatalf("once restaged a held store read %q (complete %v, restaged %v); want none, incomplete", redactingNames(got), complete, restaged())
	}
	// No store, and a wrapper that does not forward the reader, tell no
	// answered recording: read as not complete (every place redacted), so
	// a wrapper leaks nothing by the omission.
	if got, complete := integrations.AnsweredRedacting(nil, models.HTTP); len(got) != 0 || complete {
		t.Fatalf("no store read %d (complete %v); want none, not complete", len(got), complete)
	}
	hiding := struct{ integrations.MockMemDb }{mm}
	if got, complete := integrations.AnsweredRedacting(hiding, models.HTTP); len(got) != 0 || complete {
		t.Fatalf("a wrapper that does not forward read %q (complete %v); want none, not complete", redactingNames(got), complete)
	}
}

// The agent-replacement repair resets the session mid-set
// (ResetForReplaySession, no fence): a recording that redacted something and
// answered before it is still told, the log complete, so a call that repeats
// it after the repair is redacted by its places.
func TestAMidSetResetKeepsTheAnsweredRedactingRecordings(t *testing.T) {
	byURL(t)
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.noteAnswered(*redactingRecording("mock-a", models.HTTP, "/a", `{}`, true))
	mm.ResetForReplaySession()
	got, complete := integrations.AnsweredRedacting(mm, models.HTTP)
	if redactingNames(got) != "mock-a" || !complete {
		t.Fatalf("after a mid-set reset: %q, complete %v; want mock-a, complete", redactingNames(got), complete)
	}
	// The repair serves it again: a copy noted after the reset is the one
	// kept, so resets do not spend the bound on copies.
	mm.noteAnswered(*redactingRecording("mock-a", models.HTTP, "/a", `{}`, true))
	if got, _ := integrations.AnsweredRedacting(mm, models.HTTP); redactingNames(got) != "mock-a" {
		t.Fatalf("a copy noted after the reset: %q; want mock-a once", redactingNames(got))
	}
}

// The log keeps a recording that redacted something once a name and
// content: a copy of it (each test's pool holds one), with its maps built
// in another order or another time, is the one kept; one of the same name
// with another request or other patterns, the same request under another
// name, and one whose content the key does not read (deeper than its walk)
// are kept beside it.
// sameLayoutA and sameLayoutB are two types of one layout.
type sameLayoutA struct{ S string }
type sameLayoutB struct{ S string }

func TestTheAnsweredLogKeepsARedactingRecordingOnceANameAndContent(t *testing.T) {
	byURL(t)
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	a := redactingRecording("mock-a", models.HTTP, "/a", `{"assertion":"REDACTED_abc"}`, true)
	a.Spec.HTTPReq.Header = map[string]string{"Host": "api.example", "Content-Type": "application/json", "X-A": "1"}
	copied := a.DeepCopy()
	reordered := a.ShallowCopy()
	reordered.Spec.HTTPReq = &models.HTTPReq{Method: "GET", URL: "/a", Body: a.Spec.HTTPReq.Body, Header: map[string]string{}}
	for _, k := range []string{"X-A", "Content-Type", "Host"} {
		reordered.Spec.HTTPReq.Header[k] = a.Spec.HTTPReq.Header[k]
	}
	later := a.DeepCopy()
	later.Spec.ReqTimestampMock = later.Spec.ReqTimestampMock.Add(time.Hour)
	otherBody := a.DeepCopy()
	otherBody.Spec.HTTPReq.Body = `{"memo":"REDACTED_abc"}`
	otherPatterns := a.DeepCopy()
	otherPatterns.Noise = []string{`^REDACTED_xyz$`}
	otherHeader := a.DeepCopy()
	otherHeader.Spec.HTTPReq.Header["X-A"] = "2"
	// Two splits of the same pattern bytes are two.
	split1, split2 := a.DeepCopy(), a.DeepCopy()
	split1.Noise, split2.Noise = []string{`^REDACTED_ab`, `c$`}, []string{`^REDACTED_a`, `bc$`}
	otherName := a.DeepCopy()
	otherName.Name = "mock-z"
	for _, m := range []*models.Mock{a, copied, reordered, later, otherBody, otherPatterns, otherHeader, split1, split2, otherName, otherBody.DeepCopy()} {
		mm.noteAnswered(*m)
	}
	got, complete := mm.AnsweredRedacting(models.HTTP)
	if names := redactingNames(got); names != "mock-a,mock-a,mock-a,mock-a,mock-a,mock-a,mock-z" || !complete {
		t.Fatalf("kept %q (complete %v); want mock-a, its other body, patterns, header and the two splits, mock-z, complete", names, complete)
	}
	if got[1].Spec.HTTPReq.Body != otherBody.Spec.HTTPReq.Body || got[2].Noise[0] != otherPatterns.Noise[0] || got[3].Spec.HTTPReq.Header["X-A"] != "2" {
		t.Fatalf("kept %q, %v, %v; want the other body, patterns and header", got[1].Spec.HTTPReq.Body, got[2].Noise, got[3].Spec.HTTPReq.Header)
	}
	// A value deeper than the key reads, in a list or a map, or one of a
	// kind it does not read: no key, kept each time noted.
	var deep any = "REDACTED_abc"
	for i := 0; i < 30; i++ {
		deep = []any{deep}
	}
	for _, msg := range []any{map[string]any{"k": deep}, func() {}} {
		m := redactingRecording("mock-m", models.Mongo, "/m", `{}`, true)
		m.Spec.MongoRequests = []models.MongoRequest{{Message: msg}}
		if _, ok := redactingKeyOf(answeredProbe(m)); ok {
			t.Fatalf("a recording holding a %T the key does not read has a key", msg)
		}
	}
	// One value held as two types of one layout: two.
	for i, msg := range []any{sameLayoutA{S: "q"}, sameLayoutB{S: "q"}} {
		m := redactingRecording("mock-t", models.Mongo, "/t", `{}`, true)
		m.Spec.MongoRequests = []models.MongoRequest{{Message: msg}}
		mm.noteAnswered(*m)
		if got, _ := mm.AnsweredRedacting(models.Mongo); len(got) != i+1 {
			t.Fatalf("a %T after a %T: kept %d; want %d", msg, sameLayoutA{}, len(got), i+1)
		}
	}
	d := redactingRecording("mock-d", models.Mongo, "/d", `{}`, true)
	d.Spec.MongoRequests = []models.MongoRequest{{Message: deep}}
	if _, ok := redactingKeyOf(answeredProbe(d)); ok {
		t.Fatal("a recording deeper than the key reads has a key")
	}
	mm.noteAnswered(*d)
	mm.noteAnswered(*d)
	if got, complete := mm.AnsweredRedacting(models.Mongo); redactingNames(got) != "mock-t,mock-t,mock-d,mock-d" || !complete {
		t.Fatalf("a recording with no key, noted twice: kept %q (complete %v); want it twice", redactingNames(got), complete)
	}
}
