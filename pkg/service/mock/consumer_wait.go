package mock

import (
	"time"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// streamConsumer is one of a run's stream consumers that keploy waits for
// once it has stopped capturing: it ends (done) once it has dealt with
// everything the agent handed over to it.
type streamConsumer struct {
	// what it takes ("mocks", "cases", "requests"), for the notices.
	what string
	done <-chan struct{}
	// taken and handled count what it took off its stream and what it has
	// finished with (written, or failed to be).
	taken, handled func() uint64
}

// waitConsumers waits, once the capture has stopped, for every consumer to
// end. It has no bound of any length: a consumer still writing to a slow disk
// is still working, and what it holds is part of the run, however long the
// write takes. Every mockDrainNotice it says what each consumer still has in
// hand (words.writing), and, once the run has been interrupted
// (utils.StopNowNotice), that another interrupt stops it now and what that
// costs (words.stopNow); from then the next one does, and it returns false:
// what the consumers still held was not written, and the caller fails the run,
// never saving it as whole. Only an interrupt after this wait's own offer ends
// it (utils.StopNowNotice.Interrupt), as the mock store's close does
// (pkg/service/record closeMockStore): one the user gave an earlier wait of
// the stop (the drain, an agent read) was the answer to that wait, and a
// consumer ending as the stream closes is not given up for it. A write that never returns (a stuck
// disk) ends so, at the user's interrupt or the run's own timeout outside
// keploy, never as a run that looks complete.
//
// Before, a consumer that took nothing for 5 s was given up and its run saved
// as whole while it was still writing: the mocks it held were left out of the
// set, and what it counted was read as it wrote it.
func (m *mockService) waitConsumers(words drainWords, consumers ...streamConsumer) (ended bool) {
	allDone := func() bool {
		for _, c := range consumers {
			select {
			case <-c.done:
			default:
				return false
			}
		}
		return true
	}
	if allDone() {
		return true
	}
	notice := utils.NewStopNowNotice(mockDrainNotice)
	defer notice.Withdraw()
	notices := time.NewTicker(mockDrainNotice)
	defer notices.Stop()
	for _, c := range consumers {
		for {
			select {
			case <-c.done:
			case <-notice.Interrupt():
				if allDone() { // it ended as it was given up
					return true
				}
				m.logger.Error(words.notWritten, append(m.consumerFields(consumers), zap.String("next_step", words.nextStep))...)
				return false
			case <-notices.C:
				msg := words.writing
				if notice.Next() {
					msg += "; " + words.stopNow
				}
				m.logger.Info(msg, m.consumerFields(consumers)...)
				continue
			}
			break
		}
	}
	return true
}

// consumerFields says, for each consumer that has not ended, how many of what
// it took it has finished with (Done), and how many it still holds (Left).
func (m *mockService) consumerFields(consumers []streamConsumer) []zap.Field {
	var fields []zap.Field
	for _, c := range consumers {
		select {
		case <-c.done:
			continue
		default:
		}
		// handled first: it never passes taken read after it.
		handled := c.handled()
		taken := c.taken()
		fields = append(fields, zap.Uint64(c.what+"Done", handled), zap.Uint64(c.what+"Left", taken-handled))
	}
	return fields
}
