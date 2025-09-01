package orchestrator

import "context"

type Service interface {
	ReRecord(ctx context.Context) error
	StartNetworkPacketReplay(ctx context.Context) error
}
