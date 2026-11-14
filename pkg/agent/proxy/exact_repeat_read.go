package proxy

import (
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/models"
)

// A REPEATED READ: ONE MORE OR ONE FEWER THAN RECORDED.
//
// Exact serving answers each of an open test's own recordings at most once:
// a call one more than the test recorded misses (models.MissExtraCall), and
// a recording the test did not make is charged to it at its end. A replay
// may name a test's repeated read (models.ScopeTableReq.RepeatReads,
// models.TestModeInfo.RepeatRead on each of its recordings): the test
// recorded that read at least twice, every time with one answer. An app
// makes such a read a number of times that follows how fast the earlier
// answers came (a page that renders again, a debounced fetch), and a
// replay's answers come faster than the recording's. For such a read only:
//
//   - once every recording of it has answered, one call more is answered
//     again with its last recording (AnswerRepeatRead); a second call more
//     misses as one call more than the test recorded;
//   - one of its recordings left unmade is not charged to the test
//     (oneFewerLocked); a second one is.
//
// A replay may also name a read the test recorded ONCE
// (models.RepeatRead.Once), which its namer knows reads by the request
// alone (keploy enterprise: an HTTP GET, HEAD or OPTIONS, or a GraphQL
// query). Exact serving honours it only when it holds exactly one of the
// test's own recordings, one whose call and answer it can read
// (httpint.RepeatReadOf): one call more is answered again with that
// recording, a second call more misses, and the recording left unmade is
// charged to the test as any other.
//
// The replay decides which recordings are one read: each read it names marks
// its recordings with one mark (models.MarkRepeatReads), and two reads it
// names alike carry two marks. Exact serving honours a
// named read only when it holds at least two of the test's own recordings,
// every one of one call with one answer by the proof the namer uses too
// (httpint.RepeatReadOf, under the header noise the session's matcher
// compares the set's calls with: SetRepeatReadNoise), whatever their method
// (repeatReads). Any other named read is served as if it were not named.

// repeatReadNoise is the header noise a test's repeated reads are proved
// under (httpint.RepeatReadNoise).
type repeatReadNoise = httpint.RepeatReadNoise

// SetRepeatReadNoise sets, from the session's options opts, the header noise
// its strict matcher compares the set's calls with
// (httpint.NewRepeatReadNoise), which proves a test's repeated reads from the
// next test view on. Never set, it excuses no header: a proof is then never
// wider than the matcher.
func (m *MockManager) SetRepeatReadNoise(opts models.OutgoingOptions) {
	n := httpint.NewRepeatReadNoise(opts)
	m.repeatNoise.Store(&n)
}

// provingNoise is the noise SetRepeatReadNoise set, the zero noise when
// none was.
func (m *MockManager) provingNoise() repeatReadNoise {
	if n := m.repeatNoise.Load(); n != nil {
		return *n
	}
	return repeatReadNoise{}
}

// repeatReads is the repeated reads of v's test that exact serving honours
// (exactView.repeats): by mark (never by name, which two reads may share),
// the positions of each one's recordings among the test's own, in recorded
// order, proved under noise. nil when none is.
func (v *exactView) repeatReads(noise repeatReadNoise) map[models.RepeatReadMark][]int {
	var named map[models.RepeatReadMark][]int
	for i, mk := range v.owned {
		if v.isCand(i) || !mk.TestModeInfo.RepeatRead.Marked() {
			continue
		}
		if named == nil {
			named = map[models.RepeatReadMark][]int{}
		}
		read := mk.TestModeInfo.ReadMark()
		named[read] = append(named[read], i)
	}
	for read, at := range named {
		if !sized(read, at) || !v.oneRead(at, noise) {
			delete(named, read)
		}
	}
	if len(named) == 0 {
		return nil
	}
	return named
}

// sized reports whether a read holds as many recordings as its mark says:
// exactly one for a read recorded once, at least two for every other.
func sized(read models.RepeatReadMark, at []int) bool {
	if read.Once {
		return len(at) == 1
	}
	return len(at) >= 2
}

// oneRead reports whether the recordings at positions at are one read with
// one answer: one call and one answer by httpint.RepeatReadOf under noise,
// each one a recording it can prove that of.
func (v *exactView) oneRead(at []int, noise repeatReadNoise) bool {
	var first, firstAnswer string
	for k, i := range at {
		call, answer, ok := httpint.RepeatReadOf(v.owned[i], noise)
		if !ok {
			return false
		}
		if k == 0 {
			first, firstAnswer = call, answer
			continue
		}
		if call != first || answer != firstAnswer {
			return false
		}
	}
	return true
}

// AnswerRepeatRead implements integrations.RepeatReadAnswerer for the test
// view in force: matched are the open test's answered recordings a call one
// more than recorded matches. It answers with the last recording of the
// repeated read they all belong to, once per read per test, the test's ID
// and how many times the test recorded the read (its recordings exact
// serving holds).
func (m *MockManager) AnswerRepeatRead(matched []*models.Mock) (integrations.RepeatReadAnswer, bool) {
	v := m.exactView()
	if v == nil {
		return integrations.RepeatReadAnswer{}, false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	var read models.RepeatReadMark
	for _, mk := range matched {
		// matched are the view's own copies (ConsumedTestMocks): one of a
		// view installed since, or a shared recording, is not the test's.
		i, own := v.index[mk.Name]
		if !own || v.owned[i] != mk || v.isCand(i) {
			return integrations.RepeatReadAnswer{}, false
		}
		r := v.owned[i].TestModeInfo.ReadMark()
		if !r.Marked() || read.Marked() && r != read {
			return integrations.RepeatReadAnswer{}, false
		}
		read = r
	}
	at, ok := v.repeats[read]
	if !ok || v.answeredAgain[read] {
		return integrations.RepeatReadAnswer{}, false
	}
	for _, i := range at {
		if !v.consumed[i] {
			return integrations.RepeatReadAnswer{}, false
		}
	}
	if v.answeredAgain == nil {
		v.answeredAgain = map[models.RepeatReadMark]bool{}
	}
	v.answeredAgain[read] = true
	return integrations.RepeatReadAnswer{Mock: v.owned[at[len(at)-1]], Test: v.test, Recorded: len(at)}, true
}

var _ integrations.RepeatReadAnswerer = (*MockManager)(nil)

// oneFewerLocked is, v.mu held, the positions of v's own recordings its
// outcome does not charge as unmade: of each repeated read v honours
// (exactView.repeats), the one recording the test left unmade, when it made
// all of the read's others (the last one: a test's own recordings answer in
// recorded order). A read made two or more times fewer than recorded is
// charged every recording it left unmade, and so is a read recorded once
// (models.RepeatReadMark.Once).
func (v *exactView) oneFewerLocked() map[int]bool {
	var excused map[int]bool
	for read, at := range v.repeats {
		if read.Once {
			continue
		}
		var unmade []int
		for _, i := range at {
			if !v.consumed[i] {
				unmade = append(unmade, i)
			}
		}
		if len(unmade) != 1 {
			continue
		}
		if excused == nil {
			excused = map[int]bool{}
		}
		excused[unmade[0]] = true
	}
	return excused
}
