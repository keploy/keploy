package mock

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	rec "go.keploy.io/server/v3/pkg/service/record"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/logcontent"
	"go.uber.org/zap"
)

// streamEnder is an agent client whose agent ends a recording's frame streams
// on request, each with a marker once it has put on the stream all it had
// (AgentClient.EndRecordingStreams), and that says whether a stream ended so
// (StreamEnded).
type streamEnder interface {
	EndRecordingStreams(ctx context.Context, token string) error
	StreamEnded(kind models.FrameStream) bool
}

// roundEnd is how a recording round ends the agent's frame streams when the
// agent can end them itself (models.AgentBacklog.EndMarkers): the round's
// streams carry its token (models.StreamEndTokenHeader), the drain notes when
// the agent told it "clear" (endable), and the round then has the agent end
// them and reads each to its marker (endRound): the round ends on the
// marker, every frame the agent put on the stream read, and not on a stream's
// quiet, which a frame still on its way could outlast. An agent that cannot
// end its streams never says it can, and its round ends as before: on quiet,
// with the stream closed here. Each round has its own token: a stream opened
// with an ended one ends at once.
type roundEnd struct {
	ender streamEnder
	token string
	// endable: the drain's last look was "clear" from an agent that ends its
	// streams on request.
	endable bool
}

// newRoundEnd is a round's end by the agent's markers, or nil when inst
// cannot ask for them.
func newRoundEnd(inst any) *roundEnd {
	e, ok := inst.(streamEnder)
	if !ok {
		return nil
	}
	return &roundEnd{ender: e, token: rand.Text()}
}

// on marks the frame streams opened on ctx as the round's.
func (r *roundEnd) on(ctx context.Context) context.Context {
	if r == nil {
		return ctx
	}
	return models.WithStreamEndToken(ctx, r.token)
}

// clear notes the drain's last look: whether the agent said it holds nothing
// more from before the run ended, and can end its streams.
func (r *roundEnd) clear(b models.AgentBacklog, err error) {
	if r == nil {
		return
	}
	r.endable = err == nil && b.Verdict == models.BacklogClear && b.EndMarkers
}

// endsByMarkers reports whether the round ends on the agent's markers
// (endRound): the drain's last look said it may.
func (r *roundEnd) endsByMarkers() bool { return r != nil && r.endable }

// roundStream is one of a round's frame streams: its kind, and what closes
// once its consumer has taken all its reader handed on.
type roundStream struct {
	kind models.FrameStream
	done <-chan struct{}
}

// endRound ends a round the agent has said "clear" for (roundEnd.endsByMarkers):
// it asks the agent to end the round's streams, and waits until each has
// been read to the agent's marker, and all it carried taken by its consumer.
// A frame the agent had handed on and this client had not yet read when it
// said "clear" is read, not lost to the stream's close (models.BacklogClear:
// clear is not delivered). No clock bounds it: the markers, a stream's close,
// the agent's going away, or the user end it.
//
// A stream that closes before its marker is a round the agent left before it
// was through: what it had still to put on the stream is lost. strict (asked
// as each stream is judged, so a stop the user makes meanwhile counts) -- an
// agent this client started, which only it stops (and, recording, which holds
// its streams open for this round) -- makes that the run's failure (lost,
// said at ERROR, keploy's exit code). Otherwise (an agent compose, kubelet or
// the user runs and stops) it is said at WARN, as the drain says such an end.
// An agent this client started that can no longer be reached to ask
// (utils.AgentGone) has died, and its streams close: each is still judged by
// its marker, whichever comes first, so a marker the agent sent before it
// went is read and counts. One something else stops, and an agent that
// answers but does not take the request, leave the streams for the caller to
// close, as before markers, said at WARN: a frame still on its way is lost.
//
// Every mockDrainNotice it says what it is still doing, and, once the run has
// been interrupted, that another interrupt stops it now and what that costs
// (as drainTrailingMocks does); the next one then does, and the round is
// lost. A stop-now the user already took (an earlier read's offer) is not
// waited out: the streams are ended here, as before markers.
func (m *mockService) endRound(r *roundEnd, strict func() bool, streams []roundStream, words drainWords) (lost bool) {
	select {
	case <-utils.StopNow():
		m.logger.Warn("not waiting for the keploy agent to end its streams with their markers: the user's interrupt already gave up waiting on the agent; ending them here, and what was still on its way is lost",
			zap.String("next_step", words.endedHere))
		return false
	default:
	}
	reqCtx, cancelReq := context.WithCancel(context.Background())
	defer cancelReq()
	asked := make(chan error, 1)
	go func() {
		err := errors.New("asking the keploy agent to end its streams failed in keploy itself")
		defer func() { asked <- err }()
		defer utils.Recover(m.logger)
		err = r.ender.EndRecordingStreams(reqCtx, r.token)
	}()
	// gone is the request's error, from an agent this client started that
	// could no longer be reached: its streams are judged by their markers
	// as they close.
	var gone error
	notice := utils.NewStopNowNotice(mockDrainNotice)
	defer notice.Withdraw()
	notices := time.NewTicker(mockDrainNotice)
	defer notices.Stop()
	for _, s := range streams {
		for waiting := true; waiting; {
			select {
			case <-s.done:
				waiting = false
			case err := <-asked:
				asked = nil
				if err == nil {
					continue
				}
				if utils.AgentGone(err) && strict() {
					// The agent died after it said "clear": its streams
					// close, each judged by its marker below.
					gone = err
					m.logger.Debug("could not reach the keploy agent to end its streams; judging each by its marker as it closes", logcontent.Error(err))
					continue
				}
				if utils.AgentGone(err) {
					return m.endedEarly(false, s.kind, words, err)
				}
				m.logger.Warn("the keploy agent did not take the request to end its streams; ending them here, and what was still on its way is lost",
					logcontent.Error(err), zap.String("next_step", words.endedHere))
				return false
			case <-utils.StopNow():
				// A stream that is through goes by its marker, whatever
				// else is ready.
				select {
				case <-s.done:
					waiting = false
				default:
					return m.stoppedNow(words)
				}
			case <-notices.C:
				msg := words.saving
				if notice.Next() {
					msg += "; " + words.stopNow
				}
				m.logger.Info(msg)
			}
		}
		if r.ender.StreamEnded(s.kind) {
			m.logger.Debug(rec.StreamEndedLog, zap.String("stream", string(s.kind)))
			continue
		}
		// A second interrupt that reached the agent too ends its streams as
		// it gives the round up: the user's stop.
		select {
		case <-utils.StopNow():
			return m.stoppedNow(words)
		default:
		}
		if lost := m.endedEarly(strict(), s.kind, words, gone); lost {
			return true
		}
	}
	return false
}

// endedEarly says the agent left a round before it was through (endRound): it
// ended a stream without its marker, or could no longer be reached (err).
// strict makes it the run's failure (lost).
func (m *mockService) endedEarly(strict bool, kind models.FrameStream, words drainWords, err error) (lost bool) {
	fields := []zap.Field{zap.String("stream", string(kind))}
	if err != nil {
		fields = append(fields, logcontent.NamedError("end_request_error", err))
	}
	if !strict {
		m.logger.Warn("the keploy agent ended its stream before its end marker: what it had still to put on the stream, if anything, is lost",
			append(fields, zap.String("next_step", words.notTheAgent))...)
		return false
	}
	m.logger.Error("the keploy agent ended its stream before its end marker, after it said it held nothing more from before the run ended: what it had still to put on the stream is lost",
		append(fields, zap.String("next_step", words.endedEarly))...)
	utils.SetExitCodeOnce(utils.ExitKeployError)
	return true
}
