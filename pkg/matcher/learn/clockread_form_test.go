package learn

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A value is of a learned read's form when it reads as an instant (a time
// of day, at a place learned as one) in the learned layout, the word of a
// word read with the bytes beside it the recorded text's, whatever instant
// it names: a stale one is, and whether it holds is the clock check's. A
// word that is no instant, an instant in another layout, or a text whose
// bytes beside the word changed is not.
func TestReadsAsClockReadIsTheLearnedForm(t *testing.T) {
	const text = "user-1730000000000"
	word := models.ClockWordOf(text, 5, len(text))
	for _, c := range []struct {
		name  string
		o     models.ClockOffset
		value string
		want  bool
	}{
		{"a stale instant, no layout learned", models.ClockOffset{Res: time.Second}, "2026-09-01T09:00:00Z", true},
		{"no instant, no layout learned", models.ClockOffset{Res: time.Second}, "me", false},
		{"a stale instant in the learned layout", models.ClockOffset{Res: time.Millisecond, Layout: "epoch-ms"}, "1730000000999", true},
		{"an instant in another layout", models.ClockOffset{Res: time.Millisecond, Layout: "epoch-ms"}, "1730000000", false},
		{"no instant where a layout was learned", models.ClockOffset{Res: time.Millisecond, Layout: "epoch-ms"}, "me", false},
		{"an instant where more than one layout was learned", models.ClockOffset{Res: time.Second, Layout: models.LayoutApart}, "1730000000", true},
		{"a time of day in the learned layout", models.ClockOffset{Res: time.Second, Layout: "time-second"}, "09:00:00", true},
		{"a time of day in another layout", models.ClockOffset{Res: time.Second, Layout: "time-second"}, "09:00", false},
		{"no time of day", models.ClockOffset{Res: time.Second, Layout: "time-second"}, "me", false},
		{"a stale word, the text beside it the recorded one", models.ClockOffset{Res: time.Millisecond, Layout: "epoch-ms", Word: word}, "user-1730000000999", true},
		{"no instant as the word", models.ClockOffset{Res: time.Millisecond, Layout: "epoch-ms", Word: word}, "user-me", false},
		{"the text before the word changed", models.ClockOffset{Res: time.Millisecond, Layout: "epoch-ms", Word: word}, "team-1730000000999", false},
	} {
		if got := ReadsAsClockRead(c.o, Leaf{Path: "url.path.2", Noise: "url.path.2", Type: String, Value: c.value}); got != c.want {
			t.Errorf("%s: ReadsAsClockRead(%q) = %v; want %v", c.name, c.value, got, c.want)
		}
	}
}
