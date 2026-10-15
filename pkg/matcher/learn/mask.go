package learn

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// maxMaskedValue is the longest recorded value the learn step masks
// (ClockMask): a mask holds the whole recorded value, so a longer one is
// learned whole instead.
const maxMaskedValue = 4096

// ClockMask is the mask of a value that drifted only by F's clock (founder
// decision 26): an anchored pattern of the recorded value in which each
// clock read that moved by FullRoundLead (movedReads) stands as any read of
// its layout, and everything else as it was recorded, so that what did not
// drift ("order 1234 placed at <ts>") is still compared. A layout read by
// its words (a time-ordered UUID) masks its time words alone. It is "" when
// the value drifted in more than its clock reads (the live value is not the
// recorded one outside them), when a clock read moved only in another form
// of the value (an escaped or encoded one), and for a recorded value longer
// than maxMaskedValue: the value is then learned whole.
func ClockMask(recorded, live string) string {
	if len(recorded) > maxMaskedValue {
		return ""
	}
	reads := movedReads(recorded, live)
	if len(reads) == 0 {
		return ""
	}
	sort.Slice(reads, func(i, j int) bool { return reads[i].rec[0] < reads[j].rec[0] })
	var b strings.Builder
	b.WriteString("^")
	at := 0
	for _, m := range reads {
		b.WriteString(regexp.QuoteMeta(recorded[at:m.rec[0]]))
		b.WriteString(spanPattern(clockLayouts[m.lay], recorded[m.rec[0]:m.rec[1]]))
		at = m.rec[1]
	}
	b.WriteString(regexp.QuoteMeta(recorded[at:]))
	b.WriteString("$")
	mask := b.String()
	re := compiledMask(mask)
	if re == nil || !re.MatchString(recorded) || !re.MatchString(live) {
		return ""
	}
	return mask
}

// spanPattern is the pattern a clock read of layout lay, recorded as span,
// stands as in a mask: any read of the layout (whatever its precision or
// zone: a formatter that drops a fraction's trailing zeros reads the clock
// all the same), or, for a layout read by its words, the span with each time
// word as any hexadecimal word of its length and the other words as
// recorded.
func spanPattern(lay clockLayout, span string) string {
	if lay.timeWord == nil {
		return "(?:" + lay.re.String() + ")"
	}
	var b strings.Builder
	i := 0
	for _, seg := range segmentsBy(span, isWord) {
		switch {
		case !seg.word:
			b.WriteString(regexp.QuoteMeta(seg.text))
			continue
		case lay.timeWord(span, i):
			b.WriteString("[0-9a-fA-F]{" + strconv.Itoa(len(seg.text)) + "}")
		default:
			b.WriteString(regexp.QuoteMeta(seg.text))
		}
		i++
	}
	return b.String()
}

// Masked reports whether noise learned at a leaf with patterns excuses its
// value: any value when it lists none (the leaf learned whole), else a
// scalar's text that one of them matches (a clock read's mask, founder
// decision 26). A pattern that does not compile excuses nothing.
func Masked(patterns []string, value string, scalar bool) bool {
	if len(patterns) == 0 {
		return true
	}
	if !scalar {
		return false
	}
	for _, p := range patterns {
		if re := compiledMask(p); re != nil && re.MatchString(value) {
			return true
		}
	}
	return false
}

// MergeMasks merges noise learned onto a mock (existing) with noise learned
// again (detected), each a leaf's patterns (Masked): a leaf learned whole on
// either side stays learned whole, and a leaf masked on both is excused by
// either side's patterns. Neither input is changed, and the result shares no
// slice with them.
func MergeMasks(existing, detected map[string][]string) map[string][]string {
	out := make(map[string][]string, len(existing)+len(detected))
	for k, v := range existing {
		out[k] = append([]string{}, v...)
	}
	for k, v := range detected {
		have, ok := out[k]
		switch {
		case !ok:
			out[k] = append([]string{}, v...)
		case len(have) == 0:
		case len(v) == 0:
			out[k] = []string{}
		default:
			for _, p := range v {
				dup := false
				for _, h := range have {
					if h == p {
						dup = true
						break
					}
				}
				if !dup {
					have = append(have, p)
				}
			}
			out[k] = have
		}
	}
	return out
}

// maskCache holds the masks compiled so far (nil for one that does not
// compile); it is emptied when it grows past maxCachedMasks.
var (
	maskMu    sync.Mutex
	maskCache = map[string]*regexp.Regexp{}
)

const maxCachedMasks = 4096

func compiledMask(p string) *regexp.Regexp {
	maskMu.Lock()
	re, ok := maskCache[p]
	maskMu.Unlock()
	if ok {
		return re
	}
	re, err := regexp.Compile(p)
	if err != nil {
		re = nil
	}
	maskMu.Lock()
	if len(maskCache) >= maxCachedMasks {
		maskCache = map[string]*regexp.Regexp{}
	}
	maskCache[p] = re
	maskMu.Unlock()
	return re
}
