package http

import (
	"context"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
)

// answerOrder keeps, under the proven switch, the order the recording
// answered GraphQL queries in when a client's cache needs it: two queries
// that select one root field with the same arguments (graphQLRoot) are filed
// by the client under one entry, the one answered last overwriting the other
// (Apollo's cache with no normalization). A page's GetBranchPage and
// GetBranchPulse both select branch(branchId); answered the other way round
// from the recording, the page finds the first incomplete, asks for it again,
// and that call takes a recording the test makes only later (class X).
//
// So an answer is written only once every other call in flight that selects
// one of its root fields has its own answer written, when that call is still
// being matched, or when the recording it was matched to was, at record, in
// flight beside this one's and answered before it (answeredBefore). Only a
// call already in flight is waited for, never one that may come: a call
// being matched waits for nothing, and one matched waits only for recordings
// answered before its own, so no wait comes back round to itself.
type answerOrder struct {
	mu      sync.Mutex
	calls   map[*orderedCall]struct{}
	changed chan struct{}
}

// orderedCall is a GraphQL query in flight that answerOrder orders.
type orderedCall struct {
	keys    []string     // what it selects at the root (graphQLRoot)
	matched *models.Mock // the recording that answers it, once matched
}

// begin files a call in flight that selects keys at the root; nil, ordering
// nothing, when it selects none.
func (o *answerOrder) begin(keys []string) *orderedCall {
	if len(keys) == 0 {
		return nil
	}
	c := &orderedCall{keys: keys}
	o.mu.Lock()
	if o.calls == nil {
		o.calls = map[*orderedCall]struct{}{}
	}
	o.calls[c] = struct{}{}
	o.signalLocked()
	o.mu.Unlock()
	return c
}

// matched says the recording c was matched to.
func (o *answerOrder) matched(c *orderedCall, m *models.Mock) {
	if c == nil {
		return
	}
	o.mu.Lock()
	c.matched = m
	o.signalLocked()
	o.mu.Unlock()
}

// end says c's call is over: answered, refused, or its connection gone.
func (o *answerOrder) end(c *orderedCall) {
	if c == nil {
		return
	}
	o.mu.Lock()
	if _, ok := o.calls[c]; ok {
		delete(o.calls, c)
		o.signalLocked()
	}
	o.mu.Unlock()
}

// signalLocked wakes every hold to read the calls again. o.mu is held.
func (o *answerOrder) signalLocked() {
	if o.changed != nil {
		close(o.changed)
	}
	o.changed = make(chan struct{})
}

// hold returns once c's answer may be written: no other call in flight that
// selects one of c's root fields is being matched still, or was matched to a
// recording answered before c's (answeredBefore). It returns at once when ctx
// ends.
func (o *answerOrder) hold(ctx context.Context, c *orderedCall) {
	if c == nil || c.matched == nil {
		return
	}
	for {
		o.mu.Lock()
		if o.changed == nil {
			o.changed = make(chan struct{})
		}
		wait := false
		for x := range o.calls {
			if x == c || !sharesKey(x.keys, c.keys) {
				continue
			}
			if x.matched == nil || answeredBefore(x.matched, c.matched) {
				wait = true
				break
			}
		}
		ch := o.changed
		o.mu.Unlock()
		if !wait {
			return
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return
		}
	}
}

// sharesKey reports whether a and b (sorted) hold a key in common.
func sharesKey(a, b []string) bool {
	for i, j := 0, 0; i < len(a) && j < len(b); {
		switch {
		case a[i] == b[j]:
			return true
		case a[i] < b[j]:
			i++
		default:
			j++
		}
	}
	return false
}

// answeredBefore reports whether recording a was answered before b while
// both were in flight at record: each requested before the other was
// answered, and a answered first.
func answeredBefore(a, b *models.Mock) bool {
	if a == nil || b == nil || a.Name == b.Name {
		return false
	}
	ar, aa := a.Spec.ReqTimestampMock, a.Spec.ResTimestampMock
	br, ba := b.Spec.ReqTimestampMock, b.Spec.ResTimestampMock
	if ar.IsZero() || aa.IsZero() || br.IsZero() || ba.IsZero() {
		return false
	}
	return ar.Before(ba) && br.Before(aa) && aa.Before(ba)
}
