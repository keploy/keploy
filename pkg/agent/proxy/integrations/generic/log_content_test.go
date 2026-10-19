package generic

import (
	"context"
	"net"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils/logcontent/logtest"
)

// canary is in the bytes of an exchange of a protocol keploy does not parse.
const canary = "kp-canary-generic-8e3c"

// A miss is logged with the size of what came and the closest recording's
// name, not its bytes, unless KEPLOY_LOG_CONTENT is set.
func TestDecodeGeneric_AMissIsLoggedWithoutItsBytes(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		store := answeredStore{answered: []*models.Mock{exchange("mock-1", "SET k "+canary+"-recorded")}}
		app, proxySide := net.Pipe()
		defer app.Close()
		logger, logs := logtest.Observe()
		if err := decodeGeneric(context.Background(), logger, []byte("SET k "+canary+"-live"), proxySide, nil, store, models.OutgoingOptions{}); err == nil {
			t.Fatal("the exchange was answered")
		}
		logtest.Check(t, logs, canary, on, "no matching generic mock found for outgoing call")
	})
}
