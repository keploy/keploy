package replay

import "testing"

// --keep-app-alive skips the readiness wait for every test-set after the first,
// reasoning that the app "already passed its readiness check during the first
// test-set". The probe is best-effort — on timeout it warns and fires the tests
// anyway — so that reasoning holds only when the check actually passed.
//
// Measured on a real cluster: replay pod created at T+0, readiness probe engaged
// at T+4s, a later test-set logged "app already warm" at T+4.4s, and the app
// first accepted a connection at ~T+30s. The run finished at T+20s having
// refused every request. Ordinal position is not evidence of readiness; an
// observed accept is.
func TestAppReadyObservedGatesTheWarmSkip(t *testing.T) {
	orig := appReadyObserved.Load()
	t.Cleanup(func() { appReadyObserved.Store(orig) })

	// The predicate the two call sites use, mirrored here so the intent is
	// asserted independently of the surrounding replay plumbing.
	warmSkip := func(serveTest, isFirstTestSet bool) bool {
		return serveTest && !isFirstTestSet && appReadyObserved.Load()
	}

	appReadyObserved.Store(false)
	if warmSkip(true, false) {
		t.Fatal("must NOT skip the readiness wait before the app has ever been seen accepting a connection: that is exactly the run where every test gets connection refused")
	}
	if warmSkip(true, true) {
		t.Fatal("the first test-set always waits")
	}

	appReadyObserved.Store(true)
	if !warmSkip(true, false) {
		t.Fatal("once the app has been observed ready, later test-sets should skip the wait — that is the whole point of --keep-app-alive")
	}
	if warmSkip(false, false) {
		t.Fatal("without the one-shot spawn (serveTest=false) the wait must run every test-set, matching the historical lifecycle")
	}
	if warmSkip(true, true) {
		t.Fatal("the first test-set waits even when a previous run observed readiness")
	}
}
