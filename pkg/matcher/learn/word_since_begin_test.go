package learn

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// wordSinceRun is one run of run 7's TestStopGeneration (api-server's e2e
// suite under keploy mock): the test's own clock read for an app's name,
// "[E2E] generate-stop-<UnixNano>" (tests/framework/e2e/fixtures.go), made
// 0.7 ms after the test began, then the session look-up the app makes (the
// previous data-plane call) and the call that sends the name to Mongo.
func wordSinceRun(began, read time.Time) earlierReadRun {
	return earlierReadRun{began: began, read: read, prev: read.Add(100 * time.Millisecond), call: read.Add(250 * time.Millisecond)}
}

var (
	// The recording, and record's full round 91.633 s later (its clock
	// never goes back, so the test began past its recorded begin plus the
	// lead): the two reads run 7 carried.
	wordRec = wordSinceRun(time.Unix(0, 1793336961101925882).UTC(), time.Unix(0, 1793336961102639022).UTC())
	wordF   = wordSinceRun(time.Unix(0, 1793337052734912528).UTC(), time.Unix(0, 1793337052735625668).UTC())
)

// untoldServe is mock-4565 of run 7: another test's recording F served
// before the call (an SQS poll) whose adapter did not say what it gave the
// application there, so the word rule refuses any word the value took in F
// that may stand in it (servedWhy).
func untoldServe(at time.Time) []Raw {
	sqs := &models.Mock{Name: "mock-4565", Kind: models.HTTP, Spec: models.MockSpec{
		Metadata:         map[string]string{"type": "HTTP_CLIENT"},
		HTTPReq:          &models.HTTPReq{Method: "POST", URL: "http://localhost:9324/", Body: `Action=ReceiveMessage&QueueUrl=q&WaitTimeSeconds=20`, Header: map[string]string{}},
		HTTPResp:         &models.HTTPResp{StatusCode: 200, Header: map[string]string{}, Body: `{"Messages":[]}`},
		ReqTimestampMock: at, ResTimestampMock: at.Add(time.Millisecond),
	}}
	return []Raw{RawOf(sqs, true).ServedIn(1, 1).OfAnotherTest()}
}

// In keploy mock's client tests, a text whose one changed word is a read
// of the run's clock made after its test began and before its call's
// window, in both runs (the test's own clock read, which keploy's launcher
// puts on the run's clock as it does the application's), is learned as a
// read of that word since its test began (models.ClockOffset.Word), never
// as noise and never refused, whatever another test's recording F served
// unstated holds: the clock evidence comes before the word rule. The text
// around the word is kept so that a replay compares it. A word read before
// the test began, a text whose words around the read changed too, or one
// with two changed reads, stay refused; and a word read in its call's own
// window stays the word noise it was, served unstated or not.
func TestAClientTestLearnsAWordOfATextReadSinceItsTestBegan(t *testing.T) {
	const p = "body.cmd.query.app.name"
	name := func(prefix string) func(time.Time) string {
		return func(r time.Time) string { return fmt.Sprintf("%s%d", prefix, r.UnixNano()) }
	}
	str := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	w := earlierReadWindow(wordRec, wordF)
	inWindow := &Window{Rec: Span{From: wordRec.read.Add(-100 * time.Millisecond), To: wordRec.call, Began: wordRec.began}, F: Span{From: wordF.read.Add(-100 * time.Millisecond), To: wordF.call, Began: wordF.began}}
	// A read cut to the second, made 3 s before the previous call: a
	// window of the call alone does not hold it within its cut.
	secs := func(r earlierReadRun) Span {
		return Span{From: r.read.Add(3 * time.Second), To: r.read.Add(3200 * time.Millisecond), Began: r.began}
	}
	wSecs := &Window{Rec: secs(wordRec), F: secs(wordF)}
	lateBegin := &Window{Rec: Span{From: wordRec.prev, To: wordRec.call, Began: wordRec.read.Add(time.Millisecond)}, F: w.F}
	type want int
	const (
		read want = iota
		noise
		refused
	)
	for _, tc := range []struct {
		name      string
		rec, live string
		w         *Window
		want      want
		word      models.ClockWord
		res       time.Duration
	}{
		{"run 7's name", name("[E2E] generate-stop-")(wordRec.read), name("[E2E] generate-stop-")(wordF.read), w, read, models.ClockWord{Before: "[E2E] generate-stop-"}, time.Nanosecond},
		{"a word with text after it", "job " + nanosText(wordRec.read) + " queued", "job " + nanosText(wordF.read) + " queued", w, read, models.ClockWord{Before: "job ", After: " queued"}, time.Nanosecond},
		{"epoch seconds at their cut", "batch-" + secsText(wordRec.read), "batch-" + secsText(wordF.read), wSecs, read, models.ClockWord{Before: "batch-"}, time.Second},
		{"an ISO time", "at " + wordRec.read.Format(time.RFC3339Nano), "at " + wordF.read.Format(time.RFC3339Nano), w, read, models.ClockWord{Before: "at "}, time.Nanosecond},
		{"read in its call's window", name("[E2E] generate-stop-")(wordRec.read), name("[E2E] generate-stop-")(wordF.read), inWindow, noise, models.ClockWord{}, 0},
		{"read before the recording's test began", name("[E2E] generate-stop-")(wordRec.read), name("[E2E] generate-stop-")(wordF.read), lateBegin, refused, models.ClockWord{}, 0},
		{"the text before the word changed too", name("[E2E] generate-stop-")(wordRec.read), name("[E2E] generate-halt-")(wordF.read), w, refused, models.ClockWord{}, 0},
		{"a separator changed too", name("[E2E] generate-stop-")(wordRec.read), name("[E2E] generate_stop-")(wordF.read), w, refused, models.ClockWord{}, 0},
		{"two reads changed", nanosText(wordRec.read) + "-" + nanosText(wordRec.read.Add(time.Millisecond)), nanosText(wordF.read) + "-" + nanosText(wordF.read.Add(time.Millisecond)), w, refused, models.ClockWord{}, 0},
	} {
		for _, untold := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, served unstated %v", tc.name, untold), func(t *testing.T) {
				var raw []Raw
				if untold {
					raw = untoldServe(wordRec.read.Add(-time.Second))
				}
				ln := ClassifyFollowing(clientTestCache(), []string{p}, str(tc.live), str(tc.rec), wordRec.call, nil, raw, nil, tc.w, nil)[0]
				o, ok := ln.Clock()
				switch tc.want {
				case read:
					if ln.Kind != KindClockRead || !ok || !o.Plain() || o.Since != models.SinceTestBegin || o.Word == nil || *o.Word != tc.word || o.Res != tc.res {
						t.Fatalf("%q -> %q: %s Read=%+v Word=%+v (%s); want a read of the word since its test began at %v, around %+v", tc.rec, tc.live, ln.Kind, ln.Read, o.Word, ln.Why, tc.res, tc.word)
					}
					if ln.Recorded.Value != tc.rec || ln.Live.Value != tc.live {
						t.Fatalf("learned the leaf as %q -> %q, want the whole text", ln.Recorded.Value, ln.Live.Value)
					}
					// It names where the word stands, never the text.
					if s := o.String(); strings.Contains(s, "generate") || strings.Contains(s, "queued") || !strings.Contains(s, "word after the text's first") {
						t.Fatalf("String() = %q; want where the word stands, and no text of the value", s)
					}
				case noise:
					if ln.Kind != KindNoise || ln.Read != nil {
						t.Fatalf("%q -> %q: %s Read=%+v (%s); want the word noise a read in its call's window is", tc.rec, tc.live, ln.Kind, ln.Read, ln.Why)
					}
				case refused:
					if ln.Kind != KindUnaccounted || ok {
						t.Fatalf("%q -> %q: %s Read=%+v (%s); want it refused", tc.rec, tc.live, ln.Kind, ln.Read, ln.Why)
					}
				}
			})
		}
	}
}

// Two learnings of one place meet as one read only where both read the
// same word of the same text, or both read the value whole: a read whole
// and one inside a text, or two at different places of it, were written in
// two forms, and their Meet holds nothing (Empty), in either order.
func TestClockWordsMeetOnlyWhereTheyAreOneWord(t *testing.T) {
	whole := models.ClockOffset{Res: time.Nanosecond}
	word := func(before, after string) models.ClockOffset {
		o := whole
		o.Word = &models.ClockWord{Before: before, After: after}
		return o
	}
	for _, tc := range []struct {
		name  string
		a, b  models.ClockOffset
		empty bool
	}{
		{"both whole", whole, whole, false},
		{"one word of one text", word("[E2E] a-", ""), word("[E2E] a-", ""), false},
		{"whole and a word", whole, word("[E2E] a-", ""), true},
		{"two places of a text", word("[E2E] a-", ""), word("[E2E] ", "-a"), true},
	} {
		for _, m := range []models.ClockOffset{tc.a.Meet(tc.b), tc.b.Meet(tc.a)} {
			if m.Empty() != tc.empty {
				t.Fatalf("%s: Meet = %+v (Empty %v), want Empty %v", tc.name, m, m.Empty(), tc.empty)
			}
			if !tc.empty && (m.Word == nil) != (tc.a.Word == nil) || !tc.empty && m.Word != nil && *m.Word != *tc.a.Word {
				t.Fatalf("%s: Meet's Word = %+v, want %+v", tc.name, m.Word, tc.a.Word)
			}
		}
	}
}
