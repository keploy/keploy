package learn

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.mongodb.org/mongo-driver/v2/bson"
	"gopkg.in/yaml.v3"
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

// sides is the text a word read stands between, as a test states it; at is
// its ClockWord in rec, built here from those two texts' lengths and
// SHA-256, apart from models.ClockWordOf, so a row checks the learn step's
// positions and hashes, not its own arithmetic.
type sides struct{ before, after string }

func (sd sides) at(t *testing.T, rec string) models.ClockWord {
	t.Helper()
	if !strings.HasPrefix(rec, sd.before) || !strings.HasSuffix(rec, sd.after) || len(rec) <= len(sd.before)+len(sd.after) {
		t.Fatalf("the text of %d bytes holds no word between %d bytes and %d", len(rec), len(sd.before), len(sd.after))
	}
	return models.ClockWord{Start: len(sd.before), End: len(rec) - len(sd.after), After: len(sd.after), BeforeSHA256: sha256Hex(sd.before), AfterSHA256: sha256Hex(sd.after)}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

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
// unstated holds: the clock evidence comes before the word rule. Where the
// word stands is kept, with the length and SHA-256 of the text on each side
// of it (never the text), so that a replay compares the rest. A word read before
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
		word      sides
		res       time.Duration
	}{
		{"run 7's name", name("[E2E] generate-stop-")(wordRec.read), name("[E2E] generate-stop-")(wordF.read), w, read, sides{"[E2E] generate-stop-", ""}, time.Nanosecond},
		{"a word with text after it", "job " + nanosText(wordRec.read) + " queued", "job " + nanosText(wordF.read) + " queued", w, read, sides{"job ", " queued"}, time.Nanosecond},
		{"epoch seconds at their cut", "batch-" + secsText(wordRec.read), "batch-" + secsText(wordF.read), wSecs, read, sides{"batch-", ""}, time.Second},
		{"an ISO time", "at " + wordRec.read.Format(time.RFC3339Nano), "at " + wordF.read.Format(time.RFC3339Nano), w, read, sides{"at ", ""}, time.Nanosecond},
		{"read in its call's window", name("[E2E] generate-stop-")(wordRec.read), name("[E2E] generate-stop-")(wordF.read), inWindow, noise, sides{}, 0},
		{"read before the recording's test began", name("[E2E] generate-stop-")(wordRec.read), name("[E2E] generate-stop-")(wordF.read), lateBegin, refused, sides{}, 0},
		{"the text before the word changed too", name("[E2E] generate-stop-")(wordRec.read), name("[E2E] generate-halt-")(wordF.read), w, refused, sides{}, 0},
		{"a separator changed too", name("[E2E] generate-stop-")(wordRec.read), name("[E2E] generate_stop-")(wordF.read), w, refused, sides{}, 0},
		{"two reads changed", nanosText(wordRec.read) + "-" + nanosText(wordRec.read.Add(time.Millisecond)), nanosText(wordF.read) + "-" + nanosText(wordF.read.Add(time.Millisecond)), w, refused, sides{}, 0},
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
					if ln.Kind != KindClockRead || !ok || !o.Plain() || o.Since != models.SinceTestBegin || o.Word != tc.word.at(t, tc.rec) || o.Res != tc.res {
						t.Fatalf("%q -> %q: %s Read=%+v Word=%+v (%s); want a read of the word since its test began at %v, around %+v", tc.rec, tc.live, ln.Kind, ln.Read, o.Word, ln.Why, tc.res, tc.word)
					}
					if ln.Recorded.Value != tc.rec || ln.Live.Value != tc.live {
						t.Fatalf("learned the leaf as %q -> %q, want the whole text", ln.Recorded.Value, ln.Live.Value)
					}
					// It names where the word stands, never the text.
					at := fmt.Sprintf("word at bytes %d-%d of the text as recorded, with %d bytes after it", len(tc.word.before), len(tc.rec)-len(tc.word.after), len(tc.word.after))
					if s := o.String(); strings.Contains(s, "generate") || strings.Contains(s, "queued") || !strings.Contains(s, at) {
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
	word := func(rec string, start, end int) models.ClockOffset {
		o := whole
		o.Word = models.ClockWordOf(rec, start, end)
		return o
	}
	for _, tc := range []struct {
		name  string
		a, b  models.ClockOffset
		empty bool
	}{
		{"both whole", whole, whole, false},
		{"one word of one text", word("[E2E] a-123", 8, 11), word("[E2E] a-123", 8, 11), false},
		{"whole and a word", whole, word("[E2E] a-123", 8, 11), true},
		{"two places of a text", word("[E2E] a-123", 8, 11), word("[E2E] 123-a", 6, 9), true},
		{"one place of two texts", word("[E2E] a-123", 8, 11), word("[E2E] b-123", 8, 11), true},
		{"one place, another text after it", word("[E2E] a-123-x", 8, 11), word("[E2E] a-123-y", 8, 11), true},
	} {
		for _, m := range []models.ClockOffset{tc.a.Meet(tc.b), tc.b.Meet(tc.a)} {
			if m.Empty() != tc.empty {
				t.Fatalf("%s: Meet = %+v (Empty %v), want Empty %v", tc.name, m, m.Empty(), tc.empty)
			}
			if !tc.empty && m.Word != tc.a.Word {
				t.Fatalf("%s: Meet's Word = %+v, want %+v", tc.name, m.Word, tc.a.Word)
			}
		}
	}
}

// wordReplay is a replay of run 7's TestStopGeneration 90 s after record's
// full round: its test's clock read for the app's name 0.7 ms after the
// test began, as in both learned runs.
var wordReplay = wordSinceRun(wordF.began.Add(90*time.Second), wordF.read.Add(90*time.Second))

// A replay checks a read learned as one word of a text
// (models.ClockOffset.Word) as that word alone: the text this call carries
// must be the recorded text before the word, then a read of the clock made
// after this test began and no later than the call, then the recorded text
// after it. Any other byte of the text changed is refused, on either side of
// the word, and so is a word that is no such read; a refusal names the path
// and where the word stands, never the text. A keploy that does not read
// Word checks the whole text as one read, which it is not: it refuses it,
// so a file learned here fails closed there.
func TestAReplayReadsAClockWordAloneAndComparesTheRestOfItsText(t *testing.T) {
	const p = "body.cmd.query.app.name"
	str := func(v string) Leaf { return Leaf{Path: p, Noise: p, Type: String, Value: v} }
	shape := func(v string) Shape { return Shape{Leaves: map[string]Leaf{p: str(v)}} }
	secs := func(r earlierReadRun) Span {
		return Span{From: r.read.Add(3 * time.Second), To: r.read.Add(3200 * time.Millisecond), Began: r.began}
	}
	span := func(r earlierReadRun) Span { return Span{From: r.prev, To: r.call, Began: r.began} }
	type text func(time.Time) string
	around := func(before string, word func(time.Time) string, after string) text {
		return func(r time.Time) string { return before + word(r) + after }
	}
	iso := func(r time.Time) string { return r.Format(time.RFC3339Nano) }
	for _, lc := range []struct {
		name          string
		value         text
		before, after string
		w             *Window
		at            func(earlierReadRun) Span
	}{
		{"run 7's name", around("[E2E] generate-stop-", nanosText, ""), "[E2E] generate-stop-", "", earlierReadWindow(wordRec, wordF), span},
		{"a word with text after it", around("job ", nanosText, " queued"), "job ", " queued", earlierReadWindow(wordRec, wordF), span},
		{"epoch seconds at their cut", around("batch-", secsText, ""), "batch-", "", &Window{Rec: secs(wordRec), F: secs(wordF)}, secs},
		{"an ISO time", around("at ", iso, " UTC"), "at ", " UTC", earlierReadWindow(wordRec, wordF), span},
	} {
		ln := ClassifyFollowing(clientTestCache(), []string{p}, shape(lc.value(wordF.read)), shape(lc.value(wordRec.read)), wordRec.call, nil, nil, nil, lc.w, nil)[0]
		o, ok := ln.Clock()
		if ln.Kind != KindClockRead || !ok || o.Word != (sides{lc.before, lc.after}).at(t, lc.value(wordRec.read)) {
			t.Fatalf("%s: learned %s Read=%+v (%s); want a read of the word", lc.name, ln.Kind, ln.Read, ln.Why)
		}
		s := lc.at(wordReplay)
		word := lc.value(wordReplay.read)[len(lc.before) : len(lc.value(wordReplay.read))-len(lc.after)]
		// flip changes the byte at i of the text this replay carries, the
		// text's length kept, so the word stands where it did.
		flip := func(i int) string {
			b := []byte(lc.value(wordReplay.read))
			b[i] ^= 0x01
			return string(b)
		}
		// The word alone is too short to hold the text on both sides of it
		// apart where that text is longer than the word; else its first
		// bytes are not the text before it.
		alone := "before that word"
		if len(word) <= len(lc.before)+len(lc.after) {
			alone = "too short"
		}
		rows := []struct {
			name, live string
			typed      bool
			ok         bool
			why        string
		}{
			{"the word read since this test began, the rest kept", lc.value(wordReplay.read), false, true, ""},
			{"the recording's own word, read before this test began", lc.value(wordRec.read), false, false, "it changed"},
			{"the text before the word changed", flip(0), false, false, "before that word"},
			{"the last byte before the word changed", flip(len(lc.before) - 1), false, false, "before that word"},
			{"text after a word that had none", lc.value(wordReplay.read) + "x", false, false, "it changed"},
			{"the word alone", word, false, false, alone},
			{"a typed instant", lc.value(wordReplay.read), true, false, "no text"},
		}
		if lc.after != "" {
			rows = append(rows,
				struct {
					name, live string
					typed      bool
					ok         bool
					why        string
				}{"the text after the word changed", flip(len(lc.value(wordReplay.read)) - 1), false, false, "after that word"},
				struct {
					name, live string
					typed      bool
					ok         bool
					why        string
				}{"the first byte after the word changed", flip(len(lc.value(wordReplay.read)) - len(lc.after)), false, false, "after that word"})
		}
		for _, r := range rows {
			live := str(r.live)
			if r.typed {
				live = InstantLeaf(p, p, wordReplay.read, time.Nanosecond)
			}
			got, why := CheckClockRead(p, o, live, s, true)
			if got != r.ok || !r.ok && (!strings.HasPrefix(why, p+" was learned as ") || !strings.Contains(why, r.why)) {
				t.Errorf("%s, %s: CheckClockRead = %v (%s); want %v, saying %q", lc.name, r.name, got, why, r.ok, r.why)
			}
			for _, part := range []string{lc.before, lc.after, word} {
				if len(part) > 4 && strings.Contains(why, part) {
					t.Errorf("%s, %s: the refusal quotes the text (%q): %s", lc.name, r.name, part, why)
				}
			}
		}
		// A keploy that does not read Word reads the whole text as one
		// read: it refuses it whatever it is.
		old := o
		old.Word = models.ClockWord{}
		if got, why := CheckClockRead(p, old, str(lc.value(wordReplay.read)), s, true); got {
			t.Errorf("%s: a reader of no Word passed the whole text (%s); want it refused", lc.name, why)
		}
		// A replay that knows no begin of its test refuses the word as it
		// does a whole read since its test began.
		if got, _ := CheckClockRead(p, o, str(lc.value(wordReplay.read)), Span{From: s.From, To: s.To}, true); got {
			t.Errorf("%s: passed with no begin known; want it refused", lc.name)
		}
	}
}

// A text too short to hold the recorded text before and after the word
// apart (the two would overlap), or one that holds them with no word
// between, is refused, never read out of range; and so is a text at a word
// whose place is no place a text has (models.ClockWord.Valid: a hand edit
// or a cut copy of the learned file), whatever the text is.
func TestAClockWordReplayRefusesATextThatHoldsNoWord(t *testing.T) {
	const p = "name"
	good := sides{"x-", "-y"}.at(t, "x-"+nanosText(wordRec.read)+"-y")
	o := models.ClockOffset{Res: time.Nanosecond, Since: models.SinceTestBegin, Word: good}
	s := Span{From: wordReplay.prev, To: wordReplay.call, Began: wordReplay.began}
	live := "x-" + nanosText(wordReplay.read) + "-y"
	if got, why := CheckClockRead(p, o, Leaf{Path: p, Type: String, Value: live}, s, true); !got {
		t.Fatalf("the replay's own read between the recorded sides was refused: %s", why)
	}
	for _, tc := range []struct{ name, live, why string }{
		{"overlapping", "x-y", "too short"},
		{"no word between", "x--y", "too short"},
		{"empty", "", "too short"},
	} {
		if got, why := CheckClockRead(p, o, Leaf{Path: p, Type: String, Value: tc.live}, s, true); got || !strings.Contains(why, tc.why) {
			t.Errorf("%s: CheckClockRead = %v (%s); want it refused, saying %q", tc.name, got, why, tc.why)
		}
	}
	for _, tc := range []struct {
		name string
		edit func(*models.ClockWord)
	}{
		{"a negative start", func(w *models.ClockWord) { w.Start = -1 }},
		{"an end at its start", func(w *models.ClockWord) { w.End = w.Start }},
		{"a negative after", func(w *models.ClockWord) { w.After = -1 }},
		{"no text on either side", func(w *models.ClockWord) { w.Start, w.After = 0, 0 }},
		{"a cut hash before", func(w *models.ClockWord) { w.BeforeSHA256 = w.BeforeSHA256[:63] }},
		{"an upper-case hash after", func(w *models.ClockWord) { w.AfterSHA256 = strings.ToUpper(w.AfterSHA256) }},
		{"no hash before", func(w *models.ClockWord) { w.BeforeSHA256 = "" }},
	} {
		bad := o
		tc.edit(&bad.Word)
		if got, why := CheckClockRead(p, bad, Leaf{Path: p, Type: String, Value: live}, s, true); got || !strings.Contains(why, "no place a text has") {
			t.Errorf("%s: CheckClockRead = %v (%s); want it refused as no place of a text", tc.name, got, why)
		}
	}
}

// A word read inside a long text (a MiB on each side of it, which may hold
// a secret) is learned in the same few bytes as one in a short text: the
// learned read holds the word's place and the length and SHA-256 of each
// side, never a byte of the text, in json, yaml and bson. A replay still
// compares every byte of both sides: one byte changed in the middle of
// either, the length kept, is refused, and the replay's own read with both
// sides kept passes.
func TestAClockWordInAMiBTextIsLearnedInFewBytesAndStillComparesEveryByte(t *testing.T) {
	const p = "body.note"
	str := func(v string) Shape {
		return Shape{Leaves: map[string]Leaf{p: {Path: p, Noise: p, Type: String, Value: v}}}
	}
	before := "token=" + strings.Repeat("s", 1<<20) + " stop-"
	after := " " + strings.Repeat("y", 1<<20)
	text := func(r time.Time) string { return before + nanosText(r) + after }
	rec := text(wordRec.read)
	ln := ClassifyFollowing(clientTestCache(), []string{p}, str(text(wordF.read)), str(rec), wordRec.call, nil, nil, nil, earlierReadWindow(wordRec, wordF), nil)[0]
	o, ok := ln.Clock()
	if ln.Kind != KindClockRead || !ok || o.Since != models.SinceTestBegin || o.Word != (sides{before, after}).at(t, rec) {
		t.Fatalf("learned %s Read=%+v (%s); want a read of the word since its test began", ln.Kind, ln.Read, ln.Why)
	}
	short := models.ClockOffset{Res: o.Res, Layout: o.Layout, Since: o.Since, Word: sides{"stop-", ""}.at(t, "stop-"+nanosText(wordRec.read))}
	for _, e := range []struct {
		name    string
		marshal func(any) ([]byte, error)
	}{{"json", json.Marshal}, {"yaml", yaml.Marshal}, {"bson", bson.Marshal}} {
		b, err := e.marshal(o)
		if err != nil {
			t.Fatal(err)
		}
		sb, err := e.marshal(short)
		if err != nil {
			t.Fatal(err)
		}
		// The positions of a MiB text take a few more digits, no more.
		if len(b) > len(sb)+32 || strings.Contains(string(b), "ssss") || strings.Contains(string(b), "yyyy") || strings.Contains(string(b), "token") {
			t.Fatalf("%s: the learned read of a word in a %d-byte text is %d bytes (%d for a short text), or holds its text", e.name, len(rec), len(b), len(sb))
		}
	}
	s := Span{From: wordReplay.prev, To: wordReplay.call, Began: wordReplay.began}
	live := text(wordReplay.read)
	flip := func(i int) string {
		b := []byte(live)
		b[i] ^= 0x01
		return string(b)
	}
	for _, r := range []struct {
		name, live string
		ok         bool
		why        string
	}{
		{"both sides kept", live, true, ""},
		{"a byte in the middle of the text before changed", flip(len(before) / 2), false, "before that word"},
		{"a byte in the middle of the text after changed", flip(len(live) - len(after)/2), false, "after that word"},
	} {
		got, why := CheckClockRead(p, o, Leaf{Path: p, Noise: p, Type: String, Value: r.live}, s, true)
		if got != r.ok || !r.ok && !strings.Contains(why, r.why) || len(why) > 1024 {
			t.Errorf("%s: CheckClockRead = %v (%.300s); want %v, saying %q", r.name, got, why, r.ok, r.why)
		}
	}
}
