package generic

import (
	"fmt"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
)

// exchangeLifetime is the lifetime the Generic recorder states
// (models.MetadataLifetimeKey) for a connection's n-th exchange, from 0. The
// recorder cannot read the protocol, so it takes the connection's first
// exchange for its preamble (a greeting, a handshake, an auth step): session,
// every test may be served it, as every Generic exchange was before. Every
// later exchange is the data of the test it was made in: per-test, owned by
// that test and judged under the proven switch (models.Mock.IsProtocolPhase).
// A handshake longer than one exchange has its later steps owned by the test
// that opened the connection.
//
// The type tag stays "config", and serving does not read the statement
// (models.Mock.DeriveLifetime keeps every Generic mock session): with the
// switch off a recording replays as it did before the recorder stated it.
func exchangeLifetime(n int) string {
	if n == 0 {
		return models.MetadataLifetimeSession
	}
	return models.MetadataLifetimePerTest
}

// notOwnedExchange is, under exact serving (the proven switch), the name of
// the recorded exchange this request is (byte for byte, else at the per-test
// similarity), when the open test does not own it and its recorder stated it
// per-test: a later exchange of a connection recorded in another test's
// window, or between tests. The likely cause of the miss is that at replay
// this test made the call on a connection it opened itself. "" when it is
// none, or exact serving is off.
func notOwnedExchange(reqBuffs [][]byte, mockDb integrations.MockMemDb) string {
	sf, ok := mockDb.(integrations.StagedFinder)
	if !ok {
		return ""
	}
	perTest := func(mk *models.Mock) bool {
		return mk.Spec.Metadata[models.MetadataLifetimeKey] == models.MetadataLifetimePerTest
	}
	if name := sf.FindStagedNotOwned(models.GENERIC, func(mk *models.Mock) bool {
		return perTest(mk) && findExactMatch([]*models.Mock{mk}, reqBuffs) == 0
	}); name != "" {
		return name
	}
	return sf.FindStagedNotOwned(models.GENERIC, func(mk *models.Mock) bool {
		return perTest(mk) && findBinaryMatch([]*models.Mock{mk}, reqBuffs, 0.9) == 0
	})
}

// notOwnedExchangeNextSteps says what a miss on an exchange the test does not
// own (notOwnedExchange) likely means, and what to do.
func notOwnedExchangeNextSteps(name string) string {
	return fmt.Sprintf("This request matches %s, which keploy recorded as a later exchange of a connection outside this test (keploy serves a connection's first exchange, its handshake, to every test, and every later exchange to the test that recorded it). Likely, at replay this test made the call on a connection it opened itself, which it did not do when recording: record again so each test opens the connections it uses, or keep the application's connection reuse as it was when recording. If this is a heartbeat or keepalive on a long-lived connection, it is to be declared session in keploy.yaml, by destination and request prefix: that rule for Generic connections is not available yet, so until it is, a test that sends one where it was not recorded fails.", name)
}
