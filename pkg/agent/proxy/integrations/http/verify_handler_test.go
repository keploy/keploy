package http

import (
	"context"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// verifyOf gives h a verify mode of its own (HTTP.verify), apart from the
// process's (integrations.ExactViewVerify), so rows that compare a handler
// in verify mode with one out of it run their cases at once.
func verifyOf(h *HTTP, on bool) *HTTP {
	h.verify = new(atomic.Bool)
	h.verify.Store(on)
	return h
}

// Two handlers given verify modes of their own keep them, whatever the
// process's mode: over one pool, a strict call that is one recording
// compares that one on the handler whose mode is off, and every recording
// again on the one whose mode is on, with the process's mode on and off; a
// handler given none follows the process's.
func TestAHandlersVerifyModeIsItsOwn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	was := integrations.ExactViewVerify.Load()
	t.Cleanup(func() { integrations.ExactViewVerify.Store(was) })
	const host, n = "localhost:50001", 100
	ctx := context.Background()
	mocks := make([]*models.Mock, n)
	for i := range mocks {
		m := onHost(fmt.Sprintf("mock-%d", i), "/item/"+strconv.Itoa(i), host)
		mocks[i] = answered(m, time.Duration(i)*time.Millisecond, 200, `{"id":`+strconv.Itoa(i)+`}`)
	}
	db := &mockMemDb{mocks: mocks, updateUnFilteredReturn: true}
	compared := func(h *HTTP) int64 {
		t.Helper()
		var ok bool
		var stub *models.Mock
		var err error
		got := strictDiffsOf(func() {
			ok, stub, _, err = h.match(ctx, getTo("/item/"+strconv.Itoa(n/2), host), db, nil, nil, nil, true, false, false, true, true)
		})
		if err != nil || !ok || stub == nil || stub.Name != fmt.Sprintf("mock-%d", n/2) {
			t.Fatalf("match: ok %v, stub %v, err %v; want mock-%d", ok, stub, err, n/2)
		}
		return got
	}
	on, off := verifyOf(newHTTP(), true), verifyOf(newHTTP(), false)
	for _, process := range []bool{true, false} {
		integrations.ExactViewVerify.Store(process)
		for _, h := range []*HTTP{on, off} {
			if r := h.learnRules(db, learn.NewTracker()); r.verify != h.verify.Load() {
				t.Errorf("process's mode %v: a learned call's reads carry verify mode %v; want the handler's own, %v", process, r.verify, h.verify.Load())
			}
		}
		if got := compared(off); got != 1 {
			t.Errorf("process's mode %v: the handler whose own mode is off compared %d recordings; want the 1 the call is", process, got)
		}
		if got := compared(on); got != 1+n {
			t.Errorf("process's mode %v: the handler whose own mode is on compared %d recordings; want the call's 1 and all %d again", process, got, n)
		}
		want := int64(1)
		if process {
			want = 1 + n
		}
		if got := compared(newHTTP()); got != want {
			t.Errorf("process's mode %v: a handler with no mode of its own compared %d recordings; want %d, the process's", process, got, want)
		}
	}
}
