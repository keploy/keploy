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
// that answered and holds a pattern, once a name, its kind's own: whatever
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
	if names := redactingNames(got); names != "mock-a,mock-b" || !complete {
		t.Fatalf("HTTP kept %q (complete %v); want mock-a,mock-b, complete", names, complete)
	}
	if got[0].Spec.HTTPReq.URL != "/a" || got[0].Spec.HTTPResp != nil || len(got[0].Noise) != 1 {
		t.Fatalf("kept %+v; want the first noted's request and patterns, and no response", got[0].Spec)
	}
	if got, complete := mm.AnsweredRedacting(models.Mongo); redactingNames(got) != "mock-k" || !complete {
		t.Fatalf("Mongo kept %q (complete %v); want mock-k, complete", redactingNames(got), complete)
	}
}

// What the log keeps apart is bounded: past redactingMaxBytes a recording is
// not kept, and its kind is told incomplete, another kind not. A new test
// set (fence) and a reset forget them all, incomplete or not.
func TestTheAnsweredRedactingRecordingsAreBounded(t *testing.T) {
	byURL(t)
	for _, forget := range []struct {
		name string
		do   func(mm *MockManager)
	}{
		{"fence", func(mm *MockManager) {
			mm.swapMu.Lock()
			defer mm.swapMu.Unlock()
			mm.dropPreviousSetConsumes()
		}},
		{"reset", func(mm *MockManager) { mm.answered.reset() }},
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
