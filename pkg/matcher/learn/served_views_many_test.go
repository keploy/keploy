package learn

import (
	"fmt"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A recording F served many times is read with one view for each answer F
// gave of it (ServedMockLive's key names the answer, never the request): an
// SQS consumer's empty poll, answered from one recorded poll each time the
// application polls more than record saw, its request signed anew on every
// call, is one view however often it is served, the first serve's. A later
// call's new word that no view holds learns (the Mongo insert of the polled
// message, its _id a fresh ObjectID); one that a serve which gave another
// answer holds, whichever serve that was, is refused, naming the recording;
// and where the serves gave more different answers than a set keeps views
// of, every new word that may stand there is refused (fail closed: a false
// red, never a word F gave the application passed as noise).
func TestAPollServedManyTimesIsReadWithOneViewForEachAnswerItGave(t *testing.T) {
	const (
		path  = "bind.$8"
		polls = 64
	)
	rec := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "82"}}}
	live := Shape{Leaves: map[string]Leaf{path: {Path: path, Type: String, Value: "6c7d"}}}
	m := jobsMock(chanceSrcRec, "5")
	view := func(id string) func() (*models.Mock, error) {
		return func() (*models.Mock, error) { return jobsMock(id, "9"), nil }
	}
	invocation := func(i int) string { return fmt.Sprintf("%08x-%04x", uint32(i)*2654435761, i) }
	for _, c := range []struct {
		name    string
		other   int  // the serve that gave another answer, whose view holds the value's word; -1: none
		each    bool // every serve gave an answer of its own
		learned bool
	}{
		{"one answer, no view holds the word", -1, false, true},
		{"the first serve gave another answer, which holds the word", 0, false, false},
		{"a serve past the first few gave another answer, which holds the word", 40, false, false},
		{"the last serve gave another answer, which holds the word", polls - 1, false, false},
		{"every serve gave an answer of its own", -1, true, false},
	} {
		tr := NewTracker()
		tr.Begin()
		for i := 0; i < polls; i++ {
			tr.ServedMock(m)
			key, load := "the empty answer", view(invocation(i))
			switch {
			case i == c.other:
				key, load = "an answer holding the word", view("6c7d")
			case c.each:
				key = invocation(i)
			}
			tr.ServedMockLive(m, key, load)
		}
		p := tr.Placed()
		dc := noSet(clockedDrifts())
		first, last := p.Order.Of(m)
		r := p.Order.ServedView(m, dc.LazyRawOf(m, true, nil).ServedIn(first, last))
		got := ClassifyGiven(dc, []string{path}, live, rec, eRecAt, nil, append(eStartup(), r), nil, nil, nil)
		switch {
		case c.learned && got[0].Kind == KindUnaccounted:
			t.Errorf("%s: refused (%q); want learned: the one answer of the %d serves holds no word of it", c.name, got[0].Why, polls)
		case !c.learned && (got[0].Kind != KindUnaccounted || !strings.Contains(got[0].Why, "mock-6")):
			t.Errorf("FALSE GREEN? %s: %s (%q); want refused naming mock-6", c.name, got[0].Kind, got[0].Why)
		}
	}
}
