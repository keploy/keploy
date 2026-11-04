package proxy

import (
	"net/http"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// A REPEATED READ: ONE MORE OR ONE FEWER THAN RECORDED.
//
// Exact serving answers each of an open test's own recordings at most once:
// a call one more than the test recorded misses (models.MissExtraCall), and
// a recording the test did not make is charged to it at its end. A test's
// mapping may name a repeated read (models.MappedTestCase.RepeatReads,
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
// The mapping decides which recordings are one read; exact serving honours a
// named read only when it holds at least two of the test's own recordings,
// all HTTP, of one method, URL and body, none of them a write by its method
// (PUT, PATCH, DELETE), each with one status, body and binary body, none
// streamed or with its body left out (repeatReads). Any other named read is
// served as if it were not named.

// repeatReads is the repeated reads of v's test that exact serving honours
// (exactView.repeats): by name, the positions of each one's recordings among
// the test's own, in recorded order. nil when none is.
func (v *exactView) repeatReads() map[string][]int {
	var named map[string][]int
	for i, mk := range v.owned {
		if v.isCand(i) || mk.TestModeInfo.RepeatRead == "" {
			continue
		}
		if named == nil {
			named = map[string][]int{}
		}
		named[mk.TestModeInfo.RepeatRead] = append(named[mk.TestModeInfo.RepeatRead], i)
	}
	for name, at := range named {
		if len(at) < 2 || !v.oneRead(at) {
			delete(named, name)
		}
	}
	if len(named) == 0 {
		return nil
	}
	return named
}

// oneRead reports whether the recordings at positions at are one HTTP read
// with one answer: one method, URL and body, the method not a write, and one
// status, body and binary body, none streamed or with its body left out, and
// none whose answer kept apart cannot be read back.
func (v *exactView) oneRead(at []int) bool {
	var first, firstAnswer string
	for k, i := range at {
		mk := v.owned[i].ShallowCopy()
		if err := mk.HydrateResponse(); err != nil {
			return false
		}
		q, a := mk.Spec.HTTPReq, mk.Spec.HTTPResp
		if mk.Kind != models.HTTP || q == nil || a == nil || isWriteMethod(string(q.Method)) || a.BodySkipped || len(a.StreamBody) > 0 {
			return false
		}
		call := strings.Join([]string{string(q.Method), q.URL, q.Body}, "\x00")
		answer := strings.Join([]string{strconv.Itoa(a.StatusCode), a.Body, a.Binary}, "\x00")
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

// isWriteMethod reports whether an HTTP method writes by its definition
// (RFC 9110): no rule makes a PUT, PATCH or DELETE a read.
func isWriteMethod(method string) bool {
	switch method {
	case http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// AnswerRepeatRead implements integrations.RepeatReadAnswerer for the test
// view in force: matched are the open test's answered recordings a call one
// more than recorded matches. It answers with the last recording of the
// repeated read they all belong to, once per read per test.
func (m *MockManager) AnswerRepeatRead(matched []*models.Mock) (*models.Mock, bool) {
	v := m.exactView()
	if v == nil {
		return nil, false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	read := ""
	for _, mk := range matched {
		// matched are the view's own copies (ConsumedTestMocks): one of a
		// view installed since, or a shared recording, is not the test's.
		i, own := v.index[mk.Name]
		if !own || v.owned[i] != mk || v.isCand(i) {
			return nil, false
		}
		r := v.owned[i].TestModeInfo.RepeatRead
		if r == "" || read != "" && r != read {
			return nil, false
		}
		read = r
	}
	at, ok := v.repeats[read]
	if !ok || v.answeredAgain[read] {
		return nil, false
	}
	for _, i := range at {
		if !v.consumed[i] {
			return nil, false
		}
	}
	if v.answeredAgain == nil {
		v.answeredAgain = map[string]bool{}
	}
	v.answeredAgain[read] = true
	return v.owned[at[len(at)-1]], true
}

var _ integrations.RepeatReadAnswerer = (*MockManager)(nil)

// oneFewerLocked is, v.mu held, the positions of v's own recordings its
// outcome does not charge as unmade: of each repeated read v honours
// (exactView.repeats), the one recording the test left unmade, when it made
// all of the read's others (the last one: a test's own recordings answer in
// recorded order). A read made two or more times fewer than recorded is
// charged every recording it left unmade.
func (v *exactView) oneFewerLocked() map[int]bool {
	var excused map[int]bool
	for _, at := range v.repeats {
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
