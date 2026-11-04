package learn

import (
	"fmt"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A recording F served many times, each serve told with a view of its own,
// is read with every view: an SQS consumer's empty poll, answered from one
// recorded poll each time the application polls more than record saw, whose
// request carries a fresh SDK invocation id, date and signature on every
// call. A later call's new word that no view holds learns (the Mongo insert
// of the polled message, its _id a fresh ObjectID); one that a view holds,
// whichever serve gave it, is refused, naming the recording (fail closed: a
// false red, never a word F gave the application passed as noise).
func TestAPollServedManyTimesIsReadWithEveryViewItsServesGave(t *testing.T) {
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
		holding int // the serve whose view holds the value's word; -1: none
	}{
		{"no view holds the word", -1},
		{"the first view holds the word", 0},
		{"a view past the first few holds the word", 40},
		{"the last view holds the word", polls - 1},
	} {
		tr := NewTracker()
		tr.Begin()
		for i := 0; i < polls; i++ {
			tr.ServedMock(m)
			load := view(invocation(i))
			if i == c.holding {
				load = view("6c7d")
			}
			tr.ServedMockLive(m, invocation(i), load)
		}
		p := tr.Placed()
		dc := noSet(clockedDrifts())
		first, last := p.Order.Of(m)
		r := p.Order.ServedView(m, dc.LazyRawOf(m, true, nil).ServedIn(first, last))
		got := ClassifyGiven(dc, []string{path}, live, rec, eRecAt, nil, append(eStartup(), r), nil, nil, nil)
		switch {
		case c.holding < 0 && got[0].Kind == KindUnaccounted:
			t.Errorf("%s: refused (%q); want learned: no view of the %d serves holds it", c.name, got[0].Why, polls)
		case c.holding >= 0 && (got[0].Kind != KindUnaccounted || !strings.Contains(got[0].Why, "mock-6")):
			t.Errorf("FALSE GREEN? %s: %s (%q); want refused naming mock-6", c.name, got[0].Kind, got[0].Why)
		}
	}
}
