//go:build !linux

package record

import "context"

func (r *Recorder) StartNetworkPacketReplay(ctx context.Context) error {
	r.logger.Info("Network packet replay is only supported on Linux systems")
	return nil
}
