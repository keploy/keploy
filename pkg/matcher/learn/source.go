package learn

import (
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// WordFrom is the word rule's reading (unaccounted) of whether a value of a
// test that changed between its recording and this run (recorded, live: its
// response, or a request it sent) may have come from m, a recording the
// application made a call for outside the test: the first word of what the
// value changed (eachChanged: the words it lost and gained, and the words of
// its runs, in every reading, clock words left out) that m's payload holds as
// the word rule searches an earlier recording (searchedTokens: in every
// declared form, its answer's stamps and own clock reads blanked), and that
// no text of given holds (what the test was given otherwise: its request,
// its own recordings' answers). keploy cannot tell whether such a word came
// from m, so the call is the test's (keploy test's dataflow guard), as the
// word rule refuses to learn the value as noise. A word the value did not
// change ("ok", "false" at their own places) is no word of what changed, and
// reaches nothing: one definition decides "this value came from that answer"
// in a learn step and in keploy test. "" for none; err is why m's answer
// cannot be read back.
func (dc *DriftCache) WordFrom(recorded, live string, m *models.Mock, given ...string) (string, error) {
	return dc.WordFromIn(recorded, live, nil, m, given...)
}

// WordFromIn is WordFrom of a value whose clock reads are read in w too,
// the window of what carries it in each run (nil: by the lead alone, as
// WordFrom): an instant that falls in its run's span in both runs, at the
// same place of the value (the k-th of its layout in each, as clockAt pairs
// them), is a read of each run's clock, and is no change there
// (Window.inPlace). Its words are no clock words elsewhere: the rest of the
// value is read by the lead alone, as WordFrom reads it. keploy test's
// dataflow guard reads a test's response so, in the span from the test's
// request to its answer, recorded and this run: the application stamps its
// answer with its clock (an HTTP Date header), and this run's stamp is no
// word an earlier recording gave. Read by the lead alone, the stamp's minute
// or second was a word of what changed, and a start-up answer that held the
// same number by chance (its length, a version) was taken as reaching the
// test. Read as clock words of the whole value, the stamp's words hid the
// same number lost from the response's body, which may have come from the
// start-up answer.
func (dc *DriftCache) WordFromIn(recorded, live string, w *Window, m *models.Mock, given ...string) (string, error) {
	live = w.inPlace(recorded, live)
	if m == nil || recorded == live {
		return "", nil
	}
	x, err := dc.ReadRaw(m, false)
	if err != nil {
		return "", err
	}
	src := dc.searchedTokens(x, false)
	held := make([]map[string]bool, len(readings))
	for _, g := range given {
		if g == "" {
			continue
		}
		for i, toks := range dc.rawTokens(g) {
			if i >= len(held) {
				break
			}
			for t := range toks {
				if held[i] == nil {
					held[i] = map[string]bool{}
				}
				held[i][t] = true
			}
		}
	}
	return dc.ofIn(recorded, live, nil, time.Time{}).eachChanged(func(i int, t string, _ bool) string {
		if i < len(src) && src[i][t] && !held[i][t] {
			return t
		}
		return ""
	}), nil
}
