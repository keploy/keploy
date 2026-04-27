//go:build !linux

package proxy

import (
	"context"
	"errors"
	"io"
)

// pcapBroadcaster is referenced from proxy.go's struct definition on
// every OS; a stub keeps non-Linux builds compiling without dragging
// the afpacket dependency into them. The methods are unreachable
// from these builds because startPacketCapture refuses to install a
// broadcaster — guarded by SubscribePcap returning "not active".
type pcapBroadcaster struct{}

func (b *pcapBroadcaster) subscribe(_ io.Writer, _ func()) (func(), error) {
	return nil, errors.New("packet capture is not active")
}

// startPacketCapture is a no-op outside Linux. afpacket is
// Linux-only; callers should treat this error as "feature
// unavailable" and continue recording without a pcap stream.
func (p *Proxy) startPacketCapture(_ context.Context, _ string) error {
	return errors.New("packet capture is only supported on linux")
}

func (p *Proxy) stopPacketCapture() {}
