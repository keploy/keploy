package http

import "testing"

// A recordOne mark given back (ptMark.giveBack) unsets what its take set, as
// it was before, and nothing a later take set.
func TestPtMark_GiveBackUnsetsWhatItsTakeSet(t *testing.T) {
	const key = "GET pay.example:80/charge"
	t.Run("a 2xx after a kept warmup error", func(t *testing.T) {
		p := &ptRecorder{}
		if p.take(key, 503) == nil {
			t.Fatal("setup: the first call, an error, was not taken")
		}
		ok := p.take(key, 200)
		if ok == nil {
			t.Fatal("setup: the first 2xx was not taken")
		}
		ok.giveBack()
		if p.shouldRecord(key, 500) {
			t.Error("an error after the given-back 2xx was taken: the kept warmup error still stands for the errors")
		}
		if !p.shouldRecord(key, 200) {
			t.Error("a 2xx after the given-back 2xx was not taken: the endpoint keeps no 2xx")
		}
	})
	t.Run("a warmup error given back after a 2xx was kept", func(t *testing.T) {
		p := &ptRecorder{}
		warm := p.take(key, 503)
		if warm == nil || p.take(key, 200) == nil {
			t.Fatal("setup: the error, then the 2xx, were not taken")
		}
		warm.giveBack()
		if p.shouldRecord(key, 200) || p.shouldRecord(key, 500) {
			t.Error("a call after a kept 2xx was taken: giving back the warmup error unset the 2xx's mark")
		}
	})
	t.Run("a first 2xx", func(t *testing.T) {
		p := &ptRecorder{}
		ok := p.take(key, 200)
		if ok == nil {
			t.Fatal("setup: the first 2xx was not taken")
		}
		ok.giveBack()
		if !p.shouldRecord(key, 500) {
			t.Error("an error after the only call was given back was not taken: the endpoint keeps nothing")
		}
	})
	t.Run("a key evicted since", func(t *testing.T) {
		p := &ptRecorder{}
		old := p.take(key, 200)
		delete(p.seen, key)
		if p.take(key, 200) == nil {
			t.Fatal("setup: the 2xx after the eviction was not taken")
		}
		old.giveBack()
		if p.shouldRecord(key, 200) {
			t.Error("a 2xx after a kept one was taken: a mark taken before the key was evicted unset the new one's")
		}
	})
	t.Run("none", func(t *testing.T) {
		var none *ptMark
		none.giveBack()
	})
}
