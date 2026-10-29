//go:build linux

package proxy

// MockNoise stubs for the linux-only test doubles in this package (see
// mocknoise_fakes_test.go for the rationale).

import "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"

func (matchedPanicParser) MockNoise() mocknoise.Adapter    { return nil }
func (wiringParser) MockNoise() mocknoise.Adapter          { return nil }
func (unansweringParser) MockNoise() mocknoise.Adapter     { return nil }
func (panickingFinderParser) MockNoise() mocknoise.Adapter { return nil }
func (*destEndProbe) MockNoise() mocknoise.Adapter         { return nil }
