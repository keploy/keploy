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
)

// pairExactly is record's full round's pairing of an HTTP call (S12 v16
// §4.1), the step after the open test's own recordings (exactly, or by the
// learn step) and the background (exactly) left the call unanswered: a call
// that is exactly a recording another test owns is answered with it, which
// stays its owner's (integrations.ClaimPairing: a claim, no consumption),
// for judging to move it to the set's background. The store offers such
// recordings (integrations.PairingCandidates) only in a session that pairs:
// record's full round (OutgoingOptions.Learn), under the proven switch, for a
// set served by S12's rules. Anywhere else nothing pairs.
//
//   - Exactly only: the strict comparison exact serving answers by
//     (strictDiff), modulo the noise declared or learned onto the recording.
//     HTTP never pairs by drift (§4.1, release 1).
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
func (h *HTTP) pairExactly(ctx context.Context, input *req, mockDb integrations.MockMemDb, n strictNoise) (answer, recording *models.Mock) {
	cands := integrations.PairingCandidates(mockDb, models.HTTP)
	if len(cands) == 0 || sqsPoll(input) {
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
	var others []*models.Mock
	for _, m := range cands {
		if m != nil && !openTestsOwn(mockDb, m) {
			others = append(others, m)
		}
	}
	exact := inRecordedOrder(fits(others))
	if len(exact) == 0 {
		return nil, nil
	}
	background, _ := integrations.KindBackground(mockDb, models.HTTP)
	if !oneAnswer(append(append([]*models.Mock(nil), exact...), fits(background)...)) {
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
		if integrations.ClaimPairing(mockDb, x) {
			return served, x
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

// reportPairing reports a pairing (models.AnswerProvisional, S12 v16 §4.1):
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
		if input.arrived.IsZero() {
			t.ServedMock(x)
		} else {
			t.ServedMockAt(x, input.arrived)
		}
	}
}
