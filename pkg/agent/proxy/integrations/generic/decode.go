// Package generic provides functionality for decoding generic dependencies.
package generic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func decodeGeneric(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn net.Conn, dstCfg *models.ConditionalDstCfg, mockDb integrations.MockMemDb, _ models.OutgoingOptions) error {
	genericRequests := [][]byte{reqBuf}
	logger.Debug("Into the generic parser in test mode")
	// The requests it has read are held from the read on: it reads on with
	// a deadline before it matches them, blocked reading while it holds them
	// (integrations.HoldRequest, a test's end count). Released once
	// answered; a miss, returned below, is filed before the connection is
	// let go, which drops the mark.
	release, holding := func() {}, false
	if len(reqBuf) > 0 {
		release, holding = integrations.HoldRequest(ctx), true
	}
	errCh := make(chan error, 1)
	go func(errCh chan error, genericRequests [][]byte) {
		defer pUtil.Recover(logger, clientConn, nil)
		defer close(errCh)
		for {
			// Since protocol packets have to be parsed for checking stream end,
			// clientConnection have deadline for read to determine the end of stream.
			err := clientConn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
			if err != nil {
				utils.LogError(logger, err, "failed to set the read deadline for the client conn")
				return
			}

			// To read the stream of request packets from the client. The
			// connection's end is its EOF: read so that it is returned at once
			// (ReadBytes retries an empty EOF, 50 ms apart, and under this 10
			// ms deadline that retry comes back as a timeout, which hid the
			// close and left the parser polling it for ever).
			ended := false
			for {
				buffer, err := pUtil.ReadBytesUntilEOF(ctx, logger, clientConn)
				netErr, isNet := err.(net.Error)
				timeout := isNet && netErr.Timeout()
				atEOF := errors.Is(err, io.EOF) || (err != nil && err.Error() == "EOF")
				if err != nil && !timeout && !atEOF {
					utils.LogError(logger, err, "failed to read the request message in proxy for generic dependency")
					return
				}
				// What a read returned with its deadline's timeout or its EOF
				// is a request: a request as long as the read buffer (or a
				// multiple) comes back with the next read's timeout, and one
				// a close follows at once (a TLS 1.2 close_notify, or a full
				// buffer's next read) with its EOF. Any other error has
				// returned above.
				if len(buffer) > 0 {
					if !holding {
						release, holding = integrations.HoldRequest(ctx), true
					}
					genericRequests = append(genericRequests, buffer)
				}
				if timeout || atEOF {
					logger.Debug("the timeout for the client read in generic or EOF")
					ended = atEOF
					break
				}
			}

			if len(genericRequests) == 0 {
				if ended {
					logger.Debug("the application closed the generic connection")
					return
				}
				logger.Debug("the generic request buffer is empty")
				continue
			}

			// bestMatchedIndx := 0
			// fuzzy match gives the index for the best matched generic mock
			matched, genericResponses, err := fuzzyMatch(ctx, logger, genericRequests, mockDb)
			if err != nil {
				utils.LogError(logger, err, "error while matching generic mocks")
			}

			if !matched {
				preview := genericRequests[0]
				if len(preview) > 64 {
					preview = preview[:64]
				}
				// Build the universal mismatch report so generic misses show
				// up in the mismatch table / report yaml like HTTP and MySQL
				// misses do, instead of vanishing as a bare error.
				// Under exact serving: one exchange more than the test
				// recorded? Any other miss is compared with the test's
				// answered exchanges too: they are in no read now.
				n := extraCall(genericRequests, mockDb)
				var answered []*models.Mock
				if n == 0 {
					answered = answeredExchanges(mockDb)
				}
				report := buildGenericMismatchReport(ctx, genericRequests, mockDb, answered)
				if n > 0 {
					report.MissKind, report.Recorded = models.MissExtraCall, n
				} else if name := notOwnedExchange(genericRequests, mockDb); name != "" {
					report.ClosestMock, report.NotOwned = name, true
					report.NextSteps = notOwnedExchangeNextSteps(name)
				}
				// Per-call generic detail at Debug; the canonical mock-mismatch
				// WARN is emitted once in proxy.sendMockNotFoundError (covers
				// every parser), so this stays Debug to avoid double-logging.
				logger.Debug("no matching generic mock found for outgoing call",
					zap.String("protocol", report.Protocol),
					zap.Int("requestCount", len(genericRequests)),
					zap.Int("firstRequestBytes", len(genericRequests[0])),
					zap.String("closest", report.ClosestMock),
					zap.String("diff", report.Diff),
					zap.String("next_step", report.NextSteps),
					zap.Binary("preview", preview))
				errCh <- models.NewMockMismatchError(fmt.Errorf("generic: %w", models.ErrNoMockMatched), report)
				return
			}
			for _, genericResponse := range genericResponses {
				encoded := []byte(genericResponse.Message[0].Data)
				if genericResponse.Message[0].Type != models.String {
					encoded, err = util.DecodeBase64(genericResponse.Message[0].Data)
					if err != nil {
						utils.LogError(logger, err, "failed to decode the base64 response")
						return
					}
				}
				_, err := clientConn.Write(encoded)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					utils.LogError(logger, err, "failed to write the response message to the client application")
					return
				}
			}

			// Answered: no request is held until the next one is read.
			release()
			holding = false
			// Clear the genericRequests buffer for the next dependency call
			genericRequests = [][]byte{}
			if ended {
				return
			}
			logger.Debug("the genericRequests after the iteration", zap.Int("length", len(genericRequests)))
		}
	}(errCh, genericRequests)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		if err == io.EOF {
			return nil
		}
		return err
	}
}
