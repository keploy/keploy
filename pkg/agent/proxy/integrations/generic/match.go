package generic

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mismatch"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
)

// fuzzyMatch performs a fuzzy matching algorithm to find the best matching mock for the given request.
// It takes a context, a request buffer, and a mock database as input parameters.
// The function iterates over the mocks in the database and applies the fuzzy matching algorithm to find the best match.
// If a match is found, it returns the corresponding response mock and a boolean value indicating success.
// If no match is found, it returns false and a nil response.
// If an error occurs during the matching process, it returns an error.
//
// What it consumes is told to the learn step as served in this connection's
// request (integrations.ServingIn), so a request aloft on another connection
// that reads no recording leaves this one's serve in its order, and with
// what the serve gave the application (stateServed).
//
// Under exact serving (the proven switch) a recording answers only the
// exchange it recorded: one whose every request buffer is the recorded one,
// byte for byte, a buffer the obfuscator redacted included (findExactMatch).
// The closest recording is never served for an exchange that changed, at
// either similarity: that is a miss, which the replay reports as
// keploy/unmocked_call. Exact serving is read at each attempt, as it begins
// and ends with tests. Without it the match is as before.
func fuzzyMatch(ctx context.Context, logger *zap.Logger, reqBuff [][]byte, mockDb integrations.MockMemDb) (bool, []models.Payload, error) {
	serving := integrations.ServingIn(ctx, mockDb)
	for {
		select {
		case <-ctx.Done():
			return false, nil, ctx.Err()
		default:
			mocks, err := mockDb.GetSessionMocks()
			if err != nil {
				return false, nil, fmt.Errorf("error while getting session mocks: %w", err)
			}

			var filteredMocks []*models.Mock
			var unfilteredMocks []*models.Mock

			for _, mock := range mocks {
				if mock.Kind != "Generic" {
					continue
				}
				if mock.TestModeInfo.IsFiltered {
					filteredMocks = append(filteredMocks, mock)
				} else {
					unfilteredMocks = append(unfilteredMocks, mock)
				}
			}

			logger.Debug("List of mocks in the database", zap.Int("Filtered Mocks", len(filteredMocks)), zap.Int("Unfiltered Mocks", len(unfilteredMocks)))
			for i, mock := range filteredMocks {
				logger.Debug("Filtered Mocks", zap.String(fmt.Sprintf("Mock[%d]", i), mock.Name), zap.Int64("sortOrder", mock.TestModeInfo.SortOrder))
			}
			for i, mock := range unfilteredMocks {
				logger.Debug("Unfiltered Mocks", zap.String(fmt.Sprintf("Mock[%d]", i), mock.Name), zap.Int64("sortOrder", mock.TestModeInfo.SortOrder))
			}

			exact := integrations.ExactServing(mockDb)
			index := findExactMatch(filteredMocks, reqBuff, exact)

			if index == -1 && !exact {
				index = findBinaryMatch(filteredMocks, reqBuff, 0.9)
			}

			// Concurrency note for both branches below: see HTTP's
			// updateMock — filteredMocks[index] / unfilteredMocks[index]
			// are pointers into the shared mock pool, so we mutate a
			// fresh copy rather than the pool pointer and pass the copy
			// to UpdateUnFilteredMock.
			if index != -1 {
				responseMock := make([]models.Payload, len(filteredMocks[index].Spec.GenericResponses))
				copy(responseMock, filteredMocks[index].Spec.GenericResponses)
				updatedMock := *filteredMocks[index]
				updatedMock.TestModeInfo.IsFiltered = false
				updatedMock.TestModeInfo.SortOrder = pkg.GetNextSortNum()
				isUpdated := serving.UpdateUnFilteredMock(filteredMocks[index], &updatedMock)
				if !isUpdated {
					continue
				}
				stateServed(ctx, filteredMocks[index], reqBuff)
				logger.Debug("Filtered mock found for generic request", zap.String("Mock", updatedMock.Name), zap.Int64("sortOrder", updatedMock.TestModeInfo.SortOrder))
				return true, responseMock, nil
			}

			index = findExactMatch(unfilteredMocks, reqBuff, exact)

			if index == -1 && !exact {
				index = findBinaryMatch(unfilteredMocks, reqBuff, 0.4)
			}
			if index != -1 {
				responseMock := make([]models.Payload, len(unfilteredMocks[index].Spec.GenericResponses))
				copy(responseMock, unfilteredMocks[index].Spec.GenericResponses)
				updatedMock := *unfilteredMocks[index]
				updatedMock.TestModeInfo.IsFiltered = false
				updatedMock.TestModeInfo.SortOrder = pkg.GetNextSortNum()
				isUpdated := serving.UpdateUnFilteredMock(unfilteredMocks[index], &updatedMock)
				if !isUpdated {
					continue
				}
				stateServed(ctx, unfilteredMocks[index], reqBuff)
				logger.Debug("Unfiltered mock found for generic request", zap.String("Mock", updatedMock.Name), zap.Int64("sortOrder", updatedMock.TestModeInfo.SortOrder))
				return true, responseMock, nil
			}
			return false, nil, nil
		}
	}
}

// extraCall tells, for an exchange no mock answered under exact serving (the
// proven switch), whether it is one exchange more than the open test
// recorded: one its own mocks already answered, matched as exact serving
// matches the pool (fuzzyMatch): exactly. An exchange merely like one the
// test answered is a changed exchange, not one more. It returns how many
// times the test recorded that exchange, 0 when it is not one or exact
// serving is off, and the recording it repeats: of the test's answered
// recordings of the exchange, the one recorded last
// (integrations.RepeatedRecording).
func extraCall(reqBuffs [][]byte, mockDb integrations.MockMemDb) (int, *models.Mock) {
	answered := answeredExchanges(mockDb)
	if len(answered) == 0 {
		return 0, nil
	}
	idx := findExactMatch(answered, reqBuffs, true)
	if idx == -1 {
		return 0, nil
	}
	var copies []*models.Mock
	for _, mk := range answered {
		if sameRequests(mk.Spec.GenericRequests, answered[idx].Spec.GenericRequests) {
			copies = append(copies, mk)
		}
	}
	return len(copies), integrations.RepeatedRecording(copies)
}

// backgroundAgain is, for an exchange no mock answered under exact serving
// (the proven switch), the recording of the open test that judging marked as
// the application's background work and that the exchange repeats exactly:
// the same bytes in every request buffer, matched as exact serving matches
// the pool (fuzzyMatch: a buffer the recording marks as noise is compared as
// recorded), against such recordings that already answered in the test's
// window (integrations.AnsweredBackgroundReader). A timer or a start-up job ran more
// often at replay than at record, and its exchange is answered again with
// that recording, unchecked, where it would otherwise be one exchange more
// than the test recorded (extraCall). Never at the per-test pool's
// similarity: an exchange that is only like a marked recording may be the
// test's own, and it stays a miss. nil when it repeats none exactly, or the
// store marks nothing.
func backgroundAgain(reqBuffs [][]byte, mockDb integrations.MockMemDb) *models.Mock {
	br, ok := mockDb.(integrations.AnsweredBackgroundReader)
	if !ok {
		return nil
	}
	var marked []*models.Mock
	for _, mk := range br.AnsweredBackground() {
		if mk.Kind == models.GENERIC {
			marked = append(marked, mk)
		}
	}
	if len(marked) == 0 {
		return nil
	}
	idx := findExactMatch(marked, reqBuffs, true)
	if idx == -1 {
		return nil
	}
	return marked[idx]
}

// reportBackgroundAgain reports an exchange answered again
// (models.AnswerBackgroundAgain): it repeats x, a recording of the open test
// that judging marked as the application's background work and that already
// answered in its window (backgroundAgain). It counts against no test.
func reportBackgroundAgain(ctx context.Context, reqBuffs [][]byte, x *models.Mock) {
	first := 0
	if len(reqBuffs) > 0 {
		first = len(reqBuffs[0])
	}
	r := mismatch.NewReport(mismatch.ProtocolGeneric, fmt.Sprintf("opaque TCP exchange (%d request buffer(s), first %d bytes)", len(reqBuffs), first)).Build()
	r.Answer, r.ClosestMock = models.AnswerBackgroundAgain, x.Name
	models.ReportAnswer(ctx, r)
}

// answeredExchanges are, under exact serving (the proven switch), the open
// test's Generic mocks that have answered, in the order they did: in no read
// any more, each answers once. None when exact serving is off.
func answeredExchanges(mockDb integrations.MockMemDb) []*models.Mock {
	er, ok := mockDb.(integrations.ExactConsumedReader)
	if !ok {
		return nil
	}
	var answered []*models.Mock
	for _, mk := range er.ConsumedTestMocks() {
		if mk.Kind == models.GENERIC {
			answered = append(answered, mk)
		}
	}
	return answered
}

// sameRequests reports whether two recorded exchanges sent the same bytes.
func sameRequests(a, b []models.Payload) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if len(a[i].Message) != len(b[i].Message) {
			return false
		}
		for j := range a[i].Message {
			if a[i].Message[j] != b[i].Message[j] {
				return false
			}
		}
	}
	return true
}

// buildGenericMismatchReport builds the universal mock-miss report for the
// generic (opaque TCP) parser. Field-level diffs are impossible for unparsed
// wire bytes, so the report carries the closest candidate by Jaccard
// similarity with its score — enough for the user to tell "nothing remotely
// similar was recorded" (re-record) apart from "a near-miss exists"
// (protocol drift, likely needs re-record of just this dependency).
//
// answered are, under exact serving, the open test's exchanges that already
// answered (answeredExchanges), which no read shows any more: compared after
// the pool's, so a test's changed exchange is still told how close it is to
// the one it recorded. They are only compared, never served. The caller
// passes none for an extra call.
func buildGenericMismatchReport(ctx context.Context, reqBuffs [][]byte, mockDb integrations.MockMemDb, answered []*models.Mock) *models.MockMismatchReport {
	summary := fmt.Sprintf("opaque TCP exchange (%d request buffer(s), first %d bytes)", len(reqBuffs), func() int {
		if len(reqBuffs) == 0 {
			return 0
		}
		return len(reqBuffs[0])
	}())

	mocks, err := mockDb.GetSessionMocks()
	if err != nil || ctx.Err() != nil {
		return mismatch.NewReport(mismatch.ProtocolGeneric, summary).Build()
	}
	var genericMocks []*models.Mock
	for _, m := range mocks {
		if m.Kind == "Generic" {
			genericMocks = append(genericMocks, m)
		}
	}
	genericMocks = append(genericMocks, answered...)
	if len(genericMocks) == 0 {
		return mismatch.NewReport(mismatch.ProtocolGeneric, summary).
			WithPhase(models.MatchPhaseNoMocks, 0).Build()
	}

	bestIdx, bestSim := -1, -1.0
	var old [][]byte
	for idx, mock := range genericMocks {
		pieces := similarPieces(mock.Spec.GenericRequests, reqBuffs, &old)
		if pieces == nil {
			continue
		}
		var simSum float64
		comparable := true
		for i, reqBuff := range pieces {
			msg := mock.Spec.GenericRequests[i].Message[0]
			// The recorder stores ASCII payloads verbatim (Type String) and
			// binary payloads base64-encoded — decode per the recorded type,
			// otherwise the similarity is computed against nil bytes and the
			// closest-candidate ranking degrades to noise.
			var recorded []byte
			if msg.Type == models.String {
				recorded = []byte(msg.Data)
			} else {
				decoded, err := util.DecodeBase64(msg.Data)
				if err != nil {
					comparable = false
					break
				}
				recorded = decoded
			}
			simSum += fuzzyCheck(recorded, reqBuff)
		}
		if !comparable {
			continue
		}
		if avg := simSum / float64(len(pieces)); avg > bestSim {
			bestSim = avg
			bestIdx = idx
		}
	}

	b := mismatch.NewReport(mismatch.ProtocolGeneric, summary).
		WithPhase(models.MatchPhaseExhausted, len(genericMocks))
	if bestIdx >= 0 {
		// Under exact serving no similarity serves (fuzzyMatch): the
		// thresholds would tell a near-miss it should have been answered.
		served := "thresholds: 0.9 per-test, 0.4 session"
		if integrations.ExactServing(mockDb) {
			served = "exact serving answers only an identical exchange"
		}
		b = b.WithClosest(genericMocks[bestIdx].Name, nil).
			WithDiff(fmt.Sprintf("closest recorded exchange %q has Jaccard similarity %.2f (%s)", genericMocks[bestIdx].Name, bestSim, served))
	} else {
		b = b.WithDiff("no recorded exchange has the same number of request buffers")
	}
	return b.Build()
}

// TODO: need to generalize this function for different types of integrations.
func findBinaryMatch(tcsMocks []*models.Mock, reqBuffs [][]byte, mxSim float64) int {
	// TODO: need find a proper similarity index to set a benchmark for matching or need to find another way to do approximate matching
	mxIdx := -1
	var old [][]byte
	for idx, mock := range tcsMocks {
		if pieces := similarPieces(mock.Spec.GenericRequests, reqBuffs, &old); pieces != nil {
			nc := util.NewNoiseChecker(mock.Noise)
			var simSum float64
			var simCount int
			for requestIndex, reqBuff := range pieces {
				mockData := mock.Spec.GenericRequests[requestIndex].Message[0].Data

				// Skip noisy (obfuscated) buffers — don't let them influence similarity
				if nc != nil && nc.IsNoisy(mockData) {
					continue
				}

				encoded, _ := util.DecodeBase64(mockData)

				similarity := fuzzyCheck(encoded, reqBuff)
				simSum += similarity
				simCount++
			}
			// Compute average similarity across non-noisy buffers.
			// If all buffers are noisy, treat as neutral (1.0) so the
			// mock remains matchable — schema/length already matched.
			if simCount > 0 {
				avgSim := simSum / float64(simCount)
				if avgSim > mxSim {
					mxSim = avgSim
					mxIdx = idx
				}
			} else if len(pieces) > 0 {
				// All buffers were noisy — neutral match
				if 1.0 > mxSim {
					mxSim = 1.0
					mxIdx = idx
				}
			}
		}
	}
	return mxIdx
}

func fuzzyCheck(encoded, reqBuf []byte) float64 {
	k := util.AdaptiveK(len(reqBuf), 3, 8, 5)
	shingles1 := util.CreateShingles(encoded, k)
	shingles2 := util.CreateShingles(reqBuf, k)
	similarity := util.JaccardSimilarity(shingles1, shingles2)
	return similarity
}

// findExactMatch returns the index of the first mock whose recorded request
// is reqBuffs' bytes, or -1. The pieces a recording holds need not be the
// pieces this replay read: a read returns what has arrived, at most
// pUtil.ReadChunkSize, and the old read loop joined what arrived together
// into one piece (a 40 KiB request recorded whole is read here as 32 KiB and
// 8 KiB). So a mock whose pieces are not reqBuffs' is compared with
// reqBuffs' bytes cut where its own pieces end (cutLikeRecorded).
//
// Without exact serving a recorded piece that matches the mock's Noise is
// skipped, so any live piece matches it. For Generic, Noise lists the
// obfuscator's redactions (keploy writes none): a piece recorded wholly as a
// redaction is one whose secret was taken out. Under exact serving (exact,
// the proven switch) that is no excuse: the piece is compared as recorded,
// as every other one is, so a request that does not send those bytes misses
// (keploy/unmocked_call). Otherwise a one-piece recording of a redacted
// token would answer any one-piece request to its destination, and an
// exchange whose token piece changed would be served the recorded answer.
// keploy has no way to redact a live piece as the recording was, so under
// exact serving such a recording fails closed: the same token misses too.
// Under exact serving the pieces are compared as bytes (isRecordedBytes),
// never as the text the recording stores them in.
func findExactMatch(tcsMocks []*models.Mock, reqBuffs [][]byte, exact bool) int {
	req := newSentRequest(reqBuffs)
	for idx, mock := range tcsMocks {
		var nc *util.NoiseChecker
		if !exact {
			nc = util.NewNoiseChecker(mock.Noise)
		}
		if req.isRecorded(mock.Spec.GenericRequests, nc, exact) {
			return idx
		}
	}
	return -1
}

// isRecorded reports whether the request is recorded's bytes, in its own
// pieces or cut where recorded's end (cutLikeRecorded), a noisy
// (obfuscated) recorded piece not compared where nc names it (nil: every
// piece compared). With exact, each piece is compared as bytes
// (sameAsRecorded).
func (r *sentRequest) isRecorded(recorded []models.Payload, nc *util.NoiseChecker, exact bool) bool {
	if len(recorded) == len(r.pieces) && sameAsRecorded(recorded, r.pieces, nc, exact) {
		return true
	}
	pieces := r.cutLikeRecorded(recorded)
	return pieces != nil && sameAsRecorded(recorded, pieces, nc, exact)
}

// sameAsRecorded reports whether each piece is the recorded piece at its
// index. With nc set (no exact serving), a noisy (obfuscated) recorded piece
// is not compared. Under exact serving each piece must be the recorded
// piece's bytes (isRecordedBytes).
func sameAsRecorded(recorded []models.Payload, pieces [][]byte, nc *util.NoiseChecker, exact bool) bool {
	for i, piece := range pieces {
		if exact {
			if !isRecordedBytes(recorded[i], piece) {
				return false
			}
			continue
		}
		mockData := recorded[i].Message[0].Data
		if nc != nil && nc.IsNoisy(mockData) {
			continue
		}
		// The recorder keeps an ASCII piece as it is, any other base64
		// encoded (encodePayload): the piece is compared in that form.
		bufStr := string(piece)
		if !util.IsASCII(bufStr) {
			bufStr = util.EncodeBase64(piece)
		}
		if mockData != bufStr {
			return false
		}
	}
	return true
}

// isRecordedBytes reports whether piece is the bytes the recorded piece p
// holds: its message's Data for a piece the recorder kept as text
// (models.String), what that Data decodes from base64 to for one it kept as
// binary (encodePayload). The text is not compared with the piece's own
// encoding: a live binary piece whose base64 is a recorded text piece, or a
// live text piece that is a recorded binary piece's base64, is other bytes (a
// client that stops base64-wrapping what it sent, or starts). A piece of any
// other type, or binary data that is not base64, holds no bytes keploy can
// compare, so it differs. So does a recorded piece that holds no message, or
// more than one: encodePayload writes exactly one, and comparing only the
// first of several would answer a live piece that sent part of what was
// recorded (a hand-edited or imported mock).
func isRecordedBytes(p models.Payload, piece []byte) bool {
	if len(p.Message) != 1 {
		return false
	}
	msg := p.Message[0]
	switch msg.Type {
	case models.String:
		return msg.Data == string(piece)
	case binaryPayload:
		b, err := util.DecodeBase64(msg.Data)
		return err == nil && bytes.Equal(b, piece)
	}
	return false
}

// sentRequest is what a connection sent for one exchange, in the pieces its
// reads returned, and those pieces joined, made once when first asked.
type sentRequest struct {
	pieces [][]byte
	total  int
	joined []byte
}

func newSentRequest(pieces [][]byte) *sentRequest {
	r := &sentRequest{pieces: pieces}
	for _, p := range pieces {
		r.total += len(p)
	}
	return r
}

func (r *sentRequest) bytes() []byte {
	if r.joined == nil {
		if len(r.pieces) == 1 {
			r.joined = r.pieces[0]
		} else {
			r.joined = bytes.Join(r.pieces, nil)
		}
	}
	return r.joined
}

// cutLikeRecorded returns the request's bytes cut where recorded's pieces
// end, to be compared with them piece by piece. nil when the request is not
// as long as the recording (it cannot be the same bytes), when a recorded
// piece's length is unknown, or when the request's pieces already end where
// the recording's do (compared as they are).
//
// A noisy recorded piece is cut at its recorded (obfuscated) length: an
// obfuscation that changed a piece's length leaves the recording to be
// compared piece by piece only, as before.
func (r *sentRequest) cutLikeRecorded(recorded []models.Payload) [][]byte {
	if len(recorded) == 0 {
		return nil
	}
	sum, aligned := 0, len(recorded) == len(r.pieces)
	for i, p := range recorded {
		n := recordedPieceLen(p)
		if n < 0 {
			return nil
		}
		sum += n
		aligned = aligned && n == len(r.pieces[i])
	}
	if aligned || sum != r.total {
		return nil
	}
	b := r.bytes()
	out := make([][]byte, len(recorded))
	off := 0
	for i, p := range recorded {
		n := recordedPieceLen(p)
		out[i] = b[off : off+n]
		off += n
	}
	return out
}

// recordedPieceLen is how many bytes a recorded request piece holds: its
// data's length for a piece kept as text (models.String), the length its
// base64 decodes to for any other (encodePayload; the replay decodes a
// response piece the same way). -1 when that data is not base64 of a whole
// number of bytes, or the piece holds no message. A piece holding more than
// one message is cut at its first's length, as before: under exact serving
// the piece cut there is then refused (isRecordedBytes), and with the switch
// off it is compared with that first message, so switch-off is unchanged.
func recordedPieceLen(p models.Payload) int {
	if len(p.Message) == 0 {
		return -1
	}
	msg := p.Message[0]
	if msg.Type == models.String {
		return len(msg.Data)
	}
	d := msg.Data
	if len(d)%4 != 0 {
		return -1
	}
	n := len(d) / 4 * 3
	switch {
	case strings.HasSuffix(d, "=="):
		n -= 2
	case strings.HasSuffix(d, "="):
		n--
	}
	return n
}

// similarPieces is reqBuffs in pieces as many as recorded's, to be compared
// with them piece by piece for a similarity: reqBuffs as they were read, or,
// for a recording the old read loop made, as it would have read them
// (coalescedLikeOldReads, made once into *old). nil when neither has as many
// pieces. Unlike an exact match (cutLikeRecorded), a similarity never cuts the
// bytes where a recording's pieces end: a recording in other pieces than
// either is compared with no request it was not compared with before.
func similarPieces(recorded []models.Payload, reqBuffs [][]byte, old *[][]byte) [][]byte {
	if len(recorded) == len(reqBuffs) {
		return reqBuffs
	}
	if *old == nil {
		*old = coalescedLikeOldReads(reqBuffs)
	}
	if len(recorded) == len(*old) {
		return *old
	}
	return nil
}

// coalescedLikeOldReads is pieces as the old read loop would have returned
// them, for the similarity of a recording it made. It read on while a read
// filled its buffer, so what had arrived together came back as one piece; a
// read now returns at most pUtil.ReadChunkSize, and a piece that long is
// joined with the pieces after it, up to and with the first shorter one.
func coalescedLikeOldReads(pieces [][]byte) [][]byte {
	out := make([][]byte, 0, len(pieces))
	for i := 0; i < len(pieces); {
		j := i
		for j < len(pieces)-1 && len(pieces[j]) == pUtil.ReadChunkSize {
			j++
		}
		if j == i {
			out = append(out, pieces[i])
		} else {
			out = append(out, bytes.Join(pieces[i:j+1], nil))
		}
		i = j + 1
	}
	return out
}
