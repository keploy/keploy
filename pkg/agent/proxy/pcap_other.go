//go:build !linux

package proxy

import (
	"context"
	"errors"
)

// startPacketCapture is a no-op outside Linux. afpacket is Linux-only;
// callers should treat this error as "feature unavailable" and continue
// recording without a pcap file.
func (p *Proxy) startPacketCapture(_ context.Context, _ string) error {
	return errors.New("packet capture is only supported on linux")
}

func (p *Proxy) stopPacketCapture() {}
