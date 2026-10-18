package mock

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// epilogueRead runs read, an agent read made after the test command exited.
// With the proven switch off it is bounded by agentEpilogueTimeout, as every
// such read was; with it on, it has no deadline of any length (design rule
// 15): the agent's answer, the connection's close or ctx ends it, or the
// user. ctx may be one an interrupt does not cancel (a record's persistCtx,
// so a stopped recording is still checked). The user ends it as a
// recording's drain is ended (utils.StopNowNotice): every mockDrainNotice it
// says it is still waiting (waiting), and once the run has been interrupted,
// that another interrupt stops it now and what that costs (stopNow); from
// then the next one does (utils.StopNow): read's context is cancelled, and
// the error read returns says why. An interrupt before that offer is not the
// user's answer to it: a stop sent to both sudo and keploy reaches keploy
// twice.
//
// read returns once its context is done, as an agent call does, and has
// returned when this does.
func epilogueRead(ctx context.Context, logger *zap.Logger, waiting, stopNow string, read func(context.Context) error) error {
	if !provenswitch.On() {
		ctx, cancel := context.WithTimeout(ctx, agentEpilogueTimeout)
		defer cancel()
		return read(ctx)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	done := make(chan error, 1)
	go func() { done <- read(ctx) }()
	notice := utils.NewStopNowNotice(mockDrainNotice)
	defer notice.Withdraw()
	notices := time.NewTicker(mockDrainNotice)
	defer notices.Stop()
	for {
		select {
		case err := <-done:
			if err != nil && context.Cause(ctx) != nil && context.Cause(ctx) != ctx.Err() {
				err = fmt.Errorf("%w (%v)", err, context.Cause(ctx))
			}
			return err
		case <-utils.StopNow():
			cancel(errors.New("the read was given up at the user's interrupt"))
			if err := <-done; err != nil {
				return fmt.Errorf("%w (%v)", err, context.Cause(ctx))
			}
			return nil // it answered as it was given up
		case <-notices.C:
			msg := waiting
			if notice.Next() {
				msg += "; " + stopNow
			}
			logger.Info(msg)
		}
	}
}
