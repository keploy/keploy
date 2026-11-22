package record

import (
	"context"
	"slices"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// liveRevokes applies a recording's revokes as they are known, not only at its
// end, and keeps what the rest of the recording asks of a revoke by the
// capture it is about.
//
// A revoke names a test case one of whose mocks is lost: the agent dropped it
// (a RevokedTests frame), the recorder could not save it, or the agent ended
// the mock stream before it sent it. Saved without it, the case fails at
// replay as if the app had changed. Applied only at the recording's end, a
// revoked case stayed in the set for the whole recording: every auto-replay
// round in between replayed it, and the upload cycle sent it, so a long
// recording reported failures that were the recording's own losses.
//
// So a revoked case already saved is deleted from the set as soon as the
// revoke comes (the store's DeleteTests, the one the end applies), and a case
// revoked before it arrives is not saved. The end deletes only what is left (a
// delete that failed), and counts each case once.
//
// WHICH CAPTURE A REVOKE IS ABOUT. A name comes back: a request's
// Keploy-Test-Name header names every capture of that request, and a later
// capture under a name replaces the earlier one in the set. Test cases, mocks
// (with the agent's revokes) and mappings reach the recorder on three streams
// nothing orders against each other, so a revoke can come while the capture
// it is about is still on the test stream, behind an earlier capture under the
// name. A revoke never resolves to "the latest capture under the name":
//   - The agent's revoke kills the name for the rest of the recording
//     (revokeByAgent). After it drops a mock of a test under a name, the agent
//     streams no capture under that name (WasMockDroppedForTC), so a capture
//     under it that reaches the recorder after the revoke was streamed before
//     the drop and may own the dropped mock. Every capture saved under the
//     name is deleted, and every later one is not saved: the outcome of a
//     revoke applied at the end, applied as it comes. A name the agent gives a
//     test case it sent with no name is its capture id
//     (models.TestCase.CaptureID) or its request time
//     (models.RevokeNameByRequest): each names that capture alone.
//   - The recorder's revoke of a mock it could not save is about the capture
//     whose window holds that mock's request time (revokeLost): the agent bins
//     a mock into the window of the test that made it. A capture already come
//     under the name whose window holds it is revoked; when none has come yet,
//     the time waits for the capture that comes holding it. A later capture
//     under the name, whose window does not hold it, is a test case of its own
//     and is kept. Where no capture can be told (the mock's time is not known,
//     or a capture under the name carries no window), or the mock never came
//     (the agent ended the stream first), the name is killed as the agent's
//     revoke kills it. A time no capture's window took by the recording's end
//     (a capture whose agent's clock stepped back during it holds its mock
//     outside its window's wall-clock span) kills the name then: the end's
//     delete takes what the name holds, as it always did.
//
// What else the recording keeps of a revoke follows from that:
//   - heldOut: a mapping entry under a killed name is not held for its mock;
//     the capture it is about is deleted or not saved.
//   - shortTotals: a test's missing mocks are counted when the set keeps a
//     capture under its name.
//   - windows: a capture deleted gives up its test window (windowMappings),
//     so the window of a later capture under its name keeps its entry.
type liveRevokes struct {
	logger *zap.Logger
	td     testCaseDeleter
	setID  string

	// insertedMu and testCount are Start's: the saved test cases' count.
	insertedMu *sync.Mutex
	testCount  *int

	// windows are the recording's test windows, nil when it derives none.
	windows *windowMappings

	mu sync.Mutex
	// written is signalled when a name stops being written or deleted.
	written *sync.Cond
	// came are the captures that came under each name a revoke may give them
	// (keys), saved or not, in the order they came.
	came map[string][]*liveCapture
	// stored is the capture the store holds under each name it saved one
	// under: a later capture under the name replaces it there.
	stored map[string]*liveCapture
	// inFlight is the capture being saved, nil between two.
	inFlight *liveCapture
	// dead are the names killed for the rest of the recording: every capture
	// saved under one is deleted, every later one is not saved.
	dead map[string]struct{}
	// lostAt are the request times, in Unix nanoseconds, of the mocks the
	// recorder could not save, by the name their mapping gave, whose capture
	// has not come: the capture that comes under the name holding one is not
	// saved.
	lostAt map[string][]int64
	// queue are the saved captures revoked and not deleted yet.
	queue []*liveCapture
	// busy are the names a capture is being saved or deleted under: a save
	// and a delete under one name never overlap, so a delete never takes a
	// later capture the store put under the name meanwhile.
	busy map[string]bool
	// deleted are the names of the saved test cases deleted so far.
	deleted map[string]struct{}
	// notRecorded counts the test cases not saved because they were revoked
	// before they arrived.
	notRecorded int

	wake chan struct{}
	stop chan struct{}
	done chan struct{}
}

// liveCapture is one test case the recording took: its names, its window, and
// what befell it. Under liveRevokes.mu.
type liveCapture struct {
	// sent is the name the agent sent it with, "" for none, and captureID
	// the capture id it came with (models.TestCase.CaptureID), "" for none:
	// the async capture's test cases come with one and no name, and the
	// agent names them by it in its mappings and revokes.
	sent      string
	captureID string
	// req and end are its window in Unix nanoseconds, 0 for none, and
	// reqName the name a revoke by its request time gives
	// (models.RevokeNameByRequest).
	req, end int64
	reqName  string
	// name is the name the store saved it under, "" until it is saved.
	name string
	// revoked: a revoke took it. Saved, it is deleted (queued); being saved
	// when the revoke came, it is deleted once saved; not saved, nothing is.
	revoked bool
	// refused: revoked before it arrived, so it was not saved.
	refused bool
	// gone: deleted, or replaced in the store by a later capture under its
	// name. A delete of it now would take that later one.
	gone bool
	// deleted: gone because its delete succeeded.
	deleted bool
	// window is its test window in windows, -1 for none.
	window int
}

// startLiveRevokes starts applying revokes as they come. td nil (a store that
// cannot delete a test case it saved): nothing is deleted before the end,
// which then says so, but a case revoked before it arrives is still not saved.
func startLiveRevokes(ctx context.Context, logger *zap.Logger, td testCaseDeleter, setID string, insertedMu *sync.Mutex, testCount *int) *liveRevokes {
	lr := &liveRevokes{
		logger: logger, td: td, setID: setID,
		insertedMu: insertedMu, testCount: testCount,
		came:    map[string][]*liveCapture{},
		stored:  map[string]*liveCapture{},
		dead:    map[string]struct{}{},
		lostAt:  map[string][]int64{},
		busy:    map[string]bool{},
		deleted: map[string]struct{}{},
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	lr.written = sync.NewCond(&lr.mu)
	go func() {
		defer close(lr.done)
		for {
			select {
			case <-lr.wake:
				lr.apply(ctx)
			case <-lr.stop:
				return
			}
		}
	}()
	return lr
}

// giveWindows hands lr the recording's test windows (nil for none), before
// any capture comes: a capture deleted gives its window up (placed).
func (lr *liveRevokes) giveWindows(w *windowMappings) {
	lr.mu.Lock()
	lr.windows = w
	lr.mu.Unlock()
}

// arrive is told of each test case the recording takes, before anything of
// it is saved. It reports nil when a revoke came for it before it arrived:
// it is not saved, and is counted. Else it returns the capture to hand to
// saving and saved.
func (lr *liveRevokes) arrive(tc *models.TestCase) *liveCapture {
	req, resp := models.TestCaseWindow(tc)
	c := &liveCapture{sent: tc.Name, captureID: tc.CaptureID, window: -1}
	if !req.IsZero() {
		c.req, c.reqName = req.UnixNano(), models.RevokeNameByRequest(req)
	}
	if !resp.IsZero() {
		c.end = resp.UnixNano()
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if lr.revokedOnComing(c) {
		c.revoked, c.refused = true, true
	}
	lr.note(c, c.keys()...)
	if c.refused {
		lr.notRecorded++
		lr.logger.Debug("not saving a test case revoked before it arrived: one of its mocks is lost",
			zap.String("testCaseName", tc.Name), zap.String("testSetID", lr.setID))
		return nil
	}
	lr.inFlight = c
	return c
}

// revokedOnComing reports whether a revoke that came before c is about it: a
// name of its is killed, or a mock lost under a name of its lies in its window
// (taken now), or, with no window to tell by, may. With lr.mu held.
func (lr *liveRevokes) revokedOnComing(c *liveCapture) bool {
	took := false
	for _, k := range c.keys() {
		if _, ok := lr.dead[k]; ok {
			took = true
		}
		times := lr.lostAt[k]
		if len(times) == 0 {
			continue
		}
		if !c.hasWindow() {
			// Which capture under the name lost it cannot be told.
			took = true
			continue
		}
		kept := times[:0]
		for _, at := range times {
			if c.holds(at) {
				took = true
				continue
			}
			kept = append(kept, at)
		}
		if len(kept) == 0 {
			delete(lr.lostAt, k)
		} else {
			lr.lostAt[k] = kept
		}
	}
	return took
}

// note notes c as come under each of keys.
func (lr *liveRevokes) note(c *liveCapture, keys ...string) {
	for _, k := range keys {
		lr.came[k] = append(lr.came[k], c)
	}
}

// keys are the names a revoke may give c: the one it was sent with, its
// capture id, the one it was saved under, and its request time's.
func (c *liveCapture) keys() []string {
	out := make([]string, 0, 4)
	for _, k := range []string{c.sent, c.captureID, c.name} {
		if k != "" && !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	if c.reqName != "" {
		out = append(out, c.reqName)
	}
	return out
}

// hasWindow reports whether c's window is known.
func (c *liveCapture) hasWindow() bool { return c.req != 0 && c.end != 0 }

// holds reports whether c's window holds the request time at.
func (c *liveCapture) holds(at int64) bool {
	return c.hasWindow() && at >= c.req && at <= c.end
}

// saving is told that c is about to be saved under name ("" when the store
// names it): it waits out a delete under the name, and holds the name until
// saved is told.
func (lr *liveRevokes) saving(c *liveCapture, name string) {
	if c == nil || name == "" {
		return
	}
	lr.mu.Lock()
	for lr.busy[name] {
		lr.written.Wait()
	}
	lr.busy[name] = true
	lr.mu.Unlock()
}

// saved is told how the store took c, which saving held under held ("" for
// none): saved under tc.Name (err nil), or not saved. Saved, c is the capture
// under its names now, and replaces in the store the one it held under its
// name; revoked while it was saved, or saved under a name a revoke is about,
// it is deleted.
func (lr *liveRevokes) saved(c *liveCapture, held string, tc *models.TestCase, err error) {
	if c == nil {
		return
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if held != "" {
		delete(lr.busy, held)
		lr.written.Broadcast()
		if len(lr.queue) > 0 {
			// A delete apply left for this save to end.
			lr.signal()
		}
	}
	if lr.inFlight == c {
		lr.inFlight = nil
	}
	if err != nil {
		// Not saved: a revoke that took it has nothing to delete.
		return
	}
	c.name = tc.Name
	if prev := lr.stored[c.name]; prev != nil && prev != c {
		prev.gone = true
	}
	lr.stored[c.name] = c
	if c.name != "" && c.name != c.sent {
		// A name the store gave it: a revoke from now on may give it too,
		// and one that came under it before this save was told is applied
		// now. A capture id's mapping is filed under its test case's saved
		// name as soon as captureNames knows it, on the mapping consumer's
		// goroutine, so a revoke under that name may come first: what the
		// name carries (killed, or a lost time this capture's window holds)
		// takes it here, under the same lock, whichever was told first.
		if !c.revoked && lr.revokedOnComing(c) {
			c.revoked = true
		}
		lr.note(c, c.name)
	}
	if c.revoked {
		lr.queue = append(lr.queue, c)
		lr.signal()
	}
}

// placed is told the test window c's save gave the recording's windows
// (windowMappings.addTest), -1 for none: a delete of c gives it up, and one
// that came first gives it up now.
func (lr *liveRevokes) placed(c *liveCapture, window int) {
	if lr == nil || c == nil || window < 0 {
		return
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	c.window = window
	if c.deleted {
		lr.windows.lose(window)
	}
}

// revokeByAgent applies the agent's revoke of each of names: a mock of the
// test the name gives was dropped. The name is killed for the rest of the
// recording (see liveRevokes). It returns the names it killed now, whose held
// mapping entries the caller lets go (mappingWaits.revoke).
func (lr *liveRevokes) revokeByAgent(names ...string) []string {
	return lr.revoke(names...)
}

// revoke kills each of names (see liveRevokes): the agent's revoke
// (revokeByAgent), or the recorder's for a mock it cannot tell the capture of.
// It returns the names it killed now.
func (lr *liveRevokes) revoke(names ...string) []string {
	if lr == nil {
		return nil
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	var killed []string
	for _, n := range names {
		if n != "" && lr.kill(n) {
			killed = append(killed, n)
		}
	}
	lr.signal()
	return killed
}

// revokeLost applies the recorder's revoke of the capture under name whose
// window holds each of at, the request times of mocks of it the recorder could
// not save (see liveRevokes). A time no capture come under the name holds
// waits for the capture that comes holding it. With no time, a zero one, or a
// capture under the name whose window is not known, the capture cannot be
// told, and the name is killed. It returns the names it killed now.
func (lr *liveRevokes) revokeLost(name string, at []time.Time) []string {
	if lr == nil || name == "" {
		return nil
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	defer lr.signal()
	if _, ok := lr.dead[name]; ok {
		return nil
	}
	told := len(at) > 0
	for _, t := range at {
		told = told && !t.IsZero()
	}
	for _, c := range lr.came[name] {
		told = told && (c.refused || c.hasWindow())
	}
	if !told {
		if lr.kill(name) {
			return []string{name}
		}
		return nil
	}
	for _, t := range at {
		ns, hit := t.UnixNano(), false
		for _, c := range lr.came[name] {
			if c.holds(ns) {
				hit = true
				lr.take(c)
			}
		}
		if !hit {
			lr.lostAt[name] = append(lr.lostAt[name], ns)
		}
	}
	return nil
}

// kill kills name for the rest of the recording: every capture under it is
// revoked. It reports whether it killed it now. With lr.mu held.
func (lr *liveRevokes) kill(name string) bool {
	if _, ok := lr.dead[name]; ok {
		return false
	}
	lr.dead[name] = struct{}{}
	for _, c := range lr.came[name] {
		lr.take(c)
	}
	return true
}

// take revokes c: saved, it is deleted; being saved, it is deleted once
// saved (its name is the store's only then); not saved, there is nothing to
// delete. With lr.mu held.
func (lr *liveRevokes) take(c *liveCapture) {
	if c.revoked || c.gone {
		return
	}
	c.revoked = true
	if c.name != "" {
		lr.queue = append(lr.queue, c)
	}
}

// heldOut reports whether a mapping entry under name is not to be held for
// its mock: the name is killed, so the capture it is about is deleted or not
// saved, and the mock it waits for may be one the agent dropped and never
// sends. Nil-safe.
func (lr *liveRevokes) heldOut(name string) bool {
	if lr == nil {
		return false
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	_, ok := lr.dead[name]
	return ok
}

// shortTotals is how many distinct tests the set keeps with a short mock
// pool, and how many of their mocks are missing, from counts by test name
// (Start's shortPoolByTest): a name is counted when the set keeps a capture
// under it (one not deleted, nor refused for a revoke), or when no capture
// came under it at all (its mapping came, its test case never did), as it
// always was. Called once the recording's goroutines are joined, or given up.
func (lr *liveRevokes) shortTotals(counts *sync.Map) (tests, mocks int) {
	if lr != nil {
		lr.mu.Lock()
		defer lr.mu.Unlock()
	}
	counts.Range(func(k, v any) bool {
		name, _ := k.(string)
		if !lr.keeps(name) {
			return true
		}
		n, _ := v.(int64)
		tests++
		mocks += int(n)
		return true
	})
	return tests, mocks
}

// keeps reports whether the set keeps a test under name, or no capture came
// under it; with lr.mu held (lr may be nil).
func (lr *liveRevokes) keeps(name string) bool {
	if lr == nil {
		return true
	}
	cs := lr.came[name]
	if len(cs) == 0 {
		return true
	}
	for _, c := range cs {
		if !c.deleted && !c.refused {
			return true
		}
	}
	return false
}

func (lr *liveRevokes) signal() {
	select {
	case lr.wake <- struct{}{}:
	default:
	}
}

// apply deletes the revoked captures queued. A delete that fails is not tried
// again while the recording goes on (each try asks the store, which may wait
// for the case's writers): the end tries it once more. A capture whose name a
// save holds is left queued; that save replaces it, or, failing, signals.
func (lr *liveRevokes) apply(ctx context.Context) {
	if lr.td == nil {
		return
	}
	lr.mu.Lock()
	var batch []*liveCapture
	var names []string
	rest := lr.queue[:0]
	for _, c := range lr.queue {
		switch {
		case c.gone:
		case lr.busy[c.name]:
			rest = append(rest, c)
		default:
			lr.busy[c.name] = true
			batch = append(batch, c)
			names = append(names, c.name)
		}
	}
	lr.queue = rest
	lr.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	deleted := deleteRevoked(context.WithoutCancel(ctx), lr.logger, lr.td, lr.setID, names)
	lr.mu.Lock()
	n := 0
	for _, c := range batch {
		delete(lr.busy, c.name)
		if _, ok := deleted[c.name]; ok {
			lr.gone(c)
			lr.deleted[c.name] = struct{}{}
			n++
		}
	}
	lr.written.Broadcast()
	lr.mu.Unlock()
	if n == 0 {
		return
	}
	lr.insertedMu.Lock()
	*lr.testCount -= n
	lr.insertedMu.Unlock()
	lr.logger.Info("removed test cases from the recording as soon as one of their mocks was known to be lost, so no replay runs them",
		zap.Int("revoked", n), zap.String("testSetID", lr.setID))
}

// gone marks c deleted: it gives its test window up; with lr.mu held.
func (lr *liveRevokes) gone(c *liveCapture) {
	c.gone, c.deleted = true, true
	if c.window >= 0 {
		lr.windows.lose(c.window)
	}
}

// end stops applying revokes and returns the saved test cases deleted so far,
// how many were not saved because they were revoked before they arrived, and
// the names of the revoked ones still saved, for the end's own delete: a
// delete that failed, a revoke that came as the recording ended, or a name a
// lost mock no capture's window took was mapped to (killed now). What the end
// deletes of them it tells endDeleted. Nil-safe.
func (lr *liveRevokes) end() (deleted map[string]struct{}, notRecorded int, left []string) {
	if lr == nil {
		return map[string]struct{}{}, 0, nil
	}
	select {
	case <-lr.stop:
	default:
		close(lr.stop)
	}
	<-lr.done
	lr.mu.Lock()
	defer lr.mu.Unlock()
	out := make(map[string]struct{}, len(lr.deleted))
	for n := range lr.deleted {
		out[n] = struct{}{}
	}
	// A lost mock no capture's window took: which capture under its name it
	// was of cannot be told, so none of them is kept.
	untold := 0
	for name := range lr.lostAt {
		if lr.kill(name) {
			untold++
		}
	}
	if untold > 0 {
		lr.logger.Debug("deleting the test cases under names a lost mock was mapped to whose time no test case's window holds",
			zap.Int("names", untold), zap.String("testSetID", lr.setID))
	}
	for _, c := range lr.revokedLeft() {
		left = append(left, c.name)
	}
	return out, lr.notRecorded, left
}

// revokedLeft is each saved capture revoked and not gone, once per name; with
// lr.mu held.
func (lr *liveRevokes) revokedLeft() []*liveCapture {
	var out []*liveCapture
	seen := map[string]bool{}
	for _, cs := range lr.came {
		for _, c := range cs {
			if c.revoked && !c.gone && c.name != "" && !seen[c.name] {
				seen[c.name] = true
				out = append(out, c)
			}
		}
	}
	return out
}

// endDeleted is told the names the end's own delete took (of those end
// left): each one's capture is gone. Nil-safe.
func (lr *liveRevokes) endDeleted(names map[string]struct{}) {
	if lr == nil || len(names) == 0 {
		return
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	for _, c := range lr.revokedLeft() {
		if _, ok := names[c.name]; ok {
			lr.gone(c)
			lr.deleted[c.name] = struct{}{}
		}
	}
}
