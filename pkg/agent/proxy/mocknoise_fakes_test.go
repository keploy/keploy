package proxy

// MockNoise stubs for the test doubles in this package. The Integrations
// interface now requires MockNoise(); these fakes do no request-body noise, so
// they return nil (the same explicit opt-out a blob/canonical parser uses).
// Value receivers so the method is in both the value and pointer method sets.

import "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"

func (panickedParser) MockNoise() mocknoise.Adapter        { return nil }
func (relayParser) MockNoise() mocknoise.Adapter           { return nil }
func (h2Probe) MockNoise() mocknoise.Adapter               { return nil }
func (fakeParser) MockNoise() mocknoise.Adapter            { return nil }
func (legacyOnlyParser) MockNoise() mocknoise.Adapter      { return nil }
func (*recordingMySQLParser) MockNoise() mocknoise.Adapter { return nil }
func (stubParser) MockNoise() mocknoise.Adapter            { return nil }
func (ctxProbe) MockNoise() mocknoise.Adapter              { return nil }
func (starttlsParser) MockNoise() mocknoise.Adapter        { return nil }
func (*decliningParser) MockNoise() mocknoise.Adapter      { return nil }
func (*feedProbe) MockNoise() mocknoise.Adapter            { return nil }
func (*heldParser) MockNoise() mocknoise.Adapter           { return nil }
func (*holdWantingParser) MockNoise() mocknoise.Adapter    { return nil }
func (returnsAfterOneParser) MockNoise() mocknoise.Adapter { return nil }
