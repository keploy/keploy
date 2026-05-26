package agent

import (
	"context"
	"io"

	"go.keploy.io/server/v3/pkg/models"
)

type Service interface {
	Setup(ctx context.Context, startCh chan int) error
	StartIncomingProxy(ctx context.Context, opts models.IncomingOptions) (chan *models.TestCase, error) // Commenting out this for now need to move this and the instrument in the agent setup only
	GetOutgoing(ctx context.Context, opts models.OutgoingOptions) (<-chan *models.Mock, error)
	GetMapping(ctx context.Context) (<-chan models.TestMockMapping, error)
	MockOutgoing(ctx context.Context, opts models.OutgoingOptions) error
	SetMocks(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error
	GetConsumedMocks(ctx context.Context) ([]models.MockState, error)
	GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error)
	StoreMocks(ctx context.Context, filtered []*models.Mock, unFiltered []*models.Mock) error
	UpdateMockParams(ctx context.Context, params models.MockFilterParams) error
	// SetGracefulShutdown sets a flag to indicate the application is shutting down gracefully.
	// When this flag is set, connection errors will be logged as debug instead of error.
	SetGracefulShutdown(ctx context.Context) error
	// SubscribePcap synchronously subscribes w to the proxy's packet
	// broadcaster. Returns an unsub func and nil on success; returns
	// an error (and nil unsub) when capture is not active. Unlike
	// StreamPcap, this does NOT block — it lets the caller write the
	// correct HTTP status code before the long-lived relay begins.
	SubscribePcap(w io.Writer, flush func()) (func(), error)
	// StreamPcap subscribes w to the proxy's packet broadcaster and
	// blocks until ctx is cancelled or the proxy stops capturing.
	// flush, when non-nil, runs after each packet so chunked HTTP
	// responses push bytes per-frame. Returns nil on graceful end
	// (subscription unwound, ctx cancelled). Errors when capture is
	// not active.
	StreamPcap(ctx context.Context, w io.Writer, flush func()) error
	// StreamKeylog subscribes w to the package-level NSS keylog
	// fanout and blocks until ctx is cancelled. Lines arrive
	// asynchronously as TLS handshakes happen on the proxy.
	StreamKeylog(ctx context.Context, w io.Writer) error
	// SendKtInfo(ctx context.Context, tb models.TestBenchReq) error
}
