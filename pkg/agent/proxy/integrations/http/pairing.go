package http

import (
	"context"
	"net/url"
	"reflect"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// pairExactly is record's full round's pairing of an HTTP call, the step
// after the open test's own recordings (exactly, or by the
// learn step) and the background (exactly) left the call unanswered: a call
// that is exactly a recording another test owns is answered with it, which
// stays its owner's (integrations.ClaimPairing: a claim, no consumption),
// for judging to move it to the set's background. The store offers such
// recordings (integrations.PairingCandidates) only in a session that pairs:
// record's full round (OutgoingOptions.Learn), under the proven switch, for a
// set served by the background rules. Anywhere else nothing pairs.
//
//   - Exactly only: the strict comparison exact serving answers by
//     (strictDiff), modulo the noise declared or learned onto the recording.
//     HTTP never pairs by drift.
//   - A poll (an SQS ReceiveMessage) never pairs: it is answered from the
//     background or not at all.
//   - A recording that declares variables is the variable pass's, and never
//     pairs.
//   - The first in recorded order pairs. Recordings of this very call that
//     hold different answers, another test's or the background's (whatever
//     its place), are told apart by nothing the call carries: none pairs.
//   - A recording another call claimed first is passed over; a call left with
//     none pairs with none.
//
// It returns the recording paired, and its answer read back (withResponse),
// or nils.
//
// Read from one snapshot of the store's round (integrations.RoundReader), the
// candidates, whose each is, and the background are of one instant, and a
// claim takes a recording only while the round is still at it
// (integrations.ClaimPairingIn): one the round moved under is looked at
// again, in the round as it is now.
func (h *HTTP) pairExactly(ctx context.Context, input *req, mockDb integrations.MockMemDb, n strictNoise) (answer, recording *models.Mock) {
	return h.pairExactlyFrom(ctx, input, mockDb, n, integrations.CallRound{})
}

// pairExactlyFrom is pairExactly for a call whose first read of the round is
// call (integrations.CallRound; the zero CallRound: none): a look pairs the
// call with no recording the first read's view held as its own (the replay
// ended the call's test under it: that test's recordings are no other
// test's for the call), and the call pairs with nothing once the replay
// staged again after its first read. The store cannot tell the same set
// staged again from the next one (a staging is all it counts), and a
// recording of the next set must never answer a call of the set before, nor
// be spent on it: the call misses. Each look reads its round in the call's
// staging, and its claim lands only while the round is still at that look
// (integrations.ClaimPairingIn), so a claim is never taken in a later
// staging.
func (h *HTTP) pairExactlyFrom(ctx context.Context, input *req, mockDb integrations.MockMemDb, n strictNoise, call integrations.CallRound) (answer, recording *models.Mock) {
	if sqsPoll(input) {
		return nil, nil
	}
	for ctx.Err() == nil {
		snap, ok := integrations.SnapshotRound(mockDb, integrations.RoundQuery{Kind: models.HTTP, Pairing: true})
		if ok && call.Read() && snap.At.Epoch != call.Epoch() {
			return nil, nil
		}
		if !ok {
			if p, paced := mockDb.(integrations.WindowPacer); call.Read() && (!paced || p.StagingEpoch() != call.Epoch()) {
				return nil, nil
			}
			return h.pairExactlyIn(ctx, input, n, integrations.PairingCandidates(mockDb, models.HTTP),
				func(m *models.Mock) bool { return !openTestsOwn(mockDb, m) },
				func() []*models.Mock { b, _ := integrations.KindBackground(mockDb, models.HTTP); return b },
				func(x *models.Mock) (bool, bool) { return integrations.ClaimPairing(mockDb, x), false })
		}
		cands, errs := snap.LoadCandidates(models.HTTP)
		for _, e := range errs {
			if e.Name == "" {
				h.Logger.Warn("record's full round could not read the test set's recordings kept on disk, so no call pairs with those it did not read: a call that is one misses where it was made",
					zap.String("kind", string(models.HTTP)), zap.Error(e.Err),
					zap.String("next_step", "fix the error above and record the test set again"))
				continue
			}
			h.Logger.Warn("record's full round could not read a recording kept on disk, so no call pairs with it: a call that is that recording misses where it was made",
				zap.String("mock", e.Name), zap.Error(e.Err),
				zap.String("next_step", "fix the error above and record the test set again"))
		}
		answer, recording, moved := h.pairExactlyInRound(ctx, input, n, mockDb, snap, cands, call)
		if !moved {
			return answer, recording
		}
	}
	return nil, nil
}

// pairExactlyInRound is pairExactly of one snapshot of the round, cands its
// candidates decoded: moved when a claim found the round moved since.
// The recordings the call's first read held as its view's own
// (CallRound.Held) are never another test's for the call.
func (h *HTTP) pairExactlyInRound(ctx context.Context, input *req, n strictNoise, mockDb integrations.MockMemDb, snap integrations.RoundSnapshot, cands []*models.Mock, call integrations.CallRound) (answer, recording *models.Mock, moved bool) {
	answer, recording = h.pairExactlyIn(ctx, input, n, cands,
		// None of the view's own: it is the view's, never another test's.
		func(m *models.Mock) bool { return snap.Owner(m) == integrations.OwnerNone && !call.Held(m.Name) },
		func() []*models.Mock { return snap.BackgroundUnused },
		func(x *models.Mock) (bool, bool) {
			claimed, m, _ := integrations.ClaimPairingIn(mockDb, x, snap.At)
			moved = m
			return claimed, m
		})
	return answer, recording, moved
}

// pairExactlyIn is pairExactly of cands: others says which are another
// test's, background is the background's unused, read when one fits, and
// claim claims one, stop saying the claim's round moved (nothing more is
// tried).
func (h *HTTP) pairExactlyIn(ctx context.Context, input *req, n strictNoise, cands []*models.Mock, others func(*models.Mock) bool, background func() []*models.Mock, claim func(*models.Mock) (claimed, stop bool)) (answer, recording *models.Mock) {
	if len(cands) == 0 {
		return nil, nil
	}
	live := liveDestination(ctx, input)
	fits := func(ms []*models.Mock) []*models.Mock {
		var out []*models.Mock
		for _, m := range ms {
			if m == nil || m.Kind != models.HTTP || m.Spec.HTTPReq == nil || m.IsProtocolPhase() || m.IsAsync() || hasVariables(m) {
				continue
			}
			if len(strictDiff(input, live, m, n)) == 0 {
				out = append(out, m)
			}
		}
		return out
	}
	var theirs []*models.Mock
	for _, m := range cands {
		if m != nil && others(m) {
			theirs = append(theirs, m)
		}
	}
	exact := inRecordedOrder(fits(theirs))
	if len(exact) == 0 {
		return nil, nil
	}
	if !oneAnswer(append(append([]*models.Mock(nil), exact...), fits(background())...)) {
		return nil, nil
	}
	for _, x := range exact {
		if ctx.Err() != nil {
			return nil, nil
		}
		served, err := withResponse(x)
		if err != nil {
			// An answer keploy cannot read back pairs nothing.
			return nil, nil
		}
		claimed, stop := claim(x)
		if claimed {
			return served, x
		}
		if stop {
			return nil, nil
		}
	}
	return nil, nil
}

// openTestsOwn reports whether the store says m is the open test's own: in
// flight into it (integrations.InFlightOwner) or owned by it
// (integrations.TestOwner), each asked only of a store that tells. A store
// that tells neither is taken at its word (integrations.PairingCandidates):
// m is another test's.
func openTestsOwn(mockDb integrations.MockMemDb, m *models.Mock) bool {
	if f, ok := mockDb.(integrations.InFlightOwner); ok && f.InFlightIntoOpenTest(m) {
		return true
	}
	if o, ok := mockDb.(integrations.TestOwner); ok && o.OwnedByOpenTest(m) {
		return true
	}
	return false
}

// oneAnswer reports whether the recordings hold one answer: one status, one
// body and one set of headers, as recorded, but for the Date header, which
// HTTP defines as the moment the origin answered (RFC 9110 §6.6.1), no part
// of what it answered: two recordings of one answer made a second apart
// differ there. false when one's answer cannot be read back, which cannot be
// told equal.
func oneAnswer(mocks []*models.Mock) bool {
	var first *models.HTTPResp
	for _, m := range mocks {
		r, err := withResponse(m)
		if err != nil || r.Spec.HTTPResp == nil {
			return false
		}
		resp := r.Spec.HTTPResp
		if first == nil {
			first = resp
			continue
		}
		if resp.StatusCode != first.StatusCode || resp.Body != first.Body || !reflect.DeepEqual(withoutDate(resp.Header), withoutDate(first.Header)) {
			return false
		}
	}
	return true
}

// withoutDate is h without its Date header, whatever its name's case.
func withoutDate(h map[string]string) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if !strings.EqualFold(k, "Date") {
			out[k] = v
		}
	}
	return out
}

// sqsPoll reports whether the call is an SQS ReceiveMessage, by its action as
// the AWS protocols name it: the JSON protocol's X-Amz-Target, or the query
// protocol's Action parameter, in the query or a form body.
func sqsPoll(input *req) bool {
	const action = "ReceiveMessage"
	for _, t := range input.header.Values(amzTargetHeader) {
		if strings.EqualFold(strings.TrimSpace(t), "AmazonSQS."+action) {
			return true
		}
	}
	if input.url != nil && input.url.Query().Get("Action") == action {
		return true
	}
	if strings.HasPrefix(strings.ToLower(input.header.Get("Content-Type")), "application/x-www-form-urlencoded") {
		if form, err := url.ParseQuery(string(input.body)); err == nil && form.Get("Action") == action {
			return true
		}
	}
	return false
}

// reportPairing reports a pairing (models.AnswerProvisional):
// the call is x, a recording another test owns (NotOwned, ClosestMock),
// matched exactly. RecordedIn (the owner) is not the parser's to fill: the
// store names no recording's test, and judging reads the owner of
// ClosestMock off the set's mappings. The pairing's call was served from x
// though no delete door consumed it: the learn step's tracker is told so, as
// the store tells it of a consumed one.
func reportPairing(ctx context.Context, input *req, x *models.Mock) {
	path, dest := "/", input.header.Get("Host")
	if input.url != nil {
		if input.url.Path != "" {
			path = input.url.Path
		}
		if dest == "" {
			dest = input.url.Host
		}
	}
	r := mismatch.NewReport(mismatch.ProtocolHTTP, input.method+" "+path).WithDestination(dest).Build()
	r.Answer, r.ClosestMock, r.NotOwned = models.AnswerProvisional, x.Name, true
	r.ClosestMockReq = renderMockRequest(x.Spec.HTTPReq, x.Noise)
	models.ReportAnswer(ctx, r)
	if t := learn.TrackerFrom(ctx); t != nil {
		// In this connection's request (integrations.InFlightOf), as a
		// consume door tells it (consumeWith).
		in := integrations.InFlightOf(ctx)
		if input.arrived.IsZero() {
			t.ServedMockIn(x, in)
		} else {
			t.ServedMockAtIn(x, input.arrived, in)
		}
	}
}
